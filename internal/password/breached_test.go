package password

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBreachedPasswordsRefused(t *testing.T) {
	// Among the most used 12+ character passwords in published breaches,
	// and not on pi-fleet's own list.
	for _, pw := range []string{"tequieromucho", "winniethepooh", "123qweasdzxc", "startfinding", "WinnieThePooh"} {
		err := CheckPolicy(pw, "tess")
		if err == nil || !strings.Contains(err.Error(), "data breaches") {
			t.Errorf("%q: %v", pw, err)
		}
	}
	// pi-fleet's own and its tests' passwords are fine.
	for _, pw := range []string{"tumbleweed-gasket-42", "brass-kettle-orchard-7", "copper-ladder-sunrise"} {
		if err := CheckPolicy(pw, "tess"); err != nil {
			t.Errorf("%q: %v", pw, err)
		}
	}
}

// About 1 in 1000 unbreached passwords is refused by chance.
func TestBreachedFalsePositiveRate(t *testing.T) {
	b := breachedSet()
	if b.N < 2_000_000 {
		t.Fatalf("filter holds only %d passwords", b.N)
	}
	const n = 200_000
	hits := 0
	buf := make([]byte, 9)
	for i := 0; i < n; i++ {
		rand.Read(buf)
		if b.Has(hex.EncodeToString(buf)) {
			hits++
		}
	}
	if rate := float64(hits) / n; rate > 0.002 {
		t.Fatalf("false-positive rate %.4f", rate)
	}
}

func TestBloomFileChecks(t *testing.T) {
	b := NewBloom(1000, 7)
	b.Add("correct-horse-battery")
	data, _ := b.MarshalBinary()
	c, err := ParseBloom(data)
	if err != nil || !c.Has("correct-horse-battery") || c.Has("incorrect-horse-battery") || c.N != 1 {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParseBloom(data[:len(data)-1]); err == nil {
		t.Fatal("accepted a truncated filter")
	}
	if _, err := ParseBloom(append([]byte("XXXXXXXX"), data[8:]...)); err == nil {
		t.Fatal("accepted a filter without the magic")
	}
}
