package web

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/fleetsync"
)

// --- inventory ---

func (s *Server) inventory(w http.ResponseWriter, r *http.Request, sess *session) error {
	d, err := loadInventory(r.Context(), s.App.Store.DB())
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "inventory", "Inventory", d)
}

func (s *Server) stockTxn(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	t := domain.StockTxnRecorded{
		Kind: f("kind"), PartID: f("part"), StockLocationID: f("location"), ToLocationID: f("to_location"),
		WorkOrderID: strings.TrimSpace(f("work_order")), Reason: strings.TrimSpace(f("reason")),
	}
	n, err := strconv.ParseInt(strings.TrimSpace(f("quantity")), 10, 64)
	if err != nil {
		return s.done(w, r, sess, "/inventory", "Not saved: quantity must be a whole number.")
	}
	if t.Kind == domain.StockCount {
		t.ObservedQty = &n
	} else {
		t.Quantity = n
	}
	if t.Kind != domain.StockTransfer {
		t.ToLocationID = ""
	}
	if err := s.phiCheck(r, t.Reason); err != nil {
		return s.failed(w, r, sess, "/inventory", err)
	}
	if t.Kind == domain.StockCount && s.Fleet != nil {
		var owner string
		s.App.Store.DB().QueryRowContext(r.Context(), `SELECT owner_user_id FROM stock_locations WHERE id = ?`, t.StockLocationID).Scan(&owner)
		if owner == "" { // a shared stockroom: count where the full ledger is
			msg, _ := s.countShared(r, t.PartID, t.StockLocationID, n)
			return s.done(w, r, sess, "/inventory", msg)
		}
	}
	if _, err := s.App.RecordStock(r.Context(), s.actor(sess), t); err != nil {
		return s.failed(w, r, sess, "/inventory", err)
	}
	return s.done(w, r, sess, "/inventory", "Recorded.")
}

func (s *Server) partCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	if _, err := s.App.CreatePart(r.Context(), s.actor(sess), domain.PartCreated{
		PartNo: strings.TrimSpace(f("part_no")), Description: strings.TrimSpace(f("description")), Unit: strings.TrimSpace(f("unit")),
	}); err != nil {
		return s.failed(w, r, sess, "/inventory", err)
	}
	return s.done(w, r, sess, "/inventory", "Part added.")
}

func (s *Server) stockLocationCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	l := domain.StockLocationCreated{SiteID: r.PostFormValue("site"), Name: strings.TrimSpace(r.PostFormValue("name"))}
	if r.PostFormValue("personal") == "yes" {
		l.OwnerUserID = sess.User.ID
	}
	if _, err := s.App.CreateStockLocation(r.Context(), s.actor(sess), l); err != nil {
		return s.failed(w, r, sess, "/inventory", err)
	}
	return s.done(w, r, sess, "/inventory", "Stock location added.")
}

// --- schedules ---

type schedulesData struct {
	Rows       []scheduleRow
	Assets     []assetRow
	Procedures []option
	Usage      map[string]usageRow
}

func (s *Server) scheduleList(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	rows, err := listSchedules(ctx, q, "")
	if err != nil {
		return err
	}
	assets, err := listAssets(ctx, q, "")
	if err != nil {
		return err
	}
	procs, err := procedureOptions(r, s)
	if err != nil {
		return err
	}
	usage, err := usageSchedules(r.Context(), s.App.Store.DB(), "", false)
	if err != nil {
		return err
	}
	byID := map[string]usageRow{}
	for _, u := range usage {
		byID[u.Schedule.ID] = u
	}
	return s.render(w, r, sess, "schedules", "PM schedules", schedulesData{Rows: rows, Assets: assets, Procedures: procs, Usage: byID})
}

func (s *Server) scheduleCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	interval, _ := strconv.Atoi(f("interval"))
	grace, _ := strconv.Atoi(f("grace"))
	if _, err := s.App.CreateSchedule(r.Context(), s.actor(sess), domain.PMScheduleCreated{
		AssetID: f("asset"), WOType: f("type"), Title: strings.TrimSpace(f("title")), Procedure: strings.TrimSpace(f("procedure")),
		IntervalDays: interval, GraceDays: grace, FirstDue: f("first_due"), ProcedureID: f("procedure_id"),
		Meter: strings.ToLower(strings.TrimSpace(f("meter"))), MeterInterval: strings.TrimSpace(f("meter_interval")),
		MeterLead: strings.TrimSpace(f("meter_lead")),
	}); err != nil {
		return s.failed(w, r, sess, "/schedules", err)
	}
	return s.done(w, r, sess, "/schedules", "Schedule created.")
}

