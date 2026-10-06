// Package event defines the signed, hash-chained event envelope that is
// the source of truth on every node and on central (DESIGN.md §4.2).
//
// An event's hash covers every header field and the SHA-256 of its
// canonical payload, but not the payload bytes themselves. That lets a
// payload be redacted (DESIGN.md §3.6) while the chain still verifies.
package event

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"

	"pi-fleet/internal/canon"
)

// hashDomain separates event hashes from any other SHA-256 use.
const hashDomain = "pi-fleet/event/v1\x00"

// WallTimeLayout is the fixed-width UTC format used in hashes and storage.
const WallTimeLayout = "2006-01-02T15:04:05.000Z"

// Well-known event types handled by the core.
const (
	TypeChainStarted    = "chain.started"
	TypePayloadRedacted = "payload.redacted"
)

// EntityEvent is the entity type of events that refer to another event,
// such as a redaction.
const EntityEvent = "event"

var (
	ErrBadSignature    = errors.New("event: signature does not verify")
	ErrHashMismatch    = errors.New("event: hash does not match contents")
	ErrPayloadHash     = errors.New("event: payload does not match payload_hash")
	ErrNotCanonical    = errors.New("event: stored payload is not canonical")
	ErrBrokenLink      = errors.New("event: does not follow the previous event in its chain")
	ErrInvalidEnvelope = errors.New("event: invalid envelope")
)

var typeRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Hash is a SHA-256 digest.
type Hash [32]byte

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// IsZero reports whether h is all zeroes (the prev_hash of a chain's
// first event).
func (h Hash) IsZero() bool { return h == Hash{} }

// ClockState records whether the authoring node's clock was verified
// when the event was created (DESIGN.md §3.4).
type ClockState string

const (
	ClockVerified   ClockState = "verified"
	ClockUnverified ClockState = "unverified"
)

// Draft is what a caller supplies; Seal turns it into an Event.
type Draft struct {
	ActorUserID    string
	ActorSessionID string
	Type           string
	EntityType     string
	EntityID       string
	BaseVersion    int64
	LeaseID        string
	SchemaVersion  int
	Payload        []byte // any JSON; canonicalized by Seal
}

// Event is a sealed, signed event.
type Event struct {
	EventID        string
	NodeID         string
	ChainID        string
	Seq            int64
	PrevHash       Hash
	HLC            int64
	WallTime       time.Time
	ClockState     ClockState
	ActorUserID    string
	ActorSessionID string
	Type           string
	EntityType     string
	EntityID       string
	BaseVersion    int64
	LeaseID        string
	SchemaVersion  int
	Payload        []byte // canonical JSON; nil once redacted
	PayloadHash    Hash
	Hash           Hash
	Sig            []byte
	KeyID          string
}

// Signer signs events with a node's event key.
type Signer struct {
	KeyID string
	Key   ed25519.PrivateKey
}

// Position is where in which chain an event is being written, and when.
type Position struct {
	NodeID     string
	ChainID    string
	Seq        int64
	PrevHash   Hash
	HLC        int64
	WallTime   time.Time
	ClockState ClockState
}

