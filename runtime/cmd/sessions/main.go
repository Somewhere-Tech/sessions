package main

import (
	"io"
	"os"
)

var version = "0.2.19"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// The JSON preference is read from the raw arguments so that a failure
	// during construction is still reported in the format the caller asked
	// for. app may not exist yet at that point.
	_, _, _, wantJSON := parseGlobalArgs(args)
	app, err := newApp(args, stdin, stdout, stderr)
	if err != nil {
		writeFailure(stdout, stderr, wantJSON, err)
		return exitCode(err)
	}
	defer app.close()
	if err := app.dispatch(); err != nil {
		app.reportFailure(err)
		return exitCode(err)
	}
	return app.exitCode
}
