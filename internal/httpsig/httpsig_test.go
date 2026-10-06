package httpsig

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"events":[]}`)
	resolve := func(id string) (ed25519.PublicKey, error) {
		if id == "k1" {
			return pub, nil
		}
		return nil, errors.New("unknown key")
	}

	newReq := func() *http.Request {
		r := httptest.NewRequest("POST", "https://central.example/v1/sync/events?x=1", strings.NewReader(string(body)))
		if err := Sign(r, body, "k1", priv, now, "n-123"); err != nil {
			t.Fatal(err)
		}
		return r
	}

	v, err := Verify(newReq(), body, resolve, now.Add(time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if v.KeyID != "k1" || v.Nonce != "n-123" || !v.Created.Equal(now) {
		t.Errorf("Verified = %+v", v)
	}

	tests := []struct {
		name   string
		mutate func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time)
		want   error
	}{
		{"body changed", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			return []byte(`{"events":[1]}`), resolve, now
		}, ErrDigest},
		{"digest and body changed together", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			b := []byte(`{"events":[1]}`)
			r.Header.Set("Content-Digest", ContentDigest(b))
			return b, resolve, now
		}, ErrSignature},
		{"path changed", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			r.URL.Path = "/v1/sync/hello"
			return body, resolve, now
		}, ErrSignature},
		{"query changed", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			r.URL.RawQuery = "x=2"
			return body, resolve, now
		}, ErrSignature},
		{"method changed", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			r.Method = "PUT"
			return body, resolve, now
		}, ErrSignature},
		{"nonce changed", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			r.Header.Set("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), "n-123", "n-999", 1))
			return body, resolve, now
		}, ErrSignature},
		{"wrong key", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			return body, func(string) (ed25519.PublicKey, error) { return otherPub, nil }, now
		}, ErrSignature},
		{"too old", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			return body, resolve, now.Add(6 * time.Minute)
		}, ErrExpired},
		{"from the future", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			return body, resolve, now.Add(-6 * time.Minute)
		}, ErrExpired},
		{"unsigned", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			r.Header.Del("Signature")
			return body, resolve, now
		}, ErrMissing},
		{"fewer components", func(r *http.Request) ([]byte, func(string) (ed25519.PublicKey, error), time.Time) {
			r.Header.Set("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), ` "content-digest"`, "", 1))
			return body, resolve, now
		}, ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReq()
			b, res, at := tt.mutate(r)
			if _, err := Verify(r, b, res, at, 5*time.Minute); !errors.Is(err, tt.want) {
				t.Errorf("Verify error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestSignatureBaseFormat pins the exact RFC 9421 signature base.
func TestSignatureBaseFormat(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	r := httptest.NewRequest("GET", "https://c.example/v1/sync/snapshot", nil)
	if err := Sign(r, nil, "k1", priv, time.Unix(1700000000, 0), "abc"); err != nil {
		t.Fatal(err)
	}
	params, _ := strings.CutPrefix(r.Header.Get("Signature-Input"), "sig1=")
	want := `"@method": GET
"@path": /v1/sync/snapshot
"@query": ?
"content-digest": sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:
"@signature-params": ("@method" "@path" "@query" "content-digest");created=1700000000;keyid="k1";alg="ed25519";nonce="abc"`
	if got := string(signatureBase(r, params)); got != want {
		t.Errorf("signature base:\n%s\nwant:\n%s", got, want)
	}
}
