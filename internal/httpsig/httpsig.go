// Package httpsig signs and verifies HTTP requests with RFC 9421 HTTP
// Message Signatures using Ed25519 (DESIGN.md §5.2).
//
// It implements the profile pi-fleet needs: the covered components are
// always "@method", "@path", "@query" and "content-digest" (RFC 9530),
// with created, keyid, alg and nonce parameters, under the label "sig1".
// Signatures cover the body through Content-Digest, so integrity holds
// end to end even when a proxy terminates TLS.
package httpsig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	label      = "sig1"
	components = `("@method" "@path" "@query" "content-digest")`
	alg        = "ed25519"
)

var (
	ErrMissing   = errors.New("httpsig: request is not signed")
	ErrMalformed = errors.New("httpsig: malformed signature headers")
	ErrDigest    = errors.New("httpsig: body does not match Content-Digest")
	ErrExpired   = errors.New("httpsig: signature created outside the allowed window")
	ErrSignature = errors.New("httpsig: signature does not verify")
)

// ContentDigest returns the RFC 9530 Content-Digest value for body.
func ContentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

// Sign adds Content-Digest, Signature-Input and Signature headers to req,
// whose body is body.
func Sign(req *http.Request, body []byte, keyID string, key ed25519.PrivateKey, created time.Time, nonce string) error {
	if strings.ContainsAny(keyID+nonce, "\"\\") {
		return errors.New("httpsig: key id and nonce must not contain quotes or backslashes")
	}
	req.Header.Set("Content-Digest", ContentDigest(body))
	params := fmt.Sprintf(`%s;created=%d;keyid="%s";alg="%s";nonce="%s"`, components, created.Unix(), keyID, alg, nonce)
	sig := ed25519.Sign(key, signatureBase(req, params))
	req.Header.Set("Signature-Input", label+"="+params)
	req.Header.Set("Signature", label+"=:"+base64.StdEncoding.EncodeToString(sig)+":")
	return nil
}

func signatureBase(req *http.Request, params string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "\"@method\": %s\n", req.Method)
	fmt.Fprintf(&b, "\"@path\": %s\n", req.URL.EscapedPath())
	fmt.Fprintf(&b, "\"@query\": ?%s\n", req.URL.RawQuery)
	fmt.Fprintf(&b, "\"content-digest\": %s\n", req.Header.Get("Content-Digest"))
	fmt.Fprintf(&b, "\"@signature-params\": %s", params)
	return []byte(b.String())
}

// Verified describes a request whose signature checked out.
type Verified struct {
	KeyID   string
	Nonce   string
	Created time.Time
}

// Verify checks req's signature over body. resolve returns the public key
// for a key id. created must lie within maxSkew of now. The caller must
// reject reused nonces.
func Verify(req *http.Request, body []byte, resolve func(keyID string) (ed25519.PublicKey, error), now time.Time, maxSkew time.Duration) (Verified, error) {
	input := req.Header.Get("Signature-Input")
	sigHeader := req.Header.Get("Signature")
	if input == "" || sigHeader == "" {
		return Verified{}, ErrMissing
	}
	params, ok := strings.CutPrefix(input, label+"=")
	if !ok || !strings.HasPrefix(params, components+";") {
		return Verified{}, fmt.Errorf("%w: unexpected label or covered components", ErrMalformed)
	}
	fields := map[string]string{}
	for _, kv := range strings.Split(strings.TrimPrefix(params, components+";"), ";") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return Verified{}, ErrMalformed
		}
		if _, dup := fields[k]; dup {
			return Verified{}, ErrMalformed
		}
		if strings.HasPrefix(v, `"`) {
			if len(v) < 2 || !strings.HasSuffix(v, `"`) {
				return Verified{}, ErrMalformed
			}
			v = v[1 : len(v)-1]
		}
		fields[k] = v
	}
	if fields["alg"] != alg || fields["keyid"] == "" || fields["nonce"] == "" || len(fields) != 4 {
		return Verified{}, fmt.Errorf("%w: need exactly created, keyid, alg=ed25519 and nonce", ErrMalformed)
	}
	createdUnix, err := strconv.ParseInt(fields["created"], 10, 64)
	if err != nil {
		return Verified{}, ErrMalformed
	}
	created := time.Unix(createdUnix, 0)
	if d := now.Sub(created); d > maxSkew || d < -maxSkew {
		return Verified{}, ErrExpired
	}
	want := ContentDigest(body)
	if subtle.ConstantTimeCompare([]byte(req.Header.Get("Content-Digest")), []byte(want)) != 1 {
		return Verified{}, ErrDigest
	}
	sigB64, ok := strings.CutPrefix(sigHeader, label+"=:")
	if !ok || !strings.HasSuffix(sigB64, ":") {
		return Verified{}, ErrMalformed
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(sigB64, ":"))
	if err != nil {
		return Verified{}, ErrMalformed
	}
	pub, err := resolve(fields["keyid"])
	if err != nil {
		return Verified{}, err
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, signatureBase(req, params), sig) {
		return Verified{}, ErrSignature
	}
	return Verified{KeyID: fields["keyid"], Nonce: fields["nonce"], Created: created}, nil
}
