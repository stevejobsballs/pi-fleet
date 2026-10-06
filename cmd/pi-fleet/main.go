// Command pi-fleet is the single binary for nodes and central.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/term"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/hlc"
	"pi-fleet/internal/keys"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: pi-fleet <command> [flags]

commands:
  version     print the version
  init        create node keys, database, and chain (standalone, pre-activation)
  bootstrap   create the first super user (console only; prompts for a password)
  verify      re-check every event chain in the database
  rebuild     rebuild all projections from the event log
`

// passwordParams is replaced in tests to keep them fast.
var passwordParams = password.Default

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "pi-fleet:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
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
	case "bootstrap":
		return cmdBootstrap(ctx, rest, stdin, out)
	case "verify":
		return cmdVerify(ctx, rest, out)
	case "rebuild":
		return cmdRebuild(ctx, rest, out)
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

// openData opens the database with the domain projector installed and
// pointed at this node's identity, if it has one yet.
func openData(ctx context.Context, dir string) (*store.Store, *domain.Projector, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	p := &domain.Projector{}
	s, err := store.Open(filepath.Join(dir, "pi-fleet.db"), store.WithApplier(p))
	if err != nil {
		return nil, nil, err
	}
	ln, err := s.LocalNode(ctx)
	switch {
	case err == nil:
		p.LocalNodeID = ln.NodeID
	case !errors.Is(err, store.ErrNotFound):
		s.Close()
		return nil, nil, err
	}
	return s, p, nil
}

// openExisting opens a data directory that must already be initialised.
func openExisting(ctx context.Context, dir string) (*store.Store, error) {
	if _, err := os.Stat(filepath.Join(dir, "pi-fleet.db")); err != nil {
		return nil, fmt.Errorf("%w (run pi-fleet init first)", err)
	}
	s, _, err := openData(ctx, dir)
	return s, err
}

// loadApp opens an initialised node ready to run commands.
func loadApp(ctx context.Context, dir string) (*app.App, error) {
	s, err := openExisting(ctx, dir)
	if err != nil {
		return nil, err
	}
	ln, err := s.LocalNode(ctx)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("node not initialised: %w", err)
	}
	k, _, err := keys.LoadOrCreate(filepath.Join(dir, "keys"))
	if err != nil {
		s.Close()
		return nil, err
	}
	last, err := s.MaxHLC(ctx)
	if err != nil {
		s.Close()
		return nil, err
	}
	return &app.App{
		Store:  s,
		Author: &store.Author{NodeID: ln.NodeID, ChainID: ln.ChainID, Signer: k.EventSigner(), Clock: hlc.New(nil, last)},
		Params: passwordParams,
	}, nil
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
	s, p, err := openData(ctx, *data)
	if err != nil {
		return err
	}
	defer s.Close()

	if p.LocalNodeID != "" {
		ln, err := s.LocalNode(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "already initialised: node %s chain %s\n", ln.NodeID, ln.ChainID)
		return nil
	}

	ln := store.LocalNode{NodeID: uuid.Must(uuid.NewV7()).String(), ChainID: uuid.Must(uuid.NewV7()).String()}
	signer := k.EventSigner()
	if err := s.TrustKey(ctx, ln.NodeID, signer.Key.Public().(ed25519.PublicKey)); err != nil {
		return err
	}
	if err := s.SetLocalNode(ctx, ln); err != nil {
		return err
	}
	p.LocalNodeID = ln.NodeID
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

func cmdBootstrap(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	data := dataFlag(fs)
	username := fs.String("username", "", "username for the first super user")
	legalName := fs.String("name", "", "legal name, shown on e-signatures")
	email := fs.String("email", "", "work email")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" || *legalName == "" || *email == "" {
		return errors.New("bootstrap needs -username, -name and -email")
	}
	a, err := loadApp(ctx, *data)
	if err != nil {
		return err
	}
	defer a.Store.Close()

	fmt.Fprintf(out, "Choose a password for %s (at least %d characters).\n", *username, password.MinLength)
	pw, err := readPassword(stdin, out, "Password: ")
	if err != nil {
		return err
	}
	if err := password.CheckPolicy(pw, *username); err != nil {
		return err
	}
	again, err := readPassword(stdin, out, "Confirm:  ")
	if err != nil {
		return err
	}
	if pw != again {
		return errors.New("passwords do not match")
	}
	id, err := a.BootstrapSuperUser(ctx, app.NewUser{
		Username: *username, LegalName: *legalName, Email: *email,
		IdentityVerification: "console bootstrap",
	}, pw)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "created super user %s (%s)\n", *username, id)
	return nil
}

// stdinLines lets piped input supply several passwords, one per line.
var stdinLines *bufio.Reader

// readPassword reads a line without echo from a terminal, or plainly
// from piped input (for scripted installs and tests).
func readPassword(stdin io.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		return string(b), err
	}
	if stdinLines == nil {
		stdinLines = bufio.NewReader(stdin)
	}
	line, err := stdinLines.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", fmt.Errorf("reading password: %w", err)
	}
	fmt.Fprintln(out)
	return strings.TrimRight(line, "\r\n"), nil
}

func cmdVerify(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	data := dataFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openExisting(ctx, *data)
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

func cmdRebuild(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("rebuild", flag.ContinueOnError)
	data := dataFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openExisting(ctx, *data)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Rebuild(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "projections rebuilt")
	return nil
}
