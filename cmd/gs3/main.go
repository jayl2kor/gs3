// Command gs3 is a fast, drop-in implementation of the AWS CLI 1.31.13
// `aws s3` commands.
package main

import (
	"os"

	"gs3/internal/command"
)

func main() {
	os.Exit(command.Main(os.Args, command.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}
