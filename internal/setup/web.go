package setup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WebUI runs the wizard in a page of the web browser instead of a
// terminal: what it says is shown as it goes, and each question is a form
// (buttons for yes or no and for choices, a box for answers and
// passwords). Commands it runs (pi-fleet bootstrap, activate) write to it
// as to a terminal; when one stops at a prompt, such as for a password,
// the page asks for the answer and passes it on.
//
// It serves on this Pi's loopback address only, and only to a browser that
// opened the secret address it was started with.
type WebUI struct {
	token string

	mu      sync.Mutex
	entries []logEntry
	step    int
	seen    int // entries before the current question
	q       *question
	nextID  int
	partial string // a command's output not yet ended by a newline
	done    bool
	result  string // how setup ended
	failed  bool
	closed  chan struct{}
	// The page's signs of life: when it was opened, and the last time the
	// page (its script) said it is still open; a page that said goodbye is
	// given a moment to come back (a reload says goodbye too).
	opened, alive, bye time.Time

	answers chan string    // the wizard's own questions
	stdinR  *io.PipeReader // what commands read
	stdinW  *io.PipeWriter
}

type logEntry struct {
	Kind string // step, say, out (a command's output) or answer
	Text string
}

type question struct {
	ID      int
	Kind    string // ask, confirm, choose, pause, line, or input (for a command)
	Text    string
	Default string
	Options []string
	Error   string // why the last answer wasn't accepted
	Secret  bool
}

// NewWebUI makes a web UI that opens with this secret token.
func NewWebUI(token string) *WebUI {
	r, w := io.Pipe()
	return &WebUI{token: token, answers: make(chan string), stdinR: r, stdinW: w, closed: make(chan struct{})}
}

// NewToken makes a secret token for the setup page's address.
func NewToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Stdin is what commands the wizard runs read their input from.
func (u *WebUI) Stdin() io.Reader { return u.stdinR }

func (u *WebUI) add(kind, text string) {
	u.entries = append(u.entries, logEntry{kind, text})
}

func (u *WebUI) Say(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.flushPartial()
	u.add("say", fmt.Sprintf(format, args...))
}

func (u *WebUI) Step(title string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.flushPartial()
	u.step++
	u.add("step", fmt.Sprintf("Step %d: %s", u.step, title))
}

func (u *WebUI) Progress() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if n := len(u.entries); n > 0 && u.entries[n-1].Kind == "say" {
		u.entries[n-1].Text += "."
	}
}

// ask shows a question and waits for its answer.
func (u *WebUI) ask(q question) (string, error) {
	u.mu.Lock()
	u.flushPartial()
	u.nextID++
	q.ID = u.nextID
	u.q = &q
	u.mu.Unlock()
	select {
	case a := <-u.answers:
		return a, nil
	case <-u.closed:
		return "", ErrCancelled
	}
}

// retry shows the same question again, saying why.
func (u *WebUI) retry(q question, why string) question {
	q.Error = why
	return q
}

func (u *WebUI) Ask(text, def string, check func(string) error) (string, error) {
	q := question{Kind: "ask", Text: text, Default: def, Secret: secretPrompt(text)}
	for {
		a, err := u.ask(q)
		if err != nil {
			return "", err
		}
		if a = strings.TrimSpace(a); a == "" {
			a = def
		}
		if a == "" {
			q = u.retry(q, "Please type an answer.")
			continue
		}
		if check != nil {
			if err := check(a); err != nil {
				q = u.retry(q, err.Error())
				continue
			}
		}
		u.answered(text, a, q.Secret)
		return a, nil
	}
}

func (u *WebUI) Confirm(text string, def bool) (bool, error) {
	d := "no"
	if def {
		d = "yes"
	}
	a, err := u.ask(question{Kind: "confirm", Text: text, Default: d})
	if err != nil {
		return false, err
	}
	yes := a == "yes"
	u.answered(text, map[bool]string{true: "Yes", false: "No"}[yes], false)
	return yes, nil
}

