package web

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// firefox makes test requests look like a real browser's and checks every
// page it is shown for things a browser would refuse or ignore. Requests
// carry the headers Firefox sends: Sec-Fetch-*, a Referer and, on form
// posts, an Origin, each following the Referrer-Policy of the page the
// request came from. (A page served with "no-referrer" makes Firefox send
// "Origin: null", which once got every sign-in refused.) Headers a test
// sets itself are left alone, so a test can play another site.
type firefox struct {
	t    *testing.T
	base http.RoundTripper

	mu     sync.Mutex
	page   *url.URL // the page on screen
	policy string   // its Referrer-Policy
}

const firefoxUA = "Mozilla/5.0 (X11; Linux aarch64; rv:140.0) Gecko/20100101 Firefox/140.0"

func (f *firefox) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	page, policy := f.page, f.policy
	f.mu.Unlock()

	h := req.Header.Clone()
	setDefault := func(k, v string) {
		if h.Get(k) == "" && v != "" {
			h.Set(k, v)
		}
	}
	setDefault("User-Agent", firefoxUA)
	setDefault("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	setDefault("Accept-Language", "en-US,en;q=0.5")
	setDefault("Sec-Fetch-Dest", "document")
	setDefault("Sec-Fetch-Mode", "navigate")
	setDefault("Sec-Fetch-User", "?1")
	if page == nil {
		setDefault("Sec-Fetch-Site", "none") // typed into the address bar
	} else {
		setDefault("Sec-Fetch-Site", fetchSite(page, req.URL))
		setDefault("Referer", referrer(page, req.URL, policy))
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			setDefault("Origin", requestOrigin(page, policy))
		}
	}
	if req.Method == http.MethodPost && h.Get("Origin") == "" {
		h.Set("Origin", "null") // a post with no page behind it
	}
	req = req.Clone(req.Context())
	req.Header = h

	resp, err := f.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	checkCookies(f.t, req, resp)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	checkPage(f.t, req, resp, string(body))
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		f.mu.Lock()
		f.page, f.policy = req.URL, resp.Header.Get("Referrer-Policy")
		if f.policy == "" {
			f.policy = "strict-origin-when-cross-origin" // the browser default
		}
		f.mu.Unlock()
	}
	return resp, nil
}

// reporter is the part of testing.T the checks use, so they can be tested.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

func fetchSite(from, to *url.URL) string {
	if origin(from) == origin(to) {
		return "same-origin"
	}
	if from.Hostname() == to.Hostname() {
		return "same-site"
	}
	return "cross-site"
}

// referrer is the Referer a browser sends from page to target under policy.
func referrer(page, target *url.URL, policy string) string {
	full := *page
	full.Fragment, full.User = "", nil
	same := origin(page) == origin(target)
	downgrade := page.Scheme == "https" && target.Scheme != "https"
	switch policy {
	case "no-referrer":
		return ""
	case "same-origin":
		if same {
			return full.String()
		}
		return ""
	case "origin":
		return origin(page) + "/"
	case "strict-origin":
		if downgrade {
			return ""
		}
		return origin(page) + "/"
	case "no-referrer-when-downgrade", "unsafe-url":
		if downgrade && policy != "unsafe-url" {
			return ""
		}
		return full.String()
	default: // strict-origin-when-cross-origin
		if same {
			return full.String()
		}
		if downgrade {
			return ""
		}
		return origin(page) + "/"
	}
}

// requestOrigin is the Origin a browser sends on a form post: "null" when
// the page's policy is no-referrer, whatever the target.
func requestOrigin(page *url.URL, policy string) string {
	if policy == "no-referrer" {
		return "null"
	}
	return origin(page)
}

func checkCookies(t reporter, req *http.Request, resp *http.Response) {
	t.Helper()
	for _, c := range resp.Cookies() {
		if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
			t.Errorf("%s %s: cookie %s must be HttpOnly and SameSite=Strict", req.Method, req.URL.Path, c.Name)
		}
		if req.URL.Scheme == "https" && !c.Secure {
			t.Errorf("%s %s: cookie %s set over HTTPS without Secure", req.Method, req.URL.Path, c.Name)
		}
	}
}

