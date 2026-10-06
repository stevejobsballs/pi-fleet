package fleetsync

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// The activation protocol (DESIGN.md §6.3). K is the Argon2id key of the
// one-time password: central stores it as the user's verifier, and the
// node derives it from the password and the salt central disclosed.
// From K come an encryption key and a MAC key.

func activationKeys(k []byte) (enc, mac []byte) {
	h := func(label string) []byte {
		m := hmac.New(sha256.New, k)
		m.Write([]byte(label))
		return m.Sum(nil)
	}
	return h("pi-fleet/activate/enc"), h("pi-fleet/activate/mac")
}

func sealVerifier(enc []byte, nonce, verifier string) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(enc)
	if err != nil {
		return nil, err
	}
	n := make([]byte, aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return aead.Seal(n, n, []byte(verifier), []byte(nonce)), nil
}

func openVerifier(enc []byte, nonce string, sealed []byte) (string, error) {
	aead, err := chacha20poly1305.NewX(enc)
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize() {
		return "", errors.New("sealed verifier too short")
	}
	pt, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(nonce))
	return string(pt), err
}

func mac(key []byte, label string, fields ...string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	for _, f := range fields {
		m.Write([]byte{0})
		m.Write([]byte(f))
	}
	return m.Sum(nil)
}

func requestProof(macKey []byte, r *ActivateRequest) []byte {
	return mac(macKey, "pi-fleet/activate/proof", r.Nonce, r.Username, r.NodeID,
		strings.ToLower(r.EventPublicKey), strings.ToLower(r.TransportPublicKey), string(r.SealedVerifier))
}

func responseMAC(macKey []byte, nonce string, r *ActivateResponse) []byte {
	return mac(macKey, "pi-fleet/activate/response", nonce, r.NodeID, r.Status, r.PairingWords, r.CentralNodeID, r.CentralEventPub)
}
