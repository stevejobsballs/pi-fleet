// Package keys manages a node's Ed25519 key pairs on disk (DESIGN.md §6.1).
//
// Keys are generated on the device, stored as PKCS#8 PEM files with mode
// 0600 in a directory with mode 0700, and never leave the node.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"pi-fleet/internal/event"
)

const (
	eventKeyFile     = "event.key"
	transportKeyFile = "transport.key"
	pemType          = "PRIVATE KEY"
)

// NodeKeys holds a node's private keys.
type NodeKeys struct {
	Event     ed25519.PrivateKey // signs events
	Transport ed25519.PrivateKey // signs requests to central
}

// EventSigner returns the signer for the event key.
func (k NodeKeys) EventSigner() event.Signer {
	pub := k.Event.Public().(ed25519.PublicKey)
	return event.Signer{KeyID: event.KeyID(pub), Key: k.Event}
}

// LoadOrCreate loads the node keys from dir, generating any that are
// missing. It reports whether new keys were created.
func LoadOrCreate(dir string) (NodeKeys, bool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return NodeKeys{}, false, err
	}
	if err := checkPerm(dir, 0o700); err != nil {
		return NodeKeys{}, false, err
	}
	var k NodeKeys
	created := false
	for _, f := range []struct {
		name string
		dst  *ed25519.PrivateKey
	}{{eventKeyFile, &k.Event}, {transportKeyFile, &k.Transport}} {
		key, made, err := loadOrCreateKey(filepath.Join(dir, f.name))
		if err != nil {
			return NodeKeys{}, false, err
		}
		*f.dst = key
		created = created || made
	}
	return k, created, nil
}

func loadOrCreateKey(path string) (ed25519.PrivateKey, bool, error) {
	key, err := load(path)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	_, key, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := pem.Encode(f, &pem.Block{Type: pemType, Bytes: der}); err != nil {
		f.Close()
		return nil, false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, false, err
	}
	return key, true, f.Close()
}

func load(path string) (ed25519.PrivateKey, error) {
	if err := checkPerm(path, 0o600); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != pemType {
		return nil, fmt.Errorf("keys: %s: not a PEM private key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("keys: %s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("keys: %s: not an Ed25519 key", path)
	}
	return key, nil
}

// checkPerm refuses files or directories that grant more than max.
func checkPerm(path string, max fs.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&^max != 0 {
		return fmt.Errorf("keys: %s has mode %v; must be no more permissive than %v", path, info.Mode().Perm(), max)
	}
	return nil
}
