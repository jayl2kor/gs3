// Package awscfg resolves configuration the way botocore 1.33 and the AWS CLI
// 1.31 do: environment variables and the shared config file, in botocore's
// precedence order, including the AWS-CLI-only nested `s3 =` block that the
// Go SDK does not read.
package awscfg

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// iniFile is a parsed INI file: section name -> option name -> value.
// Option names are lower-cased, as Python's configparser does.
type iniFile map[string]map[string]string

// parseINI reads a file with the semantics of Python's
// configparser.RawConfigParser, which is what botocore uses:
//
//   - `key = value` or `key: value`;
//   - full-line comments start with # or ; (no inline comments);
//   - a line indented deeper than its key continues that key's value, which
//     is how the nested `s3 =` block is written.
//
// A missing file yields an empty result and no error.
func parseINI(path string) (iniFile, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return iniFile{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := iniFile{}
	var section map[string]string
	var option string
	var lines map[string]map[string][]string = map[string]map[string][]string{}
	var sectionLines map[string][]string
	indent := 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		value := strings.TrimSpace(line)
		if strings.HasPrefix(value, "#") || strings.HasPrefix(value, ";") {
			continue
		}
		if value == "" {
			// Blank lines are kept inside a multi-line value.
			if section != nil && option != "" {
				sectionLines[option] = append(sectionLines[option], "")
			}
			continue
		}
		curIndent := len(line) - len(strings.TrimLeft(line, " \t"))
		if section != nil && option != "" && curIndent > indent {
			sectionLines[option] = append(sectionLines[option], value)
			continue
		}
		indent = curIndent

		if strings.HasPrefix(value, "[") && strings.Contains(value, "]") {
			name := value[1:strings.LastIndex(value, "]")]
			if _, exists := out[name]; !exists {
				out[name] = map[string]string{}
				lines[name] = map[string][]string{}
			}
			section, sectionLines, option = out[name], lines[name], ""
			continue
		}
		if section == nil {
			return nil, fmt.Errorf("File contains no section headers.\nfile: %q, line: %d\n%q", path, lineNo, line)
		}
		i := strings.IndexAny(value, "=:")
		if i < 0 {
			return nil, fmt.Errorf("Source contains parsing errors: %q\n\t[line %2d]: %q", path, lineNo, line)
		}
		option = strings.ToLower(strings.TrimSpace(value[:i]))
		sectionLines[option] = []string{strings.TrimSpace(value[i+1:])}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	for name, opts := range lines {
		for opt, parts := range opts {
			out[name][opt] = strings.TrimRight(strings.Join(parts, "\n"), " \t\n")
		}
	}
	return out, nil
}

// parseNested parses the body of a nested block ("\nkey = value\n...").
func parseNested(value string) (map[string]string, error) {
	parsed := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("not enough values to unpack (expected 2, got 1)")
		}
		parsed[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return parsed, nil
}

// sectionName returns the name inside a `[kind name]` section header
// ("profile dev", `profile "my profile"`), and whether header is of that kind.
func sectionName(header, kind string) (string, bool) {
	if !strings.HasPrefix(header, kind) {
		return "", false
	}
	parts := shellSplit(header)
	if len(parts) != 2 {
		return "", false
	}
	return parts[1], true
}

// shellSplit is a minimal shlex.split: whitespace-separated words with
// optional single or double quotes.
func shellSplit(s string) []string {
	var parts []string
	var cur strings.Builder
	inWord := false
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				parts = append(parts, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		parts = append(parts, cur.String())
	}
	return parts
}
