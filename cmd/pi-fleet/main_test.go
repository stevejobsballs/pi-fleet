package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/app"
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
