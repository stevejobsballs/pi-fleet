package fleetsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/httpsig"
	"pi-fleet/internal/keys"
	"pi-fleet/internal/pairing"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

// Node configuration keys (store.Config).
const (
	ConfigRole            = "role"
	ConfigCentralURL      = "central_url"
	ConfigCentralNodeID   = "central_node_id"
	ConfigCentralEventPub = "central_event_pub"
	ConfigActivation      = "activation_status"
	ConfigStateVersion    = "snapshot_state_version"
	ConfigClockVerifiedAt = "clock_verified_at"
	ConfigLastSync        = "last_sync"
	ConfigAckedSeq        = "acked_seq"
)

// Errors a node may hit while syncing.
var (
	ErrNotActivated = errors.New("fleetsync: this Pi is not activated and confirmed yet")
	ErrRevoked      = errors.New("fleetsync: this Pi was revoked by a super user and has been wiped")
	ErrQuarantined  = errors.New("fleetsync: central quarantined this Pi after a chain fork; contact a super user")
	ErrCentralAhead = errors.New("fleetsync: central holds newer events for this chain than this Pi (restored from an old backup?); start a new chain")
	ErrForked       = errors.New("fleetsync: central's copy of this chain differs from this Pi's; contact a super user")
)

const (
	pushBatch      = 200
	clockTolerance = 2 * time.Minute
	// Retention is how long a node keeps its own events (decision D8).
	Retention = 31 * 24 * time.Hour
)

// Client is a node's connection to central.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Store   *store.Store
	Keys    keys.NodeKeys
	NodeID  string
	ChainID string
	// Version is reported to central.
	Version string
	// Wipe deletes this node's data; called when central reports the Pi
	// revoked.
	Wipe func() error
	// Now defaults to time.Now.
	Now func() time.Time
	// Blobs caches attachment files on this Pi.
	Blobs *blobs.Store

	// offset corrects request-signing time when this Pi's clock is wrong
	// (no RTC battery, no NTP). Event timestamps keep the local clock and
	// are marked unverified; only the transport adapts.
	offset time.Duration
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) transportKeyID() string {
	return event.KeyID(c.Keys.Transport.Public().(ed25519.PublicKey))
}

// do sends a request, signed with the transport key unless unsigned. If
// central refuses the signature time, it retries once using central's
// clock from the Date header.
func (c *Client) do(ctx context.Context, method, path string, in, out any, signed bool) (*http.Response, []byte, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return nil, nil, err
		}
	}
	return c.doRaw(ctx, method, path, body, out, signed)
}

func (c *Client) doRaw(ctx context.Context, method, path string, body []byte, out any, signed bool) (*http.Response, []byte, error) {
	resp, respBody, err := c.send(ctx, method, path, body, out, signed)
	if signed && resp != nil && resp.StatusCode == http.StatusUnauthorized && strings.Contains(string(respBody), "outside the allowed window") {
		if d, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
			c.offset = d.Sub(c.now())
			return c.send(ctx, method, path, body, out, signed)
		}
	}
	return resp, respBody, err
}

func (c *Client) send(ctx context.Context, method, path string, body []byte, out any, signed bool) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if signed {
		n := make([]byte, 16)
		if _, err := rand.Read(n); err != nil {
			return nil, nil, err
		}
		if err := httpsig.Sign(req, body, c.transportKeyID(), c.Keys.Transport, c.now().Add(c.offset), hex.EncodeToString(n)); err != nil {
			return nil, nil, err
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp, nil, err
	}
	if resp.StatusCode == http.StatusNotFound && method == http.MethodHead {
		return resp, respBody, errNotThere
	}
	if resp.StatusCode != http.StatusOK {
		return resp, respBody, fmt.Errorf("fleetsync: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return resp, respBody, fmt.Errorf("fleetsync: %s: %w", path, err)
		}
	}
	return resp, respBody, nil
}

// --- live fleet reads ---

