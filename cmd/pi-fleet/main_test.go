package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestInitThenVerify(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var out bytes.Buffer

	if err := run(ctx, []string{"init", "-data", dir}, &out); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "initialised node") {
		t.Errorf("init output: %s", out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"init", "-data", dir}, &out); err != nil || !strings.Contains(out.String(), "already initialised") {
		t.Errorf("second init: %v %s", err, out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"verify", "-data", dir}, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "chains 1, events 1, redacted 0, problems 0") {
		t.Errorf("verify output: %s", out.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	if err := run(context.Background(), []string{"frobnicate"}, &bytes.Buffer{}); err == nil {
		t.Error("unknown command accepted")
	}
}
