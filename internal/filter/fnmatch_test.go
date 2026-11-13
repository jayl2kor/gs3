package filter

import (
	"encoding/json"
	"os"
	"testing"

	"gs3/internal/core"
)

// TestFnmatchMatchesPython replays name/pattern pairs evaluated by Python's
// fnmatch.fnmatch (test/compat/python_vectors.json, generated inside the
// reference image) and requires the same answers.
func TestFnmatchMatchesPython(t *testing.T) {
	data, err := os.ReadFile("../../test/compat/python_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Fnmatch [][]any `json:"fnmatch"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Fnmatch) < 1000 {
		t.Fatalf("only %d vectors; the file looks truncated", len(vectors.Fnmatch))
	}
	failures := 0
	for _, v := range vectors.Fnmatch {
		name, pat, want := v[0].(string), v[1].(string), v[2].(bool)
		if got := compile(pat).match(name); got != want {
			failures++
			if failures <= 20 {
				t.Errorf("fnmatch(%q, %q) = %v, Python says %v", name, pat, got, want)
			}
		}
	}
	if failures > 20 {
		t.Errorf("... and %d more mismatches", failures-20)
	}
}

func TestFilterLastMatchingRuleWins(t *testing.T) {
	stat := func(src string) *core.FileStat { return &core.FileStat{Src: src} }
	cases := []struct {
		name  string
		rules []Rule
		src   string
		want  bool
	}{
		{"no rules includes everything", nil, "bkt/p/a.txt", true},
		{"exclude all", []Rule{{false, "*"}}, "bkt/p/a.txt", false},
		{"exclude then include", []Rule{{false, "*"}, {true, "*.txt"}}, "bkt/p/a.txt", true},
		{"exclude then include other", []Rule{{false, "*"}, {true, "*.txt"}}, "bkt/p/a.bin", false},
		{"include then exclude", []Rule{{true, "*.txt"}, {false, "*"}}, "bkt/p/a.txt", false},
		{"later exclude overrides include", []Rule{{false, "*"}, {true, "map_*.pcd"}, {false, "map_occ_*.pcd"}}, "bkt/p/map_occ_1.pcd", false},
		{"pattern is rooted at the prefix", []Rule{{false, "a.txt"}}, "bkt/p/sub/a.txt", true},
		{"star crosses directories", []Rule{{false, "*.txt"}}, "bkt/p/sub/a.txt", false},
		{"other prefix is untouched", []Rule{{false, "*"}}, "bkt/other/a.txt", true},
	}
	for _, c := range cases {
		f := New(c.rules, "s3://bkt/p/", "s3://dst/q/", true)
		if got := f.Match(stat(c.src)); got != c.want {
			t.Errorf("%s: Match(%q) = %v, want %v", c.name, c.src, got, c.want)
		}
	}
}

// The same rules must also apply to the destination listing of a sync, so
// that excluded files are never deleted by --delete.
func TestFilterAppliesToDestinationRoot(t *testing.T) {
	f := New([]Rule{{false, "*.tmp"}}, "s3://bkt/p/", "s3://dst/q/", true)
	if f.Match(&core.FileStat{Src: "dst/q/old.tmp"}) {
		t.Error("destination file matching an exclude rule was not filtered out")
	}
	if !f.Match(&core.FileStat{Src: "dst/q/keep.txt"}) {
		t.Error("destination file not matching any rule was filtered out")
	}
}
