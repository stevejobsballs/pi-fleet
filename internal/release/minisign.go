// Package release verifies signed releases and installs them safely
// (DESIGN.md §9).
//
// Signatures use the minisign format (Ed25519 over a BLAKE2b-512 prehash,
// with a signed trusted comment), so anyone can check a release with the
// standard minisign tool. The public keys this binary trusts are compiled
// in, so a compromised master Pi cannot add one.
package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
)

var (
	ErrBadSignature = errors.New("release: signature does not verify")
	ErrUnknownKey   = errors.New("release: signed by a key this build does not trust")
	ErrMalformed    = errors.New("release: malformed key or signature")
)

// PublicKey is a minisign public key.
type PublicKey struct {
	ID  uint64
	Key ed25519.PublicKey
}

// SecretKey is a signing key. pi-fleet stores it encrypted with a
// passphrase (see cmd release-keygen); the format is not minisign's.
type SecretKey struct {
	ID  uint64
	Key ed25519.PrivateKey
}

// GenerateKey makes a new release signing key pair.
func GenerateKey() (PublicKey, SecretKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return PublicKey{}, SecretKey{}, err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return PublicKey{}, SecretKey{}, err
	}
	n := binary.LittleEndian.Uint64(id[:])
	return PublicKey{ID: n, Key: pub}, SecretKey{ID: n, Key: priv}, nil
}

// Public returns the public half.
func (s SecretKey) Public() PublicKey {
	return PublicKey{ID: s.ID, Key: s.Key.Public().(ed25519.PublicKey)}
}

func (p PublicKey) idHex() string { return fmt.Sprintf("%016X", p.ID) }

// String encodes the key as the base64 line of a minisign public key.
func (p PublicKey) String() string {
	b := make([]byte, 0, 42)
	b = append(b, 'E', 'd')
	b = binary.LittleEndian.AppendUint64(b, p.ID)
	b = append(b, p.Key...)
	return base64.StdEncoding.EncodeToString(b)
}

// File renders a minisign public key file.
func (p PublicKey) File() string {
	return "untrusted comment: minisign public key " + p.idHex() + "\n" + p.String() + "\n"
}

// ParsePublicKey accepts the base64 line or a whole minisign key file.
func ParsePublicKey(s string) (PublicKey, error) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil || len(b) != 42 || b[0] != 'E' || b[1] != 'd' {
		return PublicKey{}, ErrMalformed
	}
	return PublicKey{ID: binary.LittleEndian.Uint64(b[2:10]), Key: ed25519.PublicKey(b[10:])}, nil
}

// Sign produces a minisign signature file for data.
func Sign(sk SecretKey, data []byte, trustedComment string) (string, error) {
	if strings.ContainsAny(trustedComment, "\r\n") {
		return "", errors.New("release: trusted comment must be one line")
	}
	h := blake2b.Sum512(data)
	sig := ed25519.Sign(sk.Key, h[:])
	global := ed25519.Sign(sk.Key, append(append([]byte{}, sig...), trustedComment...))
	b := make([]byte, 0, 74)
	b = append(b, 'E', 'D')
	b = binary.LittleEndian.AppendUint64(b, sk.ID)
	b = append(b, sig...)
	return "untrusted comment: signature from pi-fleet release key " + sk.Public().idHex() + "\n" +
		base64.StdEncoding.EncodeToString(b) + "\n" +
		"trusted comment: " + trustedComment + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n", nil
}

// Verify checks a minisign signature file over data against any of the
// trusted keys, and returns the trusted comment.
func Verify(trusted []PublicKey, data []byte, sigFile []byte) (string, error) {
	lines := strings.Split(strings.ReplaceAll(string(sigFile), "\r\n", "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "untrusted comment:") || !strings.HasPrefix(lines[2], "trusted comment: ") {
		return "", ErrMalformed
	}
	sb, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(sb) != 74 {
		return "", ErrMalformed
	}
	// "ED" signs a BLAKE2b-512 prehash (minisign's default); legacy "Ed"
	// signs the data itself. Both are plain Ed25519.
	if sb[0] != 'E' || (sb[1] != 'D' && sb[1] != 'd') {
		return "", fmt.Errorf("%w: unknown signature algorithm", ErrMalformed)
	}
	prehashed := sb[1] == 'D'
	id := binary.LittleEndian.Uint64(sb[2:10])
	var key ed25519.PublicKey
	for _, k := range trusted {
		if k.ID == id {
			key = k.Key
		}
	}
	if key == nil {
		return "", fmt.Errorf("%w (key id %016X)", ErrUnknownKey, id)
	}
	sig := sb[10:]
	msg := data
	if prehashed {
		h := blake2b.Sum512(data)
		msg = h[:]
	}
	if !ed25519.Verify(key, msg, sig) {
		return "", ErrBadSignature
	}
	comment := strings.TrimPrefix(lines[2], "trusted comment: ")
	global, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(global) != ed25519.SignatureSize {
		return "", ErrMalformed
	}
	if !ed25519.Verify(key, append(append([]byte{}, sig...), comment...), global) {
		return "", fmt.Errorf("%w: trusted comment altered", ErrBadSignature)
	}
	return comment, nil
}

// trustedKeys is set at build time:
//
//	-ldflags "-X pi-fleet/internal/release.trustedKeys=<key>,<next key>"
//
// with each key the base64 line of a minisign public key. A build without
// keys refuses every update.
var trustedKeys string

// TrustedKeys returns the release keys compiled into this binary.
func TrustedKeys() ([]PublicKey, error) {
	var out []PublicKey
	for _, s := range strings.Split(trustedKeys, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		k, err := ParsePublicKey(s)
		if err != nil {
			return nil, fmt.Errorf("release: compiled-in key %q: %w", s, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// timestampComment is the trusted comment pi-fleet writes.
func timestampComment(t time.Time, file string) string {
	return fmt.Sprintf("timestamp:%d\tfile:%s", t.Unix(), file)
}