// --- review queue ---

type reviewData struct {
	All        bool
	Rows       []flagRow
	Duplicates []domain.DuplicateGroup
}

func (s *Server) review(w http.ResponseWriter, r *http.Request, sess *session) error {
	all := r.URL.Query().Get("all") == "1"
	rows, err := listFlags(r.Context(), s.App.Store.DB(), all)
	if err != nil {
		return err
	}
	dups, err := domain.Duplicates(r.Context(), s.App.Store.DB(), "")
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "review", "Review queue", reviewData{All: all, Rows: rows, Duplicates: dups})
}

// --- users ---

type usersData struct {
	Users []userRow
	Roles []string
	Sites []option
}

func (s *Server) userList(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	users, err := listUsers(ctx, q)
	if err != nil {
		return err
	}
	sites, err := siteOptions(ctx, q)
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "users", "Users", usersData{Users: users, Roles: []string{domain.RoleUser, domain.RoleMidTier, domain.RoleSuperUser}, Sites: sites})
}

type oneTimeData struct{ Username, Password, What string }

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	u := app.NewUser{
		Username: strings.TrimSpace(f("username")), LegalName: strings.TrimSpace(f("legal_name")), Email: strings.TrimSpace(f("email")),
		Role: f("role"), HomeSites: r.PostForm["home_sites"], IdentityVerification: strings.TrimSpace(f("verified")),
	}
	_, pw, err := s.App.CreateUser(r.Context(), s.actor(sess), u)
	if err != nil {
		return s.failed(w, r, sess, "/admin/users", err)
	}
	// Shown once, in this response only (Cache-Control: no-store).
	return s.render(w, r, sess, "onetime", "One-time password", oneTimeData{Username: u.Username, Password: pw, What: "created"})
}

func (s *Server) userRole(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.ChangeRole(r.Context(), s.actor(sess), r.PathValue("id"), r.PostFormValue("role")); err != nil {
		return s.failed(w, r, sess, "/admin/users", err)
	}
	return s.done(w, r, sess, "/admin/users", "Role changed.")
}

func (s *Server) userDisable(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.DisableUser(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, "/admin/users", err)
	}
	return s.done(w, r, sess, "/admin/users", "User disabled. Revoke their Pi from the Pis page.")
}

func (s *Server) userReset(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx := r.Context()
	u, err := domain.GetUser(ctx, s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	pw, err := s.App.ResetPassword(ctx, s.actor(sess), u.ID, strings.TrimSpace(r.PostFormValue("verified")))
	if err != nil {
		return s.failed(w, r, sess, "/admin/users", err)
	}
	return s.render(w, r, sess, "onetime", "One-time password", oneTimeData{Username: u.Username, Password: pw, What: "reset"})
}

func (s *Server) userUnlock(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.UnlockUser(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, "/admin/users", err)
	}
	return s.done(w, r, sess, "/admin/users", "Unlocked.")
}

// --- sites ---

type sitesData struct {
	Pending []domain.ProposedLocation
	Sites   []siteRow
	Kinds   []string
	Parents []option // sites another can be put inside
}

func (s *Server) siteList(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	d := sitesData{Kinds: locationKinds}
	var err error
	if d.Pending, err = domain.PendingLocations(ctx, q); err != nil {
		return err
	}
	if d.Sites, err = listSites(ctx, q); err != nil {
		return err
	}
	if d.Parents, err = siteOptions(ctx, q); err != nil {
		return err
	}
	return s.render(w, r, sess, "sites", "Sites and locations", d)
}

// locationReview approves or rejects a location an employee proposed.
func (s *Server) locationReview(w http.ResponseWriter, r *http.Request, sess *session) error {
	approve := r.PostFormValue("decision") == "approve"
	if err := s.App.ReviewLocation(r.Context(), s.actor(sess), r.PathValue("id"), approve, strings.TrimSpace(r.PostFormValue("note"))); err != nil {
		return s.failed(w, r, sess, "/admin/sites#review", err)
	}
	if approve {
		return s.done(w, r, sess, "/admin/sites#review", "Location approved.")
	}
	return s.done(w, r, sess, "/admin/sites#review", "Location rejected. Its equipment is now at the Unallocated site.")
}

func (s *Server) siteCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	if _, err := s.App.CreateSiteIn(r.Context(), s.actor(sess), f("parent"), strings.ToUpper(strings.TrimSpace(f("code"))), strings.TrimSpace(f("name")), strings.TrimSpace(f("timezone"))); err != nil {
		return s.failed(w, r, sess, "/admin/sites", err)
	}
	return s.done(w, r, sess, "/admin/sites", "Site created.")
}

