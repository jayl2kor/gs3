// Package cli implements the command-line front end of gs3.
//
// The AWS CLI parses its arguments with Python's argparse, in three passes
// (global options, the service command, the subcommand). Several of argparse's
// quirks are observable and scripts depend on them, so this file is a
// deliberately close port of the relevant parts of CPython 3.11's
// argparse.ArgumentParser.parse_known_args rather than a conventional Go flag
// parser:
//
//   - options and positionals may be interleaved, but a positional that takes
//     N values needs them adjacent (`cp a --recursive b` fails);
//   - unambiguous prefixes of long options are accepted (`--recur`);
//   - `--opt=value` and `--opt value` are both accepted;
//   - nargs='?' / '+' options swallow following non-option tokens;
//   - unknown tokens are returned to the caller instead of failing, which is
//     what lets global options appear anywhere on the command line.
package cli

import (
	"fmt"
	"regexp"
	"strings"
)

// NargsKind mirrors argparse's nargs.
type NargsKind int

const (
	// NargsNone is argparse's default: exactly one value, stored as a scalar.
	NargsNone NargsKind = iota
	// NargsOptional is '?': zero or one value.
	NargsOptional
	// NargsOneOrMore is '+'.
	NargsOneOrMore
	// NargsExact is an integer nargs; the value is stored as a list even
	// when N == 1.
	NargsExact
)

// Action mirrors the argparse actions the AWS CLI uses.
type Action int

const (
	ActStore Action = iota
	ActStoreTrue
	ActStoreFalse
	// ActAppendFilter is awscli's AppendFilter: appends [option, value] to a
	// list shared by --include and --exclude, preserving command-line order.
	ActAppendFilter
	// ActVersion prints the version and exits 0.
	ActVersion
)

// Arg describes one argument, positional or optional.
type Arg struct {
	// Name is the CLI name without leading dashes ("recursive") or the
	// positional's name ("paths").
	Name       string
	Positional bool
	// Dest is the namespace key the value is stored under.
	Dest   string
	Action Action
	Nargs  NargsKind
	// NargsN is the count for NargsExact.
	NargsN int
	// Const is the value used when an NargsOptional option is given without
	// a value. HasConst distinguishes "" from "not set".
	Const    string
	HasConst bool
	// Default is the initial namespace value. nil means None.
	Default any
	// Choices restricts the accepted values when non-nil.
	Choices []string
	// TypeName is awscli's cli_type_name ("string", "integer", "boolean",
	// "blob", "map"). It drives conversion after parsing, not parsing itself.
	TypeName string
}

// optionString returns "--name" for options.
func (a *Arg) optionString() string { return "--" + a.Name }

// displayName is argparse's _get_action_name: what error messages call the
// argument.
func (a *Arg) displayName() string {
	if a.Positional {
		return a.Dest
	}
	return a.optionString()
}

// takesNoValue reports whether the action consumes no command-line values.
func (a *Arg) takesNoValue() bool {
	return a.Action == ActStoreTrue || a.Action == ActStoreFalse || a.Action == ActVersion
}

// required mirrors argparse: positionals are required unless nargs is '?'.
func (a *Arg) required() bool {
	return a.Positional && a.Nargs != NargsOptional
}

// nargsPattern is argparse's _get_nargs_pattern: a regular expression over the
// alphabet {A: argument, O: option, -: the "--" separator}.
func (a *Arg) nargsPattern() string {
	var p string
	switch {
	case a.takesNoValue():
		p = "(-*-*)"
	case a.Nargs == NargsNone:
		p = "(-*A-*)"
	case a.Nargs == NargsOptional:
		p = "(-*A?-*)"
	case a.Nargs == NargsOneOrMore:
		p = "(-*A[A-]*)"
	default:
		p = "(-*" + strings.Join(strings.Split(strings.Repeat("A", a.NargsN), ""), "-*") + "-*)"
	}
	if !a.Positional {
		// "--" is not allowed inside an option's values.
		p = strings.ReplaceAll(p, "-*", "")
		p = strings.ReplaceAll(p, "-", "")
	}
	return p
}

