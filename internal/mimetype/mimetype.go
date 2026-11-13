// Package mimetype guesses a Content-Type from a file name the way Python's
// mimetypes.guess_type does, because that is what the AWS CLI uses and the
// result is stored with every uploaded object.
//
// The built-in table is CPython 3.11's. Like Python, the system mime.types
// files are consulted too, so gs3 and the AWS CLI agree when run in the same
// image.
package mimetype

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

var (
	loadOnce sync.Once
	types    map[string]string
)

func load() {
	types = make(map[string]string, len(defaultTypes)+64)
	for ext, t := range defaultTypes {
		types[ext] = t
	}
	for _, path := range knownFiles {
		readMimeFile(path)
	}
}

// readMimeFile parses a mime.types file: "type ext ext ...", "#" comments.
// Later files override earlier ones, as in Python.
func readMimeFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		words := strings.Fields(sc.Text())
		for i, w := range words {
			if strings.HasPrefix(w, "#") {
				words = words[:i]
				break
			}
		}
		if len(words) < 2 {
			continue
		}
		for _, suffix := range words[1:] {
			types["."+suffix] = words[0]
		}
	}
}

// Guess returns the MIME type for name, or "" when it cannot be guessed (in
// which case no Content-Type is sent and S3 applies its default).
func Guess(name string) string {
	loadOnce.Do(load)

	// Python parses the name as a URL first: with a scheme of two or more
	// characters only the path component is considered.
	if path, ok := urlPath(name); ok {
		name = path
	}

	base, ext := splitExt(name)
	for {
		mapped, ok := suffixMap[strings.ToLower(ext)]
		if !ok {
			break
		}
		base, ext = splitExt(base + mapped)
	}
	if _, isEncoding := encodingsMap[ext]; isEncoding {
		_, ext = splitExt(base)
	}
	return types[strings.ToLower(ext)]
}

// splitExt is posixpath.splitext: the extension starts at the last dot of the
// final path component, and leading dots do not start an extension.
func splitExt(p string) (root, ext string) {
	slash := strings.LastIndexByte(p, '/')
	dot := strings.LastIndexByte(p, '.')
	if dot <= slash {
		return p, ""
	}
	// Skip the leading dots of the file name (".bashrc" has no extension).
	for i := slash + 1; i < dot; i++ {
		if p[i] != '.' {
			return p[:dot], p[dot:]
		}
	}
	return p, ""
}

// urlPath mimics urllib.parse.urlparse far enough to find the path of a name
// that looks like "scheme://host/path?query#fragment".
func urlPath(name string) (string, bool) {
	i := strings.IndexByte(name, ':')
	if i < 2 {
		return "", false
	}
	scheme := name[:i]
	for j, c := range scheme {
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !(isAlpha || (j > 0 && ((c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'))) {
			return "", false
		}
	}
	rest := name[i+1:]
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		if k := strings.IndexAny(rest, "/?#"); k >= 0 {
			rest = rest[k:]
		} else {
			rest = ""
		}
	}
	if k := strings.IndexByte(rest, '#'); k >= 0 {
		rest = rest[:k]
	}
	if k := strings.IndexByte(rest, '?'); k >= 0 {
		rest = rest[:k]
	}
	return rest, true
}
