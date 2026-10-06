package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"pi-fleet/internal/fleetsync"
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
	httpc := &http.Client{Timeout: 60 * time.Second}
	if pem, err := n.store.Config(ctx, configCentralCA); err == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, errors.New("stored central CA is not valid PEM")
		}
		httpc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
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
	var central, username, caFile *string
	_, data, err := parse("activate", args, func(fs *flag.FlagSet) {
		central = fs.String("central", "", "master Pi URL, e.g. https://fleet.example.org:8443")
		username = fs.String("username", "", "your username, from the super user")
		caFile = fs.String("ca", "", "PEM CA certificate, if the master Pi uses a private certificate")
	})
	if err != nil {
		return err
	}
	if *central == "" || *username == "" {
		return errors.New("activate needs -central and -username")
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
	c.printf("Now choose your own password (at least 12 characters).\n")
	chosen, err := c.newPassword(*username, "New password: ")
	if err != nil {
		return err
	}
	act, err := cl.Activate(ctx, *username, oneTime, chosen, passwordParams)
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
func cmdRun(ctx context.Context, args []string, c *cli) error {
	var listen *string
	var every *time.Duration
	_, data, err := parse("run", args, func(fs *flag.FlagSet) {
		listen = fs.String("listen", "127.0.0.1:8080", "address for the web interface (localhost by default; see DESIGN.md §11)")
		every = fs.Duration("every", 5*time.Minute, "sync interval")
	})
	if err != nil {
		return err
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
	cfg := func(key string) func(context.Context) string {
		return func(ctx context.Context) string { v, _ := n.store.Config(ctx, key); return v }
	}
	ui, err := (&web.Server{
		App: a, Role: "node", PHIPatterns: web.DefaultPHIPatterns, Fleet: cl,
		Sync: web.NodeSyncInfo(n.store.DB(), n.local.ChainID, cfg(fleetsync.ConfigLastSync), cfg(fleetsync.ConfigAckedSeq)),
	}).Handler()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() { // background sync with backoff
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
	c.printf("pi-fleet %s: open http://%s in a browser on this Pi\n", version, *listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