// Namespace holds parsed values keyed by Dest. Values are nil, bool, string,
// []string, or []FilterRule.
type Namespace map[string]any

// FilterRule is one --include/--exclude occurrence, in command-line order.
type FilterRule struct {
	// Option is the full option string: "--include" or "--exclude".
	Option  string
	Pattern string
}

// String returns the string stored under dest, or "" when unset.
func (n Namespace) String(dest string) string {
	s, _ := n[dest].(string)
	return s
}

// IsSet reports whether dest holds a non-nil value.
func (n Namespace) IsSet(dest string) bool { return n[dest] != nil }

// Bool returns the bool stored under dest.
func (n Namespace) Bool(dest string) bool {
	b, _ := n[dest].(bool)
	return b
}

// Strings returns the list stored under dest.
func (n Namespace) Strings(dest string) []string {
	l, _ := n[dest].([]string)
	return l
}

// Filters returns the ordered include/exclude rules.
func (n Namespace) Filters() []FilterRule {
	l, _ := n["filters"].([]FilterRule)
	return l
}

// UsageError is an argparse error: usage text plus "prog: error: msg" on
// stderr, exit status 2.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// versionRequested is returned when a version action fires.
type versionRequested struct{}

func (versionRequested) Error() string { return "version requested" }

// Parser is one argparse.ArgumentParser.
type Parser struct {
	args []*Arg
	// options maps "--name" to its Arg, and optionOrder keeps insertion
	// order, which determines how ambiguity errors list their matches.
	options     map[string]*Arg
	optionOrder []string
}

// NewParser builds a parser from an argument table.
func NewParser(args []*Arg) *Parser {
	p := &Parser{args: args, options: make(map[string]*Arg)}
	for _, a := range args {
		if a.Positional {
			continue
		}
		s := a.optionString()
		if _, dup := p.options[s]; !dup {
			p.optionOrder = append(p.optionOrder, s)
		}
		p.options[s] = a
	}
	return p
}

// optionTuple is argparse's (action, option_string, explicit_arg).
type optionTuple struct {
	arg         *Arg // nil: looks like an option but is unknown to this parser
	option      string
	explicit    string
	hasExplicit bool
}

var negativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// parseOptional is argparse's _parse_optional. ok=false means the string is a
// positional argument.
func (p *Parser) parseOptional(s string) (t optionTuple, ok bool, err error) {
	if s == "" || s[0] != '-' {
		return t, false, nil
	}
	if a, found := p.options[s]; found {
		return optionTuple{arg: a, option: s}, true, nil
	}
	if len(s) == 1 {
		// A lone "-" is the stdin/stdout placeholder.
		return t, false, nil
	}
	if i := strings.IndexByte(s, '='); i >= 0 {
		if a, found := p.options[s[:i]]; found {
			return optionTuple{arg: a, option: s[:i], explicit: s[i+1:], hasExplicit: true}, true, nil
		}
	}

	// Abbreviations: every option the string is a prefix of.
	var matches []optionTuple
	if strings.HasPrefix(s, "--") {
		prefix, explicit, hasExplicit := s, "", false
		if i := strings.IndexByte(s, '='); i >= 0 {
			prefix, explicit, hasExplicit = s[:i], s[i+1:], true
		}
		for _, name := range p.optionOrder {
			if strings.HasPrefix(name, prefix) {
				matches = append(matches, optionTuple{
					arg: p.options[name], option: name, explicit: explicit, hasExplicit: hasExplicit,
				})
			}
		}
	}
	// Single-dash strings never match: every option here starts with "--".
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.option
		}
		return t, false, &UsageError{Msg: fmt.Sprintf(
			"ambiguous option: %s could match %s", s, strings.Join(names, ", "))}
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}

	if negativeNumber.MatchString(s) {
		return t, false, nil
	}
	if strings.Contains(s, " ") {
		return t, false, nil
	}
	return optionTuple{option: s}, true, nil
}

