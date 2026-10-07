package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"filippo.io/age"

	"pi-fleet/internal/release"
	"pi-fleet/internal/store"
)

// cmdSelfcheck is run by an update on the new binary: open the data
// directory (applying migrations), then check integrity and every chain.
func cmdSelfcheck(ctx context.Context, args []string, c *cli) error {
	_, data, err := parse("selfcheck", args, nil)
	if err != nil {
		return err
	}
	n, err := open(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	if err := n.store.IntegrityCheck(ctx); err != nil {
		return err
	}
	rep, err := n.store.Verify(ctx)
	if err != nil {
		return err
	}
	if !rep.OK() {
		return fmt.Errorf("chain verification failed: %v", rep.Problems[0])
	}
	c.printf("ok: pi-fleet %s, %d chains, %d events\n", version, rep.Chains, rep.Events)
	return nil
}

type secretKeyFile struct {
	ID   uint64 `json:"id"`
	Seed []byte `json:"seed"`
}

func cmdReleaseKeygen(ctx context.Context, args []string, c *cli) error {
	fs := flag.NewFlagSet("release-keygen", flag.ContinueOnError)
	out := fs.String("out", "", "file for the passphrase-encrypted secret key (keep it offline)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("release-keygen needs -out")
	}
	pass, err := c.newPassword("", "Passphrase for the release key: ")
	if err != nil {
		return err
	}
	pub, sk, err := release.GenerateKey()
	if err != nil {
		return err
	}
	plain, _ := json.Marshal(secretKeyFile{ID: sk.ID, Seed: sk.Key.Seed()})
	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return err
	}
	w.Write(plain)
	if err := w.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(*out+".pub", []byte(pub.File()), 0o644); err != nil {
		return err
	}
	c.printf("Secret key written to %s. Keep it on offline media only.\nPublic key written to %s.pub (copy it into the repo as release-keys.txt).\n\n%s", *out, *out, pub.File())
	return nil
}

func loadSecretKey(c *cli, path string) (release.SecretKey, error) {
	pass, err := c.readSecret("Passphrase for " + filepath.Base(path) + ": ")
	if err != nil {
		return release.SecretKey{}, err
	}
	id, err := age.NewScryptIdentity(pass)
	if err != nil {
		return release.SecretKey{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return release.SecretKey{}, err
	}
	defer f.Close()
	r, err := age.Decrypt(f, id)
	if err != nil {
		return release.SecretKey{}, fmt.Errorf("wrong passphrase or not a release key: %w", err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return release.SecretKey{}, err
	}
	var s secretKeyFile
	if err := json.Unmarshal(b, &s); err != nil || len(s.Seed) != ed25519.SeedSize {
		return release.SecretKey{}, errors.New("release key file is corrupt")
	}
	return release.SecretKey{ID: s.ID, Key: ed25519.NewKeyFromSeed(s.Seed)}, nil
}

func cmdReleaseSign(ctx context.Context, args []string, c *cli) error {
	fs := flag.NewFlagSet("release-sign", flag.ContinueOnError)
	key := fs.String("key", "", "encrypted release key from release-keygen")
	dir := fs.String("dir", "", "directory holding pi-fleet_<version>_<os>_<arch> binaries")
	ver := fs.String("version", "", "release version, e.g. v1.0.0")
	minFrom := fs.String("min-upgrade-from", "", "oldest version allowed to upgrade straight to this one")
	security := fs.Bool("security", false, "mark as a security release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := release.BuildManifest(*dir, *ver, *minFrom, *security, time.Now())
	if err != nil {
		return err
	}
	sk, err := loadSecretKey(c, *key)
	if err != nil {
		return err
	}
	if err := release.SignManifest(*dir, m, sk, time.Now()); err != nil {
		return err
	}
	c.printf("signed %s with %d artifact(s) in %s\n", m.Version, len(m.Artifacts), *dir)
	return nil
}

func cmdUpdate(ctx context.Context, args []string, c *cli) error {
	var from, root *string
	var downgrade *bool
	_, data, err := parse("update", args, func(fs *flag.FlagSet) {
		from = fs.String("from", "", "release directory or URL (default on an employee Pi: the master Pi's mirror)")
		root = fs.String("root", "/opt/pi-fleet", "install root holding releases/ and the current symlink")
		downgrade = fs.Bool("allow-downgrade", false, "install even if not newer (logged)")
	})
	if err != nil {
		return err
	}
	trusted, err := release.TrustedKeys()
	if err != nil {
		return err
	}
	src := release.Source{Base: *from}
	n, err := open(ctx, *data)
	if err != nil {
		return err
	}
	if src.Base == "" {
		cl, err := n.client(ctx, "")
		if err != nil {
			n.Close()
			return errors.New("give -from (a release directory or URL)")
		}
		src.Base, src.HTTP = cl.BaseURL+"/v1/releases", cl.HTTP
	}
	n.Close() // the new binary must have the database to itself

	dbPath := filepath.Join(*data, "pi-fleet.db")
	res, err := release.Update(ctx, release.Options{
		Source: src, Trusted: trusted, Root: *root, DataDir: *data, CurrentVersion: version, AllowDowngrade: *downgrade,
		Snapshot: func(ctx context.Context) (func() error, error) { return snapshotForUpdate(ctx, dbPath) },
	})
	if err != nil {
		return err
	}
	if *downgrade {
		log.Printf("update: DOWNGRADE from %s to %s allowed by operator", version, res.Manifest.Version)
	}
	c.printf("updated %s -> %s; restart the service to run it\n", version, res.Manifest.Version)
	return nil
}

// snapshotForUpdate copies the database aside before an update and
// returns how to put it back.
func snapshotForUpdate(ctx context.Context, dbPath string) (func() error, error) {
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	saved := fmt.Sprintf("%s.pre-update-%s", dbPath, time.Now().UTC().Format("20060102T150405Z"))
	err = st.SnapshotTo(ctx, saved)
	st.Close()
	for _, f := range []string{saved, dbPath + "-wal", dbPath + "-shm"} {
		matchOwner(f, filepath.Dir(dbPath))
	}
	if err != nil {
		return nil, err
	}
	// Keep the three newest pre-update copies.
	if old, _ := filepath.Glob(dbPath + ".pre-update-*"); len(old) > 3 {
		sort.Strings(old)
		for _, f := range old[:len(old)-3] {
			os.Remove(f)
		}
	}
	return func() error {
		for _, suffix := range []string{"-wal", "-shm"} {
			os.Remove(dbPath + suffix)
		}
		b, err := os.ReadFile(saved)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dbPath, b, 0o600); err != nil {
			return err
		}
		matchOwner(dbPath, filepath.Dir(dbPath))
		return nil
	}, nil
}

// matchOwner gives path the owner of dir, for updates run as root on a
// data directory owned by the service user.
func matchOwner(path, dir string) {
	if info, err := os.Stat(dir); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
			os.Chown(path, int(st.Uid), int(st.Gid))
		}
	}
}