// siteMove puts a site inside another, or at the top level.
func (s *Server) siteMove(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.MoveSite(r.Context(), s.actor(sess), r.PathValue("id"), r.PostFormValue("parent")); err != nil {
		return s.failed(w, r, sess, "/admin/sites", err)
	}
	return s.done(w, r, sess, "/admin/sites", "Site moved. Its equipment now also belongs to the sites above it.")
}

func (s *Server) locationCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	if _, err := s.App.CreateLocation(r.Context(), s.actor(sess), f("site"), f("parent"), strings.TrimSpace(f("name")), strings.TrimSpace(f("kind"))); err != nil {
		return s.failed(w, r, sess, "/admin/sites", err)
	}
	return s.done(w, r, sess, "/admin/sites", "Location created.")
}

// --- Pis (central only) ---

type nodeRow struct {
	domain.Node
	Username    string
	Quarantined bool
	Version     string // as it last reported
	Outdated    bool
}

type nodesData struct {
	Rows        []nodeRow
	Fingerprint string
	Need        fleetsync.Requirement
	Master      string // this master Pi's version
	DefaultFrom string // suggested date a new requirement starts
}

func (s *Server) nodeList(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	nodes, err := domain.ListNodes(ctx, q, "")
	if err != nil {
		return err
	}
	rows := make([]nodeRow, len(nodes))
	for i, n := range nodes {
		rows[i].Node = n
		if u, err := domain.GetUser(ctx, q, n.BoundUserID); err == nil {
			rows[i].Username = u.LegalName + " (" + u.Username + ")"
		}
		rows[i].Quarantined = s.App.Quarantined(ctx, n.ID)
	}
	need := fleetsync.NodeRequirement(ctx, s.App.Store, s.now())
	for i := range rows {
		rows[i].Version = fleetsync.NodeVersion(ctx, s.App.Store, rows[i].ID)
		rows[i].Outdated = need.Outdated(rows[i].Version)
	}
	return s.render(w, r, sess, "nodes", "Pis", nodesData{Rows: rows, Fingerprint: s.CertFingerprint, Need: need, Master: s.Version,
		DefaultFrom: s.now().AddDate(0, 0, 7).Format("2006-01-02")})
}

// nodeRequire sets the pi-fleet version employee Pis need, from a date,
// or with "clear" goes back to the built-in minimum.
func (s *Server) nodeRequire(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, f := r.Context(), r.PostFormValue
	if f("clear") == "yes" {
		if err := fleetsync.RequireVersion(ctx, s.App.Store, "", time.Time{}, s.Version); err != nil {
			return err
		}
		log.Printf("web: %s cleared the required employee Pi version", sess.User.Username)
		return s.done(w, r, sess, "/admin/nodes", "Employee Pis now need only the built-in minimum version.")
	}
	from, err := time.ParseInLocation("2006-01-02", f("from"), time.Local)
	if err != nil {
		return s.done(w, r, sess, "/admin/nodes", "Not saved: give the date the requirement starts.")
	}
	version := strings.TrimSpace(f("version"))
	if err := fleetsync.RequireVersion(ctx, s.App.Store, version, from, s.Version); err != nil {
		return s.done(w, r, sess, "/admin/nodes", "Not saved: "+err.Error()+".")
	}
	log.Printf("web: %s required employee Pis to run %s from %s", sess.User.Username, version, from.Format("2006-01-02"))
	return s.done(w, r, sess, "/admin/nodes", fmt.Sprintf("Employee Pis need pi-fleet %s from %s.", version, from.Format("2 January 2006")))
}

func (s *Server) nodeConfirm(w http.ResponseWriter, r *http.Request, sess *session) error {
	words := strings.Join(strings.Fields(strings.ToLower(r.PostFormValue("words"))), " ")
	if err := s.App.ConfirmNode(r.Context(), s.actor(sess), r.PathValue("id"), words); err != nil {
		return s.failed(w, r, sess, "/admin/nodes", err)
	}
	return s.done(w, r, sess, "/admin/nodes", "Pi confirmed. It can sync now.")
}

func (s *Server) nodeReject(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.RejectNode(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, "/admin/nodes", err)
	}
	return s.done(w, r, sess, "/admin/nodes", "Pi rejected.")
}

func (s *Server) nodeRevoke(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.RevokeNode(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason")), r.PostFormValue("keep") == "yes"); err != nil {
		return s.failed(w, r, sess, "/admin/nodes", err)
	}
	return s.done(w, r, sess, "/admin/nodes", "Pi revoked. It will wipe itself when it next connects.")
}
