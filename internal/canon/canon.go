// Package canon implements the canonical JSON encoding used for event
// payloads and headers (DESIGN.md §4.2).
//
// The encoding is RFC 8785 (JCS) restricted to integers in the IEEE-754
// safe range. Non-integer numbers are rejected: measurements and other
// decimals must be carried as strings, so every value has exactly one
// byte representation and no floating-point rounding ever touches a
// calibration result.
package canon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxSafeInt is the largest integer magnitude accepted (2^53 - 1).
const MaxSafeInt = 1<<53 - 1

var (
	ErrInvalidUTF8  = errors.New("canon: invalid UTF-8")
	ErrDuplicateKey = errors.New("canon: duplicate object key")
	ErrNotInteger   = errors.New("canon: only integer numbers are allowed; encode decimals as strings")
	ErrIntRange     = errors.New("canon: integer outside ±(2^53-1)")
	ErrTrailingData = errors.New("canon: trailing data after JSON value")
)

var integerRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// Canonicalize parses raw JSON and returns its canonical encoding.
func Canonicalize(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, ErrInvalidUTF8
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrTrailingData
	}
	return Marshal(v)
}

// Marshal canonically encodes a value built from map[string]any, []any,
// string, bool, nil, json.Number, int and int64.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("canon: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("canon: %w", err)
				}
				key := kt.(string) // the decoder guarantees object keys are strings
				if _, dup := obj[key]; dup {
					return nil, fmt.Errorf("%w: %q", ErrDuplicateKey, key)
				}
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, fmt.Errorf("canon: %w", err)
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, fmt.Errorf("canon: %w", err)
			}
			return arr, nil
		}
		return nil, fmt.Errorf("canon: unexpected delimiter %q", t)
	default:
		return t, nil // string, bool, nil, json.Number
	}
}

func encode(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		return writeString(b, t)
	case json.Number:
		return writeInteger(b, string(t))
	case int:
		return writeInteger(b, strconv.Itoa(t))
	case int64:
		return writeInteger(b, strconv.FormatInt(t, 10))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encode(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := encode(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("canon: unsupported type %T", v)
	}
	return nil
}

func writeInteger(b *bytes.Buffer, s string) error {
	if !integerRE.MatchString(s) || s == "-0" {
		return ErrNotInteger
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > MaxSafeInt || n < -MaxSafeInt {
		return ErrIntRange
	}
	b.WriteString(s)
	return nil
}

// writeString escapes as ECMAScript JSON.stringify does (RFC 8785 §3.2.2.2).
func writeString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return ErrInvalidUTF8
	}
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
	return nil
}

// lessUTF16 orders object keys by UTF-16 code units (RFC 8785 §3.2.3).
func lessUTF16(a, b string) bool {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b))) < 0
}
