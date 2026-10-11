package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/sitecert"
)

func testCert(t *testing.T, names ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(0, 0, 20)}
	issuer := *tmpl
	issuer.Subject = pkix.Name{CommonName: "Hospital Issuing CA"}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, &issuer, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})
}

// A super user installs the hospital's certificate on the Pis page: it's
// described, with a warning when it isn't for the address phones use or
// expires soon; a mismatched key is refused; it can be removed.
func TestInstallingTheHospitalsCertificate(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	own, ownKey := testCert(t, "steve.local")
	os.WriteFile(filepath.Join(dir, "cert.pem"), own, 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), ownKey, 0o600)
	certs, err := sitecert.NewServer(dir, filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{App: e.app, Role: "central", Now: func() time.Time { return time.Now() }, Secure: true, Version: "v0.15.2", SiteCert: certs}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewTLSServer(h)
	t.Cleanup(e.srv.Close)
	e.app.Store.SetConfig(e.ctx, fleetsync.ConfigReportURL, "https://pifleet.hospital.org")

	admin := e.browser()
	admin.login("admin", "tumbleweed-gasket-42")
	_, _, page := admin.get("/admin/nodes")
	if !strings.Contains(page, `id="certificate"`) || !strings.Contains(page, "None installed") {
		t.Fatalf("Pis page:\n%s", page)
	}
	cert, key := testCert(t, "pifleet.hospital.org")
	_, otherKey := testCert(t, "x.example")
	if _, page = admin.postMultipart("/admin/certificate", nil, upFile{"cert", "cert.pem", cert}, upFile{"key", "key.pem", otherKey}); !strings.Contains(page, "Not installed: the private key doesn&#39;t belong to this certificate.") {
		t.Fatalf("mismatch:\n%s", page)
	}
	if _, page = admin.postMultipart("/admin/certificate", nil, upFile{"cert", "cert.pem", cert}, upFile{"key", "key.pem", key}); !strings.Contains(page, "Installed. Browsers and phones now get the certificate for pifleet.hospital.org.") ||
		!strings.Contains(page, "Hospital Issuing CA") || !strings.Contains(page, "Ask IT for a renewed one.") || !strings.Contains(page, "The hospital&#39;s certificate expires in 19 days") {
		t.Fatalf("installed:\n%s", page)
	}
	// One that isn't for the address phones use says so.
	wrong, wrongKey := testCert(t, "intranet.hospital.org")
	if _, page = admin.postMultipart("/admin/certificate", nil, upFile{"cert", "c.pem", wrong}, upFile{"key", "k.pem", wrongKey}); !strings.Contains(page, "It isn&#39;t for pifleet.hospital.org, the address phones use") {
		t.Fatalf("wrong name:\n%s", page)
	}
	if _, page = admin.post("/admin/certificate/remove", nil); !strings.Contains(page, "Removed.") || !strings.Contains(page, "None installed") {
		t.Fatalf("removed:\n%s", page)
	}
	// Only super users.
	e.user("mona", "Mona Mid", "mid_tier")
	mona := e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	if code, _ := mona.postMultipart("/admin/certificate", nil, upFile{"cert", "cert.pem", cert}, upFile{"key", "key.pem", key}); code != 403 {
		t.Fatalf("mid-tier installed a certificate: %d", code)
	}
}