func (u *WebUI) Choose(text string, options []string) (int, error) {
	q := question{Kind: "choose", Text: text, Options: options}
	for {
		a, err := u.ask(q)
		if err != nil {
			return 0, err
		}
		if n, err := strconv.Atoi(a); err == nil && n >= 0 && n < len(options) {
			u.answered(text, options[n], false)
			return n, nil
		}
		q = u.retry(q, "Please choose one of the options.")
	}
}

func (u *WebUI) Pause(prompt string) error {
	_, err := u.ask(question{Kind: "pause", Text: onPage(prompt, "Continue")})
	return err
}

func (u *WebUI) Line(prompt string) (string, error) {
	a, err := u.ask(question{Kind: "line", Text: onPage(prompt, "Continue"), Secret: secretPrompt(prompt)})
	if err == nil {
		u.answered(prompt, a, false)
	}
	return a, err
}

// onPage rewords a terminal prompt for the page, where the button is
// pressed instead of Enter.
func onPage(prompt, button string) string {
	r := strings.NewReplacer("just press Enter", "leave it empty and press "+button,
		"press Enter", "press "+button, "Press Enter", "Press "+button)
	return r.Replace(prompt)
}

// answered notes the answer in what's shown.
func (u *WebUI) answered(question, a string, secret bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if secret {
		a = "••••••••"
	}
	if a == "" {
		a = "(nothing)"
	}
	u.add("answer", withAnswer(question, a))
}

// withAnswer is a question followed by its answer, as shown.
func withAnswer(question, a string) string {
	q := strings.TrimSpace(question)
	if strings.HasSuffix(q, "?") {
		return q + " " + a
	}
	return strings.TrimSuffix(q, ":") + ": " + a
}

// secretPrompt reports whether a prompt asks for a password.
func secretPrompt(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "password") || strings.Contains(s, "passphrase") || strings.HasPrefix(strings.TrimSpace(s), "confirm")
}

// Write takes the output of a command the wizard runs, as a terminal
// would. Output that stops partway through a line, ending like a
// question, is a prompt the command waits at: the page asks it.
func (u *WebUI) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.q != nil && u.q.Kind == "input" {
		u.q = nil // more output came, so it wasn't waiting after all
	}
	text := u.partial + strings.ReplaceAll(string(p), "\r", "")
	lines := strings.Split(text, "\n")
	u.partial = lines[len(lines)-1]
	for _, l := range lines[:len(lines)-1] {
		u.add("out", l)
	}
	if t := strings.TrimRight(u.partial, " "); strings.HasSuffix(t, ":") || strings.HasSuffix(t, "?") || strings.HasSuffix(t, "]") {
		u.nextID++
		u.q = &question{ID: u.nextID, Kind: "input", Text: strings.TrimSpace(u.partial), Secret: secretPrompt(u.partial)}
	}
	return len(p), nil
}

func (u *WebUI) flushPartial() {
	if u.partial != "" {
		u.add("out", u.partial)
		u.partial = ""
	}
}

// Finish shows how setup ended; the page stays until closed.
func (u *WebUI) Finish(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.flushPartial()
	u.done, u.q = true, nil
	if err != nil {
		u.failed = true
		u.result = "Setup stopped: " + err.Error()
	} else {
		u.result = "Setup finished."
	}
}

// Closed is closed when the person closes the page's setup.
func (u *WebUI) Closed() <-chan struct{} { return u.closed }

// --- the page ---

const cookieName = "pifleet_setup"

func (u *WebUI) authorised(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(u.token)) == 1 {
		return true
	}
	http.Error(w, "Open setup from the window it started in.", http.StatusForbidden)
	return false
}

