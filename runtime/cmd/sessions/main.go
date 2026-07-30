package main

import (
	"io"
	"os"
)

var version = "0.2.14"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	app, err := newApp(args, stdin, stdout, stderr)
	if err != nil {
		writeFailure(stderr, err)
		return exitCode(err)
	}
	defer app.close()
	if err := app.dispatch(); err != nil {
		writeFailure(stderr, err)
		return exitCode(err)
	}
	return app.exitCode
}
