package keys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")

	k1, created, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("first call: created = false")
	}
	for _, name := range []string{eventKeyFile, transportKeyFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}

	k2, created, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second call: created = true")
	}
	if !k1.Event.Equal(k2.Event) || !k1.Transport.Equal(k2.Transport) {
		t.Error("reloaded keys differ")
	}
	if k1.Event.Equal(k1.Transport) {
		t.Error("event and transport keys must be distinct")
	}
	if k1.EventSigner().KeyID == "" {
		t.Error("empty key id")
	}
}

func TestRefusesLoosePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if _, _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, eventKeyFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(dir); err == nil {
		t.Error("world-readable key accepted")
	}
}
