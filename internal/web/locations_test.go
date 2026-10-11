package web

import (
	"bytes"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

type upFile struct {
	field, name string
	data        []byte
}

// postMultipart posts a form with files, as the registration form does.
func (b *browser) postMultipart(path string, form url.Values, files ...upFile) (int, string) {
	b.e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf", b.csrf)
	for k, vs := range form {
		for _, v := range vs {
			mw.WriteField(k, v)
		}
	}
	for _, f := range files {
		fw, _ := mw.CreateFormFile(f.field, f.name)
		fw.Write(f.data)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", b.e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		_, _, page := b.get(resp.Header.Get("Location"))
		return resp.StatusCode, page
	}
	if m := csrfRE.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return resp.StatusCode, string(body)
}

func TestProposeALocationWhileRegisteringEquipment(t *testing.T) {
	e := newEnv(t)
	e.user("tess", "Tess Tech", domain.RoleUser)
	tess := e.browser()
	tess.login("tess", "brass-kettle-orchard-7")
	_, _, page := tess.get("/assets/new")
	for _, want := range []string{`popovertarget="new-location"`, `<div id="new-location" popover`, `formaction="/assets/new/location" formnovalidate`,
		`enctype="multipart/form-data"`, `name="photo_dataplate"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("registration form lacks %s:\n%s", want, page)
		}
	}
	typed := url.Values{"tag": {"US-7"}, "manufacturer": {"GE"}, "model": {"Logiq E10"}, "serial": {"LQ-77"}, "risk_class": {"medium"}}
	with := func(extra url.Values) url.Values {
		v := url.Values{}
		for k, vs := range typed {
			v[k] = vs
		}
		for k, vs := range extra {
			v[k] = vs
		}
		return v
	}

	// A location without a name isn't sent, and nothing typed is lost.
	_, page = tess.postMultipart("/assets/new/location", with(url.Values{"loc_site": {e.site}, "loc_kind": {"room"}, "loc_building": {"East"}}))
	if !strings.Contains(page, "The new location wasn&#39;t sent") || !strings.Contains(page, `value="US-7"`) || !strings.Contains(page, `name="loc_building" value="East"`) {
		t.Fatalf("missing name:\n%s", page)
	}

	// Sent: the form comes back as typed, with the new location chosen.
	_, page = tess.postMultipart("/assets/new/location", with(url.Values{"loc_site": {e.site}, "loc_name": {"Ultrasound 3"}, "loc_kind": {"room"},
		"loc_building": {"East wing"}, "loc_floor": {"2"}, "loc_room": {"2.14"}, "loc_department": {"Radiology"}, "loc_contact": {"Charge nurse"},
		"loc_phone": {"x4410"}, "loc_directions": {"Past the MRI suite"}}))
	sel := regexp.MustCompile(`<option value="([0-9a-f-]{36})" selected>NYC · Ultrasound 3 \(awaiting approval\)</option>`).FindStringSubmatch(page)
	if sel == nil || !strings.Contains(page, "was sent to a super user for review") || !strings.Contains(page, `value="Logiq E10"`) ||
		strings.Contains(page, `name="loc_building" value="East wing"`) {
		t.Fatalf("after proposing:\n%s", page)
	}

	// Registered there, with photos of the equipment and its dataplate.
	code, page := tess.postMultipart("/assets", with(url.Values{"location": {sel[1]}}),
		upFile{"photo_equipment", "front.jpg", photoWithGPS()}, upFile{"photo_dataplate", "plate.jpg", photoWithGPS()},
		upFile{"files", "notes.txt", []byte("not allowed")})
	if code != http.StatusSeeOther || !strings.Contains(page, "Equipment registered, but not attached: notes.txt") ||
		!strings.Contains(page, "Photo of the equipment") || !strings.Contains(page, "Photo of the dataplate") ||
		!strings.Contains(page, "Ultrasound 3 (awaiting approval)") {
		t.Fatalf("registered:\n%s", page)
	}
	assetID := regexp.MustCompile(`/assets/([0-9a-f-]{36})/attachments`).FindStringSubmatch(page)[1]

	// The super user is told, reviews it, and rejects it.
	admin := e.browser()
	admin.login("admin", "tumbleweed-gasket-42")
	if _, _, page = admin.get("/"); !strings.Contains(page, "1 new location awaiting your review") {
		t.Fatalf("dashboard:\n%s", page)
	}
	_, _, page = admin.get("/admin/sites")
	for _, want := range []string{"New locations awaiting review", "NYC · New York · Ultrasound 3 (room)", "Tess Tech (tess)", "East wing", "x4410",
		"Past the MRI suite", "US-7 · GE Logiq E10", "Ultrasound 3 (room) · awaiting approval"} {
		if !strings.Contains(html.UnescapeString(page), want) {
			t.Fatalf("review lacks %q:\n%s", want, page)
		}
	}
	if _, page = admin.post("/admin/locations/"+sel[1]+"/review", url.Values{"decision": {"reject"}}); !strings.Contains(page, "Not saved: rejecting a location needs a reason") {
		t.Fatalf("rejected without a reason:\n%s", page)
	}
	if _, page = admin.post("/admin/locations/"+sel[1]+"/review", url.Values{"decision": {"reject"}, "note": {"use Imaging 2"}}); !strings.Contains(page, "Its equipment is now at the Unallocated site") ||
		strings.Contains(page, "New locations awaiting review") {
		t.Fatalf("rejected:\n%s", page)
	}
	if code, _ := tess.post("/admin/locations/"+sel[1]+"/review", url.Values{"decision": {"approve"}}); code != http.StatusForbidden {
		t.Fatalf("an ordinary user reviewed: %d", code)
	}

	// Findable as Unallocated equipment, and no longer offered as a location.
	if _, _, page = tess.get("/assets?q=unallocated"); !strings.Contains(page, "/assets/"+assetID) || !strings.Contains(page, "<td>UNALLOC</td>") {
		t.Fatalf("search:\n%s", page)
	}
	if _, _, page = tess.get("/assets/new"); strings.Contains(page, "Ultrasound 3") || strings.Contains(page, ">UNALLOC · ") {
		t.Fatalf("rejected or holding location offered:\n%s", page)
	}
}

func TestApproveAProposedLocation(t *testing.T) {
	e := newEnv(t)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	loc, err := e.app.ProposeLocation(e.ctx, app.Actor{UserID: tessID, SessionID: "t"}, domain.LocationProposed{SiteID: e.site, Name: "Cath lab 2", Kind: "room"})
	if err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.login("admin", "tumbleweed-gasket-42")
	admin.get("/admin/sites")
	if _, page := admin.post("/admin/locations/"+loc+"/review", url.Values{"decision": {"approve"}}); !strings.Contains(page, "Location approved.") ||
		!strings.Contains(page, "<li>Cath lab 2 (room)</li>") {
		t.Fatalf("approved:\n%s", page)
	}
}

// Example text in empty boxes is styled apart from what people type.
func TestExampleTextLooksDifferentFromTypedText(t *testing.T) {
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"::placeholder { color: var(--hint-fg); font-style: italic;", "opacity: 1;", "--hint-fg: #4f6a8f;", "--hint-fg: #8fa9cc;"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
}
