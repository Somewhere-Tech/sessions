// Command record captures a command's raw PTY output at the mirror dimensions.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"github.com/somewhere-tech/sessions/runtime/internal/mirror"
)

func main() {
	outPath := flag.String("out", "", "recording destination")
	timeout := flag.Duration("timeout", 2*time.Second, "maximum capture duration")
	dir := flag.String("dir", "", "command working directory")
	flag.Parse()
	if *outPath == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: record -out FILE [-timeout DURATION] -- COMMAND [ARG...]")
		os.Exit(2)
	}

	cmd := exec.Command(flag.Arg(0), flag.Args()[1:]...)
	cmd.Dir = *dir
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		fmt.Sprintf("COLUMNS=%d", mirror.DefaultCols),
		fmt.Sprintf("LINES=%d", mirror.DefaultRows),
	)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(mirror.DefaultCols),
		Rows: uint16(mirror.DefaultRows),
	})
	check(err)

	var recording bytes.Buffer
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&recording, ptmx)
		close(copyDone)
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	timedOut := false
	select {
	case <-waitDone:
	case <-time.After(*timeout):
		timedOut = true
		_ = cmd.Process.Kill()
		<-waitDone
	}
	_ = ptmx.Close()
	<-copyDone
	check(os.WriteFile(*outPath, recording.Bytes(), 0o644))
	status := "exited"
	if timedOut {
		status = "timed out (expected for interactive captures)"
	}
	fmt.Printf("%s: %d bytes: %s\n", *outPath, recording.Len(), status)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
