package filter

import (
	"os"
	"strings"

	"gs3/internal/core"
	"gs3/internal/s3path"
)

// Rule is one --include or --exclude occurrence.
type Rule struct {
	Include bool
	Pattern string
}

// Filter decides which files a command touches.
//
// Every file starts out included. The rules are then applied in command-line
// order and each rule that matches overrides the decision so far, so the last
// matching rule wins. A pattern is anchored at the root of the source (and,
// for the destination listing of a sync, at the root of the destination):
// `--exclude "*"` means "everything under the root".
type Filter struct {
	rules []compiledRule
}

type compiledRule struct {
	include bool
	src     *pattern
	dst     *pattern
}

const sep = string(os.PathSeparator)

// New builds a filter for the given command-line paths. src and dest are the
// paths as typed; dirOp is whether the command works on a directory/prefix.
func New(rules []Rule, src, dest string, dirOp bool) *Filter {
	if len(rules) == 0 {
		return &Filter{}
	}
	srcRoot, dstRoot := root(src, dirOp), root(dest, dirOp)
	f := &Filter{rules: make([]compiledRule, len(rules))}
	for i, r := range rules {
		f.rules[i] = compiledRule{
			include: r.Include,
			src:     compile(joinPath(srcRoot, r.Pattern, s3path.IsS3(src))),
			dst:     compile(joinPath(dstRoot, r.Pattern, s3path.IsS3(dest))),
		}
	}
	return f
}

// root is the directory patterns are evaluated against.
func root(location string, dirOp bool) string {
	if s3path.IsS3(location) {
		bucket, key := s3path.SplitS3BucketKey(location)
		if !dirOp && !strings.HasSuffix(key, "/") {
			// For a single object the root is the prefix that contains it.
			if i := strings.LastIndex(key, "/"); i >= 0 {
				key = key[:i]
			} else {
				key = ""
			}
		}
		return bucket + "/" + key
	}
	if dirOp {
		return s3path.AbsPath(location)
	}
	// os.path.dirname, then abspath.
	dir := ""
	if i := strings.LastIndex(location, sep); i >= 0 {
		dir = location[:i+1]
		if trimmed := strings.TrimRight(dir, sep); trimmed != "" {
			dir = trimmed
		}
	}
	return s3path.AbsPath(dir)
}

// joinPath is os.path.join(root, pattern), with separators normalised for the
// side being matched.
func joinPath(rootDir, pat string, isS3 bool) string {
	joinSep := sep
	if isS3 {
		joinSep = "/"
		if sep != "/" {
			pat = strings.ReplaceAll(pat, sep, "/")
		}
	} else if sep != "/" {
		pat = strings.ReplaceAll(pat, "/", sep)
	}
	switch {
	case strings.HasPrefix(pat, sep):
		// An absolute pattern replaces the root.
		return pat
	case rootDir == "" || strings.HasSuffix(rootDir, sep):
		return rootDir + pat
	}
	return rootDir + joinSep + pat
}

// Match reports whether the file passes the filter.
func (f *Filter) Match(fs *core.FileStat) bool {
	included := true
	for i := range f.rules {
		r := &f.rules[i]
		if r.src.match(fs.Src) {
			included = r.include
		}
		if r.dst.match(fs.Src) {
			included = r.include
		}
	}
	return included
}

// Empty reports whether the filter has no rules.
func (f *Filter) Empty() bool { return len(f.rules) == 0 }
