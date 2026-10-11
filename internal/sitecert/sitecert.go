// Package sitecert lets the master Pi present the hospital's own HTTPS
// certificate to browsers and phones, which already trust it, while
// employee Pis keep getting pi-fleet's own certificate, the one they
// pinned when they were set up.
//
// The two are told apart in the TLS handshake: a Pi offers the
// application protocol SyncALPN alongside http/1.1, and gets pi-fleet's
// own certificate; anyone else gets the site certificate, when one is
// installed. The connection itself then uses http/1.1 as usual.
package sitecert

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// SyncALPN is what employee Pis offer, to be given pi-fleet's own
// certificate.
const SyncALPN = "pifleet-sync/1"

// ClientProtos are the protocols a Pi offers when connecting to the
// master Pi.
var ClientProtos = []string{SyncALPN, "http/1.1"}

// Files of the site certificate, in the master Pi's tls folder beside
// pi-fleet's own cert.pem and key.pem.
const (
	CertFile = "site-cert.pem"
	KeyFile  = "site-key.pem"
)

// Info describes a certificate, for showing it.
type Info struct {
	Subject, Issuer string
	Names           []string // DNS names and IP addresses it is for
	NotBefore       time.Time
	NotAfter        time.Time
	SelfSigned      bool
	Chain           int // certificates in the file, the server's first
}

// Covers reports whether the certificate is for host (a name or an IP
// address), as a browser would decide.
func (i Info) Covers(host string) bool {
	for _, n := range i.Names {
		if strings.EqualFold(n, host) {
			return true
		}
		if strings.HasPrefix(n, "*.") && net.ParseIP(host) == nil {
			if dot := strings.IndexByte(host, '.'); dot > 0 && strings.EqualFold(n[1:], host[dot:]) {
				return true
			}
		}
	}
	return false
}

// Check reads a certificate (with any intermediate certificates after
// it) and its private key, both PEM, and checks they belong together and
// are valid at now.
func Check(certPEM, keyPEM []byte, now time.Time) (tls.Certificate, Info, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "private key does not match"):
			return pair, Info{}, errors.New("the private key doesn't belong to this certificate")
		case strings.Contains(msg, "failed to find any PEM data in certificate"), strings.Contains(msg, "failed to find certificate PEM data"):
			return pair, Info{}, errors.New("the certificate file isn't a PEM certificate (it should start with -----BEGIN CERTIFICATE-----)")
		case strings.Contains(msg, "PEM data in key"), strings.Contains(msg, "failed to parse private key"), strings.Contains(msg, "rather than a key"):
			return pair, Info{}, errors.New("the key file isn't an unencrypted PEM private key (it should start with -----BEGIN PRIVATE KEY----- or similar)")
		}
		return pair, Info{}, fmt.Errorf("the certificate and key can't be used: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return pair, Info{}, fmt.Errorf("the certificate can't be read: %v", err)
	}
	pair.Leaf = leaf
	info := Info{Subject: leaf.Subject.CommonName, Issuer: leaf.Issuer.CommonName, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
		Chain: len(pair.Certificate), SelfSigned: leaf.Subject.String() == leaf.Issuer.String()}
	if info.Subject == "" {
		info.Subject = leaf.Subject.String()
	}
	if info.Issuer == "" {
		info.Issuer = leaf.Issuer.String()
	}
	info.Names = append(info.Names, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		info.Names = append(info.Names, ip.String())
	}
	switch {
	case now.Before(leaf.NotBefore):
		return pair, info, fmt.Errorf("the certificate isn't valid until %s", leaf.NotBefore.UTC().Format("2 January 2006"))
	case now.After(leaf.NotAfter):
		return pair, info, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.UTC().Format("2 January 2006"))
	case len(info.Names) == 0:
		return pair, info, errors.New("the certificate names no address (it has no subject alternative names), so browsers won't accept it")
	}
	return pair, info, nil
}

// Server chooses the certificate for each connection.
type Server struct {
	dir  string // the tls folder
	own  tls.Certificate
	site atomic.Pointer[tls.Certificate]
}

// NewServer serves pi-fleet's own certificate (ownCert, ownKey) and, if
// one is installed in dir, the site certificate.
func NewServer(dir, ownCert, ownKey string) (*Server, error) {
	own, err := tls.LoadX509KeyPair(ownCert, ownKey)
	if err != nil {
		return nil, err
	}
	s := &Server{dir: dir, own: own}
	return s, s.Reload()
}

// Reload loads the site certificate from the tls folder, or forgets it if
// there is none. One that can't be loaded is ignored, so the master
// stays reachable with its own certificate.
func (s *Server) Reload() error {
	certPEM, err1 := os.ReadFile(filepath.Join(s.dir, CertFile))
	keyPEM, err2 := os.ReadFile(filepath.Join(s.dir, KeyFile))
	if errors.Is(err1, os.ErrNotExist) || errors.Is(err2, os.ErrNotExist) {
		s.site.Store(nil)
		return nil
	}
	if err := errors.Join(err1, err2); err != nil {
		s.site.Store(nil)
		return err
	}
	pair, _, err := Check(certPEM, keyPEM, time.Now())
	if err != nil && pair.Leaf == nil {
		s.site.Store(nil)
		return err
	}
	// An expired site certificate is still served: browsers then say why,
	// and the super users are warned to renew it.
	s.site.Store(&pair)
	return nil
}

// Site describes the site certificate in use, if any.
func (s *Server) Site() (Info, bool) {
	p := s.site.Load()
	if p == nil {
		return Info{}, false
	}
	certPEM, _ := os.ReadFile(filepath.Join(s.dir, CertFile))
	keyPEM, _ := os.ReadFile(filepath.Join(s.dir, KeyFile))
	_, info, _ := Check(certPEM, keyPEM, time.Now())
	return info, info.Subject != ""
}

// GetCertificate is for tls.Config: pi-fleet's own certificate for
// employee Pis, the site certificate for everyone else.
func (s *Server) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if site := s.site.Load(); site != nil && !slices.Contains(hello.SupportedProtos, SyncALPN) {
		return site, nil
	}
	return &s.own, nil
}

// Install saves a checked site certificate and key in the tls folder and
// starts serving it.
func (s *Server) Install(certPEM, keyPEM []byte) error {
	if _, _, err := Check(certPEM, keyPEM, time.Now()); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.dir, KeyFile), keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.dir, CertFile), certPEM, 0o644); err != nil {
		return err
	}
	return s.Reload()
}

// Remove stops using the site certificate; browsers get pi-fleet's own
// again.
func (s *Server) Remove() error {
	for _, f := range []string{CertFile, KeyFile} {
		if err := os.Remove(filepath.Join(s.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.Reload()
}

// writeFile replaces a file in one step, so a reader never sees half.
func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".new-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
