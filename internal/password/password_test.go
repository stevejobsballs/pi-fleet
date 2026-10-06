package password

import (
	"errors"
	"regexp"
	"testing"
)

// cheap keeps tests fast; production uses Default.
var cheap = Params{Time: 1, MemoryKiB: 64, Threads: 1}

func TestHashCheckRoundTrip(t *testing.T) {
	v, err := Hash("tumbleweed-gasket-42", cheap)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Check("tumbleweed-gasket-42") || v.Check("tumbleweed-gasket-43") {
		t.Fatal("Check gave the wrong answer")
	}
	parsed, err := Parse(v.String())
	if err != nil {
		t.Fatalf("Parse(%s): %v", v, err)
	}
	if !parsed.Check("tumbleweed-gasket-42") {
		t.Error("parsed verifier does not check")
	}
	other, _ := Hash("tumbleweed-gasket-42", cheap)
	if string(other.Salt) == string(v.Salt) {
		t.Error("salts repeat")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"",
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=64,t=0,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=64,t=1,p=1$!!$a2V5a2V5a2V5a2V5a2V5",
	} {
		if _, err := Parse(s); !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%q) = %v", s, err)
		}
	}
}

func TestCheckPolicy(t *testing.T) {
	ok := []string{"tumbleweed-gasket-42", "correct horse staple", "Ünïcødé-pässwörd"}
	bad := []string{"short1!", "Password1234", "jsmith-is-great-99", "aaaaaaaaaaaaaaaa", "ababababababab"}
	for _, pw := range ok {
		if err := CheckPolicy(pw, "jsmith"); err != nil {
			t.Errorf("CheckPolicy(%q) = %v", pw, err)
		}
	}
	for _, pw := range bad {
		var pe *PolicyError
		if err := CheckPolicy(pw, "jsmith"); !errors.As(err, &pe) {
			t.Errorf("CheckPolicy(%q) accepted", pw)
		}
	}
}

func TestCheckHistory(t *testing.T) {
	old, _ := Hash("first-password-1", cheap)
	if err := CheckHistory("first-password-1", []Verifier{old}); err == nil {
		t.Error("reused password accepted")
	}
	if err := CheckHistory("second-password-2", []Verifier{old}); err != nil {
		t.Error(err)
	}
}

func TestGenerateTemporary(t *testing.T) {
	re := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		p, err := GenerateTemporary()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(p) {
			t.Fatalf("bad format %q", p)
		}
		if seen[p] {
			t.Fatalf("repeat %q", p)
		}
		seen[p] = true
	}
}