// FleetGet fetches a /v1/fleet/ resource from central (online only).
func (c *Client) FleetGet(ctx context.Context, path string, out any) error {
	_, _, err := c.do(ctx, http.MethodGet, path, nil, out, true)
	return err
}

// FleetPost sends a /v1/fleet/ request to central (online only).
func (c *Client) FleetPost(ctx context.Context, path string, in, out any) error {
	_, _, err := c.do(ctx, http.MethodPost, path, in, out, true)
	return err
}

// --- attachment files ---

var errNotThere = errors.New("fleetsync: not on central")

// uploadBlobs sends files added on this Pi that central doesn't have.
func (c *Client) uploadBlobs(ctx context.Context) (int, error) {
	if c.Blobs == nil {
		return 0, nil
	}
	rows, err := c.Store.DB().QueryContext(ctx, `SELECT sha256 FROM blob_uploads ORDER BY added_at`)
	if err != nil {
		return 0, err
	}
	var pending []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		pending = append(pending, s)
	}
	rows.Close()
	n := 0
	for _, sha := range pending {
		data, err := c.Blobs.Get(sha)
		if errors.Is(err, blobs.ErrNotFound) {
			// Purged here before it was uploaded: nothing to send.
		} else if err != nil {
			return n, err
		} else if _, _, err := c.doRaw(ctx, http.MethodHead, PathBlobs+sha, nil, nil, true); errors.Is(err, errNotThere) {
			if _, _, err := c.doRaw(ctx, http.MethodPut, PathBlobs+sha, data, nil, true); err != nil {
				return n, err
			}
			n++
		} else if err != nil {
			return n, err
		}
		if err := c.Store.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM blob_uploads WHERE sha256 = ?`, sha)
			return err
		}); err != nil {
			return n, err
		}
	}
	return n, nil
}

// cleanBlobs drops cached files no longer in the working set (and purged
// ones), keeping files still waiting to be uploaded.
func (c *Client) cleanBlobs(ctx context.Context) error {
	if c.Blobs == nil {
		return nil
	}
	have, err := c.Blobs.List()
	if err != nil {
		return err
	}
	for _, sha := range have {
		var keep int
		if err := c.Store.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM attachments WHERE sha256 = ?1 AND status != 'purged')
			+ (SELECT count(*) FROM blob_uploads WHERE sha256 = ?1)`, sha).Scan(&keep); err != nil {
			return err
		}
		if keep == 0 {
			if err := c.Blobs.Remove(sha); err != nil {
				return err
			}
		}
	}
	return nil
}

// FetchBlob returns an attachment file, from this Pi's cache or from
// central (online only), caching what it fetches.
func (c *Client) FetchBlob(ctx context.Context, sha string) ([]byte, error) {
	if c.Blobs == nil {
		return nil, errors.New("fleetsync: no blob store configured")
	}
	if b, err := c.Blobs.Get(sha); err == nil {
		return b, nil
	}
	_, body, err := c.doRaw(ctx, http.MethodGet, PathBlobs+sha, nil, nil, true)
	if err != nil {
		return nil, err
	}
	if err := c.Blobs.PutExpected(sha, body); err != nil {
		return nil, err
	}
	return body, nil
}

// --- activation ---

// Activation is the outcome of activating this Pi.
type Activation struct {
	PairingWords  string
	CentralNodeID string
}

// PairingWords returns the words this Pi will show for confirmation.
func (c *Client) PairingWords() string {
	return pairing.Words(c.Keys.Event.Public().(ed25519.PublicKey), c.Keys.Transport.Public().(ed25519.PublicKey))
}

// Activate proves the super-user-issued one-time password and registers
// this Pi's keys with central, setting the employee's chosen password
// (DESIGN.md §6.3). The Pi then waits for a super user to confirm it.
func (c *Client) Activate(ctx context.Context, username, oneTime, chosen string, params password.Params) (Activation, error) {
	if err := password.CheckPolicy(chosen, username); err != nil {
		return Activation{}, err
	}
	if chosen == oneTime {
		return Activation{}, &password.PolicyError{Reason: "must differ from the one-time password"}
	}
	newVerifier, err := password.Hash(chosen, params)
	if err != nil {
		return Activation{}, err
	}
	return c.activate(ctx, username, oneTime, newVerifier.String())
}

