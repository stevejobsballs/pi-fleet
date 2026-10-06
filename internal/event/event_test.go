package event

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testSigner(t *testing.T) (Signer, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return Signer{KeyID: KeyID(pub), Key: priv}, pub
}

func testPosition(seq int64, prev Hash) Position {
	return Position{
		NodeID:     "0199b6a0-0000-7000-8000-000000000001",
		ChainID:    "0199b6a0-0000-7000-8000-000000000002",
		Seq:        seq,
		PrevHash:   prev,
		HLC:        time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).UnixMilli()<<16 + seq,
		WallTime:   time.Date(2026, 10, 6, 12, 0, 0, 123456789, time.UTC),
		ClockState: ClockVerified,
	}
}

func testDraft(payload string) Draft {
	return Draft{
		ActorUserID:    uuid.NewString(),
		ActorSessionID: uuid.NewString(),
		Type:           "calibration.recorded",
		EntityType:     "calibration_record",
		EntityID:       uuid.NewString(),
		SchemaVersion:  1,
		Payload:        []byte(payload),
	}
}

func TestSealAndVerify(t *testing.T) {
	s, pub := testSigner(t)
	e, err := Seal(testDraft(`{"result":"pass", "as_found":"10.02"}`), testPosition(1, Hash{}), s)
	if err != nil {
		t.Fatal(err)
	}
	if string(e.Payload) != `{"as_found":"10.02","result":"pass"}` {
		t.Errorf("payload not canonicalized: %s", e.Payload)
	}
	if !e.WallTime.Equal(time.Date(2026, 10, 6, 12, 0, 0, 123000000, time.UTC)) {
		t.Errorf("wall time not truncated to ms: %v", e.WallTime)
	}
	if err := Verify(&e, pub); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	s, pub := testSigner(t)
	_, otherPub := testSigner(t)
	sealed, err := Seal(testDraft(`{"result":"fail"}`), testPosition(1, Hash{}), s)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(e *Event)
		pub    ed25519.PublicKey
		want   error
	}{
		{"payload changed", func(e *Event) { e.Payload = []byte(`{"result":"pass"}`) }, pub, ErrPayloadHash},
		{"payload not canonical", func(e *Event) { e.Payload = []byte(`{ "result":"fail"}`) }, pub, ErrNotCanonical},
		{"actor changed", func(e *Event) { e.ActorUserID = uuid.NewString() }, pub, ErrHashMismatch},
		{"wall time backdated", func(e *Event) { e.WallTime = e.WallTime.Add(-24 * time.Hour) }, pub, ErrHashMismatch},
		{"payload hash and payload swapped together", func(e *Event) {
			other, _ := Seal(testDraft(`{"result":"pass"}`), testPosition(1, Hash{}), s)
			e.Payload, e.PayloadHash = other.Payload, other.PayloadHash
		}, pub, ErrHashMismatch},
		{"wrong key", func(e *Event) {}, otherPub, ErrBadSignature},
		{"signature corrupted", func(e *Event) { e.Sig = append([]byte{}, e.Sig...); e.Sig[0] ^= 1 }, pub, ErrBadSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := sealed
			tt.mutate(&e)
			if err := Verify(&e, tt.pub); !errors.Is(err, tt.want) {
				t.Errorf("Verify error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRedactedEventStillVerifies(t *testing.T) {
	s, pub := testSigner(t)
	e, err := Seal(testDraft(`{"note":"accidental free text"}`), testPosition(1, Hash{}), s)
	if err != nil {
		t.Fatal(err)
	}
	e.Payload = nil
	if err := Verify(&e, pub); err != nil {
		t.Fatalf("Verify redacted: %v", err)
	}
}

func TestSealRejectsInvalid(t *testing.T) {
	s, _ := testSigner(t)
	tests := []struct {
		name string
		d    func() Draft
		p    Position
	}{
		{"float payload", func() Draft { return testDraft(`{"v":1.5}`) }, testPosition(1, Hash{})},
		{"bad type", func() Draft { d := testDraft(`{}`); d.Type = "Calibration"; return d }, testPosition(1, Hash{})},
		{"missing actor", func() Draft { d := testDraft(`{}`); d.ActorUserID = ""; return d }, testPosition(1, Hash{})},
		{"bad lease id", func() Draft { d := testDraft(`{}`); d.LeaseID = "x"; return d }, testPosition(1, Hash{})},
		{"first event with prev hash", func() Draft { return testDraft(`{}`) }, testPosition(1, Hash{1})},
		{"later event without prev hash", func() Draft { return testDraft(`{}`) }, testPosition(2, Hash{})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Seal(tt.d(), tt.p, s); err == nil {
				t.Error("Seal succeeded, want error")
			}
		})
	}
}

func TestVerifyLink(t *testing.T) {
	s, _ := testSigner(t)
	first, err := Seal(testDraft(`{}`), testPosition(1, Hash{}), s)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Seal(testDraft(`{}`), testPosition(2, first.Hash), s)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLink(0, Hash{}, &first); err != nil {
		t.Errorf("first link: %v", err)
	}
	if err := VerifyLink(1, first.Hash, &second); err != nil {
		t.Errorf("second link: %v", err)
	}
	if err := VerifyLink(1, Hash{9}, &second); !errors.Is(err, ErrBrokenLink) {
		t.Errorf("wrong prev hash: got %v", err)
	}
	if err := VerifyLink(2, first.Hash, &second); !errors.Is(err, ErrBrokenLink) {
		t.Errorf("seq gap: got %v", err)
	}
}
