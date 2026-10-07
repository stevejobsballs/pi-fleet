package fleetsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/release"
	"pi-fleet/internal/store"
)

// MinNodeVersion is the oldest employee Pi this master Pi can sync with
// (DESIGN.md §9.3). Raise it whenever a release changes what a Pi must
// understand: snapshot tables or columns (a Pi refuses a snapshot with
// columns it doesn't know), or event types and payload fields.
// v0.4.0 added the assets' merge columns and work orders' opened_at.
const MinNodeVersion = "v0.4.0"

const (
	// On the master Pi: a newer version a super user requires, and when.
	ConfigRequireVersion = "require_node_version"
	ConfigRequireFrom    = "require_node_version_from" // RFC 3339
	// On the master Pi, + node id: the version a Pi last reported.
	configNodeVersion = "node_version:"
	// On an employee Pi: the version the master needs now ("" when this Pi
	// is new enough), and a coming requirement ("vX.Y.Z RFC3339").
	ConfigUpdateNeeded = "update_needed"
	ConfigUpdateDue    = "update_due"
)

// ErrTooOld means this Pi must be updated before it can sync again.
var ErrTooOld = errors.New("fleetsync: this Pi must be updated before it can sync")

var releaseRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// tooOld reports whether a Pi running have is below need. Development
// builds (not vX.Y.Z) are never blocked.
func tooOld(have, need string) bool {
	return releaseRE.MatchString(have) && releaseRE.MatchString(need) && release.Compare(have, need) < 0
}

// Requirement is what the master Pi asks of employee Pis.
type Requirement struct {
	Min      string // required now
	Next     string // required from NextFrom, if set
	NextFrom time.Time
}

// NodeRequirement combines the built-in minimum with any version a super
// user requires, which applies from its date (the grace period before
// that lets Pis update without losing a day's syncing).
func NodeRequirement(ctx context.Context, st *store.Store, now time.Time) Requirement {
	r := Requirement{Min: MinNodeVersion}
	v, _ := st.Config(ctx, ConfigRequireVersion)
	f, _ := st.Config(ctx, ConfigRequireFrom)
	from, err := time.Parse(time.RFC3339, f)
	if !releaseRE.MatchString(v) || err != nil || release.Compare(v, r.Min) <= 0 {
		return r
	}
	if now.Before(from) {
		r.Next, r.NextFrom = v, from
	} else {
		r.Min = v
	}
	return r
}

// RequireVersion sets (or with version "" clears) the version employee
// Pis need from a date. It can't be newer than the master Pi itself,
// whose release folder the Pis update from.
func RequireVersion(ctx context.Context, st *store.Store, version string, from time.Time, master string) error {
	if version == "" {
		if err := st.SetConfig(ctx, ConfigRequireVersion, ""); err != nil {
			return err
		}
		return st.SetConfig(ctx, ConfigRequireFrom, "")
	}
	if !releaseRE.MatchString(version) {
		return fmt.Errorf("%q isn't a release version (like v1.2.3)", version)
	}
	if releaseRE.MatchString(master) && release.Compare(version, master) > 0 {
		return fmt.Errorf("the master Pi runs %s; update it before requiring %s of employee Pis", master, version)
	}
	if err := st.SetConfig(ctx, ConfigRequireVersion, version); err != nil {
		return err
	}
	return st.SetConfig(ctx, ConfigRequireFrom, from.UTC().Format(time.RFC3339))
}

// NodeVersion is the version a Pi reported when it last said hello.
func NodeVersion(ctx context.Context, st *store.Store, nodeID string) string {
	v, _ := st.Config(ctx, configNodeVersion+nodeID)
	return v
}

// Outdated reports whether a Pi's reported version is below the
// current requirement.
func (r Requirement) Outdated(version string) bool { return tooOld(version, r.Min) }

// current refuses records from and to a Pi that is too old, even if it
// skipped hello (an old Pi doesn't know to stop).
func (s *Server) current(h signedHandler) signedHandler {
	return func(w http.ResponseWriter, r *http.Request, n domain.Node, body []byte) error {
		have := NodeVersion(r.Context(), s.App.Store, n.ID)
		if need := NodeRequirement(r.Context(), s.App.Store, s.now()).Min; tooOld(have, need) {
			return fail(http.StatusUpgradeRequired, "this Pi runs pi-fleet %s; the master Pi needs %s or newer. Update this Pi (Update-Pi, or the newest release's setup)", have, need)
		}
		return h(w, r, n, body)
	}
}
