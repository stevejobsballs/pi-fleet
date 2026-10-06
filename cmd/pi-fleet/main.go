// Command pi-fleet is the single binary for nodes and central.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/term"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/hlc"
	"pi-fleet/internal/keys"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: pi-fleet <command> [flags]

any Pi:
  version            print the version
  init               create keys, database and chain: -role central (master Pi) or -role node
  verify             re-check every event chain in the database
  rebuild            rebuild projections (central: from events; node: from the last snapshot)
  verify-export      check a downloaded record (signed JSON) independently
  selfcheck          apply migrations and verify integrity and every chain (used by update)
  update             install a signed release with automatic rollback

master Pi (central):
  bootstrap          create the first super user (prompts for a password)
  user-create        create an account and print its one-time password
  serve              run the web interface and sync API over HTTPS, and the PM scheduler
  nodes              list Pis and their status
  node-confirm       confirm a pending Pi after checking its pairing words
  node-reject        reject a pending Pi
  node-revoke        revoke a Pi (it wipes itself when it next connects)
  node-unquarantine  release a Pi quarantined after a chain fork
  backup-keygen      generate a backup decryption identity (keep it offline)
  backup-config      set the backup disk and the age recipients
  backup-now         take a verified snapshot and export events now
  backups            list backups and warn if off-site rotation is overdue
  offsite-register   register a USB disk for off-site rotation
  offsite-write      write a verified snapshot to a plugged-in off-site disk
  offsite-confirm    confirm an off-site disk has reached the other building
  restore            restore the master Pi's database from a backup

employee Pi (node):
  activate           activate this Pi with the one-time password from a super user
  activation-status  check whether a super user has confirmed this Pi
  sync               sync with the master Pi once, or repeatedly with -every
  run                serve the web interface on this Pi and sync in the background

release maintainers (offline machine):
  release-keygen     create a release signing key, encrypted with a passphrase
  release-sign       write and sign manifest.json for a directory of binaries
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
	cmds := map[string]func(context.Context, []string, *cli) error{
		"init": cmdInit, "bootstrap": cmdBootstrap, "verify": cmdVerify, "rebuild": cmdRebuild,
		"serve": cmdServe, "nodes": cmdNodes, "node-confirm": cmdNodeConfirm, "node-reject": cmdNodeReject,
		"node-revoke": cmdNodeRevoke, "user-create": cmdUserCreate, "run": cmdRun,
		"backup-keygen": cmdBackupKeygen, "backup-config": cmdBackupConfig, "backup-now": cmdBackupNow, "backups": cmdBackups,
		"offsite-register": cmdOffsiteRegister, "offsite-write": cmdOffsiteWrite, "offsite-confirm": cmdOffsiteConfirm, "restore": cmdRestore,
		"selfcheck": cmdSelfcheck, "verify-export": cmdVerifyExport, "node-unquarantine": cmdNodeUnquarantine, "update": cmdUpdate, "release-keygen": cmdReleaseKeygen, "release-sign": cmdReleaseSign, "activate": cmdActivate, "activation-status": cmdActivationStatus, "sync": cmdSync,
	}
	c := &cli{stdin: stdin, out: out}
	switch cmd := args[0]; cmd {
	case "version":
		fmt.Fprintln(out, "pi-fleet", version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	default:
		f, ok := cmds[cmd]
		if !ok {
			fmt.Fprint(out, usage)
			return fmt.Errorf("unknown command %q", cmd)
		}
		return f(ctx, args[1:], c)
	}
}

// cli carries terminal I/O for commands.
type cli struct {
	stdin io.Reader
	out   io.Writer
	lines *bufio.Reader // piped input, shared across prompts
}

func (c *cli) printf(format string, args ...any) { fmt.Fprintf(c.out, format, args...) }

// readSecret reads a line without echo from a terminal, or plainly from
// piped input (for scripted installs and tests).
func (c *cli) readSecret(prompt string) (string, error) {
	fmt.Fprint(c.out, prompt)
	if f, ok := c.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(c.out)
		return string(b), err
	}
	line, err := c.readLine("")
	fmt.Fprintln(c.out)
	return line, err
}

