package web

import (
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPreview serves a practice copy of the web interface, full of sample
// records, for trying out changes to its look (deploy/maintainer/Preview-Website.sh).
// It runs only when PIFLEET_PREVIEW is set to an address such as
// 127.0.0.1:9080, and serves until interrupted. Nothing is kept.
//
// With PIFLEET_SNAPSHOT set to a folder, it instead saves a set of pages
// there as HTML files that open straight from disk, for screenshots.
func TestPreview(t *testing.T) {
	addr, snap := os.Getenv("PIFLEET_PREVIEW"), os.Getenv("PIFLEET_SNAPSHOT")
	if addr == "" && snap == "" {
		t.Skip("set PIFLEET_PREVIEW=127.0.0.1:9080 to serve a preview")
	}
	e := newEnv(t)
	e.seedEverything()
	if snap != "" {
		snapshotPages(t, e, snap)
		return
	}
	// Plain HTTP on this computer only, so no certificate is needed.
	s := &Server{App: e.app, Role: "central", Now: func() time.Time { return e.now }, PHIPatterns: DefaultPHIPatterns}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Addr: addr, Handler: h}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		<-stop
		srv.Close()
	}()
	t.Logf("preview at http://%s — sign in as admin (super user, password tumbleweed-gasket-42),"+
		" mona (mid-tier) or tess (user), both brass-kettle-orchard-7. Press Ctrl+C to stop.", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

func snapshotPages(t *testing.T, e *env, dir string) {
	css, err := filepath.Abs("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	_, _, list := b.get("/assets")
	first := hrefRE.FindAllStringSubmatch(list, -1)
	assetPage := ""
	for _, m := range first {
		if strings.HasPrefix(m[1], "/assets/0") {
			assetPage = m[1]
			break
		}
	}
	pages := map[string]string{"dashboard": "/", "assets": "/assets", "work-orders": "/work-orders?view=all", "review": "/review",
		"schedules": "/schedules", "new-asset": "/assets/new", "merge": "/assets/merge?master_id=M-2", "asset": assetPage}
	os.MkdirAll(dir, 0o755)
	for name, path := range pages {
		resp, err := b.c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := strings.ReplaceAll(string(body), `href="/static/style.css"`, `href="file://`+css+`"`)
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The sign-in page, signed out.
	out := e.browser()
	r2, err := out.c.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	os.WriteFile(filepath.Join(dir, "login.html"), []byte(strings.ReplaceAll(string(body), `href="/static/style.css"`, `href="file://`+css+`"`)), 0o644)
}
