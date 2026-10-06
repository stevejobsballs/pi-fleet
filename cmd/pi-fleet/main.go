// Command pi-fleet is the single binary for nodes and central.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"pi-fleet/internal/event"
	"pi-fleet/internal/hlc"
	"pi-fleet/internal/keys"
	"pi-fleet/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: pi-fleet <command> [flags]

commands:
  version   print the version
  init      create node keys, database, and chain (standalone, pre-activation)
  verify    re-check every event chain in the database
`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "pi-fleet:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return errors.New("no command given")
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "version":
		fmt.Fprintln(out, "pi-fleet", version)
		return nil
	case "init":
		return cmdInit(ctx, rest, out)
	case "verify":
		return cmdVerify(ctx, rest, out)
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	default:
		fmt.Fprint(out, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func dataFlag(fs *flag.FlagSet) *string {
	return fs.String("data", "/var/lib/pi-fleet", "data directory")
}

func openData(dir string) (*store.Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dir, "pi-fleet.db"))
}

func cmdInit(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	data := dataFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, _, err := keys.LoadOrCreate(filepath.Join(*data, "keys"))
	if err != nil {
		return err
	}
	s, err := openData(*data)
	if err != nil {
		return err
	}
	defer s.Close()

	if ln, err := s.LocalNode(ctx); err == nil {
		fmt.Fprintf(out, "already initialised: node %s chain %s\n", ln.NodeID, ln.ChainID)
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	ln := store.LocalNode{NodeID: uuid.Must(uuid.NewV7()).String(), ChainID: uuid.Must(uuid.NewV7()).String()}
	signer := k.EventSigner()
	if err := s.TrustKey(ctx, ln.NodeID, signer.Key.Public().(ed25519.PublicKey)); err != nil {
		return err
	}
	if err := s.SetLocalNode(ctx, ln); err != nil {
		return err
	}
	last, err := s.MaxHLC(ctx)
	if err != nil {
		return err
	}
	author := &store.Author{NodeID: ln.NodeID, ChainID: ln.ChainID, Signer: signer, Clock: hlc.New(nil, last)}
	e, err := s.Append(ctx, author, event.Draft{
		ActorUserID:    "system:init",
		ActorSessionID: "system",
		Type:           event.TypeChainStarted,
		EntityType:     "chain",
		EntityID:       ln.ChainID,
		SchemaVersion:  1,
		Payload:        []byte(fmt.Sprintf(`{"software_version":%q}`, version)),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "initialised node %s\nchain %s (genesis %s)\nevent key %s\n", ln.NodeID, ln.ChainID, e.Hash, signer.KeyID)
	return nil
}

func cmdVerify(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	data := dataFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*data, "pi-fleet.db")); err != nil {
		return err
	}
	s, err := openData(*data)
	if err != nil {
		return err
	}
	defer s.Close()
	rep, err := s.Verify(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "chains %d, events %d, redacted %d, problems %d\n", rep.Chains, rep.Events, rep.Redacted, len(rep.Problems))
	for _, p := range rep.Problems {
		fmt.Fprintln(out, "  ", p)
	}
	if !rep.OK() {
		return errors.New("verification failed")
	}
	return nil
}