// ParseKnownArgs parses argv and returns the namespace plus the tokens this
// parser did not recognise, in their original order.
func (p *Parser) ParseKnownArgs(argv []string) (Namespace, []string, error) {
	ns := Namespace{}
	for _, a := range p.args {
		if _, exists := ns[a.Dest]; exists {
			continue
		}
		switch {
		case a.Default != nil:
			ns[a.Dest] = a.Default
		case a.Action == ActStoreTrue:
			ns[a.Dest] = false
		case a.Action == ActStoreFalse:
			ns[a.Dest] = true
		default:
			ns[a.Dest] = nil
		}
	}

	// Classify every token: O (option), A (argument) or - (the separator).
	optionAt := map[int]optionTuple{}
	pattern := make([]byte, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			pattern = append(pattern, '-')
			for i++; i < len(argv); i++ {
				pattern = append(pattern, 'A')
			}
			break
		}
		t, isOption, err := p.parseOptional(argv[i])
		if err != nil {
			return nil, nil, err
		}
		if isOption {
			optionAt[i] = t
			pattern = append(pattern, 'O')
		} else {
			pattern = append(pattern, 'A')
		}
	}

	var positionals []*Arg
	for _, a := range p.args {
		if a.Positional {
			positionals = append(positionals, a)
		}
	}

	seen := map[*Arg]bool{}
	var extras []string

	takeAction := func(a *Arg, values []string, option string) error {
		seen[a] = true
		return p.store(ns, a, values, option)
	}

	consumePositionals := func(start int) (int, error) {
		counts := matchPartial(positionals, string(pattern[start:]))
		for i, n := range counts {
			if err := takeAction(positionals[i], argv[start:start+n], ""); err != nil {
				return 0, err
			}
			start += n
		}
		positionals = positionals[len(counts):]
		return start, nil
	}

	consumeOptional := func(start int) (int, error) {
		t := optionAt[start]
		if t.arg == nil {
			extras = append(extras, argv[start])
			return start + 1, nil
		}
		if t.hasExplicit {
			n, err := matchArgument(t.arg, "A")
			if err != nil {
				return 0, err
			}
			if n == 1 {
				return start + 1, takeAction(t.arg, []string{t.explicit}, t.option)
			}
			return 0, argumentError(t.arg, "ignored explicit argument "+pyRepr(t.explicit))
		}
		n, err := matchArgument(t.arg, string(pattern[start+1:]))
		if err != nil {
			return 0, err
		}
		stop := start + 1 + n
		return stop, takeAction(t.arg, argv[start+1:stop], t.option)
	}

	maxOption := -1
	for i := range optionAt {
		if i > maxOption {
			maxOption = i
		}
	}

	start := 0
	for start <= maxOption {
		next := start
		for {
			if _, ok := optionAt[next]; ok {
				break
			}
			next++
		}
		if start != next {
			end, err := consumePositionals(start)
			if err != nil {
				return nil, nil, err
			}
			if end > start {
				start = end
				continue
			}
			start = end
		}
		if _, ok := optionAt[start]; !ok {
			extras = append(extras, argv[start:next]...)
			start = next
		}
		var err error
		if start, err = consumeOptional(start); err != nil {
			return nil, nil, err
		}
	}

	stop, err := consumePositionals(start)
	if err != nil {
		return nil, nil, err
	}
	extras = append(extras, argv[stop:]...)

	var missing []string
	for _, a := range p.args {
		if !seen[a] && a.required() {
			missing = append(missing, a.displayName())
		}
	}
	if len(missing) > 0 {
		return nil, nil, &UsageError{Msg: "the following arguments are required: " + strings.Join(missing, ", ")}
	}
	return ns, extras, nil
}

