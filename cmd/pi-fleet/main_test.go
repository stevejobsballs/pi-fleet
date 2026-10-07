package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/app"
	"pi-fleet/internal/backup"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/password"
)

func init() { passwordParams = password.Params{Time: 1, MemoryKiB: 64, Threads: 1} }

func runCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), args, strings.NewReader(stdin), &out)
	return out.String(), err
}

func runOK(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, err := runCmd(t, stdin, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func TestCentralSetup(t *testing.T) {
	dir := t.TempDir()
	if _, err := runCmd(t, "", "init", "-data", dir); err == nil {
		t.Error("init without a role accepted")
	}
	if out := runOK(t, "", "init", "-data", dir, "-role", "central"); !strings.Contains(out, "initialised central") {
		t.Errorf("init output: %s", out)
	}
	if out := runOK(t, "", "init", "-data", dir, "-role", "central"); !strings.Contains(out, "already initialised as central") {
		t.Errorf("second init output: %s", out)
	}
	boot := []string{"bootstrap", "-data", dir, "-username", "admin", "-name", "Ada Admin", "-email", "ada@example.org"}
	if _, err := runCmd(t, "tumbleweed-gasket-42\nsomething-else-entirely\n", boot...); err == nil {
		t.Error("mismatched confirmation accepted")
	}
	if _, err := runCmd(t, "short\nshort\n", boot...); err == nil {
		t.Error("weak password accepted")
	}
	if out := runOK(t, "tumbleweed-gasket-42\ntumbleweed-gasket-42\n", boot...); !strings.Contains(out, "created super user admin") {
		t.Errorf("bootstrap output: %s", out)
	}
	if _, err := runCmd(t, "tumbleweed-gasket-42\ntumbleweed-gasket-42\n", boot...); err == nil {
		t.Error("second bootstrap accepted")
	}
	if out := runOK(t, "", "verify", "-data", dir); !strings.Contains(out, "chains 1, events 2, redacted 0, problems 0") {
		t.Errorf("verify output: %s", out)
	}
	runOK(t, "", "rebuild", "-data", dir)
}

// TestEmployeePiEndToEnd drives activation, confirmation and sync through
// the CLI, with central's sync API on a test server.
func TestEmployeePiEndToEnd(t *testing.T) {
	ctx := context.Background()
	centralDir, piDir := t.TempDir(), t.TempDir()
	runOK(t, "", "init", "-data", centralDir, "-role", "central")
	runOK(t, "tumbleweed-gasket-42\ntumbleweed-gasket-42\n", "bootstrap", "-data", centralDir, "-username", "admin", "-name", "Ada", "-email", "ada@example.org")

	// Central: a super user creates Tess's account (the web UI will do
	// this later), and the sync API runs.
	n, a, err := openCentral(ctx, centralDir)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	super, err := a.Authenticate(ctx, "admin", "tumbleweed-gasket-42")
	if err != nil {
		t.Fatal(err)
	}
	_, oneTime, err := a.CreateUser(ctx, app.Actor{UserID: super.ID, SessionID: "t"}, app.NewUser{
		Username: "tess", LegalName: "Tess Tech", Email: "tess@example.org", Role: domain.RoleUser, IdentityVerification: "badge",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&fleetsync.Server{App: a, CentralKey: n.keys.Event, Logf: t.Logf}).Handler())
	defer srv.Close()

	// Tess's Pi.
	runOK(t, "", "init", "-data", piDir, "-role", "node")
	if _, err := runCmd(t, "", "bootstrap", "-data", piDir, "-username", "x", "-name", "x", "-email", "x@example.org"); err == nil {
		t.Error("bootstrap allowed on an employee Pi")
	}
	if _, err := runCmd(t, "", "sync", "-data", piDir); err == nil {
		t.Error("sync before activation succeeded")
	}
	out := runOK(t, oneTime+"\ncopper-ladder-sunrise\ncopper-ladder-sunrise\n",
		"activate", "-data", piDir, "-central", srv.URL, "-username", "tess")
	words := regexp.MustCompile(`\n    ([a-z]+(?: [a-z]+){5})\n`).FindStringSubmatch(out)
	if words == nil {
		t.Fatalf("no pairing words in:\n%s", out)
	}
	if out := runOK(t, "", "activation-status", "-data", piDir); strings.TrimSpace(out) != domain.NodeStatusPending {
		t.Errorf("status: %s", out)
	}

	// The super user confirms at the master Pi's console.
	if out := runOK(t, "", "nodes", "-data", centralDir, "-status", domain.NodeStatusPending); !strings.Contains(out, words[1]) {
		t.Errorf("nodes output lacks the words: %s", out)
	}
	nodes, err := domain.ListNodes(ctx, n.store.DB(), domain.NodeStatusPending)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("pending nodes: %v %v", nodes, err)
	}
	runOK(t, "tumbleweed-gasket-42\n"+words[1]+"\n", "node-confirm", "-data", centralDir, "-node", nodes[0].ID, "-as", "admin")
	if out := runOK(t, "", "activation-status", "-data", piDir); strings.TrimSpace(out) != domain.NodeStatusActive {
		t.Errorf("status after confirm: %s", out)
	}

	if out := runOK(t, "", "sync", "-data", piDir); !strings.Contains(out, "pushed 1 (flagged 0), snapshot true") {
		t.Errorf("sync output: %s", out)
	}
	runOK(t, "", "rebuild", "-data", piDir)
	if out := runOK(t, "", "verify", "-data", piDir); !strings.Contains(out, "problems 0") {
		t.Errorf("Pi verify: %s", out)
	}
	if out := runOK(t, "", "verify", "-data", centralDir); !strings.Contains(out, "chains 2") {
		t.Errorf("central verify: %s", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	if _, err := runCmd(t, "", "frobnicate"); err == nil {
		t.Error("unknown command accepted")
	}
}

func TestBackupCommands(t *testing.T) {
	dir, backupDisk, offsite, restored := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	runOK(t, "", "init", "-data", dir, "-role", "central")
	runOK(t, "tumbleweed-gasket-42\ntumbleweed-gasket-42\n", "bootstrap", "-data", dir, "-username", "admin", "-name", "Ada", "-email", "ada@example.org")

	if _, err := runCmd(t, "", "backup-now", "-data", dir); err == nil {
		t.Error("backup without configuration succeeded")
	}
	out := runOK(t, "", "backup-keygen")
	secret := regexp.MustCompile(`AGE-SECRET-KEY-[0-9A-Z]+`).FindString(out)
	recipient := regexp.MustCompile(`age1[0-9a-z]+`).FindString(out)
	if secret == "" || recipient == "" {
		t.Fatalf("keygen output:\n%s", out)
	}
	runOK(t, "", "backup-config", "-data", dir, "-dir", backupDisk, "-recipient", recipient)
	if out := runOK(t, "", "backup-now", "-data", dir); !strings.Contains(out, "verified snapshot") {
		t.Errorf("backup-now: %s", out)
	}
	// Backup drive unplugged: its mark is gone from the empty mount point,
	// and nothing is written there.
	os.Rename(filepath.Join(backupDisk, backup.DriveMarker), filepath.Join(backupDisk, "away"))
	if _, err := runCmd(t, "", "backup-now", "-data", dir); err == nil || !strings.Contains(err.Error(), "backup drive isn't connected") {
		t.Errorf("backup without the drive: %v", err)
	}
	os.Rename(filepath.Join(backupDisk, "away"), filepath.Join(backupDisk, backup.DriveMarker))
	if out := runOK(t, "", "backups", "-data", dir); !strings.Contains(out, "no off-site backup has been confirmed yet") {
		t.Errorf("backups should warn: %s", out)
	}

	runOK(t, "", "offsite-register", "-data", dir, "-disk", offsite, "-label", "OFFSITE-A")
	if out := runOK(t, "", "offsite-write", "-data", dir, "-disk", offsite); !strings.Contains(out, "Safe to unplug") {
		t.Errorf("offsite-write: %s", out)
	}
	runOK(t, "tumbleweed-gasket-42\n", "offsite-confirm", "-data", dir, "-label", "OFFSITE-A", "-as", "admin")
	if out := runOK(t, "", "backups", "-data", dir); strings.Contains(out, "WARNING") || !strings.Contains(out, "off-site since") {
		t.Errorf("backups after rotation: %s", out)
	}

	// Restore the off-site copy elsewhere with the offline identity.
	identity := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(identity, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifests, _ := filepath.Glob(filepath.Join(offsite, "pifleet-*.json"))
	if len(manifests) != 1 {
		t.Fatalf("off-site disk holds %v", manifests)
	}
	out = runOK(t, "", "restore", "-data", restored, "-manifest", manifests[0], "-identity", identity, "-events", filepath.Join(backupDisk, "events"))
	if !strings.Contains(out, "chains verified") || !strings.Contains(out, "restore the master Pi's keys") {
		t.Errorf("restore: %s", out)
	}
}

func TestKioskCommands(t *testing.T) {
	ctx := context.Background()
	centralDir, kioskDir := t.TempDir(), t.TempDir()
	runOK(t, "", "init", "-data", centralDir, "-role", "central")
	runOK(t, "tumbleweed-gasket-42\ntumbleweed-gasket-42\n", "bootstrap", "-data", centralDir, "-username", "admin", "-name", "Ada", "-email", "ada@example.org")
	n, a, err := openCentral(ctx, centralDir)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	super, _ := a.Authenticate(ctx, "admin", "tumbleweed-gasket-42")
	if _, err := a.CreateSite(ctx, app.Actor{UserID: super.ID, SessionID: "t"}, "NYC", "New York", "America/New_York"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateUser(ctx, app.Actor{UserID: super.ID, SessionID: "t"}, app.NewUser{Username: "tess", LegalName: "Tess", Email: "tess@example.org", Role: domain.RoleUser, IdentityVerification: "badge"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&fleetsync.Server{App: a, CentralKey: n.keys.Event, Logf: t.Logf}).Handler())
	defer srv.Close()

	out := runOK(t, "tumbleweed-gasket-42\n", "kiosk-create", "-data", centralDir, "-as", "admin", "-name", "nyc-shop", "-site", "NYC")
	otp := regexp.MustCompile(`[0-9A-Z]{4}(-[0-9A-Z]{4}){3}`).FindString(out)
	runOK(t, "tumbleweed-gasket-42\n", "kiosk-member", "-data", centralDir, "-as", "admin", "-kiosk", "nyc-shop", "-user", "tess")
	runOK(t, "", "init", "-data", kioskDir, "-role", "node")
	out = runOK(t, otp+"\n", "activate", "-data", kioskDir, "-central", srv.URL, "-kiosk", "nyc-shop")
	words := regexp.MustCompile(`\n    ([a-z]+(?: [a-z]+){5})\n`).FindStringSubmatch(out)
	if words == nil {
		t.Fatalf("no pairing words:\n%s", out)
	}
	if out := runOK(t, "", "nodes", "-data", centralDir); !strings.Contains(out, "kiosk:nyc-shop") {
		t.Fatalf("nodes listing: %s", out)
	}
	nodes, _ := domain.ListNodes(ctx, n.store.DB(), domain.NodeStatusPending)
	runOK(t, "tumbleweed-gasket-42\n"+words[1]+"\n", "node-confirm", "-data", centralDir, "-node", nodes[0].ID, "-as", "admin")
	if out := runOK(t, "", "sync", "-data", kioskDir); !strings.Contains(out, "snapshot true") {
		t.Fatalf("kiosk sync: %s", out)
	}
}