// Handler serves the setup page.
func (u *WebUI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /start", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(u.token)) != 1 {
			http.Error(w, "This setup address isn't valid.", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: u.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		io.WriteString(w, setupCSS)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if u.authorised(w, r) {
			u.seenNow(false)
			u.page(w)
		}
	})
	mux.HandleFunc("GET /setup.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		io.WriteString(w, setupJS)
	})
	mux.HandleFunc("POST /alive", func(w http.ResponseWriter, r *http.Request) {
		if u.authorised(w, r) && sameOrigin(r) {
			u.seenNow(true)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("POST /bye", func(w http.ResponseWriter, r *http.Request) {
		if u.authorised(w, r) && sameOrigin(r) {
			u.mu.Lock()
			u.bye = time.Now()
			u.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
		if !u.authorised(w, r) || !sameOrigin(r) {
			return
		}
		u.answer(r.PostFormValue("id"), r.PostFormValue("answer"))
		http.Redirect(w, r, "/#question", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /close", func(w http.ResponseWriter, r *http.Request) {
		if !u.authorised(w, r) || !sameOrigin(r) {
			return
		}
		u.mu.Lock()
		done := u.done
		u.mu.Unlock()
		if !done && r.PostFormValue("stop") != "yes" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		u.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>pi-fleet setup</title><link rel="stylesheet" href="/style.css"></head><body><main><h1>pi-fleet setup</h1><p class="flash">You can close this window.</p></main></body></html>`)
	})
	return headers(mux)
}

func headers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Cache-Control", "no-store")
		hd.Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// sameOrigin refuses a form sent from another site.
func sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin" || s == "none"
	}
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return o == ""
	}
	p, err := url.Parse(o)
	return err == nil && p.Host == r.Host
}

func (u *WebUI) answer(id, a string) {
	u.mu.Lock()
	q := u.q
	if q == nil || strconv.Itoa(q.ID) != id {
		u.mu.Unlock()
		return // an old form, sent twice
	}
	u.q = nil
	u.seen = len(u.entries)
	if q.Kind == "input" {
		shown := a
		if q.Secret {
			shown = "••••••••"
		}
		u.add("answer", withAnswer(q.Text, shown))
		u.partial = ""
		u.mu.Unlock()
		go u.stdinW.Write([]byte(a + "\n")) // the command reads it
		return
	}
	u.mu.Unlock()
	select {
	case u.answers <- a:
	case <-time.After(5 * time.Second):
	}
}

type pageData struct {
	Recent, Earlier []logEntry
	Q               *question
	Working         string // what is being done, while there's no question
	Done            bool
	Failed          bool
	Result          string
}

func (u *WebUI) page(w http.ResponseWriter) {
	u.mu.Lock()
	d := pageData{Q: u.q, Done: u.done, Failed: u.failed, Result: u.result}
	seen := min(u.seen, len(u.entries))
	// What has happened since the last answer, and the step it's in.
	start := seen
	for i := seen - 1; i >= 0; i-- {
		if u.entries[i].Kind == "step" {
			start = i
			break
		}
	}
	d.Earlier = paragraphs(u.entries[:start])
	d.Recent = paragraphs(u.entries[start:])
	if u.partial != "" && u.q == nil {
		d.Working = u.partial
	}
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := setupPage.Execute(w, d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// paragraphs joins what the wizard says, written in lines for a terminal,
// into paragraphs; an empty line, or a line starting with spaces or a
// list mark, starts a new one.
func paragraphs(es []logEntry) []logEntry {
	var out []logEntry
	open := false
	for _, e := range es {
		if e.Kind != "say" {
			out, open = append(out, e), false
			continue
		}
		t := e.Text
		if strings.TrimSpace(t) == "" {
			open = false
			continue
		}
		if open && !strings.HasPrefix(t, " ") && !strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "•") {
			out[len(out)-1].Text += " " + t
			continue
		}
		out, open = append(out, logEntry{"say", strings.TrimSpace(t)}), true
	}
	return out
}

// seenNow notes that the page is open; alive, that its script said so.
func (u *WebUI) seenNow(alive bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	if u.opened.IsZero() {
		u.opened = now
	}
	if alive {
		u.alive = now
	}
	u.bye = time.Time{}
}

// How long setup waits for its page before taking it as closed.
var (
	// NeverOpened: the page was never opened (the browser didn't start).
	NeverOpened = 10 * time.Minute
	// Silent: the page's script stopped saying it's open. Generous, since
	// browsers slow down the timers of pages in the background.
	Silent = 3 * time.Minute
	// AfterBye: the page said goodbye and didn't come back (a reload or a
	// form sent comes back within moments).
	AfterBye = 15 * time.Second
)

// abandoned reports whether the page seems to have been closed.
func (u *WebUI) abandoned(now, started time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.opened.IsZero():
		return now.Sub(started) > NeverOpened
	case !u.bye.IsZero():
		return now.Sub(u.bye) > AfterBye
	case !u.alive.IsZero(): // only once its script has spoken
		return now.Sub(u.alive) > Silent
	}
	return false
}

// Watch stops setup when its page is closed: at its next question, so
// nothing is left half done. It returns when setup is closed.
func (u *WebUI) Watch(every time.Duration) {
	started := time.Now()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-u.closed:
			return
		case now := <-t.C:
			if u.abandoned(now, started) {
				u.Close()
				return
			}
		}
	}
}

// Close ends setup: its next question stops it, and the page goes.
func (u *WebUI) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	select {
	case <-u.closed:
	default:
		close(u.closed)
	}
}

// setupJS keeps setup informed that its page is open (and says goodbye
// when it closes), so a setup whose window was closed doesn't wait on.
const setupJS = `"use strict";
setInterval(() => fetch("/alive", { method: "POST" }).catch(() => {}), 5000);
fetch("/alive", { method: "POST" }).catch(() => {});
addEventListener("pagehide", () => navigator.sendBeacon("/bye"));
`

// Serve serves the setup page on addr (a loopback address) until ctx ends.
func (u *WebUI) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: u.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

var setupPage = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if and (not .Q) (not .Done)}}<meta http-equiv="refresh" content="2">{{end}}
<title>pi-fleet setup</title>
<link rel="stylesheet" href="/style.css">
<script src="/setup.js" defer></script>
</head>
<body>
<header><span class="mark" aria-hidden="true"></span>pi-fleet setup</header>
<main>
{{with .Earlier}}<details class="earlier"><summary>Earlier steps</summary>{{range .}}{{template "entry" .}}{{end}}</details>{{end}}
<section class="recent">{{range .Recent}}{{template "entry" .}}{{end}}
{{with .Working}}<p class="out working">{{.}}</p>{{end}}</section>
{{if .Done}}
  <p class="{{if .Failed}}error{{else}}flash{{end}}" role="status">{{.Result}}</p>
  <form method="post" action="/close"><button>Close setup</button></form>
{{else if .Q}}{{$q := .Q}}
  <form method="post" action="/answer" id="question" class="question" autocomplete="off">
    <input type="hidden" name="id" value="{{$q.ID}}">
    {{with $q.Error}}<p class="error" role="alert">{{.}}</p>{{end}}
    {{if eq $q.Kind "confirm"}}
      <p class="q">{{$q.Text}}</p>
      <p class="buttons"><button name="answer" value="yes"{{if eq $q.Default "yes"}} autofocus{{else}} class="secondary"{{end}}>Yes</button>
        <button name="answer" value="no"{{if eq $q.Default "no"}} autofocus{{else}} class="secondary"{{end}}>No</button></p>
    {{else if eq $q.Kind "choose"}}
      <p class="q">{{$q.Text}}</p>
      <div class="choices">{{range $i, $o := $q.Options}}<button name="answer" value="{{$i}}" class="choice">{{$o}}</button>{{end}}</div>
    {{else if eq $q.Kind "pause"}}
      <p class="q">{{$q.Text}}</p>
      <p class="buttons"><button name="answer" value="" autofocus>Continue</button></p>
    {{else}}
      <label class="q">{{$q.Text}}
        <input name="answer" {{if $q.Secret}}type="password"{{end}} value="{{if not $q.Secret}}{{$q.Default}}{{end}}" autofocus{{if and (ne $q.Kind "line") (ne $q.Kind "input")}} required{{end}}></label>
      {{if $q.Default}}<p class="hint">Suggested: {{$q.Default}}</p>{{end}}
      <p class="buttons"><button>Continue</button></p>
    {{end}}
  </form>
  <form method="post" action="/close" class="stop"><input type="hidden" name="stop" value="yes"><button class="link">Stop setup</button></form>
{{else}}
  <p class="hint working">Working… this page updates by itself.</p>
{{end}}
</main>
</body>
</html>
{{define "entry"}}{{if eq .Kind "step"}}<h2>{{.Text}}</h2>{{else if eq .Kind "answer"}}<p class="answer">{{.Text}}</p>{{else if eq .Kind "out"}}<p class="out">{{.Text}}</p>{{else if .Text}}<p>{{.Text}}</p>{{end}}{{end}}`))

// setupCSS: the look of pi-fleet, large and plain, for the setup page.
const setupCSS = `:root { --fg: #15181c; --bg: #eceef1; --panel: #fff; --muted: #3a3f45; --line: #b9bec3; --accent: #a11552; --on-accent: #fff; --leaf: #5f9e2f; --bad: #b3261e; --bar: #1c1f24; color-scheme: light dark; }
@media (prefers-color-scheme: dark) { :root { --fg: #e6e8eb; --bg: #121417; --panel: #1a1d21; --muted: #c9ced3; --line: #4a5158; --accent: #f06aa3; --on-accent: #121417; --bad: #ef6f66; --bar: #16181c; } }
* { box-sizing: border-box; }
body { margin: 0; font: 20px/1.6 system-ui, sans-serif; color: var(--fg); background: var(--bg); }
header { display: flex; align-items: center; gap: .7rem; padding: .8rem 1.25rem; background: var(--bar); color: #e4e7eb; font-weight: 700; font-size: 1.3rem; border-bottom: 5px solid var(--leaf); }
.mark { width: 1rem; height: 1.2rem; background: #c51a4a; border-top: .3rem solid var(--leaf); border-radius: 1px 1px 5px 5px; }
main { max-width: 52rem; margin: 0 auto; padding: 1.25rem 1.25rem 3rem; }
h2 { font-size: 1.4rem; margin: 1.5rem 0 .6rem; padding-bottom: .4rem; border-bottom: 3px solid var(--line); }
p { margin: .35rem 0; }
.out { font-family: ui-monospace, monospace; font-size: .9rem; color: var(--muted); white-space: pre-wrap; margin: .1rem 0; }
.answer { color: var(--accent); font-weight: 700; }
.question { margin: 1.25rem 0; padding: 1.1rem 1.25rem; background: var(--panel); border: 2px solid var(--line); border-left: 8px solid var(--accent); border-radius: 8px; display: grid; gap: .8rem; }
.q { font-weight: 700; font-size: 1.15rem; display: grid; gap: .5rem; }
input { font: inherit; font-size: 1.25rem; padding: .6rem .8rem; min-height: 3.5rem; border: 2px solid var(--muted); border-radius: 6px; background: var(--panel); color: var(--fg); width: 100%; }
button { font: 700 1.2rem/1.2 system-ui, sans-serif; min-height: 3.5rem; padding: .7rem 1.4rem; border-radius: 8px; border: 2px solid var(--accent); background: var(--accent); color: var(--on-accent); cursor: pointer; }
button.secondary, button.choice { background: var(--panel); color: var(--fg); border-color: var(--muted); }
button.choice { text-align: left; font-weight: 600; }
button.link { background: none; border: 0; color: var(--accent); text-decoration: underline; min-height: 0; padding: 0; font-weight: 600; }
.choices { display: grid; gap: .6rem; }
.buttons { display: flex; flex-wrap: wrap; gap: 1rem; }
.error { color: var(--bad); font-weight: 700; }
.flash { padding: 1rem 1.25rem; background: var(--panel); border-left: 8px solid var(--leaf); border-radius: 8px; font-weight: 700; }
.hint { color: var(--muted); }
.working::after { content: " ⏳"; }
details.earlier { margin: 0 0 1rem; } details.earlier summary { cursor: pointer; color: var(--accent); font-weight: 700; }
:focus-visible { outline: 4px solid var(--accent); outline-offset: 3px; }
`
