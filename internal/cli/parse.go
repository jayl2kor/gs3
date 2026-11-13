package cli

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jmespath/go-jmespath"
)

// Error is a failure the AWS CLI reports as "\n<message>\n" on stderr with
// exit status 255 (clidriver.main's catch-all).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Errorf builds an *Error.
func Errorf(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// VersionRequest is returned by Parse when --version was given.
type VersionRequest struct{}

func (VersionRequest) Error() string { return "version requested" }

// HelpRequest is returned by Parse for `help`, `s3 help` and `s3 <cmd> help`.
type HelpRequest struct {
	// Subcommand is "" for top-level or `s3 help`.
	Subcommand string
}

func (HelpRequest) Error() string { return "help requested" }

// Globals are the resolved global options.
type Globals struct {
	Debug bool
	// EndpointURL is "" when --endpoint-url was not given.
	EndpointURL string
	VerifySSL   bool
	Output      string
	Query       string
	Profile     string
	Region      string
	Color       string
	SignRequest bool
	CABundle    string
	// ReadTimeout and ConnectTimeout default to 60s; 0 means no timeout.
	ReadTimeout    time.Duration
	ConnectTimeout time.Duration
}

// Invocation is a fully parsed command line.
type Invocation struct {
	Globals    Globals
	Subcommand string
	Args       Namespace
}

// ParamLoader resolves file://, fileb:// and http(s):// argument values. A nil
// loader disables expansion.
type ParamLoader func(value string) (replacement string, replaced bool, err error)

// Parse runs the AWS CLI's three parsing passes over argv (without the
// program name) and returns the resolved invocation.
//
// The returned error is one of *UsageError (exit 2), *Error (exit 255),
// VersionRequest or HelpRequest.
func Parse(argv []string, load ParamLoader) (*Invocation, error) {
	inv := &Invocation{}
	raw, err := parsePasses(argv, func(globals Namespace) error {
		return resolveGlobals(globals, &inv.Globals)
	})
	if err != nil {
		return nil, err
	}
	if raw.subcommand == "" {
		if len(raw.remaining) > 0 {
			return nil, Errorf("Unknown options: %s", strings.Join(raw.remaining, ","))
		}
		return nil, Errorf("usage: aws [options] <command> <subcommand> [parameters]\naws: error: too few arguments")
	}
	inv.Subcommand = raw.subcommand
	if err := unpackArgs(raw.subcommand, subcommandArgs(raw.subcommand), raw.args, load); err != nil {
		return nil, err
	}
	if len(raw.remaining) > 0 {
		return nil, Errorf("Unknown options: %s", strings.Join(raw.remaining, ","))
	}
	inv.Args = raw.args
	return inv, nil
}

// rawParse is the outcome of the three argparse passes, before any
// validation or type conversion.
type rawParse struct {
	globals Namespace
	// subcommand is "" when the command line named none.
	subcommand string
	args       Namespace
	// remaining holds the tokens no pass recognised.
	remaining []string
}

// parsePasses runs the three passes. afterGlobals, when non-nil, runs between
// pass 1 and pass 2, where the AWS CLI validates global options.
func parsePasses(argv []string, afterGlobals func(Namespace) error) (*rawParse, error) {
	// Pass 1: global options and the command, over the whole command line.
	// Anything it does not know is handed down untouched, which is why global
	// options work in any position.
	command := positional("command", NargsNone, 0)
	command.Choices = append([]string{"s3", "help"}, subcommandOrder...)

	globals, remaining, err := NewParser(append(globalArgs(), command)).ParseKnownArgs(argv)
	if err != nil {
		if _, ok := err.(versionRequested); ok {
			return nil, VersionRequest{}
		}
		if ue, ok := err.(*UsageError); ok && strings.HasPrefix(ue.Msg, "argument command: ") {
			ue.Msg += "\n\ngs3 implements only the `s3` commands of the AWS CLI."
		}
		return nil, err
	}
	if afterGlobals != nil {
		if err := afterGlobals(globals); err != nil {
			return nil, err
		}
	}
	raw := &rawParse{globals: globals}

	switch cmd := globals.String("command"); cmd {
	case "help":
		return nil, HelpRequest{}
	case "s3":
	default:
		// Shorthand: `gs3 sync …` means `gs3 s3 sync …`.
		remaining = append([]string{cmd}, remaining...)
	}

	// Pass 2: the `s3` command, which only picks out the subcommand.
	if len(remaining) == 1 && remaining[0] == "help" {
		return nil, HelpRequest{}
	}
	sub := positional("subcommand", NargsOptional, 0)
	sub.Choices = subcommandOrder
	ns, remaining, err := NewParser([]*Arg{sub}).ParseKnownArgs(remaining)
	if err != nil {
		return nil, err
	}
	raw.subcommand = ns.String("subcommand")
	if raw.subcommand == "" {
		raw.remaining = remaining
		return raw, nil
	}

	// Pass 3: the subcommand's own arguments.
	if len(remaining) == 1 && remaining[0] == "help" {
		return nil, HelpRequest{Subcommand: raw.subcommand}
	}
	raw.args, raw.remaining, err = NewParser(subcommandArgs(raw.subcommand)).ParseKnownArgs(remaining)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// resolveGlobals validates and converts the global options in the order the
// AWS CLI's top-level-args-parsed handlers run.
func resolveGlobals(ns Namespace, g *Globals) error {
	g.Debug = ns.Bool("debug")
	g.VerifySSL = ns.Bool("verify_ssl")
	g.Output = ns.String("output")
	g.Profile = ns.String("profile")
	g.Region = ns.String("region")
	g.Color = ns.String("color")
	g.SignRequest = ns.Bool("sign_request")
	g.CABundle = ns.String("ca_bundle")

	if ns.IsSet("query") {
		g.Query = ns.String("query")
		if _, err := jmespath.Compile(g.Query); err != nil {
			return Errorf("Bad value for --query %s: %s", g.Query, err)
		}
	}
	if ns.IsSet("endpoint_url") {
		g.EndpointURL = ns.String("endpoint_url")
		if u, err := url.Parse(g.EndpointURL); err != nil || u.Scheme == "" {
			return Errorf("Bad value for --endpoint-url %q: scheme is missing.  "+
				"Must be of the form http://<hostname>/ or https://<hostname>/", g.EndpointURL)
		}
	}

	var err error
	if g.ReadTimeout, err = resolveTimeout(ns, "read_timeout"); err != nil {
		return err
	}
	if g.ConnectTimeout, err = resolveTimeout(ns, "connect_timeout"); err != nil {
		return err
	}
	return nil
}

// defaultTimeout is botocore's DEFAULT_TIMEOUT.
const defaultTimeout = 60 * time.Second

func resolveTimeout(ns Namespace, key string) (time.Duration, error) {
	if !ns.IsSet(key) {
		return defaultTimeout, nil
	}
	n, err := pyInt(ns.String(key))
	if err != nil {
		return 0, &Error{Msg: err.Error()}
	}
	// 0 means "no timeout".
	return time.Duration(n) * time.Second, nil
}

// paramfileDisabled lists the arguments awscli exempts from file:// and
// http(s):// expansion (PARAMFILE_DISABLED).
var paramfileDisabled = map[string]bool{
	"cp.website-redirect":   true,
	"mv.website-redirect":   true,
	"sync.website-redirect": true,
}

// unpackArgs is the post-parse step of BasicCommand.__call__: paramfile
// expansion, then conversion by cli_type_name.
func unpackArgs(subcommand string, table []*Arg, ns Namespace, load ParamLoader) error {
	for _, a := range table {
		s, isString := ns[a.Dest].(string)
		if !isString || a.takesNoValue() {
			continue
		}
		// Only scalar string options are expanded. awscli would also expand a
		// one-element list (e.g. the path of `rm`), which then breaks the
		// command; that case is deliberately not reproduced.
		if load != nil && !a.Positional && !paramfileDisabled[subcommand+"."+a.Name] {
			replacement, replaced, err := load(s)
			if err != nil {
				return Errorf("Error parsing parameter '%s': %s", a.optionString(), err)
			}
			if replaced {
				s = replacement
				ns[a.Dest] = s
			}
		}
		switch a.TypeName {
		case "integer":
			n, err := pyInt(s)
			if err != nil {
				return &Error{Msg: err.Error()}
			}
			ns[a.Dest] = n
		case "map":
			m, err := parseMapArg(a, s)
			if err != nil {
				return err
			}
			ns[a.Dest] = m
		}
	}
	return nil
}

// Int returns the integer stored under dest and whether it was set.
func (n Namespace) Int(dest string) (int, bool) {
	v, ok := n[dest].(int)
	return v, ok
}

// Map returns the string map stored under dest (nil when unset).
func (n Namespace) Map(dest string) map[string]string {
	m, _ := n[dest].(map[string]string)
	return m
}

// UsageText is the usage banner printed before an argparse error.
func UsageText(prog string) string {
	return "usage: " + prog + " [options] <command> <subcommand> [<subcommand> ...] [parameters]\n" +
		"To see help text, you can run:\n" +
		"\n" +
		"  " + prog + " help\n" +
		"  " + prog + " <command> help\n" +
		"  " + prog + " <command> <subcommand> help\n"
}
