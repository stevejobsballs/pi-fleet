package setup

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

var dnsNameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// parseNetworkNames reads the names and addresses employee Pis at other
// sites use to reach the master Pi, separated by commas or spaces.
func parseNetworkNames(s string) (names []string, ips []net.IP, err error) {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		f = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(f, "https://"), "http://"), "/"))
		if h, _, err := net.SplitHostPort(f); err == nil {
			f = h // a port is set when the Pis connect, not in the certificate
		}
		if ip := net.ParseIP(f); ip != nil {
			ips = append(ips, ip)
			continue
		}
		if !dnsNameRE.MatchString(f) {
			return nil, nil, fmt.Errorf("%q isn't a network name (like pi-fleet.example.org) or an address (like 10.20.30.40)", f)
		}
		names = append(names, f)
	}
	return names, ips, nil
}

// askNetworkNames asks for the names IT gives the master Pi for Pis at
// other sites; they must be in its certificate, which Pis check.
func (w *Wizard) askNetworkNames() ([]string, []net.IP, error) {
	u := w.UI
	u.Say("Employee Pis on this network find the master by its own name. Pis at")
	u.Say("other sites reach it by a name or address your IT department gives it")
	u.Say("(see docs/NETWORK.md). Employee Pis check that name against the master's")
	u.Say("certificate, so it has to be included now.")
	for {
		a, err := u.line("Network names or addresses for other sites, separated by commas (or just press Enter if every Pi is on this network): ")
		if err != nil {
			return nil, nil, err
		}
		names, ips, err := parseNetworkNames(a)
		if err != nil {
			u.Say("  %v", err)
			continue
		}
		return names, ips, nil
	}
}

// certNames returns the names and addresses in a certificate.
func certNames(certPEM []byte) ([]string, []net.IP, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return nil, nil, errors.New("not a PEM certificate")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return c.DNSNames, c.IPAddresses, nil
}

func addNames(names []string, ips []net.IP, moreNames []string, moreIPs []net.IP) ([]string, []net.IP) {
	for _, n := range moreNames {
		if !containsString(names, n) {
			names = append(names, n)
		}
	}
	for _, ip := range moreIPs {
		found := false
		for _, have := range ips {
			if have.Equal(ip) {
				found = true
			}
		}
		if !found {
			ips = append(ips, ip)
		}
	}
	return names, ips
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var tlsPathsRE = regexp.MustCompile(`-tls-cert (\S+) -tls-key (\S+)`)

// addCertNames adds names or addresses to an existing master Pi's
// certificate. Employee Pis already connected must then trust the new one.
func (w *Wizard) addCertNames(unit string, port int) error {
	u := w.UI
	u.Step("Add a network name to the master's certificate")
	m := tlsPathsRE.FindStringSubmatch(unit)
	if m == nil {
		return errors.New("couldn't find the master Pi's certificate in its service settings")
	}
	certPath, keyPath := m[1], m[2]
	old, err := w.Sys.ReadFile(certPath)
	if err != nil {
		return err
	}
	names, ips, err := certNames(old)
	if err != nil {
		return err
	}
	have := append([]string{}, names...)
	for _, ip := range ips {
		have = append(have, ip.String())
	}
	u.Say("The certificate covers: %s", strings.Join(have, ", "))
	moreNames, moreIPs, err := w.askNetworkNames()
	if err != nil {
		return err
	}
	if len(moreNames)+len(moreIPs) == 0 {
		u.Say("Nothing to add.")
		return nil
	}
	u.Say("")
	u.Say("This makes a new certificate with a new fingerprint. Employee Pis that")
	u.Say("are already connected stop syncing until, on each one, setup is run and")
	u.Say("\"Connect to the master again\" chosen (it shows the new fingerprint to")
	u.Say("check). Their work is kept meanwhile.")
	ok, err := u.Confirm("Make the new certificate?", false)
	if err != nil || !ok {
		if err == nil {
			err = ErrCancelled
		}
		return err
	}
	names, ips = addNames(names, ips, moreNames, moreIPs)
	cert, key, err := NewCertificate(names, ips, w.Now())
	if err != nil {
		return err
	}
	if err := w.Sys.WriteFile(certPath, cert, 0o644); err != nil {
		return err
	}
	if err := w.Sys.WriteFile(keyPath, key, 0o600); err != nil {
		return err
	}
	if err := w.Sys.Run("chown", "pifleet:pifleet", certPath, keyPath); err != nil {
		return err
	}
	if err := w.restart(fmt.Sprintf("https://127.0.0.1:%d/login", port)); err != nil {
		return err
	}
	fp, _ := Fingerprint(cert)
	u.Say("")
	u.Say("New certificate fingerprint (also on the Pis page):")
	u.Say("    %s", fp)
	return nil
}

// reconnect points an employee Pi at the master Pi again, after its
// address or certificate changed.
func (w *Wizard) reconnect() error {
	base, err := w.masterAddress()
	if err != nil {
		return err
	}
	if err := w.asPifleet("set-master", "-data", NodeDataDir, "-url", base, "-ca", MasterCA); err != nil {
		return fmt.Errorf("couldn't connect to the master Pi (see above): %w", err)
	}
	return w.restart("http://127.0.0.1:8080/login")
}
