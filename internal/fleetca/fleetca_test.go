package fleetca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreate(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreate(dir, "test")
	if err != nil || !again.Cert.Equal(ca.Cert) {
		t.Fatalf("reload: %v", err)
	}
	keyPEM, csrPEM, err := NewKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	certPEM, err := ca.Issue(csrPEM, "node-1", []string{"kiosk-nyc.local", "192.168.1.20"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("cert does not match key: %v", err)
	}
	b, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(b.Bytes)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM)
	for _, name := range []string{"kiosk-nyc.local", "192.168.1.20"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "bank.example.com"}); err == nil {
		t.Error("certificate valid for a name it wasn't issued for")
	}
	if NeedsRenewal(certPEM, []string{"kiosk-nyc.local"}, now) {
		t.Error("fresh certificate needs renewal")
	}
	if !NeedsRenewal(certPEM, []string{"kiosk-nyc.local"}, now.Add(340*24*time.Hour)) {
		t.Error("expiring certificate not renewed")
	}
	if !NeedsRenewal(certPEM, []string{"other.local"}, now) {
		t.Error("certificate lacking a wanted name not renewed")
	}
	renewCSR, err := CSRFromKey(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Issue(renewCSR, "node-1", []string{"kiosk-nyc.local"}, now); err != nil {
		t.Fatalf("renewal: %v", err)
	}
}

func TestIssueRefusals(t *testing.T) {
	ca, _ := LoadOrCreate(t.TempDir(), "test")
	_, csrPEM, _ := NewKeyAndCSR()
	for _, names := range [][]string{nil, {"*.example.com"}, {"bad name"}, {"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}} {
		if _, err := ca.Issue(csrPEM, "n", names, time.Now()); err == nil {
			t.Errorf("issued for %v", names)
		}
	}
	tampered := []byte(string(csrPEM)[:len(csrPEM)/2] + "AAAA" + string(csrPEM)[len(csrPEM)/2+4:])
	if _, err := ca.Issue(tampered, "n", []string{"x.local"}, time.Now()); err == nil {
		t.Error("issued for a tampered CSR")
	}
}
