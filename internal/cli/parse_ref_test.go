package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// refParse is one record of test/compat/parse_expected.json: what the real
// AWS CLI 1.31.13 parsers produced for an argv (see
// deploy/awscli-ref/dump_parse.py).
type refParse struct {
	Argv []string `json:"argv"`
	OK   *struct {
		Globals    map[string]any `json:"globals"`
		Subcommand *string        `json:"subcommand"`
		Args       map[string]any `json:"args"`
		Remaining  []string       `json:"remaining"`
	} `json:"ok"`
	Error *struct {
		Exit    int    `json:"exit"`
		Message string `json:"message"`
	} `json:"error"`
}

// normalize renders a namespace value in a form comparable across the two
// implementations: Python ints and strings compare by their text, filter
// rules become [option, pattern] pairs.
func normalize(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case bool:
		return t
	case string:
		return t
	case float64:
		return fmt.Sprint(int64(t))
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	case []FilterRule:
		out := make([]any, len(t))
		for i, r := range t {
			out[i] = []any{r.Option, r.Pattern}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalize(e)
		}
		return out
	}
	return fmt.Sprintf("%v", v)
}

func diffNamespace(got Namespace, want map[string]any, ignore ...string) []string {
	skip := map[string]bool{}
	for _, k := range ignore {
		skip[k] = true
	}
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		if !skip[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)

	var diffs []string
	for _, k := range names {
		g, gok := got[k]
		w, wok := want[k]
		if gok != wok {
			diffs = append(diffs, fmt.Sprintf("%s: present got=%v want=%v", k, gok, wok))
			continue
		}
		if ng, nw := normalize(g), normalize(w); !reflect.DeepEqual(ng, nw) {
			diffs = append(diffs, fmt.Sprintf("%s: got %#v, want %#v", k, ng, nw))
		}
	}
	return diffs
}

// TestParseMatchesReference replays every recorded command line through
// gs3's parser and requires the same namespaces, leftovers and error messages
// as the real CLI.
func TestParseMatchesReference(t *testing.T) {
	data, err := os.ReadFile("../../test/compat/parse_expected.json")
	if err != nil {
		t.Fatalf("reading reference results: %v", err)
	}
	var cases []refParse
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parsing reference results: %v", err)
	}
	if len(cases) < 100 {
		t.Fatalf("only %d reference cases; the file looks truncated", len(cases))
	}

	for _, c := range cases {
		name := strings.Join(c.Argv, " ")
		t.Run(name, func(t *testing.T) {
			raw, err := parsePasses(c.Argv, nil)

			if c.Error != nil {
				switch c.Error.Exit {
				case 0:
					if _, ok := err.(VersionRequest); !ok {
						t.Fatalf("want version request, got %v (%T)", err, err)
					}
				case 2:
					ue, ok := err.(*UsageError)
					if !ok {
						t.Fatalf("want usage error %q, got %v (%T)", c.Error.Message, err, err)
					}
					if ue.Msg != c.Error.Message {
						t.Fatalf("usage error differs\n got: %q\nwant: %q", ue.Msg, c.Error.Message)
					}
				default:
					t.Fatalf("unexpected reference exit status %d", c.Error.Exit)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v (%T)", err, err)
			}
			// "version" exists only in gs3's namespace: argparse suppresses
			// the dest of a version action.
			if d := diffNamespace(raw.globals, c.OK.Globals, "version"); len(d) > 0 {
				t.Errorf("global options differ:\n  %s", strings.Join(d, "\n  "))
			}
			wantSub := ""
			if c.OK.Subcommand != nil {
				wantSub = *c.OK.Subcommand
			}
			if raw.subcommand != wantSub {
				t.Errorf("subcommand: got %q, want %q", raw.subcommand, wantSub)
			}
			if c.OK.Args != nil {
				if d := diffNamespace(raw.args, c.OK.Args); len(d) > 0 {
					t.Errorf("arguments differ:\n  %s", strings.Join(d, "\n  "))
				}
			}
			if got, want := strings.Join(raw.remaining, "\x00"), strings.Join(c.OK.Remaining, "\x00"); got != want {
				t.Errorf("remaining: got %q, want %q", raw.remaining, c.OK.Remaining)
			}
		})
	}
}
