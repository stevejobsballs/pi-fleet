package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"pi-fleet/internal/app"
	"pi-fleet/internal/backup"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

const (
	configBackupDir        = "backup_dir"
	configBackupRecipients = "backup_recipients"
	configLastNightly      = "backup_last_nightly"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// runner builds the backup runner from central's configuration.
func runner(ctx context.Context, n *node, a *app.App) (*backup.Runner, error) {
	dir, err := n.store.Config(ctx, configBackupDir)
	if err != nil {
		return nil, errors.New("backups are not configured; run pi-fleet backup-config")
	}
	keys, err := n.store.Config(ctx, configBackupRecipients)
	if err != nil {
		return nil, errors.New("no backup recipients configured; run pi-fleet backup-config")
	}
	recipients, err := backup.ParseRecipients(strings.Fields(keys))
	if err != nil {
		return nil, err
	}
	return &backup.Runner{App: a, Dir: dir, Recipients: recipients, Version: version}, nil
}

// signIn asks for a user's password at the console and checks their role.
func signIn(ctx context.Context, a *app.App, c *cli, username, minRole string) (app.Actor, error) {
	if username == "" {
		return app.Actor{}, errors.New("give your username with -as")
	}
	pw, err := c.readSecret("Password for " + username + ": ")
	if err != nil {
		return app.Actor{}, err
	}
	u, err := a.Authenticate(ctx, username, pw)
	if err != nil {
		return app.Actor{}, err
	}
	rank := map[string]int{domain.RoleUser: 1, domain.RoleMidTier: 2, domain.RoleSuperUser: 3}
	if rank[u.Role] < rank[minRole] {
		return app.Actor{}, fmt.Errorf("%s needs the %s role", username, minRole)
	}
	return app.Actor{UserID: u.ID, SessionID: "cli-" + u.ID}, nil
}

func cmdBackupKeygen(ctx context.Context, args []string, c *cli) error {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	c.printf(`Backup decryption identity (SECRET: store it offline, in two places,
one of them sealed in the off-site building, never on the master Pi):

    %s

Backup recipient (public: give it to pi-fleet backup-config -recipient):

    %s
`, id.String(), id.Recipient().String())
	return nil
}

func cmdBackupConfig(ctx context.Context, args []string, c *cli) error {
	var dir *string
	var recipients stringList
	_, data, err := parse("backup-config", args, func(fs *flag.FlagSet) {
		dir = fs.String("dir", "", "backup disk mount point, e.g. /srv/pi-fleet-backup")
		fs.Var(&recipients, "recipient", "age recipient (repeat for each super user and the escrow key)")
	})
	if err != nil {
		return err
	}
	if *dir == "" || len(recipients) == 0 {
		return errors.New("backup-config needs -dir and at least one -recipient")
	}
	if _, err := backup.ParseRecipients(recipients); err != nil {
		return err
	}
	if st, err := os.Stat(*dir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory (is the backup disk mounted?)", *dir)
	}
	n, _, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	abs, _ := filepath.Abs(*dir)
	if err := n.store.SetConfig(ctx, configBackupDir, abs); err != nil {
		return err
	}
	if err := n.store.SetConfig(ctx, configBackupRecipients, strings.Join(recipients, "\n")); err != nil {
		return err
	}
	c.printf("backups go to %s, encrypted to %d recipient(s)\n", abs, len(recipients))
	return nil
}

func cmdBackupNow(ctx context.Context, args []string, c *cli) error {
	_, data, err := parse("backup-now", args, nil)
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	r, err := runner(ctx, n, a)
	if err != nil {
		return err
	}
	m, removed, err := r.Nightly(ctx)
	if err != nil {
		return err
	}
	exported, err := r.Export(ctx)
	if err != nil {
		return err
	}
	c.printf("verified snapshot %s (%d events, %d chains); exported %d events; pruned %d files\n",
		m.File, m.LocalOrder, len(m.Heads), exported, len(removed))
	return nil
}

func cmdOffsiteRegister(ctx context.Context, args []string, c *cli) error {
	var disk, label *string
	_, data, err := parse("offsite-register", args, func(fs *flag.FlagSet) {
		disk = fs.String("disk", "", "mount point of the USB disk")
		label = fs.String("label", "", "label written on the disk, e.g. OFFSITE-A")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	r, err := runner(ctx, n, a)
	if err != nil {
		return err
	}
	d, err := r.RegisterOffsite(ctx, *disk, *label)
	if err != nil {
		return err
	}
	c.printf("registered %s (%s)\n", d.Label, d.ID)
	return nil
}

func cmdOffsiteWrite(ctx context.Context, args []string, c *cli) error {
	var disk *string
	_, data, err := parse("offsite-write", args, func(fs *flag.FlagSet) {
		disk = fs.String("disk", "", "mount point of the plugged-in off-site disk")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	r, err := runner(ctx, n, a)
	if err != nil {
		return err
	}
	_, m, err := r.WriteOffsite(ctx, *disk)
	if err != nil {
		return err
	}
	d, _ := backup.OpenDisk(*disk)
	c.printf("wrote and verified %s on %s. Safe to unplug.\nTake it to the other building, then run: pi-fleet offsite-confirm -label %s -as <you>\n", m.File, d.Label, d.Label)
	return nil
}

func cmdOffsiteConfirm(ctx context.Context, args []string, c *cli) error {
	var label, as *string
	_, data, err := parse("offsite-confirm", args, func(fs *flag.FlagSet) {
		label = fs.String("label", "", "label of the disk now in the other building")
		as = fs.String("as", "", "your username (mid-tier or super user)")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	r, err := runner(ctx, n, a)
	if err != nil {
		return err
	}
	actor, err := signIn(ctx, a, c, *as, domain.RoleMidTier)
	if err != nil {
		return err
	}
	if err := r.ConfirmOffsite(ctx, actor, *label); err != nil {
		return err
	}
	c.printf("confirmed %s is off-site. Employee Pis may now purge records older than 31 days that it holds.\n", *label)
	return nil
}

func cmdBackups(ctx context.Context, args []string, c *cli) error {
	_, data, err := parse("backups", args, nil)
	if err != nil {
		return err
	}
	n, _, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	list, err := domain.ListBackups(ctx, n.store.DB())
	if err != nil {
		return err
	}
	for i, b := range list {
		if i == 20 {
			c.printf("...\n")
			break
		}
		where := "backup disk"
		if b.Kind == "offsite" {
			where = b.DiskLabel
			if b.ConfirmedAt.IsZero() {
				where += " (not yet confirmed off-site)"
			} else {
				where += " (off-site since " + b.ConfirmedAt.Format(time.DateTime) + ")"
			}
		}
		c.printf("%s  %-40s  %s\n", b.WrittenAt.Format(time.DateTime), b.File, where)
	}
	last, err := domain.LastOffsiteConfirmed(ctx, n.store.DB())
	if err != nil {
		return err
	}
	if msg := offsiteWarning(last, time.Now()); msg != "" {
		c.printf("\nWARNING: %s\n", msg)
	}
	return nil
}

func offsiteWarning(last, now time.Time) string {
	switch {
	case last.IsZero():
		return "no off-site backup has been confirmed yet"
	case now.Sub(last) > domain.OffsiteOverdue:
		return fmt.Sprintf("the last off-site backup was confirmed %d days ago; rotate the USB disks", int(now.Sub(last).Hours()/24))
	}
	return ""
}

func cmdRestore(ctx context.Context, args []string, c *cli) error {
	var manifest, identityFile, segments *string
	_, data, err := parse("restore", args, func(fs *flag.FlagSet) {
		manifest = fs.String("manifest", "", "the snapshot's .json manifest (on the backup or off-site disk)")
		identityFile = fs.String("identity", "", "file holding the backup decryption identity")
		segments = fs.String("events", "", "event segment directory to replay after the snapshot (optional)")
	})
	if err != nil {
		return err
	}
	if *manifest == "" || *identityFile == "" {
		return errors.New("restore needs -manifest and -identity")
	}
	f, err := os.Open(*identityFile)
	if err != nil {
		return err
	}
	ids, err := age.ParseIdentities(f)
	f.Close()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	db := filepath.Join(*data, "pi-fleet.db")
	rep, err := backup.Restore(ctx, *manifest, ids, *segments, db, func(id string) store.Applier { return &domain.Projector{LocalNodeID: id} })
	if err != nil {
		return err
	}
	c.printf("restored %s: %d events in the snapshot, %d replayed from segments; %d chains verified\n",
		rep.Snapshot.File, rep.Snapshot.LocalOrder, rep.EventsReplay, rep.Verify.Chains)
	if _, err := os.Stat(filepath.Join(*data, "keys", "event.key")); err != nil {
		c.printf("Now restore the master Pi's keys from escrow into %s, then start pi-fleet serve.\nEmployee Pis will re-send anything newer when they next sync.\n", filepath.Join(*data, "keys"))
		return nil
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	return a.RecordRestore(ctx, domain.BackupRestored{Source: rep.Snapshot.File, LocalOrder: rep.Snapshot.LocalOrder, Events: rep.EventsReplay, Verified: rep.Verify.OK()})
}

// runBackups exports events every 5 minutes and takes the nightly
// verified snapshot after 02:00 local time.
func runBackups(ctx context.Context, n *node, a *app.App) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		if r, err := runner(ctx, n, a); err == nil {
			if _, err := r.Export(ctx); err != nil {
				log.Printf("backup: export: %v", err)
			}
			today := time.Now().Format(time.DateOnly)
			last, _ := n.store.Config(ctx, configLastNightly)
			if time.Now().Hour() >= 2 && last != today {
				if m, _, err := r.Nightly(ctx); err != nil {
					log.Printf("backup: NIGHTLY SNAPSHOT FAILED: %v", err)
				} else {
					log.Printf("backup: verified snapshot %s", m.File)
					n.store.SetConfig(ctx, configLastNightly, today)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
