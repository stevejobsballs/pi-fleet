package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"pi-fleet/internal/password"
)

func init() { passwordParams = password.Params{Time: 1, MemoryKiB: 64, Threads: 1} }

func runOK(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	stdinLines = nil
	var out bytes.Buffer
	if err := run(context.Background(), args, strings.NewReader(stdin), &out); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestInitBootstrapVerifyRebuild(t *testing.T) {
	dir := t.TempDir()

	if out := runOK(t, "", "init", "-data", dir); !strings.Contains(out, "initialised node") {
		t.Errorf("init output: %s", out)
	}
	if out := runOK(t, "", "init", "-data", dir); !strings.Contains(out, "already initialised") {
		t.Errorf("second init output: %s", out)
	}

	boot := []string{"bootstrap", "-data", dir, "-username", "admin", "-name", "Ada Admin", "-email", "ada@example.org"}
	stdinLines = nil
	if err := run(context.Background(), boot, strings.NewReader("tumbleweed-gasket-42\nsomething-else-entirely\n"), &bytes.Buffer{}); err == nil {
		t.Error("mismatched confirmation accepted")
	}
	stdinLines = nil
	if err := run(context.Background(), boot, strings.NewReader("short\nshort\n"), &bytes.Buffer{}); err == nil {
		t.Error("weak password accepted")
	}
	if out := runOK(t, "tumbleweed-gasket-42\ntumbleweed-gasket-42\n", boot...); !strings.Contains(out, "created super user admin") {
		t.Errorf("bootstrap output: %s", out)
	}
	stdinLines = nil
	if err := run(context.Background(), boot, strings.NewReader("tumbleweed-gasket-42\ntumbleweed-gasket-42\n"), &bytes.Buffer{}); err == nil {
		t.Error("second bootstrap accepted")
	}

	if out := runOK(t, "", "verify", "-data", dir); !strings.Contains(out, "chains 1, events 2, redacted 0, problems 0") {
		t.Errorf("verify output: %s", out)
	}
	if out := runOK(t, "", "rebuild", "-data", dir); !strings.Contains(out, "projections rebuilt") {
		t.Errorf("rebuild output: %s", out)
	}
}

func TestCommandsNeedInit(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"verify", "-data", dir}, {"rebuild", "-data", dir}} {
		if err := run(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Errorf("%v on empty dir succeeded", args)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	if err := run(context.Background(), []string{"frobnicate"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Error("unknown command accepted")
	}
}
