package descriptor

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Kind is the JSON type of a Value.
type Kind uint8

// JSON value kinds. Numbers are integers only: no wire document in docs/design/23-release-descriptors.md has a
// fraction or exponent, so the parser rejects them instead of guessing at their canonical form.
const (
	Null Kind = iota
	Bool
	Number
	String
	Array
	Object
)

// Value is one strictly parsed JSON value. Object members keep their input order; Canonical sorts them.
type Value struct {
	Kind    Kind
	Bool    bool
	Int     int64
	Str     string
	Items   []Value
	Members []Member
}

// Member is one object member.
type Member struct {
	Key   string
	Value Value
}

// Limits bound a parse before and while it decodes (§23.1, §23.2, §23.6).
type Limits struct {
	Bytes int // input length
	Depth int // nesting of arrays and objects; a scalar document has depth 0
	Nodes int // values, counting every member value and array item, plus the root
}

// maxSafeInteger is the largest integer RFC 8785 serializes exactly (2^53 - 1).
const maxSafeInteger = 1<<53 - 1

// Parse errors are fixed: they never quote input (§23.4).
var (
	ErrTooLarge     = errors.New("JSON exceeds its size limit")
	ErrTooDeep      = errors.New("JSON exceeds its nesting limit")
	ErrTooManyNodes = errors.New("JSON exceeds its node limit")
	ErrSyntax       = errors.New("invalid JSON")
	ErrInvalidUTF8  = errors.New("JSON is not valid UTF-8")
	ErrDuplicateKey = errors.New("JSON object has a duplicate key")
	ErrNumber       = errors.New("JSON number is not a supported integer")
	ErrTrailing     = errors.New("JSON has trailing content")
)

// Parse decodes exactly one JSON value. It rejects a byte-order mark, invalid UTF-8, lone surrogates, duplicate
// object keys, non-integer or unsafe numbers and trailing content, and stops as soon as a limit is exceeded.
func Parse(data []byte, lim Limits) (Value, error) {
	if len(data) > lim.Bytes {
		return Value{}, ErrTooLarge
	}
	if !utf8.Valid(data) {
		return Value{}, ErrInvalidUTF8
	}
	p := parser{data: data, lim: lim}
	p.space()
	v, err := p.value(0)
	if err != nil {
		return Value{}, err
	}
	p.space()
	if p.pos != len(p.data) {
		return Value{}, ErrTrailing
	}
	return v, nil
}

type parser struct {
	data  []byte
	pos   int
	nodes int
	lim   Limits
}

func (p *parser) space() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (Value, error) {
	p.nodes++
	if p.nodes > p.lim.Nodes {
		return Value{}, ErrTooManyNodes
	}
	if p.pos >= len(p.data) {
		return Value{}, ErrSyntax
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(depth + 1)
	case c == '[':
		return p.array(depth + 1)
	case c == '"':
		s, err := p.string()
		return Value{Kind: String, Str: s}, err
	case c == 't':
		return Value{Kind: Bool, Bool: true}, p.literal("true")
	case c == 'f':
		return Value{Kind: Bool}, p.literal("false")
	case c == 'n':
		return Value{Kind: Null}, p.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return Value{}, ErrSyntax
}

func (p *parser) literal(word string) error {
	if !strings.HasPrefix(string(p.data[p.pos:min(len(p.data), p.pos+len(word))]), word) {
		return ErrSyntax
	}
	p.pos += len(word)
	return nil
}

func (p *parser) object(depth int) (Value, error) {
	if depth > p.lim.Depth {
		return Value{}, ErrTooDeep
	}
	p.pos++ // {
	v := Value{Kind: Object}
	seen := map[string]bool{}
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return v, nil
	}
	for {
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return Value{}, ErrSyntax
		}
		key, err := p.string()
		if err != nil {
			return Value{}, err
		}
		if seen[key] {
			return Value{}, ErrDuplicateKey
		}
		seen[key] = true
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return Value{}, ErrSyntax
		}
		p.pos++
		p.space()
		member, err := p.value(depth)
		if err != nil {
			return Value{}, err
		}
		v.Members = append(v.Members, Member{Key: key, Value: member})
		p.space()
		if p.pos >= len(p.data) {
			return Value{}, ErrSyntax
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return v, nil
		default:
			return Value{}, ErrSyntax
		}
	}
}

func (p *parser) array(depth int) (Value, error) {
	if depth > p.lim.Depth {
		return Value{}, ErrTooDeep
	}
	p.pos++ // [
	v := Value{Kind: Array}
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return v, nil
	}
	for {
		p.space()
		item, err := p.value(depth)
		if err != nil {
			return Value{}, err
		}
		v.Items = append(v.Items, item)
		p.space()
		if p.pos >= len(p.data) {
			return Value{}, ErrSyntax
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return v, nil
		default:
			return Value{}, ErrSyntax
		}
	}
}

