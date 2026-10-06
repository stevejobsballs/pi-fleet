// Package fleetsync implements node activation and sync between employee
// Pis and the master Pi over outbound HTTPS (DESIGN.md §5–6).
//
// Nodes push their own signed event chains up. Central sends back a
// signed working-set snapshot: the current state of everything due across
// the fleet in the next 31 days, open work, reference standards and the
// node's own user. A node rebases its unsynced events onto each snapshot.
package fleetsync

import (
	"encoding/hex"
	"fmt"
	"time"

	"pi-fleet/internal/event"
)

// Paths of the sync API.
const (
	PathChallenge = "/v1/activate/challenge"
	PathActivate  = "/v1/activate"
	PathStatus    = "/v1/activate/status"
	PathHello     = "/v1/sync/hello"
	PathEvents    = "/v1/sync/events"
	PathSnapshot  = "/v1/sync/snapshot"
)

// SnapshotSignatureHeader carries central's signature over a snapshot body.
const SnapshotSignatureHeader = "X-PiFleet-Snapshot-Signature"

// snapshotDomain separates snapshot signatures from event signatures.
const snapshotDomain = "pi-fleet/snapshot/v1\x00"

// WireEvent is an event as sent over the network. Payload bytes travel as
// base64 so they arrive byte-for-byte as signed.
type WireEvent struct {
	EventID        string `json:"event_id"`
	NodeID         string `json:"node_id"`
	ChainID        string `json:"chain_id"`
	Seq            int64  `json:"seq"`
	PrevHash       string `json:"prev_hash"`
	HLC            int64  `json:"hlc"`
	WallTime       string `json:"wall_time"`
	ClockState     string `json:"clock_state"`
	ActorUserID    string `json:"actor_user_id"`
	ActorSessionID string `json:"actor_session_id"`
	Type           string `json:"type"`
	EntityType     string `json:"entity_type"`
	EntityID       string `json:"entity_id"`
	BaseVersion    int64  `json:"base_version"`
	LeaseID        string `json:"lease_id"`
	SchemaVersion  int    `json:"schema_version"`
	Payload        []byte `json:"payload"` // null once redacted
	PayloadHash    string `json:"payload_hash"`
	Hash           string `json:"hash"`
	Sig            string `json:"sig"`
	KeyID          string `json:"key_id"`
}

// ToWire converts an event for sending.
func ToWire(e event.Event) WireEvent {
	return WireEvent{
		EventID: e.EventID, NodeID: e.NodeID, ChainID: e.ChainID, Seq: e.Seq,
		PrevHash: e.PrevHash.String(), HLC: e.HLC, WallTime: e.WallTime.UTC().Format(event.WallTimeLayout),
		ClockState: string(e.ClockState), ActorUserID: e.ActorUserID, ActorSessionID: e.ActorSessionID,
		Type: e.Type, EntityType: e.EntityType, EntityID: e.EntityID, BaseVersion: e.BaseVersion,
		LeaseID: e.LeaseID, SchemaVersion: e.SchemaVersion, Payload: e.Payload,
		PayloadHash: e.PayloadHash.String(), Hash: e.Hash.String(), Sig: hex.EncodeToString(e.Sig), KeyID: e.KeyID,
	}
}

func decodeHash(s string) (event.Hash, error) {
	var h event.Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) {
		return h, fmt.Errorf("bad hash %q", s)
	}
	copy(h[:], b)
	return h, nil
}