// ActivateKiosk activates this Pi as the shared kiosk with the given
// name, using the kiosk's one-time activation password. Members sign in
// with their own passwords once a super user confirms it.
func (c *Client) ActivateKiosk(ctx context.Context, name, oneTime string) (Activation, error) {
	return c.activate(ctx, domain.KioskPrefix+name, oneTime, "")
}

func (c *Client) activate(ctx context.Context, username, oneTime, newVerifier string) (Activation, error) {
	var ch ChallengeResponse
	if _, _, err := c.do(ctx, http.MethodPost, PathChallenge, ChallengeRequest{Username: username}, &ch, false); err != nil {
		return Activation{}, err
	}
	if ch.KeyLen < 16 || len(ch.Salt) < 8 {
		return Activation{}, errors.New("fleetsync: central sent an unusable challenge")
	}
	k := password.Derive(oneTime, ch.Salt, password.Params{Time: ch.Time, MemoryKiB: ch.MemoryKiB, Threads: ch.Threads})
	k.Key = k.Key[:min(len(k.Key), ch.KeyLen)]
	enc, macKey := activationKeys(k.Key)

	sealed, err := sealVerifier(enc, ch.Nonce, newVerifier)
	if err != nil {
		return Activation{}, err
	}
	req := ActivateRequest{
		Username: username, Nonce: ch.Nonce, NodeID: c.NodeID,
		EventPublicKey:     hex.EncodeToString(c.Keys.Event.Public().(ed25519.PublicKey)),
		TransportPublicKey: hex.EncodeToString(c.Keys.Transport.Public().(ed25519.PublicKey)),
		SealedVerifier:     sealed,
	}
	req.Proof = requestProof(macKey, &req)
	var resp ActivateResponse
	if _, _, err := c.do(ctx, http.MethodPost, PathActivate, req, &resp, false); err != nil {
		return Activation{}, err
	}
	// Only the real central knows K, so a valid MAC authenticates it even
	// through a TLS-intercepting proxy. Pin its identity.
	if !hmac.Equal(resp.MAC, responseMAC(macKey, ch.Nonce, &resp)) {
		return Activation{}, errors.New("fleetsync: central's activation response failed authentication; not pinning it")
	}
	if resp.NodeID != c.NodeID || resp.PairingWords != c.PairingWords() {
		return Activation{}, errors.New("fleetsync: central's activation response does not match this Pi")
	}
	if _, err := hex.DecodeString(resp.CentralEventPub); err != nil || len(resp.CentralEventPub) != 64 {
		return Activation{}, errors.New("fleetsync: central sent a malformed key")
	}
	for k, v := range map[string]string{
		ConfigRole: "node", ConfigCentralURL: c.BaseURL, ConfigCentralNodeID: resp.CentralNodeID,
		ConfigCentralEventPub: resp.CentralEventPub, ConfigActivation: resp.Status,
	} {
		if err := c.Store.SetConfig(ctx, k, v); err != nil {
			return Activation{}, err
		}
	}
	return Activation{PairingWords: resp.PairingWords, CentralNodeID: resp.CentralNodeID}, nil
}

// ActivationStatus asks central whether a super user has confirmed this
// Pi yet, and records the answer.
func (c *Client) ActivationStatus(ctx context.Context) (string, error) {
	var s StatusResponse
	if _, _, err := c.do(ctx, http.MethodGet, PathStatus, nil, &s, true); err != nil {
		return "", err
	}
	return s.Status, c.Store.SetConfig(ctx, ConfigActivation, s.Status)
}

// --- sync ---

// Report summarises one sync.
type Report struct {
	Pushed         int
	Flagged        int
	SnapshotLoaded bool
	Purged         int64
	ClockVerified  bool
	Uploaded       int
}