// store applies an action, converting the raw values the way argparse's
// _get_values does.
func (p *Parser) store(ns Namespace, a *Arg, raw []string, option string) error {
	switch a.Action {
	case ActStoreTrue:
		ns[a.Dest] = true
		return nil
	case ActStoreFalse:
		ns[a.Dest] = false
		return nil
	case ActVersion:
		return versionRequested{}
	}

	// The first "--" among a positional's values is the separator itself.
	values := raw
	for i, v := range raw {
		if v == "--" {
			values = append(append([]string{}, raw[:i]...), raw[i+1:]...)
			break
		}
	}

	for _, v := range values {
		if err := checkChoice(a, v); err != nil {
			return err
		}
	}

	switch {
	case a.Action == ActAppendFilter:
		rules, _ := ns[a.Dest].([]FilterRule)
		ns[a.Dest] = append(rules, FilterRule{Option: option, Pattern: values[0]})
	case a.Nargs == NargsOptional && len(values) == 0:
		if a.Positional {
			ns[a.Dest] = a.Default
		} else if a.HasConst {
			if err := checkChoice(a, a.Const); err != nil {
				return err
			}
			ns[a.Dest] = a.Const
		} else {
			ns[a.Dest] = nil
		}
	case a.Nargs == NargsNone || a.Nargs == NargsOptional:
		ns[a.Dest] = values[0]
	default:
		ns[a.Dest] = append([]string{}, values...)
	}
	return nil
}

// matchArgument is argparse's _match_argument: how many of the following
// tokens an option consumes.
func matchArgument(a *Arg, pattern string) (int, error) {
	m := compilePattern(a.nargsPattern()).FindStringSubmatch(pattern)
	if m == nil {
		var msg string
		switch {
		case a.Nargs == NargsNone:
			msg = "expected one argument"
		case a.Nargs == NargsOptional:
			msg = "expected at most one argument"
		case a.Nargs == NargsOneOrMore:
			msg = "expected at least one argument"
		case a.NargsN == 1:
			msg = "expected 1 argument"
		default:
			msg = fmt.Sprintf("expected %d arguments", a.NargsN)
		}
		return 0, argumentError(a, msg)
	}
	return len(m[1]), nil
}

// matchPartial is argparse's _match_arguments_partial: match as many leading
// positionals as possible against the pattern and return how many tokens each
// one takes.
func matchPartial(positionals []*Arg, pattern string) []int {
	for i := len(positionals); i > 0; i-- {
		var re strings.Builder
		for _, a := range positionals[:i] {
			re.WriteString(a.nargsPattern())
		}
		if m := compilePattern(re.String()).FindStringSubmatch(pattern); m != nil {
			counts := make([]int, i)
			for j := range counts {
				counts[j] = len(m[j+1])
			}
			return counts
		}
	}
	return nil
}

var patternCache = map[string]*regexp.Regexp{}

// compilePattern anchors and caches an nargs pattern. Parsing is
// single-threaded, so the cache needs no lock.
func compilePattern(p string) *regexp.Regexp {
	if re, ok := patternCache[p]; ok {
		return re
	}
	re := regexp.MustCompile("^" + p)
	patternCache[p] = re
	return re
}

func argumentError(a *Arg, msg string) error {
	return &UsageError{Msg: "argument " + a.displayName() + ": " + msg}
}

// checkChoice is awscli's CLIArgParser._check_value, including its layout of
// the valid choices and the "maybe you meant" suggestions.
func checkChoice(a *Arg, value string) error {
	if a.Choices == nil {
		return nil
	}
	for _, c := range a.Choices {
		if c == value {
			return nil
		}
	}
	const perLine = 2
	lines := []string{"Invalid choice, valid choices are:\n"}
	for i := 0; i < len(a.Choices); i += perLine {
		end := min(i+perLine, len(a.Choices))
		cells := make([]string, 0, perLine)
		for _, c := range a.Choices[i:end] {
			cells = append(cells, fmt.Sprintf("%-40s", c))
		}
		lines = append(lines, strings.Join(cells, " | "))
	}
	if close := closeMatches(value, a.Choices, 3, 0.8); len(close) > 0 {
		lines = append(lines, "\n\nInvalid choice: "+pyRepr(value)+", maybe you meant:\n")
		for _, w := range close {
			lines = append(lines, "  * "+w)
		}
	}
	return argumentError(a, strings.Join(lines, "\n"))
}
