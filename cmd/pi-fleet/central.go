package main

import (
	"context"
	"crypto/tls"
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

	"github.com/google/uuid"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/fleetca"
	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/setup"
	"pi-fleet/internal/web"
)

func openCentral(ctx context.Context, dir string) (*node, *app.App, error) {
	n, err := open(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	if err := n.require("central"); err != nil {
		n.Close()
		return nil, nil, err
	}
	a, err := n.app(ctx)
	if err != nil {
		n.Close()
		return nil, nil, err
	}
	return n, a, nil
}

func cmdBootstrap(ctx context.Context, args []string, c *cli) error {
	var username, legalName, email *string
	var check *bool
	_, data, err := parse("bootstrap", args, func(fs *flag.FlagSet) {
		username = fs.String("username", "", "username for the first super user")
		legalName = fs.String("name", "", "legal name, shown on e-signatures")
		email = fs.String("email", "", "work email")
		check = fs.Bool("check", false, "only report whether a super user exists (used by setup)")
	})
	if err != nil {
		return err
	}
	if *check {
		n, _, err := openCentral(ctx, *data)
		if err != nil {
			return err
		}
		defer n.Close()
		var users int
		if err := n.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
			return err
		}
		if users > 0 {
			c.printf("this master Pi has a super user\n")
		} else {
			c.printf("no users yet\n")
		}
		return nil
	}
	if *username == "" || *legalName == "" || *email == "" {
		return errors.New("bootstrap needs -username, -name and -email")
	}
	if err := domain.CheckUsername(*username); err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	c.printf("Choose a password for %s (at least 12 characters, not one known from data breaches).\n", *username)
	pw, err := c.newPassword(*username, "Password: ")
	if err != nil {
		return err
	}
	id, err := a.BootstrapSuperUser(ctx, app.NewUser{
		Username: *username, LegalName: *legalName, Email: *email, IdentityVerification: "console bootstrap",
	}, pw)
	if err != nil {
		return err
	}
	c.printf("created super user %s (%s)\n", *username, id)
	return nil
}

// superUser asks for a super user's credentials at the console.
func superUser(ctx context.Context, a *app.App, c *cli, username string) (app.Actor, error) {
	if username == "" {
		return app.Actor{}, errors.New("give your super-user name with -as")
	}
	pw, err := c.readSecret("Password for " + username + ": ")
	if err != nil {
		return app.Actor{}, err
	}
	u, err := a.Authenticate(ctx, username, pw)
	if err != nil {
		return app.Actor{}, err
	}
	if u.Role != domain.RoleSuperUser {
		return app.Actor{}, fmt.Errorf("%s is not a super user", username)
	}
	return app.Actor{UserID: u.ID, SessionID: "cli-" + uuid.NewString()}, nil
}

func cmdNodes(ctx context.Context, args []string, c *cli) error {
	var status *string
	_, data, err := parse("nodes", args, func(fs *flag.FlagSet) {
		status = fs.String("status", "", "only nodes with this status (e.g. pending_confirmation)")
	})
	if err != nil {
		return err
	}
	n, _, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	nodes, err := domain.ListNodes(ctx, n.store.DB(), *status)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		c.printf("no Pis\n")
	}
	for _, nd := range nodes {
		owner := ""
		if nd.Mode == "kiosk" {
			n.store.DB().QueryRowContext(ctx, `SELECT 'kiosk:' || name FROM kiosks WHERE id = ?`, nd.KioskID).Scan(&owner)
		} else {
			u, err := domain.GetUser(ctx, n.store.DB(), nd.BoundUserID)
			if err != nil {
				return err
			}
			owner = u.Username
		}
		c.printf("%s  %-20s  %-16s  %s  words: %s\n", nd.ID, nd.Status, owner, nd.ActivatedAt, nd.PairingWords)
	}
	return nil
}

