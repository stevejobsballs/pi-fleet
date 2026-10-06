package pairing

import (
	"strings"
	"testing"
)

func TestWordlistUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range wordlist {
		if w == "" || seen[w] || strings.ToLower(w) != w {
			t.Fatalf("bad or duplicate word %q", w)
		}
		seen[w] = true
	}
}

func TestWords(t *testing.T) {
	a := Words([]byte("event-key"), []byte("transport-key"))
	if len(strings.Fields(a)) != 6 {
		t.Fatalf("Words = %q", a)
	}
	if a != Words([]byte("event-key"), []byte("transport-key")) {
		t.Error("not deterministic")
	}
	if a == Words([]byte("event-key"), []byte("transport-kez")) {
		t.Error("different keys gave the same words")
	}
}