func (p *parser) number() (Value, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	digits := p.pos
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == digits || (p.data[digits] == '0' && p.pos-digits > 1) {
		return Value{}, ErrSyntax
	}
	if p.pos < len(p.data) && (p.data[p.pos] == '.' || p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		return Value{}, ErrNumber
	}
	n, err := strconv.ParseInt(string(p.data[start:p.pos]), 10, 64)
	if err != nil || n > maxSafeInteger || n < -maxSafeInteger || (n == 0 && p.data[start] == '-') {
		return Value{}, ErrNumber
	}
	return Value{Kind: Number, Int: n}, nil
}

func (p *parser) string() (string, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for {
		if p.pos >= len(p.data) {
			return "", ErrSyntax
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c < 0x20:
			return "", ErrSyntax
		case c == '\\':
			if err := p.escape(&b); err != nil {
				return "", err
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
}

func (p *parser) escape(b *strings.Builder) error {
	if p.pos+1 >= len(p.data) {
		return ErrSyntax
	}
	c := p.data[p.pos+1]
	p.pos += 2
	switch c {
	case '"', '\\', '/':
		b.WriteByte(c)
	case 'b':
		b.WriteByte('\b')
	case 'f':
		b.WriteByte('\f')
	case 'n':
		b.WriteByte('\n')
	case 'r':
		b.WriteByte('\r')
	case 't':
		b.WriteByte('\t')
	case 'u':
		r, err := p.hex4()
		if err != nil {
			return err
		}
		if utf16.IsSurrogate(r) {
			// I-JSON (RFC 7493), which RFC 8785 requires: only a high surrogate followed by a low one.
			if r >= 0xdc00 || p.pos+1 >= len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
				return ErrInvalidUTF8
			}
			p.pos += 2
			low, err := p.hex4()
			if err != nil {
				return err
			}
			if low < 0xdc00 || low > 0xdfff {
				return ErrInvalidUTF8
			}
			r = utf16.DecodeRune(r, low)
		}
		b.WriteRune(r)
	default:
		return ErrSyntax
	}
	return nil
}

func (p *parser) hex4() (rune, error) {
	if p.pos+4 > len(p.data) {
		return 0, ErrSyntax
	}
	var r rune
	for _, c := range p.data[p.pos : p.pos+4] {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, ErrSyntax
		}
	}
	p.pos += 4
	return r, nil
}

// Canonical encodes v under RFC 8785 (JCS): members sorted by UTF-16 code units, no insignificant whitespace,
// minimal string escapes and integer numbers.
func Canonical(v Value) []byte {
	var b []byte
	return appendCanonical(b, v)
}

func appendCanonical(b []byte, v Value) []byte {
	switch v.Kind {
	case Null:
		return append(b, "null"...)
	case Bool:
		return strconv.AppendBool(b, v.Bool)
	case Number:
		return strconv.AppendInt(b, v.Int, 10)
	case String:
		return appendString(b, v.Str)
	case Array:
		b = append(b, '[')
		for i, item := range v.Items {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendCanonical(b, item)
		}
		return append(b, ']')
	default:
		members := append([]Member(nil), v.Members...)
		sort.Slice(members, func(i, j int) bool { return utf16Less(members[i].Key, members[j].Key) })
		b = append(b, '{')
		for i, m := range members {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendString(b, m.Key)
			b = append(b, ':')
			b = appendCanonical(b, m.Value)
		}
		return append(b, '}')
	}
}

func utf16Less(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c == '\b':
			b = append(b, '\\', 'b')
		case c == '\f':
			b = append(b, '\\', 'f')
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '\r':
			b = append(b, '\\', 'r')
		case c == '\t':
			b = append(b, '\\', 't')
		case c < 0x20:
			b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}

// Str builds a string value for Canonical.
func Str(s string) Value { return Value{Kind: String, Str: s} }

// Int builds an integer value.
func Int(n int64) Value { return Value{Kind: Number, Int: n} }

// NullValue builds JSON null.
func NullValue() Value { return Value{Kind: Null} }

// Obj builds an object from key/value members.
func Obj(members ...Member) Value { return Value{Kind: Object, Members: members} }

// Arr builds an array.
func Arr(items ...Value) Value { return Value{Kind: Array, Items: items} }

// fields returns an object's members by key after checking it has exactly the required keys plus any of optional.
// It reports false for any other shape; callers map that to their fixed error and schema path.
func fields(v Value, required []string, optional ...string) (map[string]Value, bool) {
	if v.Kind != Object {
		return nil, false
	}
	allowed := map[string]bool{}
	for _, k := range optional {
		allowed[k] = true
	}
	out := make(map[string]Value, len(v.Members))
	for _, m := range v.Members {
		out[m.Key] = m.Value
	}
	for _, k := range required {
		if _, ok := out[k]; !ok {
			return nil, false
		}
		allowed[k] = true
	}
	for k := range out {
		if !allowed[k] {
			return nil, false
		}
	}
	return out, true
}
