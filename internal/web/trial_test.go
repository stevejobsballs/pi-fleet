package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTrialBannerOnEveryPage(t *testing.T) {
	e := newEnv(t)
	s := &Server{App: e.app, Role: "central", Now: func() time.Time { return e.now }, PHIPatterns: DefaultPHIPatterns, Trial: true}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	trial := *e
	trial.srv = srv
	b := trial.browser()
	const banner = "Trial installation."
	if _, _, page := b.get("/login"); !strings.Contains(page, banner) {
		t.Fatal("no banner on the sign-in page")
	}
	b.login("admin", "tumbleweed-gasket-42")
	for _, p := range []string{"/", "/assets", "/work-orders"} {
		if _, _, page := b.get(p); !strings.Contains(page, banner) || !strings.Contains(page, "only for testing") {
			t.Errorf("no banner on %s", p)
		}
	}
	// Without the trial mark there is none.
	if _, _, page := e.browser().get("/login"); strings.Contains(page, banner) {
		t.Fatal("banner on a normal installation")
	}
}
