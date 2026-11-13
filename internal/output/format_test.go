package output

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestHumanReadableSizeMatchesPython compares against values computed by
// awscli's human_readable_size inside the reference image.
func TestHumanReadableSizeMatchesPython(t *testing.T) {
	data, err := os.ReadFile("../../test/compat/python_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Sizes [][]any `json:"human_readable_size"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Sizes) < 20 {
		t.Fatalf("only %d vectors; the file looks truncated", len(vectors.Sizes))
	}
	for _, v := range vectors.Sizes {
		size, want := int64(v[0].(float64)), v[1].(string)
		if got := HumanReadableSize(size); got != want {
			t.Errorf("HumanReadableSize(%d) = %q, Python says %q", size, got, want)
		}
	}
}

func TestReporterExitCodes(t *testing.T) {
	cases := []struct {
		name    string
		results []Result
		want    int
	}{
		{"all succeeded", []Result{{Kind: Queued}, {Kind: Success, TransferType: "upload", Src: "a", Dest: "b"}}, 0},
		{"warning only", []Result{{Kind: Warning, Message: "warning: x"}}, 2},
		{"failure beats warning", []Result{{Kind: Warning, Message: "warning: x"}, {Kind: Failure, TransferType: "upload", Src: "a", Dest: "b", Message: "boom"}}, 1},
		{"fatal error", []Result{{Kind: Fatal, Message: "boom"}}, 1},
		{"dry run", []Result{{Kind: DryRun, TransferType: "upload", Src: "a", Dest: "b"}}, 0},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		r := NewReporter(&out, &errOut, ModeNoProgress)
		for _, res := range c.results {
			r.Send(res)
		}
		if got := r.Close().ExitCode(); got != c.want {
			t.Errorf("%s: exit code %d, want %d", c.name, got, c.want)
		}
	}
}

func TestReporterModes(t *testing.T) {
	results := []Result{
		{Kind: Queued, Size: 3, SizeKnown: true},
		{Kind: Success, TransferType: "upload", Src: "./a", Dest: "s3://b/a"},
		{Kind: Failure, TransferType: "upload", Src: "./x", Dest: "s3://b/x", Message: "boom"},
		{Kind: Warning, Message: "warning: Skipping file y. File does not exist."},
		{Kind: DryRun, TransferType: "delete", Src: "s3://b/z"},
		{Kind: FinalTotal, Total: 2},
	}
	cases := []struct {
		mode    Mode
		wantOut string
		wantErr string
	}{
		{ModeNoProgress, "upload: ./a to s3://b/a\n(dryrun) delete: s3://b/z\n",
			"upload failed: ./x to s3://b/x boom\nwarning: Skipping file y. File does not exist.\n"},
		{ModeOnlyErrors, "(dryrun) delete: s3://b/z\n",
			"upload failed: ./x to s3://b/x boom\nwarning: Skipping file y. File does not exist.\n"},
		// --quiet silences errors and warnings too, as in the AWS CLI.
		{ModeQuiet, "", ""},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		r := NewReporter(&out, &errOut, c.mode)
		for _, res := range results {
			r.Send(res)
		}
		r.Close()
		if out.String() != c.wantOut {
			t.Errorf("mode %d stdout = %q, want %q", c.mode, out.String(), c.wantOut)
		}
		if errOut.String() != c.wantErr {
			t.Errorf("mode %d stderr = %q, want %q", c.mode, errOut.String(), c.wantErr)
		}
	}
}

// A result line that follows a progress line must be padded to overwrite it.
func TestReporterPadsOverProgress(t *testing.T) {
	var out, errOut bytes.Buffer
	r := &Reporter{mode: ModeFull}
	r.out = newTestWriter(&out)
	r.errOut = &errOut
	r.progressLength = 40
	r.printLine("upload: a to b")
	r.out.Flush()
	line := strings.TrimSuffix(out.String(), "\n")
	if len(line) != 40 || !strings.HasPrefix(line, "upload: a to b") {
		t.Errorf("line = %q (len %d), want it padded to 40 columns", line, len(line))
	}
	if r.progressLength != 0 {
		t.Errorf("progressLength = %d after a result line, want 0", r.progressLength)
	}
}
