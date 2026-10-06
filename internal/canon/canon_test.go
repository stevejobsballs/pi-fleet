package canon

import (
	"errors"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"sorts keys", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"strips whitespace", " { \"a\" : [ 1 , true , null ] } ", `{"a":[1,true,null]}`},
		{"nested", `{"z":{"y":1,"x":[{"b":0,"a":-5}]}}`, `{"z":{"x":[{"a":-5,"b":0}],"y":1}}`},
		{"no slash or html escaping", `{"a":"</b> & é"}`, `{"a":"</b> & é"}`},
		{"short escapes", `"\u0008\u000c\n\r\t\"\\"`, `"\b\f\n\r\t\"\\"`},
		{"other controls lowercase hex", `"\u001F\u0001"`, `"\u001f\u0001"`},
		{"unicode escapes decoded", `"é😀"`, `"é😀"`},
		{"max safe int", `9007199254740991`, `9007199254740991`},
		{"min safe int", `-9007199254740991`, `-9007199254740991`},
		// U+FB33 sorts after U+1F600 in UTF-16 (0xFB33 > 0xD83D) even
		// though it sorts before it by code point.
		{"utf16 key order", `{"דּ":1,"😀":2}`, `{"😀":2,"דּ":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Canonicalize([]byte(tt.in))
			if err != nil {
				t.Fatalf("Canonicalize(%s): %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Errorf("Canonicalize(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonicalizeRejects(t *testing.T) {
	tests := []struct {
		name, in string
		want     error
	}{
		{"float", `{"a":1.5}`, ErrNotInteger},
		{"exponent", `1e3`, ErrNotInteger},
		{"trailing zero fraction", `1.0`, ErrNotInteger},
		{"negative zero", `-0`, ErrNotInteger},
		{"too large", `9007199254740992`, ErrIntRange},
		{"too small", `-9007199254740992`, ErrIntRange},
		{"duplicate key", `{"a":1,"a":2}`, ErrDuplicateKey},
		{"trailing data", `{} {}`, ErrTrailingData},
		{"invalid utf8", "\"\xff\"", ErrInvalidUTF8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Canonicalize([]byte(tt.in))
			if !errors.Is(err, tt.want) {
				t.Errorf("Canonicalize(%q) error = %v, want %v", tt.in, err, tt.want)
			}
		})
	}
	if _, err := Canonicalize([]byte(`{"a":`)); err == nil {
		t.Error("truncated input: want error")
	}
}

func TestCanonicalizeIdempotent(t *testing.T) {
	in := `{"steps":[{"id":"s1","value":"12.50","pass":true}],"asset":"A-1","n":-3}`
	once, err := Canonicalize([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Canonicalize(once)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Errorf("not idempotent: %s vs %s", once, twice)
	}
}
