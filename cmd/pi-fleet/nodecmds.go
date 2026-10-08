package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/localnames"
	"pi-fleet/internal/web"
)

// ConfigCentralCA holds a PEM CA bundle for a central with a private cert.
const configCentralCA = "central_ca_pem"

func openNode(ctx context.Context, dir string) (*node, error) {
	n, err := open(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := n.require("node"); err != nil {
		n.Close()
		return nil, err
	}
	return n, nil
}

// client builds the sync client for an employee Pi.
func (n *node) client(ctx context.Context, baseURL string) (*fleetsync.Client, error) {
	if baseURL == "" {
		var err error
		if baseURL, err = n.store.Config(ctx, fleetsync.ConfigCentralURL); err != nil {
			return nil, fmt.Errorf("this Pi is not activated: %w", err)
		}
	}
	// The master Pi is often reached by its .local name, which the system
	// resolves (mDNS) but a static program's own resolver doesn't.
	dial := &localnames.Dialer{Dialer: net.Dialer{Timeout: 10 * time.Second}}
	tr := &http.Transport{DialContext: dial.DialContext, TLSHandshakeTimeout: 10 * time.Second}
	httpc := &http.Client{Timeout: 60 * time.Second, Transport: tr}
	if pem, err := n.store.Config(ctx, configCentralCA); err == nil && pem != "" { // "" means none
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, errors.New("stored central CA is not valid PEM")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &fleetsync.Client{
		BaseURL: baseURL, HTTP: httpc, Store: n.store, Keys: n.keys,
		NodeID: n.local.NodeID, ChainID: n.local.ChainID, Version: version,
		Wipe: func() error { return wipe(n) }, Blobs: n.blobs(),
	}, nil
}

// wipe deletes this Pi's database and keys after central revoked it.
func wipe(n *node) error {
	n.store.Close()
	var errs []error
	for _, f := range []string{"pi-fleet.db", "pi-fleet.db-wal", "pi-fleet.db-shm", "keys/event.key", "keys/transport.key"} {
		if err := os.Remove(filepath.Join(n.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, os.RemoveAll(filepath.Join(n.dir, "blobs")))
	return errors.Join(errs...)
}

func cmdActivate(ctx context.Context, args []string, c *cli) error {
	var central, username, caFile, kiosk *string
	_, data, err := parse("activate", args, func(fs *flag.FlagSet) {
		central = fs.String("central", "", "master Pi URL, e.g. https://fleet.example.org:8443")
		username = fs.String("username", "", "your username, from the super user")
		kiosk = fs.String("kiosk", "", "activate this Pi as the shared kiosk with this name instead")
		caFile = fs.String("ca", "", "PEM CA certificate, if the master Pi uses a private certificate")
	})
	if err != nil {
		return err
	}
	if *central == "" || (*username == "") == (*kiosk == "") {
		return errors.New("activate needs -central and either -username or -kiosk")
	}
	n, err := openNode(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			return err
		}
		if err := n.store.SetConfig(ctx, configCentralCA, string(pem)); err != nil {
			return err
		}
	}
	cl, err := n.client(ctx, *central)
	if err != nil {
		return err
	}
	oneTime, err := c.readSecret("One-time password from your super user: ")
	if err != nil {
		return err
	}
	var act fleetsync.Activation
	if *kiosk != "" {
		act, err = cl.ActivateKiosk(ctx, *kiosk, oneTime)
	} else {
		c.printf("Now choose your own password (at least 12 characters, not one known from data breaches).\n")
		chosen, perr := c.newPassword(*username, "New password: ")
		if perr != nil {
			return perr
		}
		act, err = cl.Activate(ctx, *username, oneTime, chosen, passwordParams)
	}
	if err != nil {
		return err
	}
	if err := n.store.SetConfig(ctx, fleetsync.ConfigCentralNodeID, act.CentralNodeID); err != nil {
		return err
	}
	c.printf("\nThis Pi is waiting for a super user to confirm it.\nRead them these six words:\n\n    %s\n\nThen run: pi-fleet activation-status\n", act.PairingWords)
	return nil
}

func cmdActivationStatus(ctx context.Context, args []string, c *cli) error {
	_, data, err := parse("activation-status", args, nil)
	if err != nil {
		return err
	}
	n, err := openNode(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	cl, err := n.client(ctx, "")
	if err != nil {
		return err
	}
	s, err := cl.ActivationStatus(ctx)
	if err != nil {
		return err
	}
	c.printf("%s\n", s)
	return nil
}

// cmdSetMaster points an activated Pi at the master Pi's new address, or
// its new certificate, checking that the master answers before saving.
func cmdSetMaster(ctx context.Context, args []string, c *cli) error {
	var url, caFile *string
	_, data, err := parse("set-master", args, func(fs *flag.FlagSet) {
		url = fs.String("url", "", "the master Pi's new address, e.g. https://pi-fleet.example.org")
		caFile = fs.String("ca", "", "the master Pi's certificate (PEM), if it changed")
	})
	if err != nil {
		return err
	}
	if *url == "" && *caFile == "" {
		return errors.New("set-master needs -url, -ca or both")
	}
	n, err := openNode(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	oldURL, err := n.store.Config(ctx, fleetsync.ConfigCentralURL)
	if err != nil {
		return fmt.Errorf("this Pi is not activated: %w", err)
	}
	oldCA, _ := n.store.Config(ctx, configCentralCA)
	newURL, newCA := oldURL, oldCA
	if *url != "" {
		newURL = strings.TrimRight(*url, "/")
	}
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			return err
		}
		newCA = string(pem)
	}
	set := func(u, ca string) error {
		if err := n.store.SetConfig(ctx, fleetsync.ConfigCentralURL, u); err != nil {
			return err
		}
		return n.store.SetConfig(ctx, configCentralCA, ca)
	}
	if err := set(newURL, newCA); err != nil {
		return err
	}
	cl, err := n.client(ctx, "")
	if err == nil {
		_, err = cl.ActivationStatus(ctx)
	}
	if err != nil {
		set(oldURL, oldCA)
		return fmt.Errorf("the master Pi didn't answer at %s, so nothing was changed: %w", newURL, err)
	}
	c.printf("this Pi now syncs with the master Pi at %s\n", newURL)
	return nil
}

func cmdSync(ctx context.Context, args []string, c *cli) error {
	var every *time.Duration
	_, data, err := parse("sync", args, func(fs *flag.FlagSet) {
		every = fs.Duration("every", 0, "keep syncing at this interval (e.g. 5m) instead of once")
	})
	if err != nil {
		return err
	}
	n, err := openNode(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	cl, err := n.client(ctx, "")
	if err != nil {
		return err
	}
	once := func() error {
		r, err := cl.Sync(ctx)
		if err != nil {
			return err
		}
		c.printf("%s pushed %d (flagged %d), snapshot %v, purged %d, clock verified %v\n",
			time.Now().Format(time.TimeOnly), r.Pushed, r.Flagged, r.SnapshotLoaded, r.Purged, r.ClockVerified)
		return nil
	}
	if *every <= 0 {
		return once()
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	backoff := 30 * time.Second
	for {
		wait := *every
		switch err := once(); {
		case errors.Is(err, fleetsync.ErrRevoked):
			return err
		case err != nil:
			log.Printf("sync: %v (retrying in %s)", err, backoff)
			wait, backoff = backoff, min(backoff*2, 30*time.Minute)
		default:
			backoff = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// cmdRun serves the web interface on an employee Pi and keeps it synced.
// The interface listens on localhost unless HTTPS is enabled with a
// certificate from the master Pi's fleet CA (DESIGN.md §10).
func cmdRun(ctx context.Context, args []string, c *cli) error {
	var listen, names *string
	var every *time.Duration
	var https, insecureLAN *bool
	_, data, err := parse("run", args, func(fs *flag.FlagSet) {
		listen = fs.String("listen", "127.0.0.1:8080", "address for the web interface")
		every = fs.Duration("every", 5*time.Minute, "sync interval")
		https = fs.Bool("https", false, "serve HTTPS with a certificate from the master Pi (for use from other devices)")
		names = fs.String("tls-names", "", "comma-separated host names and IPs for the certificate (default: this Pi's host name)")
		insecureLAN = fs.Bool("insecure-lan", false, "serve plain HTTP beyond localhost (not recommended; shows a warning)")
	})
	if err != nil {
		return err
	}
	if !*https && !*insecureLAN && !loopback(*listen) {
		return fmt.Errorf("%s is reachable from other devices: use -https (recommended) or -insecure-lan", *listen)
	}
	n, err := openNode(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	a, err := n.app(ctx)
	if err != nil {
		return err
	}
	cl, err := n.client(ctx, "")
	if err != nil {
		return err
	}
	keysDir := filepath.Join(n.dir, "keys")
	certNames := tlsNames(*names)
	if *https {
		if _, err := cl.EnsureTLSCert(ctx, keysDir, certNames); err != nil {
			if _, statErr := os.Stat(filepath.Join(keysDir, fleetsync.TLSCertFile)); statErr != nil {
				return fmt.Errorf("getting a certificate from the master Pi: %w", err)
			}
			log.Printf("tls: renewal failed, using the current certificate: %v", err)
		}
	}
	cfg := func(ctx context.Context, key string) string { v, _ := n.store.Config(ctx, key); return v }
	ui, err := (&web.Server{
		App: a, Role: "node", PHIPatterns: web.DefaultPHIPatterns, Fleet: cl, Secure: *https,
		InsecureLAN: *insecureLAN && !*https,
		Sync:        web.NodeSyncInfo(n.store.DB(), n.local.ChainID, cfg), Version: version,
	}).Handler()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() { // background sync with backoff, and certificate renewal
		backoff := 30 * time.Second
		for {
			wait := *every
			r, err := cl.Sync(ctx)
			switch {
			case errors.Is(err, fleetsync.ErrRevoked):
				log.Printf("sync: %v", err)
				stop()
				return
			case err != nil:
				log.Printf("sync: %v (retrying in %s)", err, backoff)
				wait, backoff = backoff, min(backoff*2, 30*time.Minute)
			default:
				backoff = 30 * time.Second
				if r.Pushed > 0 || r.SnapshotLoaded {
					log.Printf("sync: pushed %d (flagged %d), snapshot %v", r.Pushed, r.Flagged, r.SnapshotLoaded)
				}
				if *https {
					if renewed, err := cl.EnsureTLSCert(ctx, keysDir, certNames); err != nil {
						log.Printf("tls: renewal: %v", err)
					} else if renewed {
						log.Printf("tls: installed a renewed certificate")
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()

	srv := &http.Server{Addr: *listen, Handler: ui, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if *https {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certLoader(keysDir)}
		c.printf("pi-fleet %s: serving https://%s for %v\n", version, *listen, certNames)
		err = srv.ListenAndServeTLS("", "")
	} else {
		c.printf("pi-fleet %s: open http://%s in a browser\n", version, *listen)
		err = srv.ListenAndServe()
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loopback reports whether addr only listens on this machine.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tlsNames returns the requested certificate names, or this Pi's host
// name and its .local (mDNS) form.
func tlsNames(flagValue string) []string {
	var out []string
	for _, n := range strings.Split(flagValue, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		if h, err := os.Hostname(); err == nil {
			h = strings.ToLower(h)
			out = append(out, h, h+".local")
		}
	}
	return out
}

// certLoader serves the current certificate, picking up renewals without
// a restart.
func certLoader(keysDir string) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	var mu sync.Mutex
	var cached *tls.Certificate
	var loadedAt time.Time
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		certPath := filepath.Join(keysDir, fleetsync.TLSCertFile)
		info, err := os.Stat(certPath)
		if err != nil {
			return nil, err
		}
		if cached == nil || info.ModTime().After(loadedAt) {
			pair, err := tls.LoadX509KeyPair(certPath, filepath.Join(keysDir, fleetsync.TLSKeyFile))
			if err != nil {
				return nil, err
			}
			cached, loadedAt = &pair, info.ModTime()
		}
		return cached, nil
	}
}
