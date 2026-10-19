package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Canonical returns the RFC 8785 (JCS) canonical form of the JSON value raw: no whitespace, object keys sorted by
// their UTF-16 code units, strings escaped as ECMAScript's JSON.stringify does and numbers in its shortest form. A
// pipeline version's identity is the hash of this form (docs/design/14-config-versions.md §14.1).
func Canonical(raw []byte) ([]byte, error) {
	return canonicalContext(context.Background(), raw)
}

func canonicalContext(ctx context.Context, raw []byte) ([]byte, error) {
	if err := ExpansionStep(ctx, int64(len(raw))); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical JSON: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("canonical JSON: trailing data")
	}
	var b bytes.Buffer
	if err := writeCanonicalContext(ctx, &b, v); err != nil {
		return nil, fmt.Errorf("canonical JSON: %w", err)
	}
	return b.Bytes(), nil
}

func writeCanonicalContext(ctx context.Context, b *bytes.Buffer, v any) error {
	if err := ExpansionStep(ctx, 1); err != nil {
		return err
	}
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case string:
		if err := ExpansionStep(ctx, int64(len(v))); err != nil {
			return err
		}
		writeCanonicalString(b, v)
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.IsInf(f, 0) {
			return fmt.Errorf("a number is not an IEEE 754 double") // the number is input, which errors don't echo
		}
		b.WriteString(canonicalNumber(f))
	case []any:
		b.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonicalContext(ctx, b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		var sortErr error
		slices.SortFunc(keys, func(a, b string) int {
			if sortErr != nil {
				return 0
			}
			sortErr = ExpansionStep(ctx, int64(len(a)+len(b)))
			if sortErr != nil {
				return 0
			}
			return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
		})
		if sortErr != nil {
			return sortErr
		}
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			if err := writeCanonicalContext(ctx, b, v[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unexpected %T", v)
	}
	return nil
}

// writeCanonicalString escapes only what JSON.stringify escapes: the quote, the backslash and C0 controls.
func writeCanonicalString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// canonicalNumber formats f as ECMAScript's Number.prototype.toString does (ECMA-262 Number::toString), which
// RFC 8785 §3.2.2.3 requires: the shortest digits that round-trip, in plain notation for exponents in [-6, 21).
func canonicalNumber(f float64) string {
	if f == 0 {
		return "0" // also -0
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// 'e' with precision -1 gives the shortest round-trip digits: d.ddde±x.
	mantissa, exp, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	x, _ := strconv.Atoi(exp)
	k, n := len(digits), x+1 // f = 0.digits × 10^n
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	e := "e+"
	if n-1 < 0 {
		e = "e-"
	}
	frac := ""
	if k > 1 {
		frac = "." + digits[1:]
	}
	return sign + digits[:1] + frac + e + strconv.Itoa(abs(n-1))
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