func (c *cli) readLine(prompt string) (string, error) {
	fmt.Fprint(c.out, prompt)
	if c.lines == nil {
		c.lines = bufio.NewReader(c.stdin)
	}
	line, err := c.lines.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// newPassword prompts for a password twice and checks the policy.
func (c *cli) newPassword(username, prompt string) (string, error) {
	pw, err := c.readSecret(prompt)
	if err != nil {
		return "", err
	}
	if err := password.CheckPolicy(pw, username); err != nil {
		return "", err
	}
	again, err := c.readSecret("Confirm:  ")
	if err != nil {
		return "", err
	}
	if pw != again {
		return "", errors.New("passwords do not match")
	}
	return pw, nil
}

func dataFlag(fs *flag.FlagSet) *string {
	return fs.String("data", "/var/lib/pi-fleet", "data directory")
}

func parse(name string, args []string, setup func(fs *flag.FlagSet)) (*flag.FlagSet, *string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	data := dataFlag(fs)
	if setup != nil {
		setup(fs)
	}
	return fs, data, fs.Parse(args)
}

// node is an opened data directory.
type node struct {
	dir   string
	store *store.Store
	proj  *domain.Projector
	role  string
	local store.LocalNode
	keys  keys.NodeKeys
}

func (n *node) Close() error { return n.store.Close() }

// open opens an initialised data directory with the projector configured
// for its role.
func open(ctx context.Context, dir string) (*node, error) {
	if _, err := os.Stat(filepath.Join(dir, "pi-fleet.db")); err != nil {
		return nil, fmt.Errorf("%w (run pi-fleet init first)", err)
	}
	n := &node{dir: dir, proj: &domain.Projector{}}
	var err error
	if n.store, err = store.Open(filepath.Join(dir, "pi-fleet.db"), store.WithApplier(n.proj)); err != nil {
		return nil, err
	}
	if n.local, err = n.store.LocalNode(ctx); err != nil {
		n.Close()
		return nil, fmt.Errorf("data directory not initialised: %w", err)
	}
	n.proj.LocalNodeID = n.local.NodeID
	if n.role, err = n.store.Config(ctx, fleetsync.ConfigRole); err != nil {
		n.Close()
		return nil, fmt.Errorf("role not set: %w", err)
	}
	if id, err := n.store.Config(ctx, fleetsync.ConfigCentralNodeID); err == nil {
		n.proj.CentralNodeID = id
	}
	if n.keys, _, err = keys.LoadOrCreate(filepath.Join(dir, "keys")); err != nil {
		n.Close()
		return nil, err
	}
	return n, nil
}

func (n *node) require(role string) error {
	if n.role != role {
		return fmt.Errorf("this command runs on a %s, but this Pi is a %s", role, n.role)
	}
	return nil
}

// app returns the command runner for this node.
func (n *node) app(ctx context.Context) (*app.App, error) {
	last, err := n.store.MaxHLC(ctx)
	if err != nil {
		return nil, err
	}
	author := &store.Author{NodeID: n.local.NodeID, ChainID: n.local.ChainID, Signer: n.keys.EventSigner(), Clock: hlc.New(nil, last)}
	if n.role == "node" {
		author.ClockState = func() event.ClockState { return fleetsync.ClockState(ctx, n.store, time.Now()) }
	} else {
		author.ClockState = func() event.ClockState { return event.ClockVerified } // central runs NTP and an RTC
	}
	return &app.App{Store: n.store, Author: author, Params: passwordParams}, nil
}

func cmdInit(ctx context.Context, args []string, c *cli) error {
	var role *string
	_, data, err := parse("init", args, func(fs *flag.FlagSet) {
		role = fs.String("role", "", "central (the master Pi) or node (an employee Pi)")
	})
	if err != nil {
		return err
	}
	if *role != "central" && *role != "node" {
		return errors.New("init needs -role central or -role node")
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	k, _, err := keys.LoadOrCreate(filepath.Join(*data, "keys"))
	if err != nil {
		return err
	}
	s, err := store.Open(filepath.Join(*data, "pi-fleet.db"))
	if err != nil {
		return err
	}
	defer s.Close()

	if ln, err := s.LocalNode(ctx); err == nil {
		r, _ := s.Config(ctx, fleetsync.ConfigRole)
		c.printf("already initialised as %s: node %s chain %s\n", r, ln.NodeID, ln.ChainID)
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
	if err := s.SetConfig(ctx, fleetsync.ConfigRole, *role); err != nil {
		return err
	}
	author := &store.Author{NodeID: ln.NodeID, ChainID: ln.ChainID, Signer: signer, Clock: hlc.New(nil, 0)}
	e, err := s.Append(ctx, author, event.Draft{
		ActorUserID: "system:init", ActorSessionID: "system", Type: event.TypeChainStarted,
		EntityType: "chain", EntityID: ln.ChainID, SchemaVersion: 1,
		Payload: []byte(fmt.Sprintf(`{"software_version":%q,"role":%q}`, version, *role)),
	})
	if err != nil {
		return err
	}
	c.printf("initialised %s %s\nchain %s (genesis %s)\nevent key %s\n", *role, ln.NodeID, ln.ChainID, e.Hash, signer.KeyID)
	return nil
}

func cmdVerify(ctx context.Context, args []string, c *cli) error {
	_, data, err := parse("verify", args, nil)
	if err != nil {
		return err
	}
	n, err := open(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	rep, err := n.store.Verify(ctx)
	if err != nil {
		return err
	}
	c.printf("chains %d, events %d, redacted %d, problems %d\n", rep.Chains, rep.Events, rep.Redacted, len(rep.Problems))
	for _, p := range rep.Problems {
		c.printf("   %s\n", p)
	}
	if !rep.OK() {
		return errors.New("verification failed")
	}
	return nil
}

func cmdRebuild(ctx context.Context, args []string, c *cli) error {
	_, data, err := parse("rebuild", args, nil)
	if err != nil {
		return err
	}
	n, err := open(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	if n.role == "node" {
		pubHex, err := n.store.Config(ctx, fleetsync.ConfigCentralEventPub)
		if err != nil {
			return fmt.Errorf("not activated: %w", err)
		}
		pub, err := hex.DecodeString(pubHex)
		if err != nil {
			return err
		}
		err = fleetsync.ReapplyStoredSnapshot(ctx, n.store, pub, n.local.NodeID)
		if err != nil {
			return err
		}
	} else if err := n.store.Rebuild(ctx); err != nil {
		return err
	}
	c.printf("projections rebuilt\n")
	return nil
}
