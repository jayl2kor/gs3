// Package filter implements --include / --exclude with the semantics of the
// AWS CLI, which matches full paths with Python's fnmatch.
package filter

import (
	"strings"
	"unicode/utf8"
)

// pattern is a compiled fnmatch pattern.
//
// It differs from Go's path.Match in the ways that matter for `aws s3`:
// "*" also matches "/" (so "*.txt" matches at any depth), "[!x]" negates a
// class, and a "[" without a closing "]" is a literal.
type pattern struct {
	tokens []token
}

type tokenKind uint8

const (
	tokLiteral tokenKind = iota
	tokAny               // ?
	tokStar              // *
	tokClass             // [...]
)

type token struct {
	kind    tokenKind
	literal string
	class   charClass
}

type charClass struct {
	negate bool
	// never is the empty class "[]", which matches nothing.
	never  bool
	ranges []runeRange
}

type runeRange struct{ lo, hi rune }

func (c *charClass) matches(r rune) bool {
	if c.never {
		return false
	}
	in := false
	for _, rg := range c.ranges {
		if r >= rg.lo && r <= rg.hi {
			in = true
			break
		}
	}
	return in != c.negate
}

// compile translates a pattern following fnmatch.translate.
func compile(pat string) *pattern {
	p := &pattern{}
	runes := []rune(pat)
	n := len(runes)
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			p.tokens = append(p.tokens, token{kind: tokLiteral, literal: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < n; {
		c := runes[i]
		i++
		switch c {
		case '*':
			flush()
			// Consecutive stars are one star.
			if len(p.tokens) == 0 || p.tokens[len(p.tokens)-1].kind != tokStar {
				p.tokens = append(p.tokens, token{kind: tokStar})
			}
		case '?':
			flush()
			p.tokens = append(p.tokens, token{kind: tokAny})
		case '[':
			// Find the closing bracket: a "!" and then a "]" directly after
			// the opening bracket are part of the class.
			j := i
			if j < n && runes[j] == '!' {
				j++
			}
			if j < n && runes[j] == ']' {
				j++
			}
			for j < n && runes[j] != ']' {
				j++
			}
			if j >= n {
				lit.WriteRune('[')
				continue
			}
			flush()
			p.tokens = append(p.tokens, token{kind: tokClass, class: parseClass(runes[i:j])})
			i = j + 1
		default:
			lit.WriteRune(c)
		}
	}
	flush()
	return p
}

// parseClass interprets the inside of "[...]".
func parseClass(body []rune) charClass {
	var c charClass
	if len(body) > 0 && body[0] == '!' {
		c.negate = true
		body = body[1:]
	}
	if len(body) == 0 {
		// "[]" cannot be written (the "]" would be taken as a member), and
		// "[!]" likewise; this is only reached for an explicitly empty set.
		c.never = !c.negate
		c.negate = false
		if !c.never {
			c.ranges = []runeRange{{0, utf8.MaxRune}}
		}
		return c
	}
	for i := 0; i < len(body); i++ {
		lo := body[i]
		// "a-b" is a range unless the "-" is the first or last member.
		if i+2 < len(body) && body[i+1] == '-' {
			hi := body[i+2]
			if lo <= hi {
				c.ranges = append(c.ranges, runeRange{lo, hi})
			}
			// An empty range (b-a) matches nothing and is dropped.
			i += 2
			continue
		}
		c.ranges = append(c.ranges, runeRange{lo, lo})
	}
	return c
}

// match reports whether the whole of s matches the pattern.
func (p *pattern) match(s string) bool {
	return matchTokens(p.tokens, s)
}

func matchTokens(tokens []token, s string) bool {
	// Iterative matching with a single backtrack point: the most recent
	// star. On a mismatch, let that star swallow one more character.
	ti, si := 0, 0
	starTok, starPos := -1, 0
	for {
		if ti < len(tokens) {
			t := &tokens[ti]
			switch t.kind {
			case tokStar:
				starTok, starPos = ti, si
				ti++
				continue
			case tokLiteral:
				if strings.HasPrefix(s[si:], t.literal) {
					si += len(t.literal)
					ti++
					continue
				}
			case tokAny, tokClass:
				if si < len(s) {
					r, size := utf8.DecodeRuneInString(s[si:])
					if t.kind == tokAny || t.class.matches(r) {
						si += size
						ti++
						continue
					}
				}
			}
		} else if si == len(s) {
			return true
		}
		if starTok < 0 || starPos >= len(s) {
			return false
		}
		_, size := utf8.DecodeRuneInString(s[starPos:])
		starPos += size
		ti, si = starTok+1, starPos
	}
}
