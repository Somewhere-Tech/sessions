//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
)

func TestPTYInputCaptureHelper(t *testing.T) {
	if os.Getenv("SESSIONS_TEST_PTY_CAPTURE") != "1" {
		return
	}
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		os.Exit(2)
	}
	n, err := strconv.Atoi(os.Getenv("SESSIONS_TEST_PTY_BYTES"))
	if err != nil {
		os.Exit(3)
	}
	fmt.Println("READY")
	data := make([]byte, n)
	if _, err := io.ReadFull(os.Stdin, data); err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(os.Getenv("SESSIONS_TEST_PTY_OUTPUT"), data, 0600); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

func TestNativePTYTransportsWholeInputAcrossReadBoundaries(t *testing.T) {
	var cases []string
	for _, size := range []int{1023, 1024, 1025, 4087, 4088, 4089, 4095, 4096, 4097, 8192, 65536} {
		cases = append(cases, "BEGIN"+strings.Repeat("x", size-8)+"END")
	}
	cases = append(cases, strings.Repeat("é🙂\n", 1024))
	for _, text := range cases {
		t.Run(fmt.Sprint(len(text)), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			output := filepath.Join(t.TempDir(), "capture")
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPTYInputCaptureHelper$")
			command.Env = append(os.Environ(), "SESSIONS_TEST_PTY_CAPTURE=1", "SESSIONS_TEST_PTY_BYTES="+strconv.Itoa(len(text)), "SESSIONS_TEST_PTY_OUTPUT="+output)
			process, err := startPlatformChildProcess(command, 120, 40, false)
			if err != nil {
				t.Fatal(err)
			}
			defer process.CloseOutput()
			ready, err := bufio.NewReader(process).ReadString('\n')
			if err != nil || strings.TrimSpace(ready) != "READY" {
				t.Fatalf("ready=%q err=%v", ready, err)
			}
			n, err := process.Write([]byte(text))
			if err != nil || n != len(text) {
				t.Fatalf("write=%d/%d err=%v", n, len(text), err)
			}
			process.Wait(false)
			got, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(got, []byte(text)) {
				t.Fatalf("captured=%d intended=%d err=%v", len(got), len(text), err)
			}
		})
	}
}
