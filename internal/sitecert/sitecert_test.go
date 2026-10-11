package sitecert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeCert returns a PEM certificate and key for names, signed by itself.
func makeCert(t *testing.T, cn string, names []string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})
}

func TestCheck(t *testing.T) {
	now := time.Now()
	good, key := makeCert(t, "pifleet.hospital.org", []string{"pifleet.hospital.org", "10.1.2.3"}, now.Add(-time.Hour), now.AddDate(1, 0, 0))
	_, info, err := Check(good, key, now)
	if err != nil || info.Subject != "pifleet.hospital.org" || !info.Covers("PIFLEET.hospital.org") || !info.Covers("10.1.2.3") || info.Covers("steve.local") {
		t.Fatalf("good: %+v %v", info, err)
	}
	wild, wkey := makeCert(t, "wild", []string{"*.hospital.org"}, now.Add(-time.Hour), now.AddDate(1, 0, 0))
	if _, info, _ := Check(wild, wkey, now); !info.Covers("pifleet.hospital.org") || info.Covers("a.b.hospital.org") || info.Covers("hospital.org") {
		t.Fatalf("wildcard: %+v", info)
	}
	_, otherKey := makeCert(t, "other", []string{"x"}, now.Add(-time.Hour), now.AddDate(1, 0, 0))
	expired, ekey := makeCert(t, "old", []string{"old"}, now.AddDate(-2, 0, 0), now.AddDate(-1, 0, 0))
	nameless, nkey := makeCert(t, "nameless", nil, now.Add(-time.Hour), now.AddDate(1, 0, 0))
	for _, c := range []struct {
		name      string
		cert, key []byte
		want      string
	}{
		{"wrong key", good, otherKey, "doesn't belong"},
		{"key as cert", key, key, "isn't a PEM certificate"},
		{"cert as key", good, good, "isn't an unencrypted PEM private key"},
		{"expired", expired, ekey, "expired on"},
		{"no names", nameless, nkey, "names no address"},
	} {
		if _, _, err := Check(c.cert, c.key, now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// Over a real connection: employee Pis (offering SyncALPN) get pi-fleet's
// own certificate; browsers get the site's once installed.
func TestPisAndBrowsersGetTheirCertificates(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	ownCert, ownKey := makeCert(t, "steve.local", []string{"steve.local"}, now.Add(-time.Hour), now.AddDate(10, 0, 0))
	os.WriteFile(filepath.Join(dir, "cert.pem"), ownCert, 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), ownKey, 0o600)
	s, err := NewServer(dir, filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: s.GetCertificate, NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	served := func(protos []string) (string, string) {
		t.Helper()
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: protos})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		st := c.ConnectionState()
		return st.PeerCertificates[0].Subject.CommonName, st.NegotiatedProtocol
	}
	browser := []string{"h2", "http/1.1"}
	if cn, _ := served(browser); cn != "steve.local" {
		t.Fatalf("before installing, a browser got %s", cn)
	}
	siteCert, siteKey := makeCert(t, "pifleet.hospital.org", []string{"pifleet.hospital.org"}, now.Add(-time.Hour), now.AddDate(1, 0, 0))
	if err := s.Install(siteCert, siteKey); err != nil {
		t.Fatal(err)
	}
	if cn, _ := served(browser); cn != "pifleet.hospital.org" {
		t.Fatalf("a browser got %s", cn)
	}
	if cn, proto := served(ClientProtos); cn != "steve.local" || proto != "http/1.1" {
		t.Fatalf("a Pi got %s over %q", cn, proto)
	}
	if info, ok := s.Site(); !ok || info.Names[0] != "pifleet.hospital.org" {
		t.Fatalf("site: %+v %v", info, ok)
	}
	// Kept across restarts; removable.
	s2, err := NewServer(dir, filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Site(); !ok {
		t.Fatal("not loaded after a restart")
	}
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if cn, _ := served(browser); cn != "steve.local" {
		t.Fatalf("after removing, a browser got %s", cn)
	}
	// A bad pair isn't installed, and the old one stays.
	_, other := makeCert(t, "x", []string{"x"}, now.Add(-time.Hour), now.AddDate(1, 0, 0))
	if err := s.Install(siteCert, other); err == nil {
		t.Fatal("installed a mismatched key")
	}
	if _, err := os.Stat(filepath.Join(dir, CertFile)); !os.IsNotExist(err) {
		t.Fatal("a bad certificate was saved")
	}
}
