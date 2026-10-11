package web

import (
	"bytes"
	"image/png"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
)

func TestQRLabels(t *testing.T) {
	e := newEnv(t)
	pump, err := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-7", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma", MasterID: "BX 0042"})
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "SCALE-1", LocationID: e.loc, Manufacturer: "Seca", Model: "877"})
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	host := strings.TrimPrefix(e.srv.URL, "https://")

	// Choosing the MasterID pops up its QR label, with Print and Download.
	_, _, page := b.get("/assets/" + pump)
	want := "https://" + host + "/r/BX%200042"
	for _, s := range []string{`popovertarget="qr-` + pump + `">BX 0042</button>`, `<div id="qr-` + pump + `" popover`, `src="/assets/` + pump + `/qr.png"`,
		"Scanning it opens " + want, `href="/assets/` + pump + `/label" target="_blank"`, `href="/assets/` + pump + `/qr.png?download=1"`} {
		if !strings.Contains(page, s) {
			t.Fatalf("equipment page lacks %s:\n%s", s, page)
		}
	}
	// Without a MasterID, the label names the asset tag.
	if _, _, page = b.get("/assets/" + plain); !strings.Contains(page, "not set · <button") || !strings.Contains(page, "/r/SCALE-1") {
		t.Fatalf("no MasterID:\n%s", page)
	}
	if _, _, page = b.get("/assets"); !strings.Contains(page, `popovertarget="qr-`+pump+`">BX 0042</button>`) {
		t.Fatalf("list:\n%s", page)
	}

	resp, err := b.c.Get(e.srv.URL + "/assets/" + pump + "/qr.png")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	got.ReadFrom(resp.Body)
	resp.Body.Close()
	expect, _ := qrPNG(want, 12)
	if resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(got.Bytes(), expect) {
		t.Fatalf("QR image: %s, %d bytes", resp.Header.Get("Content-Type"), got.Len())
	}
	if img, err := png.Decode(bytes.NewReader(got.Bytes())); err != nil || img.Bounds().Dx() < 300 {
		t.Fatalf("not a usable PNG: %v", err)
	}
	resp, _ = b.c.Get(e.srv.URL + "/assets/" + pump + "/qr.png?download=1")
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "QR%20BX%200042.png") {
		t.Fatalf("download: %q", cd)
	}

	// The label page: the label, and the one script allowed (Print).
	resp, _ = b.c.Get(e.srv.URL + "/assets/" + pump + "/label")
	var lb bytes.Buffer
	lb.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") || !strings.Contains(lb.String(), `<script src="/static/label.js" defer>`) ||
		!strings.Contains(lb.String(), `<button id="print" type="button">Print label</button>`) || !strings.Contains(lb.String(), want) {
		t.Fatalf("label page:\n%s", lb.String())
	}
	if c, _, _ := e.browser().get("/assets/" + pump + "/label"); c != http.StatusSeeOther {
		t.Fatalf("label page without signing in: %d", c)
	}
	// Every other page still may not run scripts.
	if _, _, page = b.get("/assets/" + pump); strings.Contains(page, "<script") {
		t.Fatal("script on the equipment page")
	}
}

func TestReportAProblemByQRCode(t *testing.T) {
	e := newEnv(t)
	pump, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-7", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma", MasterID: "BX-0042"})

	// A nurse's phone: not signed in.
	nurse := e.browser()
	_, _, page := nurse.get("/r/BX-0042")
	for _, s := range []string{"PUMP-7 · Baxter Sigma", "NYC › Biomed shop", `name="category" value="Not working" required`, "If anyone is in danger"} {
		if !strings.Contains(page, s) {
			t.Fatalf("report page lacks %s:\n%s", s, page)
		}
	}
	if c, _, page := nurse.get("/r/NOPE-1"); c != http.StatusNotFound || !strings.Contains(page, "No equipment has the label") {
		t.Fatalf("unknown label: %d\n%s", c, page)
	}
	form := url.Values{"category": {"Alarm or error message"}, "description": {"Occlusion alarm with no line blocked"},
		"name": {"Nora Nurse"}, "department": {"Ward 3B"}, "phone": {"x4410"}}
	missing := url.Values{"category": {"Not working"}, "description": {"won't start"}, "name": {"Nora Nurse"}, "department": {"Ward 3B"}}
	if c, page := nurse.post("/r/BX-0042", missing); c != http.StatusUnprocessableEntity || !strings.Contains(page, "Not sent:") || !strings.Contains(page, ">won&#39;t start</textarea>") {
		t.Fatalf("no phone: %d\n%s", c, page)
	}
	code, page := nurse.post("/r/BX-0042", form)
	if code != http.StatusSeeOther || !strings.Contains(page, "Your reference is <strong>R-00001</strong>") || !strings.Contains(page, "Received: waiting") {
		t.Fatalf("sent: %d\n%s", code, page)
	}
	if strings.Contains(page, "x4410") || strings.Contains(page, "Nora") {
		t.Fatal("status page shows who reported it")
	}

	// Staff see it, open a work order from it, and the nurse sees that.
	tech := e.browser()
	tech.login("admin", "tumbleweed-gasket-42")
	if _, _, page = tech.get("/"); !strings.Contains(page, "1 reported problem waiting to be looked at") {
		t.Fatalf("dashboard:\n%s", page)
	}
	_, _, page = tech.get("/requests")
	for _, s := range []string{"R-00001 · Alarm or error message", "Occlusion alarm with no line blocked", "Nora Nurse, Ward 3B · x4410"} {
		if !strings.Contains(page, s) {
			t.Fatalf("requests page lacks %s:\n%s", s, page)
		}
	}
	id := regexp.MustCompile(`/requests/([0-9a-f-]{36})/convert`).FindStringSubmatch(page)[1]
	if _, page = tech.post("/requests/"+id+"/convert", url.Values{"type": {"corrective"}, "priority": {"high"}}); !strings.Contains(page, "opened from the reported problem") ||
		!strings.Contains(page, "Reported R-00001") {
		t.Fatalf("converted:\n%s", page)
	}
	if _, _, page = nurse.get("/r/status/R-00001"); !strings.Contains(page, "Being worked on: work order NYC-WO-") {
		t.Fatalf("status after converting:\n%s", page)
	}
	if c, _, _ := nurse.get("/r/status/" + id); c != http.StatusNotFound {
		t.Fatalf("status by internal id: %d", c)
	}
	if c, _, _ := nurse.get("/requests"); c != http.StatusSeeOther {
		t.Fatalf("requests page without signing in: %d", c)
	}

	// A second report, closed with a reason the nurse sees.
	nurse.post("/r/BX-0042", form)
	_, _, page = tech.get("/requests")
	id = regexp.MustCompile(`/requests/([0-9a-f-]{36})/close`).FindStringSubmatch(page)[1]
	tech.post("/requests/"+id+"/close", url.Values{"reason": {"Same as R-00001"}})
	if _, _, page = nurse.get("/r/status/R-00002"); !strings.Contains(page, "Closed: Same as R-00001") {
		t.Fatalf("closed status:\n%s", page)
	}

	// No flooding: five reports a few minutes from one address.
	for i := 0; i < 3; i++ {
		nurse.post("/r/BX-0042", form)
	}
	if c, page := nurse.post("/r/BX-0042", form); c != http.StatusUnprocessableEntity || !strings.Contains(page, "too many problems have been reported from here") {
		t.Fatalf("sixth report: %d\n%s", c, page)
	}
	_ = pump
}
