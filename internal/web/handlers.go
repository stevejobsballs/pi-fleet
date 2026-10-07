package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

// --- sign-in ---

type loginData struct{ Error, Username string }

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if sess, _ := s.loadSession(r); sess != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := s.render(w, r, nil, "login", "Sign in", loginData{}); err != nil {
		s.fail(w, r, err)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	username, pw := strings.TrimSpace(r.PostFormValue("username")), r.PostFormValue("password")
	u, err := s.App.Authenticate(r.Context(), username, pw)
	switch {
	case err == nil:
		err = s.startSession(w, r, u, false)
		if err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	case errors.Is(err, app.ErrMustChangePassword):
		err = s.startSession(w, r, u, true)
		if err == nil {
			http.Redirect(w, r, "/password", http.StatusSeeOther)
			return
		}
	}
	if msg := message(err); msg != "" {
		w.WriteHeader(http.StatusUnauthorized)
		if err := s.render(w, r, nil, "login", "Sign in", loginData{Error: msg, Username: username}); err != nil {
			s.fail(w, r, err)
		}
		return
	}
	s.fail(w, r, err)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.exec(r.Context(), `DELETE FROM sessions WHERE id_hash = ?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type passwordData struct {
	Forced  bool
	Expires time.Time
	Error   string
}

func (s *Server) passwordPage(w http.ResponseWriter, r *http.Request, sess *session) error {
	return s.render(w, r, sess, "password", "Change password", passwordData{Forced: sess.PasswordOnly, Expires: sess.User.PasswordExpiresAt})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, sess *session) error {
	next := r.PostFormValue("new")
	var err error
	if next != r.PostFormValue("confirm") {
		err = userErr("the new passwords do not match")
	} else {
		err = s.App.ChangePassword(r.Context(), s.actor(sess), r.PostFormValue("current"), next)
	}
	if err != nil {
		msg := message(err)
		if msg == "" {
			return err
		}
		return s.render(w, r, sess, "password", "Change password", passwordData{Forced: sess.PasswordOnly, Error: msg})
	}
	if err := s.exec(r.Context(), `UPDATE sessions SET password_only = 0 WHERE id_hash = ?`, sess.IDHash); err != nil {
		return err
	}
	return s.done(w, r, sess, "/", "Password changed. It expires in 31 days.")
}

// --- dashboard ---

type dashboardData struct {
	Mine       []woRow
	Due        []dueRow
	UsageDue   []usageRow
	Unassigned int
	Flags      int
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	var d dashboardData
	var err error
	if d.Mine, err = listWorkOrders(ctx, q, "mine", sess.User.ID); err != nil {
		return err
	}
	if d.Due, err = dueSoon(ctx, q, s.now()); err != nil {
		return err
	}
	if d.UsageDue, err = usageSchedules(ctx, q, "", true); err != nil {
		return err
	}
	q.QueryRowContext(ctx, `SELECT count(*) FROM work_orders WHERE status = 'open'`).Scan(&d.Unassigned)
	q.QueryRowContext(ctx, `SELECT count(*) FROM event_flags f WHERE NOT EXISTS (SELECT 1 FROM flag_resolutions r WHERE r.event_id = f.event_id)`).Scan(&d.Flags)
	return s.render(w, r, sess, "dashboard", "Today", d)
}

// --- assets ---

type assetListData struct {
	Search string
	Assets []assetRow
}

func (s *Server) assetList(w http.ResponseWriter, r *http.Request, sess *session) error {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, err := listAssets(r.Context(), s.App.Store.DB(), q)
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "assets", "Equipment", assetListData{Search: q, Assets: rows})
}

func (s *Server) assetNew(w http.ResponseWriter, r *http.Request, sess *session) error {
	locs, err := locationOptions(r.Context(), s.App.Store.DB())
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "asset_new", "Register equipment", locs)
}

func (s *Server) assetCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	id, err := s.App.RegisterAsset(r.Context(), s.actor(sess), domain.AssetRegistered{
		Tag: strings.TrimSpace(f("tag")), LocationID: f("location"), Manufacturer: strings.TrimSpace(f("manufacturer")),
		Model: strings.TrimSpace(f("model")), Serial: strings.TrimSpace(f("serial")), RiskClass: f("risk_class"),
		IsReferenceStandard: f("reference") == "yes", MasterID: strings.TrimSpace(f("master_id")),
	})
	if err != nil {
		return s.failed(w, r, sess, "/assets/new", err)
	}
	return s.done(w, r, sess, "/assets/"+id, "Equipment registered.")
}

type assetData struct {
	Asset        domain.Asset
	Site         siteRow
	Location     string
	Schedules    []scheduleRow
	WorkOrders   []woRow
	Calibrations []calRow
	Locations    []option
	Statuses     []string
	Attachments  []domain.Attachment
	Meters       []domain.Meter
	Readings     []domain.MeterReadingRow
	Usage        []usageRow
	Now          string
	// Merging duplicate records (same MasterID).
	MergedInto option   // the record this one was merged into
	Merged     []option // records merged into this one
	Duplicates []option // other unmerged records with the same MasterID
}

func (s *Server) assetView(w http.ResponseWriter, r *http.Request, sess *session) error {
	d, err := s.loadAsset(r, r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "asset", d.Asset.Tag, d)
}

func (s *Server) loadAsset(r *http.Request, id string) (assetData, error) {
	ctx, q := r.Context(), s.App.Store.DB()
	a, err := domain.GetAsset(ctx, q, id)
	if err != nil {
		return assetData{}, err
	}
	d := assetData{Asset: a, Statuses: []string{domain.AssetInService, domain.AssetOutOfService, domain.AssetMissing, domain.AssetRetired}}
	q.QueryRowContext(ctx, `SELECT id, code, name, timezone FROM sites WHERE id = ?`, a.SiteID).Scan(&d.Site.ID, &d.Site.Code, &d.Site.Name, &d.Site.Timezone)
	q.QueryRowContext(ctx, `SELECT name FROM locations WHERE id = ?`, a.LocationID).Scan(&d.Location)
	// The history of records merged into this one shows here too.
	group, err := domain.AssetGroup(ctx, q, a.ID)
	if err != nil {
		return d, err
	}
	groupJSON, _ := json.Marshal(group)
	for _, id := range group[1:] {
		var o option
		q.QueryRowContext(ctx, `SELECT id, tag FROM assets WHERE id = ?`, id).Scan(&o.ID, &o.Label)
		d.Merged = append(d.Merged, o)
	}
	if a.MergedInto != "" {
		d.MergedInto.ID = a.MergedInto
		q.QueryRowContext(ctx, `SELECT tag FROM assets WHERE id = ?`, a.MergedInto).Scan(&d.MergedInto.Label)
	} else if a.MasterID != "" {
		rows, err := q.QueryContext(ctx, `SELECT id, tag FROM assets WHERE master_id = ? AND merged_into = '' AND id != ? ORDER BY tag`, a.MasterID, a.ID)
		if d.Duplicates, err = scanAll(rows, err, func(r *sql.Rows) (option, error) {
			var o option
			return o, r.Scan(&o.ID, &o.Label)
		}); err != nil {
			return d, err
		}
	}
	for _, id := range group {
		sched, err := listSchedules(ctx, q, id)
		if err != nil {
			return d, err
		}
		d.Schedules = append(d.Schedules, sched...)
	}
	rows, err := q.QueryContext(ctx, `SELECT w.id, w.number, w.type, w.title, w.status, w.priority, w.due_at, a.tag,
			coalesce((SELECT legal_name FROM users u WHERE u.id = w.assigned_to), '')
		FROM work_orders w JOIN assets a ON a.id = w.asset_id
		WHERE w.asset_id IN (SELECT value FROM json_each(?)) ORDER BY w.number DESC LIMIT 50`, string(groupJSON))
	if d.WorkOrders, err = scanAll(rows, err, func(r *sql.Rows) (woRow, error) {
		var x woRow
		return x, r.Scan(&x.ID, &x.Number, &x.Type, &x.Title, &x.Status, &x.Priority, &x.DueAt, &x.Asset, &x.AssignedTo)
	}); err != nil {
		return d, err
	}
	if d.Calibrations, err = calibrations(ctx, q, "c.asset_id IN (SELECT value FROM json_each(?))", string(groupJSON)); err != nil {
		return d, err
	}
	for _, id := range group {
		files, err := domain.ListAttachments(ctx, q, domain.EntityAsset, id)
		if err != nil {
			return d, err
		}
		d.Attachments = append(d.Attachments, files...)
	}
	if d.Meters, err = domain.AssetMeters(ctx, q, a.ID); err != nil {
		return d, err
	}
	if d.Readings, err = domain.RecentMeterReadings(ctx, q, a.ID, 20); err != nil {
		return d, err
	}
	if d.Usage, err = usageSchedules(ctx, q, a.ID, false); err != nil {
		return d, err
	}
	if loc, lerr := time.LoadLocation(d.Site.Timezone); lerr == nil {
		d.Now = s.now().In(loc).Format("2006-01-02T15:04")
	}
	d.Locations, err = locationOptions(ctx, q)
	return d, err
}

func formInt(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PostFormValue(name), 10, 64)
	return n
}

func (s *Server) assetStatus(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/assets/" + id
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if err := s.phiCheck(r, reason); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if err := s.App.SetAssetStatus(r.Context(), s.actor(sess), id, formInt(r, "version"), r.PostFormValue("status"), reason); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Status changed.")
}

func (s *Server) assetRelocate(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/assets/" + id
	if err := s.App.RelocateAsset(r.Context(), s.actor(sess), id, formInt(r, "version"), r.PostFormValue("location")); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Moved.")
}

// --- work orders ---

type woListData struct {
	View  string
	Views []string
	Rows  []woRow
}

func (s *Server) workOrderList(w http.ResponseWriter, r *http.Request, sess *session) error {
	view := r.URL.Query().Get("view")
	if view == "" {
		view = "mine"
	}
	rows, err := listWorkOrders(r.Context(), s.App.Store.DB(), view, sess.User.ID)
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "work_orders", "Work orders", woListData{View: view, Views: []string{"mine", "unassigned", "open", "review", "all"}, Rows: rows})
}

type woNewData struct {
	Assets     []assetRow
	Asset      string
	Types      []string
	Procedures []option
}

func (s *Server) workOrderNew(w http.ResponseWriter, r *http.Request, sess *session) error {
	assets, err := listAssets(r.Context(), s.App.Store.DB(), "")
	if err != nil {
		return err
	}
	types := []string{"corrective", "inspection"}
	if roleRank[sess.User.Role] >= roleRank[domain.RoleMidTier] {
		types = append(types, "pm", "calibration", "install", "retire")
	}
	procs, err := procedureOptions(r, s)
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "work_order_new", "New work order", woNewData{Assets: assets, Asset: r.URL.Query().Get("asset"), Types: types, Procedures: procs})
}

func (s *Server) workOrderCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	title, problem := strings.TrimSpace(f("title")), strings.TrimSpace(f("problem"))
	if err := s.phiCheck(r, title, problem); err != nil {
		return s.failed(w, r, sess, "/work-orders/new?asset="+f("asset"), err)
	}
	id, number, err := s.App.OpenWorkOrder(r.Context(), s.actor(sess), app.NewWorkOrder{
		Type: f("type"), AssetID: f("asset"), Priority: f("priority"), Title: title, Problem: problem, DueAt: f("due_at"),
		ProcedureID: f("procedure"),
	})
	if err != nil {
		return s.failed(w, r, sess, "/work-orders/new?asset="+f("asset"), err)
	}
	return s.done(w, r, sess, "/work-orders/"+id, "Opened "+number+".")
}

type woData struct {
	WO           domain.WorkOrder
	Asset        domain.Asset
	SiteTZ       string
	Assignee     string
	Holder       bool
	ContentHash  string
	Signatures   []domain.Signature
	Calibrations []calRow
	Standards    []option
	Users        []option
	Rows         int
	Attachments  []domain.Attachment
	CanAttach    bool
	Procedure    *domain.Procedure
	Steps        []domain.StepResult
	Procedures   []option
	Labor        []domain.LaborEntry
	LaborTotal   int
	Today        string
	// SignAs is the meaning the current user may sign with now, if any.
	SignAs    string
	Performer bool
}

func (s *Server) workOrderView(w http.ResponseWriter, r *http.Request, sess *session) error {
	d, err := s.loadWorkOrder(r, sess, r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "work_order", d.WO.Number, d)
}

func (s *Server) loadWorkOrder(r *http.Request, sess *session, id string) (woData, error) {
	ctx, q := r.Context(), s.App.Store.DB()
	wo, err := domain.GetWorkOrder(ctx, q, id)
	if err != nil {
		return woData{}, err
	}
	d := woData{WO: wo, Holder: wo.AssignedTo == sess.User.ID && wo.LeaseID != "", Performer: wo.AssignedTo == sess.User.ID}
	if d.Asset, err = domain.GetAsset(ctx, q, wo.AssetID); err != nil {
		return d, err
	}
	d.SiteTZ = siteTimezone(ctx, q, d.Asset.SiteID)
	if wo.AssignedTo != "" {
		if u, err := domain.GetUser(ctx, q, wo.AssignedTo); err == nil {
			d.Assignee = u.LegalName
		}
	}
	if d.ContentHash, err = domain.WorkOrderContentHash(ctx, q, wo.ID); err != nil {
		return d, err
	}
	if d.Signatures, err = domain.WorkOrderSignatures(ctx, q, wo.ID); err != nil {
		return d, err
	}
	if d.Calibrations, err = calibrations(ctx, q, "c.wo_id = ?", wo.ID); err != nil {
		return d, err
	}
	if d.Standards, err = standardOptions(ctx, q, wo.AssetID); err != nil {
		return d, err
	}
	if d.Attachments, err = domain.ListAttachments(ctx, q, domain.EntityWorkOrder, wo.ID); err != nil {
		return d, err
	}
	if d.Procedure, d.Steps, err = domain.Checklist(ctx, q, wo); err != nil {
		return d, err
	}
	if d.Labor, d.LaborTotal, err = domain.Labor(ctx, q, wo.ID); err != nil {
		return d, err
	}
	d.Today = today(s.now())
	isMid := roleRank[sess.User.Role] >= roleRank[domain.RoleMidTier]
	if isMid && d.Procedure == nil && (wo.Status == domain.WOOpen || wo.Status == domain.WOAssigned) {
		if d.Procedures, err = procedureOptions(r, s); err != nil {
			return d, err
		}
	}
	open := wo.Status != domain.WOClosed && wo.Status != domain.WOCancelled
	d.CanAttach = open && (isMid || (d.Holder && wo.Status == domain.WOInProgress))
	if isMid {
		if d.Users, err = userOptions(ctx, q); err != nil {
			return d, err
		}
	}
	switch {
	case wo.Status == domain.WOInProgress && d.Holder:
		d.SignAs = domain.MeaningPerformed
	case wo.Status == domain.WOCompleted && isMid && !d.Performer:
		d.SignAs = domain.MeaningReviewed
	case wo.Status == domain.WOReviewed && isMid:
		d.SignAs = domain.MeaningApproved
	}
	d.Rows, _ = strconv.Atoi(r.URL.Query().Get("rows"))
	d.Rows = min(max(d.Rows, 4), 50)
	return d, nil
}

func (s *Server) workOrderClaim(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	if _, err := s.App.ClaimWorkOrder(r.Context(), s.actor(sess), id); err != nil {
		return s.failed(w, r, sess, "/work-orders/"+id, err)
	}
	return s.done(w, r, sess, "/work-orders/"+id, "Claimed. If someone else claimed it first while offline, a mid-tier user will sort it out.")
}

func (s *Server) workOrderAssign(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	if _, err := s.App.AssignWorkOrder(r.Context(), s.actor(sess), id, r.PostFormValue("assignee")); err != nil {
		return s.failed(w, r, sess, "/work-orders/"+id, err)
	}
	return s.done(w, r, sess, "/work-orders/"+id, "Assigned.")
}

func (s *Server) workOrderStatus(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if err := s.phiCheck(r, reason); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if err := s.App.ChangeWorkOrderStatus(r.Context(), s.actor(sess), id, r.PostFormValue("to"), reason); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Status changed.")
}

func (s *Server) workOrderSign(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id
	err := s.App.Sign(r.Context(), s.actor(sess), id, r.PostFormValue("meaning"), r.PostFormValue("password"), r.PostFormValue("ack_clock") == "yes")
	if errors.Is(err, app.ErrClockUnverified) {
		return s.done(w, r, sess, back, "Not signed: this Pi's clock hasn't been checked against the master Pi recently. Tick the clock acknowledgement to sign anyway; the signature will record it.")
	}
	if err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Signed.")
}

func (s *Server) calibrationRecord(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id
	f := r.PostForm
	c := domain.CalibrationRecorded{
		WorkOrderID: id, Procedure: strings.TrimSpace(f.Get("procedure")), Temperature: strings.TrimSpace(f.Get("temperature")),
		Humidity: strings.TrimSpace(f.Get("humidity")), Adjusted: f.Get("adjusted") == "yes", StandardsUsed: f["standards"],
	}
	get := func(name string, i int) string {
		if v := f[name]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	for i := range f["parameter"] {
		if get("parameter", i) == "" {
			continue
		}
		kind := get("tol_kind", i)
		tol := domain.Tolerance{Kind: kind}
		if kind == domain.TolLimits {
			tol.Lower, tol.Upper = get("tol_lower", i), get("tol_upper", i)
		} else {
			tol.Value = get("tol_value", i)
		}
		c.Points = append(c.Points, domain.CalPoint{
			Parameter: get("parameter", i), Unit: get("unit", i), Nominal: get("nominal", i), Tolerance: tol,
			AsFound: get("as_found", i), AsLeft: get("as_left", i),
		})
	}
	if len(c.Points) == 0 {
		return s.done(w, r, sess, back, "Not saved: enter at least one measurement point.")
	}
	if _, err := s.App.RecordCalibration(r.Context(), s.actor(sess), c); err != nil {
		if msg := message(err); msg == "" {
			// Decimal and tolerance problems come back as plain errors.
			return s.done(w, r, sess, back, "Not saved: "+err.Error())
		}
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Calibration recorded.")
}

func (s *Server) calibrationVoid(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	var woID string
	if err := s.App.Store.DB().QueryRowContext(r.Context(), `SELECT wo_id FROM calibration_records WHERE id = ?`, id).Scan(&woID); err != nil {
		return domain.ErrNotFound
	}
	back := "/work-orders/" + woID
	if err := s.App.VoidCalibration(r.Context(), s.actor(sess), id, strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Calibration voided.")
}

func (s *Server) signatureWithdraw(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	var woID string
	if err := s.App.Store.DB().QueryRowContext(r.Context(), `SELECT target_id FROM signatures WHERE id = ?`, id).Scan(&woID); err != nil {
		return domain.ErrNotFound
	}
	back := "/work-orders/" + woID
	if err := s.App.WithdrawSignature(r.Context(), s.actor(sess), id, strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Signature withdrawn.")
}