// Sync pushes this node's unsynced events, then pulls a fresh working
// set if central's state changed, then purges what central holds durably.
func (c *Client) Sync(ctx context.Context) (Report, error) {
	var rep Report
	centralPub, err := c.centralPub(ctx)
	if err != nil {
		return rep, err
	}
	headSeq, headHash, err := c.Store.Head(ctx, c.ChainID)
	if err != nil {
		return rep, err
	}
	sent := c.now()
	var hello HelloResponse
	if _, _, err := c.do(ctx, http.MethodPost, PathHello, HelloRequest{ChainID: c.ChainID, HeadSeq: headSeq, SoftwareVersion: c.Version}, &hello, true); err != nil {
		return rep, err
	}
	rep.ClockVerified, err = c.checkClock(ctx, sent, hello.ServerTime)
	if err != nil {
		return rep, err
	}
	if hello.Quarantined {
		return rep, ErrQuarantined
	}
	if hello.Status != "active" && !(hello.Wipe && hello.KeepUnsynced) {
		if hello.Wipe {
			return rep, c.wipe()
		}
		return rep, ErrNotActivated
	}

	// Where does central stand on our chain?
	switch {
	case hello.CentralSeq > headSeq:
		return rep, ErrCentralAhead
	case hello.CentralSeq > 0:
		ours, err := c.hashAt(ctx, hello.CentralSeq, headSeq, headHash)
		if err != nil {
			return rep, err
		}
		if ours != "" && ours != hello.CentralHash {
			return rep, ErrForked
		}
	}

	if rep.Pushed, rep.Flagged, err = c.push(ctx, hello.CentralSeq, headSeq); err != nil {
		return rep, err
	}
	if rep.Uploaded, err = c.uploadBlobs(ctx); err != nil {
		return rep, err
	}
	if hello.Wipe {
		return rep, c.wipe() // revoked: unsynced work delivered, now wipe
	}

	last, _ := c.Store.Config(ctx, ConfigStateVersion)
	if rep.Pushed > 0 || last != fmt.Sprint(hello.StateVersion) {
		if err := c.pullSnapshot(ctx, centralPub); err != nil {
			return rep, err
		}
		rep.SnapshotLoaded = true
		if err := c.cleanBlobs(ctx); err != nil {
			return rep, err
		}
	}
	if rep.Purged, err = c.purge(ctx, hello.DurableSeq); err != nil {
		return rep, err
	}
	return rep, c.Store.SetConfig(ctx, ConfigLastSync, c.now().UTC().Format(time.RFC3339))
}

func (c *Client) centralPub(ctx context.Context) (ed25519.PublicKey, error) {
	v, err := c.Store.Config(ctx, ConfigCentralEventPub)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotActivated
	}
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(v)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("fleetsync: stored central key is corrupt")
	}
	return b, nil
}

// hashAt returns our hash at seq, or "" if that event was purged.
func (c *Client) hashAt(ctx context.Context, seq, headSeq int64, headHash event.Hash) (string, error) {
	if seq == headSeq {
		return headHash.String(), nil
	}
	evs, err := c.Store.Chain(ctx, c.ChainID, seq, 1)
	if err != nil {
		return "", err
	}
	if len(evs) == 0 || evs[0].Seq != seq {
		return "", nil
	}
	return evs[0].Hash.String(), nil
}

