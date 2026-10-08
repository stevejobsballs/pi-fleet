package localnames

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestDialsALocalNameAtWhicheverAddressAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// The first address (say, the master's Wi-Fi) doesn't answer.
	lookup = func(ctx context.Context, host string) ([]string, error) {
		if host != "fleet-master.local" {
			return nil, errors.New("unknown")
		}
		return []string{"192.0.2.1", "127.0.0.1"}, nil
	}
	defer func() { lookup = systemLookup }()
	d := &Dialer{PerAddress: 300 * time.Millisecond}
	c, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("fleet-master.local", port))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := d.DialContext(context.Background(), "tcp", "nowhere.local:1"); err == nil {
		t.Fatal("dialled an unknown name")
	}
}

func TestIsLocal(t *testing.T) {
	for in, want := range map[string]bool{"fleet-master.local": true, "Fleet-Master.LOCAL.": true, "pi-fleet.example.org": false, "10.0.0.5": false} {
		if IsLocal(in) != want {
			t.Errorf("%s", in)
		}
	}
}

// On a Pi with avahi, the system finds this Pi's own .local name.
func TestSystemLookupOnThisPi(t *testing.T) {
	addrs, err := systemLookup(context.Background(), "localhost")
	if err != nil || len(addrs) == 0 {
		t.Skipf("getent not usable here: %v", err)
	}
}
