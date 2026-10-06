package release

import (
	"errors"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"
)

func TestSignVerify(t *testing.T) {
	pub, sk, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := GenerateKey()
	data := []byte(`{"version":"v1.0.0"}`)
	sig, err := Sign(sk, data, timestampComment(time.Unix(1700000000, 0), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	comment, err := Verify([]PublicKey{other, pub}, data, []byte(sig))
	if err != nil || comment != "timestamp:1700000000\tfile:manifest.json" {
		t.Fatalf("Verify = %q, %v", comment, err)
	}
	if _, err := Verify([]PublicKey{pub}, []byte(`{"version":"v9.9.9"}`), []byte(sig)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("altered data: %v", err)
	}
	if _, err := Verify([]PublicKey{other}, data, []byte(sig)); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("untrusted key: %v", err)
	}
	altered := strings.Replace(sig, "file:manifest.json", "file:other.json", 1)
	if _, err := Verify([]PublicKey{pub}, data, []byte(altered)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("altered trusted comment: %v", err)
	}
	if _, err := Verify([]PublicKey{pub}, data, []byte("garbage")); !errors.Is(err, ErrMalformed) {
		t.Errorf("garbage: %v", err)
	}
	if k, err := ParsePublicKey(pub.File()); err != nil || k.ID != pub.ID || !k.Key.Equal(pub.Key) {
		t.Errorf("public key file round trip: %v", err)
	}
}

// TestMinisignInterop checks the format against an independent minisign
// implementation in both directions.
func TestMinisignInterop(t *testing.T) {
	data := []byte("pi-fleet release artifact")

	// Theirs signs, ours verifies.
	theirPub, theirPriv, err := minisign.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	theirSig := minisign.SignWithComments(theirPriv, data, "timestamp:1\tfile:x", "from aead.dev/minisign")
	pubText, err := theirPub.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	ourView, err := ParsePublicKey(string(pubText))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify([]PublicKey{ourView}, data, theirSig); err != nil {
		t.Fatalf("our verifier rejected a minisign signature: %v", err)
	}

	// Ours signs, theirs verifies.
	pub, sk, _ := GenerateKey()
	sig, err := Sign(sk, data, "timestamp:2\tfile:y")
	if err != nil {
		t.Fatal(err)
	}
	var theirView minisign.PublicKey
	if err := theirView.UnmarshalText([]byte(pub.String())); err != nil {
		t.Fatal(err)
	}
	if !minisign.Verify(theirView, data, []byte(sig)) {
		t.Fatal("minisign rejected our signature")
	}
	if minisign.Verify(theirView, []byte("tampered"), []byte(sig)) {
		t.Fatal("minisign accepted our signature on other data")
	}
}
