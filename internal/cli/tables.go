package cli

import "strings"

// The argument tables below mirror AWS CLI 1.31.13 exactly. tables_test.go
// compares them with a dump taken from the real CLI
// (test/compat/argtable-1.31.13.json), so a missing or mistyped flag fails the
// build rather than surfacing in production.

func dest(name string) string { return strings.ReplaceAll(name, "-", "_") }

// boolFlag is a store_true option.
func boolFlag(name string) *Arg {
	return &Arg{Name: name, Dest: dest(name), Action: ActStoreTrue, TypeName: "boolean"}
}

// negFlag is a store_false option writing to another option's dest.
func negFlag(name, to string) *Arg {
	return &Arg{Name: name, Dest: to, Action: ActStoreFalse, TypeName: "boolean"}
}

// strOpt is an option taking exactly one value.
func strOpt(name string, choices ...string) *Arg {
	a := &Arg{Name: name, Dest: dest(name), TypeName: "string"}
	if len(choices) > 0 {
		a.Choices = choices
	}
	return a
}

// typedOpt is strOpt with a non-string cli_type_name.
func typedOpt(name, typeName string) *Arg {
	a := strOpt(name)
	a.TypeName = typeName
	return a
}

// optValue is an nargs='?' option: `--sse` alone means konst.
func optValue(name, konst string, choices ...string) *Arg {
	return &Arg{
		Name: name, Dest: dest(name), Nargs: NargsOptional,
		Const: konst, HasConst: true, Choices: choices, TypeName: "string",
	}
}

func filterOpt(name string) *Arg {
	return &Arg{Name: name, Dest: "filters", Action: ActAppendFilter, Nargs: NargsExact, NargsN: 1, TypeName: "string"}
}

func positional(name string, nargs NargsKind, n int) *Arg {
	return &Arg{Name: name, Dest: name, Positional: true, Nargs: nargs, NargsN: n, TypeName: "string"}
}

var aclChoices = []string{
	"private", "public-read", "public-read-write", "authenticated-read",
	"aws-exec-read", "bucket-owner-read", "bucket-owner-full-control",
	"log-delivery-write",
}

var storageClassChoices = []string{
	"STANDARD", "REDUCED_REDUNDANCY", "STANDARD_IA", "ONEZONE_IA",
	"INTELLIGENT_TIERING", "GLACIER", "DEEP_ARCHIVE", "GLACIER_IR",
}

// globalArgs are the options accepted anywhere on the command line.
func globalArgs() []*Arg {
	color := strOpt("color", "on", "off", "auto")
	color.Default = "auto"
	return []*Arg{
		boolFlag("debug"),
		strOpt("endpoint-url"),
		negFlag("no-verify-ssl", "verify_ssl"),
		negFlag("no-paginate", "paginate"),
		strOpt("output", "json", "text", "table"),
		strOpt("query"),
		strOpt("profile"),
		strOpt("region"),
		{Name: "version", Dest: "version", Action: ActVersion, TypeName: "string"},
		color,
		negFlag("no-sign-request", "sign_request"),
		strOpt("ca-bundle"),
		{Name: "cli-read-timeout", Dest: "read_timeout", TypeName: "int"},
		{Name: "cli-connect-timeout", Dest: "connect_timeout", TypeName: "int"},
	}
}

// transferArgs is awscli's TRANSFER_ARGS, shared by cp, mv and sync.
func transferArgs() []*Arg {
	followSymlinks := boolFlag("follow-symlinks")
	followSymlinks.Default = true
	noFollowSymlinks := negFlag("no-follow-symlinks", "follow_symlinks")
	noFollowSymlinks.Default = true
	noGuess := negFlag("no-guess-mime-type", "guess_mime_type")
	noGuess.Default = true

	return []*Arg{
		boolFlag("dryrun"),
		boolFlag("quiet"),
		filterOpt("include"),
		filterOpt("exclude"),
		strOpt("acl", aclChoices...),
		followSymlinks,
		noFollowSymlinks,
		noGuess,
		optValue("sse", "AES256", "AES256", "aws:kms"),
		optValue("sse-c", "AES256", "AES256"),
		typedOpt("sse-c-key", "blob"),
		strOpt("sse-kms-key-id"),
		optValue("sse-c-copy-source", "AES256", "AES256"),
		typedOpt("sse-c-copy-source-key", "blob"),
		strOpt("storage-class", storageClassChoices...),
		{Name: "grants", Dest: "grants", Nargs: NargsOneOrMore, TypeName: "string"},
		strOpt("website-redirect"),
		strOpt("content-type"),
		strOpt("cache-control"),
		strOpt("content-disposition"),
		strOpt("content-encoding"),
		strOpt("content-language"),
		strOpt("expires"),
		strOpt("source-region"),
		boolFlag("only-show-errors"),
		negFlag("no-progress", "progress"),
		typedOpt("page-size", "integer"),
		boolFlag("ignore-glacier-warnings"),
		boolFlag("force-glacier-transfer"),
		optValue("request-payer", "requester", "requester"),
	}
}