// FromWire converts a received event. The result must still be verified.
func FromWire(w WireEvent) (event.Event, error) {
	e := event.Event{
		EventID: w.EventID, NodeID: w.NodeID, ChainID: w.ChainID, Seq: w.Seq, HLC: w.HLC,
		ClockState: event.ClockState(w.ClockState), ActorUserID: w.ActorUserID, ActorSessionID: w.ActorSessionID,
		Type: w.Type, EntityType: w.EntityType, EntityID: w.EntityID, BaseVersion: w.BaseVersion,
		LeaseID: w.LeaseID, SchemaVersion: w.SchemaVersion, Payload: w.Payload, KeyID: w.KeyID,
	}
	var err error
	if e.WallTime, err = time.Parse(event.WallTimeLayout, w.WallTime); err != nil {
		return e, fmt.Errorf("wall_time: %w", err)
	}
	if e.PrevHash, err = decodeHash(w.PrevHash); err != nil {
		return e, err
	}
	if e.PayloadHash, err = decodeHash(w.PayloadHash); err != nil {
		return e, err
	}
	if e.Hash, err = decodeHash(w.Hash); err != nil {
		return e, err
	}
	if e.Sig, err = hex.DecodeString(w.Sig); err != nil {
		return e, fmt.Errorf("sig: %w", err)
	}
	return e, nil
}

// ChallengeRequest asks for the parameters needed to prove knowledge of
// a one-time password.
type ChallengeRequest struct {
	Username string `json:"username"`
}

// ChallengeResponse carries the user's salt and Argon2 parameters, and a
// single-use nonce. Unknown usernames get a plausible fake salt.
type ChallengeResponse struct {
	Salt      []byte `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
	KeyLen    int    `json:"key_len"`
	Nonce     string `json:"nonce"`
}

// ActivateRequest proves the one-time password and registers the node's
// keys. The employee's new password verifier is encrypted under a key
// derived from the one-time password, so it never crosses the network in
// the clear and the password itself never crosses it at all.
type ActivateRequest struct {
	Username           string `json:"username"`
	Nonce              string `json:"nonce"`
	NodeID             string `json:"node_id"`
	EventPublicKey     string `json:"event_public_key"`
	TransportPublicKey string `json:"transport_public_key"`
	SealedVerifier     []byte `json:"sealed_verifier"`
	Proof              []byte `json:"proof"`
}

// ActivateResponse returns central's identity, MAC'd with the one-time
// password key so the node knows it reached the real central.
type ActivateResponse struct {
	NodeID          string `json:"node_id"`
	Status          string `json:"status"`
	PairingWords    string `json:"pairing_words"`
	CentralNodeID   string `json:"central_node_id"`
	CentralEventPub string `json:"central_event_public_key"` // hex
	MAC             []byte `json:"mac"`
}

// StatusResponse reports a node's activation status.
type StatusResponse struct {
	Status string `json:"status"`
}

// HelloRequest opens a sync.
type HelloRequest struct {
	ChainID         string `json:"chain_id"`
	HeadSeq         int64  `json:"head_seq"`
	SoftwareVersion string `json:"software_version"`
}

// HelloResponse tells the node where central is.
type HelloResponse struct {
	ServerTime   string `json:"server_time"`
	Status       string `json:"status"`
	Wipe         bool   `json:"wipe"`
	KeepUnsynced bool   `json:"keep_unsynced"`
	CentralSeq   int64  `json:"central_seq"`  // central's head of the node's chain
	CentralHash  string `json:"central_hash"` // hex; empty if none
	// DurableSeq is the highest seq of the node's chain that central holds
	// in a verified off-site backup. Nodes purge only at or below it.
	DurableSeq   int64 `json:"durable_seq"`
	StateVersion int64 `json:"state_version"`
	Quarantined  bool  `json:"quarantined"`
}

// EventsRequest pushes a contiguous batch of the node's own events.
type EventsRequest struct {
	Events []WireEvent `json:"events"`
}

// EventResult is central's verdict on one pushed event.
type EventResult struct {
	Seq    int64    `json:"seq"`
	Status string   `json:"status"` // stored, duplicate
	Flags  []string `json:"flags,omitempty"`
}

// EventsResponse reports how far central got.
type EventsResponse struct {
	AcceptedThrough int64         `json:"accepted_through"`
	Results         []EventResult `json:"results"`
	Error           string        `json:"error,omitempty"`
}
