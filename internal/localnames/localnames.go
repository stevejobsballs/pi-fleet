// Package localnames lets pi-fleet reach a master Pi by its ".local" name.
//
// Raspberry Pi OS resolves name.local through mDNS (avahi, via the C
// library's nss-mdns). pi-fleet is a static Go program, whose own resolver
// doesn't do mDNS and asks the DNS server instead, which doesn't know the
// name. So for .local names it asks the system, through getent, and tries
// each address in turn: a master often has several (Wi-Fi and wired), not
// all reachable from every Pi.
package localnames

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// lookup is replaced in tests.
var lookup = systemLookup

// systemLookup asks the C library (and so mDNS) for a name's addresses.
func systemLookup(ctx context.Context, host string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "getent", "ahosts", host).Output()
	if err != nil {
		return nil, fmt.Errorf("%s: no address found on this network (is the master Pi switched on and on the same network?)", host)
	}
	var addrs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || seen[f[0]] || net.ParseIP(f[0]) == nil {
			continue
		}
		seen[f[0]] = true
		addrs = append(addrs, f[0])
	}
	// IPv4 first: link-local IPv6 addresses need a zone getent doesn't give.
	var v4, v6 []string
	for _, a := range addrs {
		if strings.Contains(a, ":") {
			if !strings.HasPrefix(strings.ToLower(a), "fe80:") {
				v6 = append(v6, a)
			}
		} else {
			v4 = append(v4, a)
		}
	}
	if len(v4)+len(v6) == 0 {
		return nil, fmt.Errorf("%s: no usable address found", host)
	}
	return append(v4, v6...), nil
}

// IsLocal reports whether host is an mDNS name.
func IsLocal(host string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".local")
}

// Dialer dials like net.Dialer, resolving .local names through the system.
type Dialer struct {
	net.Dialer
	// PerAddress bounds each attempt when a name has several addresses.
	PerAddress time.Duration
}

// DialContext connects to address ("host:port").
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !IsLocal(host) {
		return d.Dialer.DialContext(ctx, network, address)
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	per := d.PerAddress
	if per == 0 {
		per = 5 * time.Second
	}
	var errs []error
	for _, a := range addrs {
		actx, cancel := context.WithTimeout(ctx, per)
		c, err := d.Dialer.DialContext(actx, network, net.JoinHostPort(a, port))
		cancel()
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%s: couldn't connect to any of its addresses: %w", host, errors.Join(errs...))
}

// Dial is DialContext without a context, for APIs that need it.
func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}
