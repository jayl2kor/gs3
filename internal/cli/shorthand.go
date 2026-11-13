package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// parseMapArg converts the value of a map-typed option (--metadata) the way
// the AWS CLI does: JSON when it starts with "{" or "[", otherwise the
// shorthand syntax `k=v,k2=v2`. The result must be a map of strings.
func parseMapArg(a *Arg, value string) (map[string]string, error) {
	if value == "" {
		return nil, nil
	}
	name := a.optionString()
	var parsed any
	if t := strings.TrimSpace(value); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		if !strings.HasPrefix(strings.TrimLeftFunc(value, unicode.IsSpace), "{") {
			return nil, Errorf("Error parsing parameter '%s': Invalid JSON:\n%s", name, value)
		}
		dec := json.NewDecoder(strings.NewReader(value))
		dec.UseNumber()
		if err := dec.Decode(&parsed); err != nil {
			return nil, Errorf("Error parsing parameter '%s': Invalid JSON: %s\nJSON received: %s", name, err, value)
		}
	} else {
		var err error
		if parsed, err = (&shorthandParser{input: []rune(value)}).parse(); err != nil {
			return nil, Errorf("Error parsing parameter '%s': %s", name, err)
		}
	}

	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, Errorf("Parameter validation failed:\nInvalid type for parameter %s, value: %s, type: %s, valid types: <class 'dict'>",
			a.Name, pyReprAny(parsed), pyTypeName(parsed))
	}
	out := make(map[string]string, len(obj))
	var problems []string
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, isString := obj[k].(string)
		if !isString {
			problems = append(problems, fmt.Sprintf(
				"Invalid type for parameter %s.%s, value: %s, type: %s, valid types: <class 'str'>",
				a.Name, k, pyReprAny(obj[k]), pyTypeName(obj[k])))
			continue
		}
		out[k] = s
	}
	if len(problems) > 0 {
		return nil, Errorf("Parameter validation failed:\n%s", strings.Join(problems, "\n"))
	}
	return out, nil
}

func pyTypeName(v any) string {
	switch v.(type) {
	case string:
		return "<class 'str'>"
	case []any:
		return "<class 'list'>"
	case map[string]any:
		return "<class 'dict'>"
	case bool:
		return "<class 'bool'>"
	case nil:
		return "<class 'NoneType'>"
	case json.Number:
		if strings.ContainsAny(v.(json.Number).String(), ".eE") {
			return "<class 'float'>"
		}
		return "<class 'int'>"
	}
	return fmt.Sprintf("<class '%T'>", v)
}

