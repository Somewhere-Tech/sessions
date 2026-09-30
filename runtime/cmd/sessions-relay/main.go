package main

import (
	"log"
	"os"

	"github.com/somewhere-tech/sessions/runtime/internal/relaycmd"
)

func main() {
	if err := relaycmd.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}
