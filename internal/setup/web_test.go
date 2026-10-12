package setup

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A wizard's questions answered on the setup page: typed answers checked
// and asked again, yes or no, choices, a command's password prompt, and
// the page only for whoever opened the secret address.
func TestSetupInTheBrowser(t *testing.T) {
	token := NewToken()
	u := NewWebUI(token)
	srv := httptest.NewServer(u.Handler())
	defer srv.Close()

	got := make(chan []string, 1)
	go func() {
		var out []string
		u.Step("Name this Pi")
		u.Say("The name other Pis and phones")
		u.Say("use to find it.")
		name, _ := u.Ask("Name for this Pi", "pifleet", func(s string) error {
			if strings.Contains(s, " ") {
				return errors.New("no spaces, please")
			}
			return nil
		})
		out = append(out, name)
		yes, _ := u.Confirm("Is this the master Pi?", false)
		out = append(out, map[bool]string{true: "yes", false: "no"}[yes])
		i, _ := u.Choose("Which drive?", []string{"USB SSD (1 TB)", "USB stick (32 GB)"})
		out = append(out, []string{"ssd", "stick"}[i])
		// A command the wizard runs, asking for a password.
		io.WriteString(u, "Choose a password: ")
		pw, _ := bufio.NewReader(u.Stdin()).ReadString('\n')
		out = append(out, strings.TrimSpace(pw))
		io.WriteString(u, "Password set.\n")
		line, _ := u.Line("Other network names (or nothing): ")
		out = append(out, "line="+line)
		u.Finish(nil)
		got <- out
	}()

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	idRE := regexp.MustCompile(`name="id" value="(\d+)"`)
	// page waits for a question (or the end) and returns the page.
	page := func(want string) string {
		t.Helper()
		for range 100 {
			_, p := get("/")
			if strings.Contains(p, want) {
				return p
			}
			time.Sleep(20 * time.Millisecond)
		}
		_, p := get("/")
		t.Fatalf("waiting for %q:\n%s", want, p)
		return ""
	}
	answer := func(p, a string) {
		t.Helper()
		m := idRE.FindStringSubmatch(p)
		if m == nil {
			t.Fatalf("no question:\n%s", p)
		}
		resp, err := c.PostForm(srv.URL+"/answer", url.Values{"id": {m[1]}, "answer": {a}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	if code, _ := get("/"); code != http.StatusForbidden {
		t.Fatalf("page without the secret address: %d", code)
	}
	if code, _ := get("/start?t=wrong"); code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := get("/start?t=" + token); code != http.StatusSeeOther {
		t.Fatalf("start: %d", code)
	}
	p := page("Name for this Pi")
	if !strings.Contains(p, "<h2>Step 1: Name this Pi</h2>") || !strings.Contains(p, "<p>The name other Pis and phones use to find it.</p>") ||
		!strings.Contains(p, `value="pifleet" autofocus required`) {
		t.Fatalf("first question:\n%s", p)
	}
	answer(p, "my pi")
	p = page("no spaces, please")
	answer(p, "")                      // the default
	p = page("Is this the master Pi?") // yes or no: two buttons
	if !strings.Contains(p, `<button name="answer" value="yes" class="secondary">Yes</button>`) || !strings.Contains(p, `value="no" autofocus>No</button>`) {
		t.Fatalf("confirm:\n%s", p)
	}
	answer(p, "yes")
	p = page("Which drive?")
	if !strings.Contains(p, `<button name="answer" value="1" class="choice">USB stick (32 GB)</button>`) || !strings.Contains(p, `Is this the master Pi? Yes`) {
		t.Fatalf("choose:\n%s", p)
	}
	answer(p, "0")
	p = page("Choose a password:") // the command's prompt
	if !strings.Contains(p, `type="password"`) {
		t.Fatalf("password prompt:\n%s", p)
	}
	answer(p, "tumbleweed-gasket-42")
	p = page("Other network names")
	if strings.Contains(p, "tumbleweed") || !strings.Contains(p, "Choose a password: ••••••••") || !strings.Contains(p, "Password set.") {
		t.Fatalf("after the password:\n%s", p)
	}
	answer(p, "")
	p = page("Setup finished.")
	if out := <-got; strings.Join(out, ",") != "pifleet,yes,ssd,tumbleweed-gasket-42,line=" {
		t.Fatalf("answers: %v", out)
	}
	resp, _ := c.PostForm(srv.URL+"/close", nil)
	resp.Body.Close()
	select {
	case <-u.Closed():
	default:
		t.Fatal("closing didn't close")
	}
}

func TestStopSetupFromThePage(t *testing.T) {
	u := NewWebUI(NewToken())
	srv := httptest.NewServer(u.Handler())
	defer srv.Close()
	errc := make(chan error, 1)
	go func() {
		_, err := u.Ask("Anything", "", nil)
		errc <- err
	}()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	c.Get(srv.URL + "/start?t=" + u.token)
	time.Sleep(50 * time.Millisecond)
	resp, err := c.PostForm(srv.URL+"/close", url.Values{"stop": {"yes"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-errc; !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v", err)
	}
}

func TestPromptsReworded(t *testing.T) {
	for in, want := range map[string]string{
		"Plug the drive in, then press Enter.":                          "Plug the drive in, then press Continue.",
		"Type ERASE to erase it, or just press Enter to choose again: ": "Type ERASE to erase it, or leave it empty and press Continue to choose again: ",
	} {
		if got := onPage(in, "Continue"); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestParagraphs(t *testing.T) {
	in := []logEntry{{"say", "One line"}, {"say", "goes on."}, {"say", ""}, {"say", "Next."}, {"say", "  1) indented"}, {"out", "cmd"}, {"say", "After."}}
	got := paragraphs(in)
	want := []logEntry{{"say", "One line goes on."}, {"say", "Next."}, {"say", "1) indented"}, {"out", "cmd"}, {"say", "After."}}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%d: %v, want %v", i, got[i], want[i])
		}
	}
}

// A setup whose page was closed stops at its next question, instead of
// waiting on in the background: the page's script says it's open, and
// goodbye when it closes; a reload comes back in time.
func TestSetupStopsWhenItsPageCloses(t *testing.T) {
	defer func(a, b, c time.Duration) { NeverOpened, Silent, AfterBye = a, b, c }(NeverOpened, Silent, AfterBye)
	NeverOpened, Silent, AfterBye = 300*time.Millisecond, 200*time.Millisecond, 100*time.Millisecond
	start := func() (*WebUI, *http.Client, *httptest.Server, chan error) {
		u := NewWebUI(NewToken())
		srv := httptest.NewServer(u.Handler())
		errc := make(chan error, 1)
		go func() { _, err := u.Ask("Anything", "", nil); errc <- err }()
		go u.Watch(10 * time.Millisecond)
		jar, _ := cookiejar.New(nil)
		return u, &http.Client{Jar: jar}, srv, errc
	}
	post := func(c *http.Client, srv *httptest.Server, path string) {
		resp, err := c.Post(srv.URL+path, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	stopped := func(errc chan error, within time.Duration) bool {
		select {
		case err := <-errc:
			return errors.Is(err, ErrCancelled)
		case <-time.After(within):
			return false
		}
	}

	// Never opened.
	_, _, srv, errc := start()
	if !stopped(errc, time.Second) {
		t.Fatal("a page never opened didn't stop setup")
	}
	srv.Close()

	// Open, alive; then a reload (goodbye, and back at once) keeps it.
	u, c, srv, errc := start()
	c.Get(srv.URL + "/start?t=" + u.token)
	post(c, srv, "/alive")
	post(c, srv, "/bye")
	c.Get(srv.URL + "/")
	for range 4 {
		time.Sleep(80 * time.Millisecond)
		post(c, srv, "/alive")
	}
	if stopped(errc, 10*time.Millisecond) {
		t.Fatal("stopped while its page was open")
	}
	// Closed: goodbye, and nothing more.
	post(c, srv, "/bye")
	if !stopped(errc, time.Second) {
		t.Fatal("a closed page didn't stop setup")
	}
	srv.Close()

	// Gone silent (the browser itself stopped).
	u, c, srv, errc = start()
	c.Get(srv.URL + "/start?t=" + u.token)
	post(c, srv, "/alive")
	if !stopped(errc, time.Second) {
		t.Fatal("a silent page didn't stop setup")
	}
	srv.Close()

	// Without its script (only page views): never taken as closed while
	// someone may be reading the question.
	u, c, srv, errc = start()
	c.Get(srv.URL + "/start?t=" + u.token)
	if stopped(errc, 500*time.Millisecond) {
		t.Fatal("stopped a page without its script")
	}
	srv.Close()
}
