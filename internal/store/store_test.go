package store

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pi-fleet/internal/event"
	"pi-fleet/internal/hlc"
)

type testNode struct {
	author *Author
	pub    ed25519.PublicKey
}

func newNode(t *testing.T) testNode {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return testNode{
		author: &Author{
			NodeID:  uuid.NewString(),
			ChainID: uuid.NewString(),
			Signer:  event.Signer{KeyID: event.KeyID(pub), Key: priv},
			Clock:   hlc.New(nil, 0),
		},
		pub: pub,
	}
}

func openStore(t *testing.T, trust ...testNode) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pi-fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, n := range trust {
		if err := s.TrustKey(context.Background(), n.author.NodeID, n.pub); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func draft(i int) event.Draft {
	return event.Draft{
		ActorUserID:    "0199b6a0-0000-7000-8000-0000000000aa",
		ActorSessionID: "0199b6a0-0000-7000-8000-0000000000bb",
		Type:           "meter.read",
		EntityType:     "asset",
		EntityID:       "0199b6a0-0000-7000-8000-0000000000cc",
		SchemaVersion:  1,
		Payload:        []byte(fmt.Sprintf(`{"meter":"hours","value":"%d"}`, i)),
	}
}

func appendN(t *testing.T, s *Store, n testNode, count int) []event.Event {
	t.Helper()
	var out []event.Event
	for i := 0; i < count; i++ {
		e, err := s.Append(context.Background(), n.author, draft(i))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestAppendBuildsVerifiableChain(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	s := openStore(t, n)
	evs := appendN(t, s, n, 5)

	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d seq = %d", i, e.Seq)
		}
		if i > 0 && e.PrevHash != evs[i-1].Hash {
			t.Errorf("event %d not linked to previous", i)
		}
		if i > 0 && e.HLC <= evs[i-1].HLC {
			t.Errorf("event %d HLC not increasing", i)
		}
	}
	seq, h, err := s.Head(ctx, n.author.ChainID)
	if err != nil || seq != 5 || h != evs[4].Hash {
		t.Errorf("Head = %d %v %v", seq, h, err)
	}
	got, err := s.Chain(ctx, n.author.ChainID, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].EventID != evs[1].EventID || !got[0].WallTime.Equal(evs[1].WallTime) {
		t.Errorf("Chain returned %d events, first %+v", len(got), got[0])
	}
	if err := event.Verify(&got[0], n.pub); err != nil {
		t.Errorf("event read back does not verify: %v", err)
	}
	rep, err := s.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Chains != 1 || rep.Events != 5 {
		t.Errorf("Verify = %+v", rep)
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	s := openStore(t, n)
	evs := appendN(t, s, n, 2)

	for _, stmt := range []string{
		`DELETE FROM events`,
		`UPDATE events SET payload = '{"meter":"hours","value":"999"}' WHERE seq = 1`,
		`UPDATE events SET wall_time = '2020-01-01T00:00:00.000Z' WHERE seq = 1`,
		`UPDATE events SET payload = NULL WHERE seq = 1`, // no redaction event exists
	} {
		_, err := s.w.ExecContext(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only", stmt, err)
		}
	}
	got, err := s.Event(ctx, evs[0].EventID)
	if err != nil || string(got.Payload) != string(evs[0].Payload) {
		t.Errorf("event changed: %s %v", got.Payload, err)
	}
}

func TestVerifyDetectsTamperingBehindTheTriggers(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	s := openStore(t, n)
	appendN(t, s, n, 3)

	// Someone with root drops the trigger and edits a result directly.
	if _, err := s.w.ExecContext(ctx, `DROP TRIGGER events_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.ExecContext(ctx, `UPDATE events SET payload = '{"meter":"hours","value":"7"}' WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 1 || rep.Problems[0].Seq != 2 || !errors.Is(rep.Problems[0].Err, event.ErrPayloadHash) {
		t.Errorf("Verify problems = %v", rep.Problems)
	}
}

func TestRedaction(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	s := openStore(t, n)
	evs := appendN(t, s, n, 2)
	actor, session := uuid.NewString(), uuid.NewString()

	if _, err := s.Redact(ctx, n.author, actor, session, evs[0].EventID, ""); err == nil {
		t.Error("redaction without reason accepted")
	}
	r, err := s.Redact(ctx, n.author, actor, session, evs[0].EventID, "free text contained a patient name")
	if err != nil {
		t.Fatal(err)
	}
	if r.Type != event.TypePayloadRedacted || r.Seq != 3 {
		t.Errorf("redaction event = %s seq %d", r.Type, r.Seq)
	}
	got, err := s.Event(ctx, evs[0].EventID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Redacted() || got.PayloadHash != evs[0].PayloadHash {
		t.Errorf("target not redacted correctly: payload=%s", got.Payload)
	}
	if _, err := s.Redact(ctx, n.author, actor, session, evs[0].EventID, "again"); !errors.Is(err, ErrRedacted) {
		t.Errorf("second redaction: %v", err)
	}
	// Other events stay protected even though a redaction event exists.
	if _, err := s.w.ExecContext(ctx, `UPDATE events SET payload = NULL WHERE seq = 2`); err == nil {
		t.Error("unredacted event payload cleared")
	}
	rep, err := s.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Redacted != 1 {
		t.Errorf("Verify after redaction = %+v", rep)
	}
}

func TestIngest(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	node := openStore(t, n)
	central := openStore(t, n)
	evs := appendN(t, node, n, 3)

	if err := central.Ingest(ctx, evs[1]); !errors.Is(err, ErrGap) {
		t.Errorf("out-of-order ingest: %v", err)
	}
	for _, e := range evs {
		if err := central.Ingest(ctx, e); err != nil {
			t.Fatalf("ingest seq %d: %v", e.Seq, err)
		}
	}
	if err := central.Ingest(ctx, evs[2]); !errors.Is(err, ErrDuplicate) {
		t.Errorf("re-ingest: %v", err)
	}

	// A node restored from an old backup writes a different seq 3.
	restored := openStore(t, n)
	for _, e := range evs[:2] {
		if err := restored.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	forked, err := restored.Append(ctx, n.author, draft(42))
	if err != nil {
		t.Fatal(err)
	}
	if err := central.Ingest(ctx, forked); !errors.Is(err, ErrFork) {
		t.Errorf("forked event: %v", err)
	}

	tampered := evs[2]
	tampered.Payload = []byte(`{"meter":"hours","value":"0"}`)
	other := openStore(t, n)
	for _, e := range evs[:2] {
		other.Ingest(ctx, e)
	}
	if err := other.Ingest(ctx, tampered); !errors.Is(err, event.ErrPayloadHash) {
		t.Errorf("tampered event: %v", err)
	}

	stranger := newNode(t)
	strangerEv := appendN(t, openStore(t), stranger, 1)[0]
	if err := central.Ingest(ctx, strangerEv); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("untrusted node: %v", err)
	}

	rep, err := central.Verify(ctx)
	if err != nil || !rep.OK() || rep.Events != 3 {
		t.Errorf("central Verify = %+v, %v", rep, err)
	}
}

func TestIngestAppliesRedaction(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	node := openStore(t, n)
	central := openStore(t, n)
	evs := appendN(t, node, n, 1)
	r, err := node.Redact(ctx, n.author, uuid.NewString(), uuid.NewString(), evs[0].EventID, "PHI")
	if err != nil {
		t.Fatal(err)
	}
	if err := central.Ingest(ctx, evs[0]); err != nil {
		t.Fatal(err)
	}
	if err := central.Ingest(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := central.Event(ctx, evs[0].EventID)
	if err != nil || !got.Redacted() {
		t.Errorf("central copy not redacted: %s %v", got.Payload, err)
	}
}

func TestReopenKeepsDataAndClock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pi-fleet.db")
	n := newNode(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.TrustKey(ctx, n.author.NodeID, n.pub)
	if err := s.SetLocalNode(ctx, LocalNode{NodeID: n.author.NodeID, ChainID: n.author.ChainID}); err != nil {
		t.Fatal(err)
	}
	evs := appendN(t, s, n, 2)
	s.Close()

	s, err = Open(path) // migrations must be idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ln, err := s.LocalNode(ctx)
	if err != nil || ln.ChainID != n.author.ChainID {
		t.Errorf("LocalNode = %+v, %v", ln, err)
	}
	max, err := s.MaxHLC(ctx)
	if err != nil || max != evs[1].HLC {
		t.Errorf("MaxHLC = %d, %v; want %d", max, err, evs[1].HLC)
	}
	// A clock seeded from MaxHLC keeps order even if the wall clock is behind.
	n.author.Clock = hlc.New(func() time.Time { return time.Unix(0, 0) }, max)
	e, err := s.Append(ctx, n.author, draft(3))
	if err != nil || e.Seq != 3 || e.HLC <= max {
		t.Errorf("append after reopen: seq %d hlc %d err %v", e.Seq, e.HLC, err)
	}
}

func TestPragmas(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	var mode string
	var sync int
	if err := s.w.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v", mode, err)
	}
	if err := s.w.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil || sync != 2 {
		t.Errorf("synchronous = %d, %v; want 2 (FULL)", sync, err)
	}
	if _, err := s.r.ExecContext(ctx, `CREATE TABLE x (y)`); err == nil {
		t.Error("reader pool accepted a write")
	}
}
