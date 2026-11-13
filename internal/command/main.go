// Package command implements the `aws s3` subcommands and the process entry
// point that maps their outcome to the AWS CLI's exit codes.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	_ "time/tzdata" // `ls` prints local time; static binaries may run without a zoneinfo database

	"gs3/internal/awscfg"
	"gs3/internal/cli"
	"gs3/internal/output"
	"gs3/internal/s3client"
)

// Version is the gs3 version, set at build time with -ldflags.
var Version = "dev"

// CompatVersion is the AWS CLI release whose interface gs3 implements.
const CompatVersion = "1.31.13"

// Exit codes used by the AWS CLI.
const (
	exitOK = 0
	// exitFailure: a transfer failed, or `ls` matched nothing.
	exitFailure = 1
	// exitWarning: nothing failed but files were skipped; also argparse
	// usage errors.
	exitWarning = 2
	// exitError: any other error (clidriver.main's catch-all).
	exitError = 255
)

// IO is the process's standard streams.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// env is what a subcommand needs to run.
type env struct {
	ctx      context.Context
	prog     string
	inv      *cli.Invocation
	settings *awscfg.Settings
	io       IO
	getenv   func(string) string
}

// newClient builds the S3 client for this invocation. maxConns sizes the
// connection pool.
func (e *env) newClient(maxConns int) (*s3client.Client, error) {
	return s3client.New(e.ctx, e.clientConfig(maxConns))
}

func (e *env) clientConfig(maxConns int) s3client.Config {
	g := e.inv.Globals
	return s3client.Config{
		Settings:       e.settings,
		VerifySSL:      g.VerifySSL,
		SignRequest:    g.SignRequest,
		Debug:          g.Debug,
		ReadTimeout:    g.ReadTimeout,
		ConnectTimeout: g.ConnectTimeout,
		MaxConns:       maxConns,
		Stderr:         e.io.Stderr,
	}
}

// Main runs gs3 with the given argv (including the program name) and returns
// the process exit code.
func Main(argv []string, stdio IO) int {
	prog := "gs3"
	if len(argv) > 0 {
		prog = filepath.Base(argv[0])
		argv = argv[1:]
	}

	load := cli.NewParamLoader(func() bool {
		s, err := awscfg.Load(os.Getenv, awscfg.Overrides{})
		return err != nil || s.FollowURLParam
	})
	inv, err := cli.Parse(argv, load)
	if err != nil {
		return reportParseError(prog, err, stdio)
	}

	g := inv.Globals
	settings, err := awscfg.Load(os.Getenv, awscfg.Overrides{
		Profile:     g.Profile,
		Region:      g.Region,
		EndpointURL: g.EndpointURL,
		CABundle:    g.CABundle,
	})
	if err != nil {
		return reportError(err, stdio)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := &env{ctx: ctx, prog: prog, inv: inv, settings: settings, io: stdio, getenv: os.Getenv}
	run, ok := subcommands[inv.Subcommand]
	if !ok {
		return reportError(fmt.Errorf("%s is not implemented yet", inv.Subcommand), stdio)
	}
	rc, err := run(e)
	if err != nil {
		return reportError(err, stdio)
	}
	return rc
}

// subcommands maps a subcommand name to its implementation. A returned error
// is printed as "\n<message>\n" with exit status 255, like an exception
// escaping the command in the AWS CLI; expected outcomes are exit codes.
var subcommands = map[string]func(*env) (int, error){
	"ls":      runLs,
	"mb":      runMb,
	"rb":      runRb,
	"presign": runPresign,
	"website": runWebsite,
}

func reportParseError(prog string, err error, stdio IO) int {
	var usage *cli.UsageError
	var help cli.HelpRequest
	switch {
	case errors.As(err, &cli.VersionRequest{}):
		fmt.Fprintf(stdio.Stdout, "gs3/%s (aws-cli/%s compatible) Go/%s %s/%s\n",
			Version, CompatVersion, runtime.Version()[2:], runtime.GOOS, runtime.GOARCH)
		return exitOK
	case errors.As(err, &help):
		printHelp(stdio.Stdout, prog, help.Subcommand)
		return exitOK
	case errors.As(err, &usage):
		fmt.Fprintf(stdio.Stderr, "%s%s: error: %s\n", cli.UsageText(prog), prog, usage.Msg)
		return exitWarning
	}
	return reportError(err, stdio)
}

// reportError is clidriver.main's catch-all: a blank line, the message, exit
// status 255.
func reportError(err error, stdio IO) int {
	msg := output.ErrorMessage(err)
	if output.IsNoCredentials(err) {
		msg += `. You can configure credentials by running "aws configure".`
	}
	fmt.Fprintf(stdio.Stderr, "\n%s\n", msg)
	return exitError
}