func (c *Client) push(ctx context.Context, from, head int64) (pushed, flagged int, err error) {
	for next := from + 1; next <= head; {
		evs, err := c.Store.Chain(ctx, c.ChainID, next, pushBatch)
		if err != nil {
			return pushed, flagged, err
		}
		if len(evs) == 0 || evs[0].Seq != next {
			return pushed, flagged, fmt.Errorf("fleetsync: event %d to push is missing locally", next)
		}
		req := EventsRequest{}
		for _, e := range evs {
			req.Events = append(req.Events, ToWire(e))
		}
		var resp EventsResponse
		if _, _, err := c.do(ctx, http.MethodPost, PathEvents, req, &resp, true); err != nil {
			return pushed, flagged, err
		}
		for _, r := range resp.Results {
			if len(r.Flags) > 0 {
				flagged++
			}
		}
		pushed += int(resp.AcceptedThrough - next + 1)
		if resp.Error != "" {
			if strings.HasPrefix(resp.Error, "fork") {
				return pushed, flagged, ErrForked
			}
			return pushed, flagged, fmt.Errorf("fleetsync: central refused seq %d: %s", resp.AcceptedThrough+1, resp.Error)
		}
		next = resp.AcceptedThrough + 1
	}
	return pushed, flagged, nil
}

func (c *Client) pullSnapshot(ctx context.Context, centralPub ed25519.PublicKey) error {
	resp, body, err := c.do(ctx, http.MethodGet, PathSnapshot+"?chain="+c.ChainID, nil, nil, true)
	if err != nil {
		return err
	}
	sig, err := hex.DecodeString(resp.Header.Get(SnapshotSignatureHeader))
	if err != nil {
		return ErrBadSnapshot
	}
	snap, err := ApplySnapshot(ctx, c.Store, body, sig, centralPub, c.NodeID)
	if err != nil {
		return err
	}
	if err := c.Store.SetConfig(ctx, ConfigAckedSeq, fmt.Sprint(snap.AckedSeq)); err != nil {
		return err
	}
	return c.Store.SetConfig(ctx, ConfigStateVersion, fmt.Sprint(snap.StateVersion))
}

// checkClock compares this Pi's clock with central's (DESIGN.md §3.4). A
// clock within tolerance counts as verified for the next 24 hours.
func (c *Client) checkClock(ctx context.Context, sent time.Time, serverTime string) (bool, error) {
	st, err := time.Parse(time.RFC3339Nano, serverTime)
	if err != nil {
		return false, nil
	}
	mid := sent.Add(c.now().Sub(sent) / 2)
	if d := st.Sub(mid); d > clockTolerance || d < -clockTolerance {
		return false, nil
	}
	return true, c.Store.SetConfig(ctx, ConfigClockVerifiedAt, c.now().UTC().Format(time.RFC3339))
}

// ClockState reports whether this node's clock was verified against
// central within the last 24 hours.
func ClockState(ctx context.Context, st *store.Store, now time.Time) event.ClockState {
	v, err := st.Config(ctx, ConfigClockVerifiedAt)
	if err != nil {
		return event.ClockUnverified
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil || now.Sub(t) > 24*time.Hour || t.After(now.Add(clockTolerance)) {
		return event.ClockUnverified
	}
	return event.ClockVerified
}

// purge deletes own events that are both older than the retention period
// and at or below central's durable watermark (DESIGN.md §5.7).
func (c *Client) purge(ctx context.Context, durable int64) (int64, error) {
	if durable <= 0 {
		return 0, nil
	}
	cutoff := c.now().Add(-Retention).UTC().Format(event.WallTimeLayout)
	var upto int64
	if err := c.Store.DB().QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM events
		WHERE chain_id = ? AND seq <= ? AND wall_time < ?
		  AND seq < (SELECT max(seq) FROM events WHERE chain_id = ?)`, c.ChainID, durable, cutoff, c.ChainID).Scan(&upto); err != nil {
		return 0, err
	}
	if upto == 0 {
		return 0, nil
	}
	var before int64
	c.Store.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE chain_id = ? AND seq <= ?`, c.ChainID, upto).Scan(&before)
	return before, c.Store.Purge(ctx, c.ChainID, upto)
}

func (c *Client) wipe() error {
	if c.Wipe == nil {
		return errors.New("fleetsync: this Pi was revoked but no wipe function is configured")
	}
	if err := c.Wipe(); err != nil {
		return fmt.Errorf("fleetsync: revoked, but wiping failed: %w", err)
	}
	return ErrRevoked
}