// Seal canonicalizes the draft's payload, assigns an event id, computes
// the hashes, and signs the result.
func Seal(d Draft, p Position, s Signer) (Event, error) {
	payload, err := canon.Canonicalize(d.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("event: payload: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Event{}, err
	}
	e := Event{
		EventID:        id.String(),
		NodeID:         p.NodeID,
		ChainID:        p.ChainID,
		Seq:            p.Seq,
		PrevHash:       p.PrevHash,
		HLC:            p.HLC,
		WallTime:       p.WallTime.UTC().Truncate(time.Millisecond),
		ClockState:     p.ClockState,
		ActorUserID:    d.ActorUserID,
		ActorSessionID: d.ActorSessionID,
		Type:           d.Type,
		EntityType:     d.EntityType,
		EntityID:       d.EntityID,
		BaseVersion:    d.BaseVersion,
		LeaseID:        d.LeaseID,
		SchemaVersion:  d.SchemaVersion,
		Payload:        payload,
		PayloadHash:    sha256.Sum256(payload),
		KeyID:          s.KeyID,
	}
	if err := e.Validate(); err != nil {
		return Event{}, err
	}
	e.Hash, err = e.ComputeHash()
	if err != nil {
		return Event{}, err
	}
	e.Sig = ed25519.Sign(s.Key, e.Hash[:])
	return e, nil
}

// Validate checks the envelope's structural rules.
func (e *Event) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidEnvelope, fmt.Sprintf(format, args...))
	}
	for name, v := range map[string]string{"event_id": e.EventID, "node_id": e.NodeID, "chain_id": e.ChainID} {
		if _, err := uuid.Parse(v); err != nil || len(v) != 36 {
			return bad("%s %q is not a UUID", name, v)
		}
	}
	if e.LeaseID != "" {
		if _, err := uuid.Parse(e.LeaseID); err != nil || len(e.LeaseID) != 36 {
			return bad("lease_id %q is not a UUID", e.LeaseID)
		}
	}
	switch {
	case e.Seq < 1:
		return bad("seq %d < 1", e.Seq)
	case e.Seq == 1 && !e.PrevHash.IsZero():
		return bad("first event in a chain must have a zero prev_hash")
	case e.Seq > 1 && e.PrevHash.IsZero():
		return bad("prev_hash missing at seq %d", e.Seq)
	case e.HLC <= 0:
		return bad("hlc must be positive")
	case e.WallTime.IsZero():
		return bad("wall_time missing")
	case e.ClockState != ClockVerified && e.ClockState != ClockUnverified:
		return bad("clock_state %q", e.ClockState)
	case e.ActorUserID == "" || e.ActorSessionID == "":
		return bad("actor_user_id and actor_session_id are required")
	case !typeRE.MatchString(e.Type):
		return bad("type %q", e.Type)
	case e.EntityType == "" || e.EntityID == "":
		return bad("entity_type and entity_id are required")
	case e.BaseVersion < 0:
		return bad("base_version %d < 0", e.BaseVersion)
	case e.SchemaVersion < 1:
		return bad("schema_version %d < 1", e.SchemaVersion)
	case e.KeyID == "":
		return bad("key_id is required")
	}
	return nil
}

// ComputeHash returns SHA-256 over the domain tag and the canonical
// encoding of every header field plus payload_hash.
func (e *Event) ComputeHash() (Hash, error) {
	header := map[string]any{
		"event_id":         e.EventID,
		"node_id":          e.NodeID,
		"chain_id":         e.ChainID,
		"seq":              e.Seq,
		"prev_hash":        e.PrevHash.String(),
		"hlc":              strconv.FormatInt(e.HLC, 10), // exceeds 2^53
		"wall_time":        e.WallTime.UTC().Format(WallTimeLayout),
		"clock_state":      string(e.ClockState),
		"actor_user_id":    e.ActorUserID,
		"actor_session_id": e.ActorSessionID,
		"type":             e.Type,
		"entity_type":      e.EntityType,
		"entity_id":        e.EntityID,
		"base_version":     e.BaseVersion,
		"lease_id":         e.LeaseID,
		"schema_version":   e.SchemaVersion,
		"payload_hash":     e.PayloadHash.String(),
		"key_id":           e.KeyID,
	}
	b, err := canon.Marshal(header)
	if err != nil {
		return Hash{}, fmt.Errorf("event: header: %w", err)
	}
	return sha256.Sum256(append([]byte(hashDomain), b...)), nil
}

// Redacted reports whether the payload has been removed.
func (e *Event) Redacted() bool { return e.Payload == nil }

// Verify checks an event on its own: envelope, payload, hash, signature.
// It does not check the event's place in its chain; see VerifyLink.
func Verify(e *Event, pub ed25519.PublicKey) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if !e.Redacted() {
		c, err := canon.Canonicalize(e.Payload)
		if err != nil || !bytes.Equal(c, e.Payload) {
			return ErrNotCanonical
		}
		if sha256.Sum256(e.Payload) != e.PayloadHash {
			return ErrPayloadHash
		}
	}
	h, err := e.ComputeHash()
	if err != nil {
		return err
	}
	if h != e.Hash {
		return ErrHashMismatch
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, e.Hash[:], e.Sig) {
		return ErrBadSignature
	}
	return nil
}

// VerifyLink checks that e directly follows the event at (prevSeq,
// prevHash) in its chain. For a chain's first event pass 0 and a zero
// hash.
func VerifyLink(prevSeq int64, prevHash Hash, e *Event) error {
	if e.Seq != prevSeq+1 || e.PrevHash != prevHash {
		return fmt.Errorf("%w: chain %s seq %d", ErrBrokenLink, e.ChainID, e.Seq)
	}
	return nil
}

// KeyID derives the identifier for a public key: the first 16 hex
// characters of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}
