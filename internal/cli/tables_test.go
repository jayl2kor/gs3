package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
)

// refArg is one entry of test/compat/argtable-1.31.13.json, the argument
// tables dumped from the real AWS CLI by deploy/awscli-ref/dump_argtable.py.
type refArg struct {
	Name        string   `json:"name"`
	Positional  bool     `json:"positional"`
	Action      string   `json:"action"`
	Nargs       any      `json:"nargs"`
	Choices     []string `json:"choices"`
	CLITypeName string   `json:"cli_type_name"`
	Default     any      `json:"default"`
	Const       any      `json:"const"`
	Dest        string   `json:"dest"`
}

type refDump struct {
	Version string              `json:"version"`
	Global  []refArg            `json:"global"`
	S3      map[string][]refArg `json:"s3"`
}

func loadRefDump(t *testing.T) refDump {
	t.Helper()
	data, err := os.ReadFile("../../test/compat/argtable-1.31.13.json")
	if err != nil {
		t.Fatalf("reading reference dump: %v", err)
	}
	var d refDump
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("parsing reference dump: %v", err)
	}
	if d.Version != "1.31.13" {
		t.Fatalf("reference dump is for awscli %s, want 1.31.13", d.Version)
	}
	return d
}

// describe renders an argument in a canonical form so that the two sources can
// be compared field by field.
type argShape struct {
	Name, Dest, Action, Nargs, Type, Default, Const string
	Positional                                      bool
	Choices                                         []string
}

func shapeOfRef(r refArg) argShape {
	s := argShape{
		Name: r.Name, Dest: r.Dest, Action: r.Action, Positional: r.Positional,
		Type: r.CLITypeName, Choices: r.Choices,
	}
	if r.Nargs != nil {
		s.Nargs = fmt.Sprint(r.Nargs)
	}
	if r.Default != nil {
		s.Default = fmt.Sprint(r.Default)
	}
	if r.Const != nil {
		s.Const = fmt.Sprint(r.Const)
	}
	return s
}

func shapeOfArg(a *Arg) argShape {
	s := argShape{Name: a.Name, Dest: a.Dest, Positional: a.Positional, Type: a.TypeName, Choices: a.Choices}
	switch a.Action {
	case ActStore:
		s.Action = "store"
	case ActStoreTrue:
		s.Action = "store_true"
	case ActStoreFalse:
		s.Action = "store_false"
	case ActAppendFilter:
		s.Action = "AppendFilter"
	case ActVersion:
		s.Action = "version"
	}
	switch a.Nargs {
	case NargsOptional:
		s.Nargs = "?"
	case NargsOneOrMore:
		s.Nargs = "+"
	case NargsExact:
		s.Nargs = fmt.Sprint(a.NargsN)
	}
	switch d := a.Default.(type) {
	case nil:
	case bool:
		if d {
			s.Default = "true"
		} else {
			s.Default = "false"
		}
	default:
		s.Default = fmt.Sprint(d)
	}
	if a.HasConst {
		s.Const = a.Const
	}
	return s
}

func compareTables(t *testing.T, scope string, got []*Arg, want []refArg) {
	t.Helper()
	if len(got) != len(want) {
		gotNames := make([]string, len(got))
		for i, a := range got {
			gotNames[i] = a.Name
		}
		wantNames := make([]string, len(want))
		for i, r := range want {
			wantNames[i] = r.Name
		}
		t.Fatalf("%s: %d arguments, reference has %d\n got: %v\nwant: %v", scope, len(got), len(want), gotNames, wantNames)
	}
	for i := range want {
		g, w := shapeOfArg(got[i]), shapeOfRef(want[i])
		// The reference dump reports the JSON-level default of the version
		// action and bool defaults in Python's spelling.
		if w.Default == "True" {
			w.Default = "true"
		}
		if w.Type == "" {
			w.Type = "string"
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s argument #%d differs from AWS CLI 1.31.13\n got: %+v\nwant: %+v", scope, i, g, w)
		}
	}
}

func TestGlobalArgsMatchReference(t *testing.T) {
	compareTables(t, "global", globalArgs(), loadRefDump(t).Global)
}

func TestSubcommandArgsMatchReference(t *testing.T) {
	d := loadRefDump(t)

	var refNames []string
	for name := range d.S3 {
		refNames = append(refNames, name)
	}
	sort.Strings(refNames)
	ours := append([]string{}, subcommandOrder...)
	sort.Strings(ours)
	if !reflect.DeepEqual(ours, refNames) {
		t.Fatalf("subcommands differ: got %v, reference %v", ours, refNames)
	}

	for _, name := range refNames {
		compareTables(t, "s3 "+name, subcommandArgs(name), d.S3[name])
		if _, ok := subcommandUsage[name]; !ok {
			t.Errorf("s3 %s has no usage string", name)
		}
	}
}
