package main

import (
	"io"
	"os"

	"coder/internal/bench"
	"coder/internal/cli"
)

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return cli.Execute(args, stdin, stdout, stderr, bench.RunCLI)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