func cmdNodeConfirm(ctx context.Context, args []string, c *cli) error {
	var nodeID, as *string
	_, data, err := parse("node-confirm", args, func(fs *flag.FlagSet) {
		nodeID = fs.String("node", "", "id of the pending Pi")
		as = fs.String("as", "", "your super-user name")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	actor, err := superUser(ctx, a, c, *as)
	if err != nil {
		return err
	}
	c.printf("Ask the employee to read the six words shown on their Pi.\n")
	words, err := c.readLine("Words: ")
	if err != nil {
		return err
	}
	if err := a.ConfirmNode(ctx, actor, *nodeID, words); err != nil {
		return err
	}
	c.printf("confirmed Pi %s\n", *nodeID)
	return nil
}

func cmdNodeReject(ctx context.Context, args []string, c *cli) error {
	var nodeID, as, reason *string
	_, data, err := parse("node-reject", args, func(fs *flag.FlagSet) {
		nodeID = fs.String("node", "", "id of the pending Pi")
		as = fs.String("as", "", "your super-user name")
		reason = fs.String("reason", "", "why")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	actor, err := superUser(ctx, a, c, *as)
	if err != nil {
		return err
	}
	if err := a.RejectNode(ctx, actor, *nodeID, *reason); err != nil {
		return err
	}
	c.printf("rejected Pi %s\n", *nodeID)
	return nil
}

func cmdNodeRevoke(ctx context.Context, args []string, c *cli) error {
	var nodeID, as, reason *string
	var keep *bool
	_, data, err := parse("node-revoke", args, func(fs *flag.FlagSet) {
		nodeID = fs.String("node", "", "id of the Pi")
		as = fs.String("as", "", "your super-user name")
		reason = fs.String("reason", "", "why")
		keep = fs.Bool("keep-unsynced", false, "accept work the Pi recorded before now when it next connects")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	actor, err := superUser(ctx, a, c, *as)
	if err != nil {
		return err
	}
	if err := a.RevokeNode(ctx, actor, *nodeID, *reason, *keep); err != nil {
		return err
	}
	c.printf("revoked Pi %s; it will wipe itself when it next connects\n", *nodeID)
	return nil
}

func cmdServe(ctx context.Context, args []string, c *cli) error {
	var listen, certFile, keyFile, releases *string
	var plain *bool
	_, data, err := parse("serve", args, func(fs *flag.FlagSet) {
		listen = fs.String("listen", ":8443", "address to listen on")
		certFile = fs.String("tls-cert", "", "TLS certificate (PEM)")
		keyFile = fs.String("tls-key", "", "TLS private key (PEM)")
		plain = fs.Bool("insecure-http", false, "serve plain HTTP, e.g. behind a TLS-terminating proxy")
		releases = fs.String("releases", "", "directory of approved signed releases to mirror for employee Pis")
	})
	if err != nil {
		return err
	}
	if !*plain && (*certFile == "" || *keyFile == "") {
		return errors.New("serve needs -tls-cert and -tls-key, or -insecure-http behind a TLS proxy")
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go runScheduler(ctx, a)
	go runBackups(ctx, n, a)

	var fingerprint string
	if pemBytes, err := os.ReadFile(*certFile); err == nil {
		fingerprint, _ = setup.Fingerprint(pemBytes)
	}
	_, trialErr := os.Stat(filepath.Join(*data, setup.TrialMarker))
	ui, err := (&web.Server{App: a, Role: "central", Secure: true, PHIPatterns: web.DefaultPHIPatterns, CertFingerprint: fingerprint, Trial: trialErr == nil, Version: version,
		Notices: func(ctx context.Context) []string {
			if _, err := n.store.Config(ctx, configBackupDir); err != nil {
				return []string{"Backups are not configured. Run setup on the master Pi and choose Set up backups."}
			}
			if _, err := runner(ctx, n, a); errors.Is(err, errNoBackupDrive) {
				return []string{"The backup drive isn't connected, so no backups are being made. Plug it back into the master Pi."}
			}
			last, err := domain.LastOffsiteConfirmed(ctx, n.store.DB())
			if msg := offsiteWarning(last, time.Now()); err == nil && msg != "" {
				return []string{"Backups: " + msg + "."}
			}
			return nil
		}}).Handler()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	ca, err := fleetca.LoadOrCreate(filepath.Join(n.dir, "keys"), n.local.NodeID[:8])
	if err != nil {
		return err
	}
	mux.Handle("/v1/", (&fleetsync.Server{App: a, CentralKey: n.keys.Event, Fleet: (&web.FleetAPI{App: a}).Handler(), Blobs: n.blobs(), CA: ca}).Handler())
	if *releases != "" {
		// Releases are verified by each Pi against its compiled-in keys,
		// so the mirror needs no authentication.
		mux.Handle("GET /v1/releases/", http.StripPrefix("/v1/releases/", http.FileServer(http.Dir(*releases))))
	}
	mux.Handle("/", ui)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("pi-fleet %s central %s listening on %s", version, n.local.NodeID, *listen)
	if *plain {
		err = srv.ListenAndServe()
	} else {
		err = srv.ListenAndServeTLS(*certFile, *keyFile)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// runScheduler opens due PM work hourly (DESIGN.md §4.3 pm_schedule).
func runScheduler(ctx context.Context, a *app.App) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		numbers, err := a.GenerateDueWorkOrders(ctx, app.WorkingSetWindow)
		if err != nil {
			log.Printf("scheduler: %v", err)
		} else if len(numbers) > 0 {
			log.Printf("scheduler: opened %v", numbers)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func cmdUserCreate(ctx context.Context, args []string, c *cli) error {
	var as, username, legalName, email, role, verified *string
	_, data, err := parse("user-create", args, func(fs *flag.FlagSet) {
		as = fs.String("as", "", "your super-user name")
		username = fs.String("username", "", "new user's username")
		legalName = fs.String("name", "", "legal name, shown on e-signatures")
		email = fs.String("email", "", "work email")
		role = fs.String("role", domain.RoleUser, "user, mid_tier or super_user")
		verified = fs.String("verified", "", "how you verified their identity (Part 11 §11.100(b)), e.g. \"in person, badge 1234\"")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	actor, err := superUser(ctx, a, c, *as)
	if err != nil {
		return err
	}
	_, oneTime, err := a.CreateUser(ctx, actor, app.NewUser{
		Username: *username, LegalName: *legalName, Email: *email, Role: *role, IdentityVerification: *verified,
	})
	if err != nil {
		return err
	}
	c.printf("created %s. Give them this one-time password in person (valid 72 hours):\n\n    %s\n\n", *username, oneTime)
	return nil
}

func cmdKioskCreate(ctx context.Context, args []string, c *cli) error {
	var as, name, site *string
	_, data, err := parse("kiosk-create", args, func(fs *flag.FlagSet) {
		as = fs.String("as", "", "your super-user name")
		name = fs.String("name", "", "kiosk name, e.g. nyc-biomed-shop")
		site = fs.String("site", "", "site code, e.g. NYC")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	actor, err := superUser(ctx, a, c, *as)
	if err != nil {
		return err
	}
	s, err := domain.GetSiteByCode(ctx, n.store.DB(), *site)
	if err != nil {
		return fmt.Errorf("site %q: %w", *site, err)
	}
	_, pw, err := a.CreateKiosk(ctx, actor, s.ID, *name)
	if err != nil {
		return err
	}
	c.printf("created kiosk %s. One-time activation password (valid 72 hours):\n\n    %s\n\nOn the shared Pi: pi-fleet activate -kiosk %s -central <URL>\n", *name, pw, *name)
	return nil
}

func cmdKioskMember(ctx context.Context, args []string, c *cli) error {
	var as, name, username *string
	var remove *bool
	_, data, err := parse("kiosk-member", args, func(fs *flag.FlagSet) {
		as = fs.String("as", "", "your super-user name")
		name = fs.String("kiosk", "", "kiosk name")
		username = fs.String("user", "", "username to add or remove")
		remove = fs.Bool("remove", false, "remove instead of add")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	actor, err := superUser(ctx, a, c, *as)
	if err != nil {
		return err
	}
	k, err := domain.GetKioskByName(ctx, n.store.DB(), *name)
	if err != nil {
		return fmt.Errorf("kiosk %q: %w", *name, err)
	}
	u, err := domain.GetUserByUsername(ctx, n.store.DB(), *username)
	if err != nil {
		return fmt.Errorf("user %q: %w", *username, err)
	}
	if *remove {
		err = a.RemoveKioskMember(ctx, actor, k.ID, u.ID)
	} else {
		err = a.AddKioskMember(ctx, actor, k.ID, u.ID)
	}
	if err != nil {
		return err
	}
	c.printf("done; the kiosk picks this up at its next sync\n")
	return nil
}
