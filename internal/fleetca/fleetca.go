// Package fleetca is the master Pi's certificate authority for employee
// Pis' local web interfaces (DESIGN.md §6.1–6.2), so tablets and laptops
// on a site's network can reach a Pi (e.g. a kiosk) over HTTPS.
//
// It issues TLS certificates only. Sync trust never depends on it: nodes
// and central authenticate each other with pinned Ed25519 keys.
package fleetca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	caCertFile = "fleet-ca.crt"
	caKeyFile  = "fleet-ca.key"
	// CertLifetime is how long a Pi's certificate is valid.
	CertLifetime = 365 * 24 * time.Hour
	// RenewBefore is how early a Pi renews.
	RenewBefore = 30 * 24 * time.Hour
	// MaxNames bounds the names on one certificate.
	MaxNames = 10
)

// CA issues certificates for Pis.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte
}

// LoadOrCreate loads the CA from dir, creating it on first use.
func LoadOrCreate(dir, fleetName string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	if certPEM, err := os.ReadFile(certPath); err == nil {
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		return parse(certPEM, keyPEM)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "pi-fleet CA " + fleetName, Organization: []string{"pi-fleet"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return parse(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func parse(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("fleetca: malformed CA files")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, PEM: certPEM}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

var dnsRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// SplitNames validates requested names into DNS names and IP addresses.
// Wildcards are refused.
func SplitNames(names []string) (dns []string, ips []net.IP, err error) {
	if len(names) == 0 || len(names) > MaxNames {
		return nil, nil, fmt.Errorf("fleetca: give 1 to %d names", MaxNames)
	}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
			continue
		}
		if !dnsRE.MatchString(n) || len(n) > 253 {
			return nil, nil, fmt.Errorf("fleetca: %q is not a host name or IP address (wildcards are not allowed)", n)
		}
		dns = append(dns, n)
	}
	return dns, ips, nil
}

// Issue signs a certificate for the CSR's key, for the given names. The
// CSR's own subject and extensions are ignored: central decides.
func (ca *CA) Issue(csrPEM []byte, nodeID string, names []string, now time.Time) ([]byte, error) {
	b, _ := pem.Decode(csrPEM)
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("fleetca: not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("fleetca: CSR signature: %w", err)
	}
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, errors.New("fleetca: only ECDSA keys are accepted")
	}
	dns, ips, err := SplitNames(names)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "pi-fleet node " + nodeID, Organization: []string{"pi-fleet"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(CertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// NewKeyAndCSR makes a Pi's TLS key and a certificate request for it.
func NewKeyAndCSR() (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "pi-fleet node"}}, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), nil
}

// CSRFromKey makes a certificate request for an existing key (renewal).
func CSRFromKey(keyPEM []byte) ([]byte, error) {
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return nil, errors.New("fleetca: malformed key")
	}
	key, err := x509.ParseECPrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "pi-fleet node"}}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), nil
}

// NeedsRenewal reports whether a certificate is missing, unparsable,
// expiring within RenewBefore, or lacks one of the wanted names.
func NeedsRenewal(certPEM []byte, names []string, now time.Time) bool {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return true
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil || now.Add(RenewBefore).After(c.NotAfter) {
		return true
	}
	for _, n := range names {
		if c.VerifyHostname(strings.TrimSpace(n)) != nil {
			return true
		}
	}
	return false
}
