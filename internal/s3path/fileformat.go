package s3path

import (
	"os"
	"path/filepath"
	"strings"

	"gs3/internal/core"
)

// Location is one side of a transfer.
type Location struct {
	// Path is an absolute local path, or "bucket/key" for S3. It ends with a
	// separator when the location is a directory or prefix.
	Path string
	Type core.PathType
}

// Files is awscli's FileFormat result: the normalised source and destination
// plus the two flags that decide how destination names are derived.
type Files struct {
	Src  Location
	Dest Location
	// DirOp is true when operating on a directory / prefix (--recursive, or
	// sync).
	DirOp bool
	// UseSrcName is true when the destination keeps the source's file name
	// instead of the name given on the command line.
	UseSrcName bool
}

const sep = string(os.PathSeparator)

// Format is awscli's FileFormat.format.
func Format(src, dest string, dirOp bool) Files {
	srcLoc, _ := formatPath(src, dirOp)
	destLoc, useSrcName := formatPath(dest, dirOp)
	return Files{Src: srcLoc, Dest: destLoc, DirOp: dirOp, UseSrcName: useSrcName}
}

func formatPath(path string, dirOp bool) (Location, bool) {
	if IsS3(path) {
		p := strings.TrimPrefix(path, Scheme)
		// A prefix operation, or a path written with a trailing slash, keeps
		// the source's name.
		if dirOp && !strings.HasSuffix(p, "/") {
			p += "/"
		}
		return Location{Path: p, Type: core.S3}, strings.HasSuffix(p, "/")
	}

	full := AbsPath(path)
	isDir := false
	if st, err := os.Stat(full); err == nil && st.IsDir() {
		isDir = true
	}
	if isDir || dirOp || strings.HasSuffix(path, sep) {
		return Location{Path: withTrailingSep(full), Type: core.Local}, true
	}
	return Location{Path: full, Type: core.Local}, false
}

func withTrailingSep(p string) string {
	if strings.HasSuffix(p, sep) {
		return p
	}
	return p + sep
}

// AbsPath is os.path.abspath: the path made absolute against the working
// directory and normalised, without resolving symlinks.
func AbsPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

// DestAndCompareKey is awscli's find_dest_path_comp_key: where a source path
// lands at the destination, and the key sync compares it by.
func (f Files) DestAndCompareKey(srcPath string) (dest, compareKey string) {
	srcSep, destSep := "/", "/"
	if f.Src.Type == core.Local {
		srcSep = sep
	}
	if f.Dest.Type == core.Local {
		destSep = sep
	}

	var rel string
	if f.DirOp {
		rel = srcPath[len(f.Src.Path):]
	} else {
		rel = srcPath[strings.LastIndex(srcPath, srcSep)+1:]
	}
	compareKey = rel
	if srcSep != "/" {
		compareKey = strings.ReplaceAll(rel, srcSep, "/")
	}
	dest = f.Dest.Path
	if f.UseSrcName {
		if srcSep != destSep {
			rel = strings.ReplaceAll(rel, srcSep, destSep)
		}
		dest += rel
	}
	return dest, compareKey
}

// RelativePath is awscli's relative_path: how a local path is shown in
// result lines, relative to cwd ("./a.txt", "sub/a.txt", "../x/a.txt").
func RelativePath(filename, cwd string) string {
	i := strings.LastIndex(filename, sep)
	if i < 0 {
		return AbsPath(filename)
	}
	dir, base := filename[:i], filename[i+1:]
	if dir == "" {
		dir = sep
	}
	rel, err := filepath.Rel(cwd, dir)
	if err != nil {
		return AbsPath(filename)
	}
	return rel + sep + base
}

// DisplayS3 renders "bucket/key" as an S3 URI.
func DisplayS3(path string) string {
	if IsS3(path) {
		return path
	}
	return Scheme + path
}
