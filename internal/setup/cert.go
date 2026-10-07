package setup

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// NewCertificate makes the master Pi's self-signed HTTPS certificate:
// ECDSA P-256, which every browser accepts (Ed25519 is not), for its host
// names and addresses.
func NewCertificate(names []string, ips []net.IP, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"pi-fleet master Pi"}},
		DNSNames:              names,
		IPAddresses:           ips,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // employee Pis trust it directly, as their CA file
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), nil
}

// Fingerprint is a certificate's SHA-256 in groups of four, as people
// compare it between two screens.
func Fingerprint(certPEM []byte) (string, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" {
		return "", errors.New("not a PEM certificate")
	}
	return fingerprintDER(b.Bytes), nil
}

func fingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var groups []string
	for i := 0; i < len(h); i += 4 {
		groups = append(groups, h[i:i+4])
	}
	return strings.Join(groups, " ")
}

// browsersAccept reports whether a certificate's key type works in web
// browsers (early trial installs made Ed25519 ones, which don't).
func browsersAccept(certPEM []byte) bool {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return false
	}
	c, err := x509.ParseCertificate(b.Bytes)
	return err == nil && c.PublicKeyAlgorithm != x509.Ed25519
}

// FetchCertificate connects to the master Pi and returns the certificate
// it presents, unverified: the user checks the fingerprint against the
// one the master Pi shows before it is trusted.
func FetchCertificate(hostport string, timeout time.Duration) ([]byte, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(d, "tcp", hostport, &tls.Config{InsecureSkipVerify: true}) // checked by the user's eyes
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("%s presented no certificate", hostport)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certs[0].Raw}), nil
}