// pyReprAny is repr() for the value shapes JSON and shorthand can produce.
func pyReprAny(v any) string {
	switch t := v.(type) {
	case string:
		return pyRepr(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyReprAny(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyRepr(k) + ": " + pyReprAny(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	case json.Number:
		return t.String()
	}
	return fmt.Sprint(v)
}

// shorthandParser is a port of awscli.shorthand.ShorthandParser.
//
//	parameter = keyval *("," keyval)
//	keyval    = key "=" [values]
//	values    = csv-list / explicit-list / hash-literal
type shorthandParser struct {
	input []rune
	index int
}

const shorthandEOF = rune(-1)

type shorthandSyntaxError struct {
	value    []rune
	expected string
	actual   string
	index    int
}

func (e *shorthandSyntaxError) Error() string {
	value := string(e.value)
	consumed, remaining, spaces := value, "", e.index
	head := string(e.value[:e.index])
	if i := strings.LastIndex(head, "\n"); i >= 0 {
		spaces = len([]rune(head[i+1:]))
	}
	tail := string(e.value[e.index:])
	if i := strings.Index(tail, "\n"); i >= 0 {
		consumed = head + tail[:i]
		remaining = tail[i:]
	}
	return fmt.Sprintf("Expected: '%s', received: '%s' for input:\n%s\n%s^%s",
		e.expected, e.actual, consumed, strings.Repeat(" ", spaces), remaining)
}

func (p *shorthandParser) parse() (any, error) {
	params := map[string]any{}
	key, val, err := p.keyval()
	if err != nil {
		return nil, err
	}
	params[key] = val
	last := p.index
	for p.index < len(p.input) {
		if err := p.expect(',', true); err != nil {
			return nil, err
		}
		key, val, err := p.keyval()
		if err != nil {
			return nil, err
		}
		if _, dup := params[key]; dup {
			return nil, fmt.Errorf(
				"Second instance of key \"%s\" encountered for input:\n%s\n%s^\nThis is often because there is a preceding \",\" instead of a space.",
				key, string(p.input), strings.Repeat(" ", last+1))
		}
		params[key] = val
		last = p.index
	}
	return params, nil
}

func (p *shorthandParser) keyval() (string, any, error) {
	key := p.key()
	if err := p.expect('=', true); err != nil {
		return "", nil, err
	}
	v, err := p.values()
	return key, v, err
}

func (p *shorthandParser) key() string {
	start := p.index
	for !p.atEOF() {
		c := p.current()
		isKeyChar := c < 0x80 && (unicode.IsLetter(c) || unicode.IsDigit(c) || strings.ContainsRune("-_.#/:", c))
		if !isKeyChar {
			break
		}
		p.index++
	}
	return string(p.input[start:p.index])
}

func (p *shorthandParser) values() (any, error) {
	switch {
	case p.atEOF():
		return "", nil
	case p.current() == '[':
		return p.explicitList()
	case p.current() == '{':
		return p.hashLiteral()
	}
	return p.csvValue()
}

// csvValue parses `bar` (a scalar) or `a,b,c` (a list). `a,b=c` is the scalar
// `a` followed by another key: on a syntax error it backtracks to the comma.
func (p *shorthandParser) csvValue() (any, error) {
	first, err := p.firstValue()
	if err != nil {
		return nil, err
	}
	p.consumeWhitespace()
	if p.atEOF() || p.current() != ',' {
		return first, nil
	}
	if err := p.expect(',', true); err != nil {
		return nil, err
	}
	list := []any{first}
	for {
		cur, err := p.secondValue()
		if err == nil {
			p.consumeWhitespace()
			if p.atEOF() {
				list = append(list, cur)
				break
			}
			err = p.expect(',', true)
			if err == nil {
				list = append(list, cur)
				continue
			}
		}
		if p.atEOF() {
			return nil, err
		}
		p.backtrackTo(',')
		break
	}
	if len(list) == 1 {
		return first, nil
	}
	return list, nil
}

func (p *shorthandParser) explicitList() (any, error) {
	if err := p.expect('[', true); err != nil {
		return nil, err
	}
	values := []any{}
	for p.current() != ']' {
		v, err := p.explicitValues()
		if err != nil {
			return nil, err
		}
		values = append(values, v)
		p.consumeWhitespace()
		if p.current() != ']' {
			if err := p.expect(',', false); err != nil {
				return nil, err
			}
			p.consumeWhitespace()
		}
	}
	return values, p.expect(']', false)
}

func (p *shorthandParser) explicitValues() (any, error) {
	switch p.current() {
	case '[':
		return p.explicitList()
	case '{':
		return p.hashLiteral()
	}
	return p.firstValue()
}

func (p *shorthandParser) hashLiteral() (any, error) {
	if err := p.expect('{', true); err != nil {
		return nil, err
	}
	keyvals := map[string]any{}
	for p.current() != '}' {
		key := p.key()
		if err := p.expect('=', true); err != nil {
			return nil, err
		}
		v, err := p.explicitValues()
		if err != nil {
			return nil, err
		}
		p.consumeWhitespace()
		if p.current() != '}' {
			if err := p.expect(',', false); err != nil {
				return nil, err
			}
			p.consumeWhitespace()
		}
		keyvals[key] = v
	}
	return keyvals, p.expect('}', false)
}

func (p *shorthandParser) firstValue() (any, error) {
	switch p.current() {
	case '\'':
		return p.quoted('\'', "singled quoted")
	case '"':
		return p.quoted('"', "double quoted")
	}
	// An unmatched first value is the empty string, not an error.
	s, _ := p.word(isFirstFollow)
	return s, nil
}

func (p *shorthandParser) secondValue() (any, error) {
	switch p.current() {
	case '\'':
		return p.quoted('\'', "singled quoted")
	case '"':
		return p.quoted('"', "double quoted")
	}
	s, ok := p.word(isSecondFollow)
	if !ok {
		return nil, &shorthandSyntaxError{value: p.input, expected: "<second>", actual: "<none>", index: p.index}
	}
	return s, nil
}

// word consumes an unquoted value: a start character followed by any number
// of follow characters, where `\,` is an escaped comma in either position.
func (p *shorthandParser) word(follow func(rune) bool) (string, bool) {
	start := p.index
	step := func(ok func(rune) bool) bool {
		if p.index+1 < len(p.input) && p.input[p.index] == '\\' && p.input[p.index+1] == ',' {
			p.index += 2
			return true
		}
		if p.index < len(p.input) && ok(p.input[p.index]) {
			p.index++
			return true
		}
		return false
	}
	if !step(isStartWord) {
		return "", false
	}
	for step(follow) {
	}
	s := strings.ReplaceAll(string(p.input[start:p.index]), `\,`, ",")
	return strings.TrimRightFunc(s, unicode.IsSpace), true
}

// quoted consumes '...' or "...", where the quote and the backslash can be
// backslash-escaped.
func (p *shorthandParser) quoted(q rune, name string) (any, error) {
	i := p.index + 1
	for i < len(p.input) {
		switch {
		case p.input[i] == '\\' && i+1 < len(p.input) && (p.input[i+1] == '\\' || p.input[i+1] == q):
			i += 2
		case p.input[i] == q:
			raw := string(p.input[p.index+1 : i])
			p.index = i + 1
			raw = strings.ReplaceAll(raw, `\`+string(q), string(q))
			return strings.ReplaceAll(raw, `\\`, `\`), nil
		default:
			i++
		}
	}
	return nil, &shorthandSyntaxError{value: p.input, expected: "<" + name + ">", actual: "<none>", index: p.index}
}

func (p *shorthandParser) expect(c rune, whitespace bool) error {
	if whitespace {
		p.consumeWhitespace()
	}
	if p.index >= len(p.input) {
		return &shorthandSyntaxError{value: p.input, expected: string(c), actual: "EOF", index: p.index}
	}
	if actual := p.input[p.index]; actual != c {
		return &shorthandSyntaxError{value: p.input, expected: string(c), actual: string(actual), index: p.index}
	}
	p.index++
	if whitespace {
		p.consumeWhitespace()
	}
	return nil
}

func (p *shorthandParser) current() rune {
	if p.index < len(p.input) {
		return p.input[p.index]
	}
	return shorthandEOF
}

func (p *shorthandParser) atEOF() bool { return p.index >= len(p.input) }

func (p *shorthandParser) backtrackTo(c rune) {
	for p.index >= 0 && (p.index >= len(p.input) || p.input[p.index] != c) {
		p.index--
	}
}

func (p *shorthandParser) consumeWhitespace() {
	for p.index < len(p.input) && strings.ContainsRune(" \t\n\r\x0b\x0c", p.input[p.index]) {
		p.index++
	}
}

func inRanges(r rune, ranges ...rune) bool {
	for i := 0; i+1 < len(ranges); i += 2 {
		if r >= ranges[i] && r <= ranges[i+1] {
			return true
		}
	}
	return false
}

// The three character classes of awscli.shorthand, as rune ranges.

// isStartWord: anything printable except " ' , = [ {
func isStartWord(r rune) bool {
	return inRanges(r, '!', '!', '#', '&', '(', '+', '-', '<', '>', 'Z', '\\', 'z', '|', 0xffff)
}

// isFirstFollow: whitespace or anything except " ' , ] }
func isFirstFollow(r rune) bool {
	return unicode.IsSpace(r) ||
		inRanges(r, '!', '!', '#', '&', '(', '+', '-', '\\', '^', '|', '~', 0xffff)
}

// isSecondFollow: whitespace or anything except " ' , =
func isSecondFollow(r rune) bool {
	return unicode.IsSpace(r) ||
		inRanges(r, '!', '!', '#', '&', '(', '+', '-', '<', '>', 0xffff)
}
