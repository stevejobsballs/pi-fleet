package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	"syscall"
	"time"

	"pi-fleet/internal/localnames"
	"pi-fleet/internal/sitecert"
	"pi-fleet/internal/standby"
	"pi-fleet/internal/store"
)

// standbyClient reaches the master Pi, trusting only its own certificate
// (pinned when the standby was paired), as employee Pis do.
func standbyClient(caPEM string) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("the master's certificate isn't valid PEM")
	}
	dial := &localnames.Dialer{Dialer: net.Dialer{Timeout: 10 * time.Second}}
	tr := &http.Transport{DialContext: dial.DialContext, TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, NextProtos: sitecert.ClientProtos}}
	return &http.Client{Timeout: 5 * time.Minute, Transport: tr}, nil
}

// cmdStandbyPair pairs this Pi with a master Pi as its standby, using the
// one-time code from the master's Pis page (used by setup).
func cmdStandbyPair(ctx context.Context, args []string, c *cli) error {
	var master, caFile, code *string
	_, data, err := parse("standby-pair", args, func(fs *flag.FlagSet) {
		master = fs.String("master", "", "the master Pi's address, e.g. https://steve.local")
		caFile = fs.String("ca", "", "the master's certificate, as checked by its fingerprint (PEM file)")
		code = fs.String("code", "", "the pairing code from the master's Pis page")
	})
	if err != nil {
		return err
	}
	if *master == "" || *caFile == "" || *code == "" {
		return errors.New("standby-pair needs -master, -ca and -code")
	}
	ca, err := os.ReadFile(*caFile)
	if err != nil {
		return err
	}
	client, err := standbyClient(string(ca))
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(*master, "/")
	token, err := standby.Pair(ctx, client, base, *code)
	if err != nil {
		return fmt.Errorf("pairing with the master didn't work: %w", err)
	}
	b, _ := json.MarshalIndent(standby.Link{Master: base, CA: string(ca), Token: token}, "", "  ")
	if err := os.MkdirAll(*data, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*data, standby.LinkFile), b, 0o600); err != nil {
		return err
	}
	c.printf("Paired with the master Pi at %s.\n", base)
	return nil
}

// cmdStandby keeps this Pi's copy of the master Pi up to date, every few
// seconds, until stopped (the standby's service).
func cmdStandby(ctx context.Context, args []string, c *cli) error {
	var every *time.Duration
	_, data, err := parse("standby", args, func(fs *flag.FlagSet) {
		every = fs.Duration("every", 5*time.Second, "how often to copy what's new")
	})
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(*data, standby.LinkFile))
	if err != nil {
		return fmt.Errorf("this Pi isn't paired as a standby (%w): set it up with setup", err)
	}
	var link standby.Link
	if err := json.Unmarshal(b, &link); err != nil {
		return err
	}
	client, err := standbyClient(link.CA)
	if err != nil {
		return err
	}
	var n *node
	f := &standby.Follower{DataDir: *data, Link: link, HTTP: client, Blobs: (&node{dir: *data}).blobs(), Logf: log.Printf,
		Open: func(ctx context.Context) (*store.Store, error) {
			var err error
			if n, err = open(ctx, *data); err != nil {
				return nil, err
			}
			return n.store, nil
		}}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("pi-fleet %s standby following %s", version, link.Master)
	return f.Run(ctx, *every)
}