func recursiveFlag() *Arg {
	a := boolFlag("recursive")
	a.Dest = "dir_op"
	return a
}

// subcommandOrder is the order `aws s3` registers its subcommands in; it is
// the order an "Invalid choice" error lists them.
var subcommandOrder = []string{"ls", "website", "cp", "mv", "rm", "sync", "mb", "rb", "presign"}

// subcommandArgs returns the argument table of one `aws s3` subcommand, or
// nil for an unknown name.
func subcommandArgs(name string) []*Arg {
	switch name {
	case "ls":
		paths := positional("paths", NargsOptional, 0)
		paths.Default = "s3://"
		return []*Arg{
			paths,
			recursiveFlag(),
			typedOpt("page-size", "integer"),
			boolFlag("human-readable"),
			boolFlag("summarize"),
			optValue("request-payer", "requester", "requester"),
		}
	case "cp":
		args := append([]*Arg{positional("paths", NargsExact, 2)}, transferArgs()...)
		return append(args,
			typedOpt("metadata", "map"),
			strOpt("metadata-directive", "COPY", "REPLACE"),
			strOpt("expected-size"),
			recursiveFlag(),
		)
	case "mv":
		args := append([]*Arg{positional("paths", NargsExact, 2)}, transferArgs()...)
		return append(args,
			typedOpt("metadata", "map"),
			strOpt("metadata-directive", "COPY", "REPLACE"),
			recursiveFlag(),
		)
	case "sync":
		args := append([]*Arg{positional("paths", NargsExact, 2)}, transferArgs()...)
		return append(args,
			typedOpt("metadata", "map"),
			strOpt("metadata-directive", "COPY", "REPLACE"),
			boolFlag("size-only"),
			boolFlag("exact-timestamps"),
			boolFlag("delete"),
		)
	case "rm":
		return []*Arg{
			positional("paths", NargsExact, 1),
			boolFlag("dryrun"),
			boolFlag("quiet"),
			recursiveFlag(),
			optValue("request-payer", "requester", "requester"),
			filterOpt("include"),
			filterOpt("exclude"),
			boolFlag("only-show-errors"),
			typedOpt("page-size", "integer"),
		}
	case "mb":
		return []*Arg{positional("path", NargsNone, 0)}
	case "rb":
		return []*Arg{positional("path", NargsNone, 0), boolFlag("force")}
	case "presign":
		expiresIn := typedOpt("expires-in", "integer")
		expiresIn.Default = "3600"
		return []*Arg{positional("path", NargsNone, 0), expiresIn}
	case "website":
		return []*Arg{
			positional("paths", NargsExact, 1),
			strOpt("index-document"),
			strOpt("error-document"),
		}
	}
	return nil
}

// subcommandUsage is each command's USAGE string, used in "Invalid argument
// type" errors.
var subcommandUsage = map[string]string{
	"ls":      "<S3Uri> or NONE",
	"cp":      "<LocalPath> <S3Uri> or <S3Uri> <LocalPath> or <S3Uri> <S3Uri>",
	"mv":      "<LocalPath> <S3Uri> or <S3Uri> <LocalPath> or <S3Uri> <S3Uri>",
	"sync":    "<LocalPath> <S3Uri> or <S3Uri> <LocalPath> or <S3Uri> <S3Uri>",
	"rm":      "<S3Uri>",
	"mb":      "<S3Uri>",
	"rb":      "<S3Uri>",
	"presign": "<S3Uri>",
	"website": "<S3Uri>",
}

// SubcommandUsage returns a subcommand's usage synopsis.
func SubcommandUsage(name string) string { return subcommandUsage[name] }
