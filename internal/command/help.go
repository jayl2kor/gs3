package command

import (
	"fmt"
	"io"
)

// The AWS CLI renders man pages for `help`. gs3 prints a short summary
// instead and points at the upstream documentation, which applies unchanged.

var subcommandHelp = map[string][2]string{
	"ls":      {"<S3Uri> or NONE", "List S3 objects and common prefixes under a prefix, or all S3 buckets."},
	"cp":      {"<LocalPath> <S3Uri> or <S3Uri> <LocalPath> or <S3Uri> <S3Uri>", "Copy a local file or S3 object to another location locally or in S3."},
	"mv":      {"<LocalPath> <S3Uri> or <S3Uri> <LocalPath> or <S3Uri> <S3Uri>", "Move a local file or S3 object to another location locally or in S3."},
	"sync":    {"<LocalPath> <S3Uri> or <S3Uri> <LocalPath> or <S3Uri> <S3Uri>", "Sync directories and S3 prefixes."},
	"rm":      {"<S3Uri>", "Delete an S3 object."},
	"mb":      {"<S3Uri>", "Create an S3 bucket."},
	"rb":      {"<S3Uri>", "Delete an empty S3 bucket (--force deletes its objects first)."},
	"presign": {"<S3Uri>", "Generate a pre-signed URL for an S3 object."},
	"website": {"<S3Uri>", "Set the website configuration for a bucket."},
}

var helpOrder = []string{"ls", "cp", "mv", "sync", "rm", "mb", "rb", "presign", "website"}

func printHelp(w io.Writer, prog, subcommand string) {
	if h, ok := subcommandHelp[subcommand]; ok {
		fmt.Fprintf(w, "%s s3 %s %s\n\n%s\n\n", prog, subcommand, h[0], h[1])
		fmt.Fprintf(w, "Options are those of AWS CLI %s:\n", CompatVersion)
		fmt.Fprintf(w, "  https://docs.aws.amazon.com/cli/v1/reference/s3/%s.html\n", subcommand)
		return
	}
	fmt.Fprintf(w, "%s %s - a fast, drop-in implementation of the AWS CLI %s `aws s3` commands\n\n", prog, Version, CompatVersion)
	fmt.Fprintf(w, "usage: %s [global options] s3 <command> [parameters]\n\n", prog)
	fmt.Fprintln(w, "commands:")
	for _, name := range helpOrder {
		fmt.Fprintf(w, "  %-8s %s\n", name, subcommandHelp[name][1])
	}
	fmt.Fprintln(w, "\nglobal options:")
	fmt.Fprintln(w, "  --debug  --endpoint-url  --no-verify-ssl  --no-paginate  --output  --query")
	fmt.Fprintln(w, "  --profile  --region  --version  --color  --no-sign-request  --ca-bundle")
	fmt.Fprintln(w, "  --cli-read-timeout  --cli-connect-timeout")
	fmt.Fprintf(w, "\nRun `%s s3 <command> help` for a command, or see\n", prog)
	fmt.Fprintln(w, "  https://docs.aws.amazon.com/cli/v1/reference/s3/")
}
