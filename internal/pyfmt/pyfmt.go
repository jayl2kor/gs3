// Package pyfmt formats values the way Python does, for the places where the
// AWS CLI's output embeds a Python repr() or str() of an exception.
package pyfmt

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// Repr renders s the way Python's repr() renders a str. AWS CLI error
// messages embed repr()s ("ignored explicit argument 'yes'"), so matching the
// quoting rules keeps those messages identical.
func Repr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == utf8.RuneError:
			b.WriteString(`�`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0x7f && !unicode.IsPrint(r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// OSError renders a file-system error like str(OSError) in Python:
// "[Errno 2] No such file or directory: 'path'". ok is false when err does
// not carry an errno.
func OSError(err error) (msg string, ok bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", false
	}
	text := errno.Error()
	if text != "" {
		text = strings.ToUpper(text[:1]) + text[1:]
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), text, Repr(pathErr.Path)), true
	}
	return fmt.Sprintf("[Errno %d] %s", int(errno), text), true
}
