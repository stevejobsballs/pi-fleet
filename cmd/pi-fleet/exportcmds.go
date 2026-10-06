package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/export"
)

// cmdVerifyExport checks an exported record bundle on its own: every
// event's payload, hash and signature against the bundled keys.
func cmdVerifyExport(ctx context.Context, args []string, c *cli) error {
	fs := flag.NewFlagSet("verify-export", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pi-fleet verify-export <file.json>")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var bundle export.Bundle
	if err := json.Unmarshal(b, &bundle); err != nil {
		return fmt.Errorf("not an export bundle: %w", err)
	}
	if bundle.Format != export.Format {
		return fmt.Errorf("unsupported export format %q", bundle.Format)
	}
	rep := export.Verify(bundle)
	c.printf("%s %s, exported %s by %s from %s\n", bundle.Subject.Type, bundle.Subject.Label,
		bundle.GeneratedAt.Format("2006-01-02 15:04 UTC"), bundle.GeneratedBy, bundle.Source)
	if !bundle.Complete {
		c.printf("note: exported from an employee Pi, which holds only its own records\n")
	}
	c.printf("events %d, signatures verified %d, redacted %d, problems %d\n", rep.Events, rep.Verified, rep.Redacted, len(rep.Problems))
	sort.Strings(rep.Keys)
	c.printf("signing keys (compare with the master Pi's records of each Pi):\n")
	for _, k := range rep.Keys {
		c.printf("  %s\n", k)
	}
	for _, p := range rep.Problems {
		c.printf("PROBLEM: %s\n", p)
	}
	if len(rep.Problems) > 0 {
		return errors.New("export verification failed")
	}
	return nil
}

func cmdNodeUnquarantine(ctx context.Context, args []string, c *cli) error {
	var nodeID, as, reason *string
	_, data, err := parse("node-unquarantine", args, func(fs *flag.FlagSet) {
		nodeID = fs.String("node", "", "id of the quarantined Pi")
		as = fs.String("as", "", "your super-user name")
		reason = fs.String("reason", "", "what the investigation found")
	})
	if err != nil {
		return err
	}
	n, a, err := openCentral(ctx, *data)
	if err != nil {
		return err
	}
	defer n.Close()
	actor, err := signIn(ctx, a, c, *as, domain.RoleSuperUser)
	if err != nil {
		return err
	}
	if err := a.ClearQuarantine(ctx, actor, *nodeID, *reason); err != nil {
		return err
	}
	c.printf("quarantine lifted for Pi %s\n", *nodeID)
	return nil
}