var (
	tagRE      = regexp.MustCompile(`(?s)<([a-zA-Z][a-zA-Z0-9]*)\b([^>]*)>`)
	attrRE     = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:\s*=\s*"([^"]*)"|\s*=\s*'([^']*)'|\s*=\s*([^\s"'>]+))?`)
	formRE     = regexp.MustCompile(`(?s)<form\b([^>]*)>(.*?)</form>`)
	csrfFormRE = regexp.MustCompile(`<input[^>]*name="csrf"[^>]*>`)
	fileRE     = regexp.MustCompile(`<input[^>]*type="file"`)
	// Policies under which a same-origin form post carries a usable Origin.
	originPolicies = map[string]bool{"same-origin": true, "strict-origin": true, "strict-origin-when-cross-origin": true,
		"origin": true, "no-referrer-when-downgrade": true}
)

func attrs(s string) map[string]string {
	m := map[string]string{}
	for _, a := range attrRE.FindAllStringSubmatch(s, -1) {
		m[strings.ToLower(a[1])] = a[2] + a[3] + a[4]
	}
	return m
}

// checkPage reports what a browser would block, ignore or refuse on an
// HTML page: missing protective headers, inline script or style (the CSP
// blocks them, so the page silently breaks), resources from elsewhere,
// and forms that can't be submitted.
func checkPage(t reporter, req *http.Request, resp *http.Response, body string) {
	t.Helper()
	where := req.Method + " " + req.URL.RequestURI()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf("%s: "+format, append([]any{where}, args...)...)
	}
	hd := resp.Header
	csp := hd.Get("Content-Security-Policy")
	for _, d := range []string{"default-src 'none'", "form-action 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, d) {
			fail("Content-Security-Policy lacks %q: %q", d, csp)
		}
	}
	if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("X-Frame-Options") != "DENY" || hd.Get("Cache-Control") != "no-store" {
		fail("missing protective headers")
	}
	if p := hd.Get("Referrer-Policy"); !originPolicies[p] {
		fail("Referrer-Policy %q makes browsers send Origin: null on form posts, which are then refused", p)
	}
	if ct := hd.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "charset=utf-8") {
		fail("Content-Type %q has no charset", ct)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return
	}

	for _, tag := range tagRE.FindAllStringSubmatch(body, -1) {
		name, a := strings.ToLower(tag[1]), attrs(tag[2])
		switch name {
		case "script", "style", "iframe", "object", "embed", "base":
			fail("<%s> element (blocked by the CSP or not allowed)", name)
		}
		for k, v := range a {
			if strings.HasPrefix(k, "on") {
				fail("<%s %s=…> inline event handler is blocked by the CSP", name, k)
			}
			if k == "style" {
				fail("<%s style=…> inline style is ignored under the CSP", name)
			}
			if (k == "href" || k == "src" || k == "action" || k == "formaction") && strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "javascript:") {
				fail("<%s %s=%q> javascript: URL", name, k, v)
			}
		}
		local := func(k string) {
			v, ok := a[k]
			if ok && (!strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//")) && !strings.HasPrefix(v, "data:") {
				fail("<%s %s=%q> loads from another origin, which the CSP blocks", name, k, v)
			}
		}
		switch name {
		case "img", "source", "audio", "video":
			local("src")
		case "link":
			if strings.Contains(a["rel"], "stylesheet") || strings.Contains(a["rel"], "icon") {
				local("href")
			}
		}
	}

	for _, f := range formRE.FindAllStringSubmatch(body, -1) {
		a, inner := attrs(f[1]), f[2]
		action, method := a["action"], strings.ToLower(a["method"])
		if action != "" && (!strings.HasPrefix(action, "/") || strings.HasPrefix(action, "//")) {
			fail("form action %q is not on this site (form-action 'self')", action)
		}
		if method != "post" {
			continue
		}
		if action != "/login" && !csrfFormRE.MatchString(inner) {
			fail("post form to %q has no csrf field, so it would be refused", action)
		}
		if fileRE.MatchString(inner) && a["enctype"] != "multipart/form-data" {
			fail("form to %q has a file input but enctype %q: the file would not be sent", action, a["enctype"])
		}
	}
}
