// Package password hashes and checks user passwords (DESIGN.md §6.5).
//
// Verifiers are Argon2id with a per-user random salt, encoded in the PHC
// string format so their parameters travel with them. Plaintext passwords
// never leave the device they are typed on: only verifiers go into events.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params are Argon2id cost parameters.
type Params struct {
	Time      uint32
	MemoryKiB uint32
	Threads   uint8
}

// Default costs roughly a quarter second on a Raspberry Pi 4.
var Default = Params{Time: 3, MemoryKiB: 64 * 1024, Threads: 2}

const (
	saltLen = 16
	keyLen  = 32

	// MinLength is the minimum password length in characters.
	MinLength = 12
	// HistoryDepth is how many previous passwords may not be reused.
	HistoryDepth = 5
	// Lifetime is how long a password stays valid (decision D11).
	Lifetime = 31 * 24 * time.Hour
	// TemporaryLifetime is how long a one-time activation or reset
	// password stays usable (DESIGN.md §6.3).
	TemporaryLifetime = 72 * time.Hour
)

var ErrMalformed = errors.New("password: malformed verifier")

// Verifier is a salted Argon2id hash.
type Verifier struct {
	Params Params
	Salt   []byte
	Key    []byte
}

// Hash derives a verifier for password with a fresh random salt.
func Hash(password string, p Params) (Verifier, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return Verifier{}, err
	}
	return derive(password, salt, p), nil
}

func derive(password string, salt []byte, p Params) Verifier {
	return Verifier{
		Params: p,
		Salt:   salt,
		Key:    argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, keyLen),
	}
}

// Check reports whether password matches, in constant time.
func (v Verifier) Check(password string) bool {
	if len(v.Salt) == 0 || len(v.Key) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), v.Salt, v.Params.Time, v.Params.MemoryKiB, v.Params.Threads, uint32(len(v.Key)))
	return subtle.ConstantTimeCompare(got, v.Key) == 1
}

var b64 = base64.RawStdEncoding

// String encodes the verifier as a PHC string:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
func (v Verifier) String() string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, v.Params.MemoryKiB, v.Params.Time, v.Params.Threads,
		b64.EncodeToString(v.Salt), b64.EncodeToString(v.Key))
}

// Parse decodes a PHC string produced by String.
func Parse(s string) (Verifier, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return Verifier{}, ErrMalformed
	}
	var v Verifier
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &v.Params.MemoryKiB, &v.Params.Time, &v.Params.Threads); err != nil {
		return Verifier{}, ErrMalformed
	}
	var err error
	if v.Salt, err = b64.DecodeString(parts[4]); err != nil || len(v.Salt) < 8 {
		return Verifier{}, ErrMalformed
	}
	if v.Key, err = b64.DecodeString(parts[5]); err != nil || len(v.Key) < 16 {
		return Verifier{}, ErrMalformed
	}
	if v.Params.Time == 0 || v.Params.MemoryKiB < 8 || v.Params.Threads == 0 {
		return Verifier{}, ErrMalformed
	}
	return v, nil
}

// PolicyError explains why a password was refused.
type PolicyError struct{ Reason string }

func (e *PolicyError) Error() string { return "password: " + e.Reason }

// CheckPolicy enforces the password rules that need the plaintext:
// length, common passwords, and containing the username. Reuse of recent
// passwords is checked with CheckHistory.
func CheckPolicy(pw, username string) error {
	if utf8.RuneCountInString(pw) < MinLength {
		return &PolicyError{fmt.Sprintf("must be at least %d characters", MinLength)}
	}
	lower := strings.ToLower(pw)
	if username != "" && strings.Contains(lower, strings.ToLower(username)) {
		return &PolicyError{"must not contain the username"}
	}
	if isCommon(lower) {
		return &PolicyError{"is too common"}
	}
	if distinct(pw) < 4 {
		return &PolicyError{"must use more distinct characters"}
	}
	return nil
}

// CheckHistory refuses a password matching any of the given previous
// verifiers.
func CheckHistory(pw string, previous []Verifier) error {
	for _, v := range previous {
		if v.Check(pw) {
			return &PolicyError{fmt.Sprintf("must differ from the last %d passwords", HistoryDepth)}
		}
	}
	return nil
}

func distinct(s string) int {
	seen := map[rune]bool{}
	for _, r := range s {
		seen[r] = true
	}
	return len(seen)
}

// Crockford base32 avoids I, L, O and U, which are easy to misread on paper.
var tempEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// GenerateTemporary returns a random one-time password with 80 bits of
// entropy, formatted for reading aloud or writing down:
// XXXX-XXXX-XXXX-XXXX.
func GenerateTemporary() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := tempEncoding.EncodeToString(b) // 16 characters
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16], nil
}
