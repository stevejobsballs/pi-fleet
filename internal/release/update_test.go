package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const goodBinary = "#!/bin/sh\n[ \"$1\" = selfcheck ] && exit 0\nexit 2\n"

// badBinary damages the database as a broken migration would, then fails.
const badBinary = "#!/bin/sh\necho damaged > \"$3/pi-fleet.db\"\necho 'migration 0042 failed' >&2\nexit 1\n"

type fixture struct {
	t       *testing.T
	sk      SecretKey
	pub     PublicKey
	root    string
	data    string
	release string // a release directory to publish into
}

func newFixture(t *testing.T) *fixture {
	pub, sk, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, sk: sk, pub: pub, root: t.TempDir(), data: t.TempDir()}
	os.WriteFile(filepath.Join(f.data, "pi-fleet.db"), []byte("original"), 0o600)
	// v1.0.0 is installed.
	dir := filepath.Join(f.root, "releases", "v1.0.0")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "pi-fleet"), []byte(goodBinary), 0o755)
	os.Symlink(dir, filepath.Join(f.root, "current"))
	return f
}

// publish builds a signed release directory containing one binary.
func (f *fixture) publish(version, script, minFrom string) string {
	f.t.Helper()
	dir := f.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pi-fleet_"+version+"_linux_arm64"), []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	m, err := BuildManifest(dir, version, minFrom, false, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	if err := SignManifest(dir, m, f.sk, time.Now()); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *fixture) update(src, current string, allowDowngrade bool) (Result, error) {
	return Update(context.Background(), Options{
		Source: Source{Base: src}, Trusted: []PublicKey{f.pub}, Root: f.root, DataDir: f.data,
		CurrentVersion: current, AllowDowngrade: allowDowngrade, OS: "linux", Arch: "arm64",
		Snapshot: func(context.Context) (func() error, error) {
			saved, err := os.ReadFile(filepath.Join(f.data, "pi-fleet.db"))
			return func() error { return os.WriteFile(filepath.Join(f.data, "pi-fleet.db"), saved, 0o600) }, err
		},
	})
}

func (f *fixture) current() string {
	target, _ := os.Readlink(filepath.Join(f.root, "current"))
	return filepath.Base(target)
}

func TestUpdateAndRollback(t *testing.T) {
	f := newFixture(t)

	res, err := f.update(f.publish("v1.1.0", goodBinary, ""), "v1.0.0", false)
	if err != nil {
		t.Fatalf("good update: %v", err)
	}
	if f.current() != "v1.1.0" || filepath.Base(res.Previous) != "v1.0.0" {
		t.Fatalf("current = %s, previous = %s", f.current(), res.Previous)
	}

	// A release whose migration fails is rolled back, database included.
	_, err = f.update(f.publish("v1.2.0", badBinary, ""), "v1.1.0", false)
	if !errors.Is(err, ErrHealthFailed) {
		t.Fatalf("bad update: %v", err)
	}
	if f.current() != "v1.1.0" {
		t.Fatalf("not rolled back: current = %s", f.current())
	}
	if db, _ := os.ReadFile(filepath.Join(f.data, "pi-fleet.db")); string(db) != "original" {
		t.Fatalf("database not restored: %q", db)
	}
}

func TestUpdateRefusals(t *testing.T) {
	f := newFixture(t)
	v11 := f.publish("v1.1.0", goodBinary, "")

	if _, err := f.update(v11, "v1.1.0", false); !errors.Is(err, ErrNotNewer) {
		t.Errorf("same version: %v", err)
	}
	if _, err := f.update(v11, "v1.2.0", false); !errors.Is(err, ErrNotNewer) {
		t.Errorf("downgrade: %v", err)
	}
	if _, err := f.update(f.publish("v2.0.0", goodBinary, "v1.5.0"), "v1.1.0", false); !errors.Is(err, ErrTooOld) {
		t.Errorf("too old to upgrade directly: %v", err)
	}

	// The artifact swapped after signing.
	tampered := f.publish("v1.3.0", goodBinary, "")
	os.WriteFile(filepath.Join(tampered, "pi-fleet_v1.3.0_linux_arm64"), []byte(badBinary), 0o755)
	if _, err := f.update(tampered, "v1.0.0", false); err == nil {
		t.Error("tampered artifact installed")
	}

	// Signed by someone else's key.
	other := newFixture(t)
	if _, err := f.update(other.publish("v1.4.0", goodBinary, ""), "v1.0.0", false); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("untrusted signer: %v", err)
	}

	// A build with no compiled-in keys verifies nothing.
	_, err := Update(context.Background(), Options{Source: Source{Base: v11}, Root: f.root, DataDir: f.data, CurrentVersion: "v1.0.0"})
	if !errors.Is(err, ErrNoKeys) {
		t.Errorf("no keys: %v", err)
	}
	if f.current() != "v1.0.0" {
		t.Errorf("a refused update changed current to %s", f.current())
	}

	// An explicit downgrade is possible.
	if _, err := f.update(v11, "v1.2.0", true); err != nil || f.current() != "v1.1.0" {
		t.Errorf("allowed downgrade: %v, current %s", err, f.current())
	}
}

func TestUpdateFromCentralMirror(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(f.publish("v1.1.0", goodBinary, ""))))
	defer srv.Close()
	if _, err := f.update(srv.URL, "v1.0.0", false); err != nil || f.current() != "v1.1.0" {
		t.Fatalf("HTTP update: %v, current %s", err, f.current())
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0}, {"v1.10.0", "v1.9.9", 1}, {"v0.9.0", "v1.0.0", -1},
		{"dev", "v0.0.1", -1}, {"3412ac2-dirty", "v1.0.0", -1},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%s, %s) = %d", tc.a, tc.b, got)
		}
	}
}
