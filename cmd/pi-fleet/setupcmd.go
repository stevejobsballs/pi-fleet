package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"pi-fleet/internal/setup"
)

// cmdSetup runs the guided installer. It is also what runs when the
// downloaded file is started without a command (or double-clicked).
func cmdSetup(ctx context.Context, args []string, c *cli) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "show what setup would do without changing anything")
	allowDev := fs.Bool("allow-dev", false, "allow installing a development build (testing only)")
	pause := fs.Bool("pause", false, "wait for Enter before exiting (used when started by double-click)")
	inTerminal := fs.Bool("terminal", false, "ask the questions in the terminal, not in the web browser")
	web := fs.Bool("web", false, "ask the questions on a page in the web browser (started by double-clicking; reads the page's token from standard input)")
	listen := fs.String("listen", "127.0.0.1:0", "with -web: this Pi's loopback address and port to serve the page on")
	forUser := fs.String("for-user", "", "with -web: the desktop user setup was started by")
	if err := fs.Parse(args); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, isFile := c.stdin.(*os.File)
	interactive := isFile && term.IsTerminal(int(in.Fd()))
	if *web {
		return webSetup(ctx, c, self, *listen, *forUser, *dry, *allowDev)
	}
	desktop := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	// Double-clicked: ask the questions in the web browser, after the
	// desktop's own window asks for the administrator password.
	if !interactive && !*inTerminal && os.Getenv("PIFLEET_IN_TERMINAL") == "" && desktop {
		if pk, err := exec.LookPath("pkexec"); err == nil {
			return launchWebSetup(ctx, pk, self, args, *dry)
		}
	}
	// Otherwise, without a terminal to ask questions in, open one.
	// (Only once: PIFLEET_IN_TERMINAL marks a window already opened for it.)
	if !interactive && os.Getenv("PIFLEET_IN_TERMINAL") == "" && desktop {
		if t, err := exec.LookPath("x-terminal-emulator"); err == nil {
			cmd := shellQuote(append([]string{self, "setup", "-pause"}, args...))
			return syscall.Exec(t, []string{t, "-t", "pi-fleet setup", "-e", cmd}, append(os.Environ(), "PIFLEET_IN_TERMINAL=1"))
		}
	}
	// Setup changes the system, so it needs administrator rights.
	if !*dry && os.Geteuid() != 0 {
		sudo, err := exec.LookPath("sudo")
		if err != nil {
			return errors.New("setup needs administrator rights: run it as root")
		}
		fmt.Fprintln(c.out, "Setup needs administrator rights; sudo will ask for your password.")
		return syscall.Exec(sudo, append([]string{"sudo", self, "setup"}, args...), os.Environ())
	}
	w := &setup.Wizard{
		UI:       setup.NewUI(c.stdin, c.out),
		Sys:      &setup.Real{DryRun: *dry, Out: c.out, In: c.stdin},
		Self:     self,
		Version:  version,
		AllowDev: *allowDev,
		DryRun:   *dry,
		SudoUser: os.Getenv("SUDO_USER"),
	}
	err = w.Run()
	if *pause {
		if err != nil {
			fmt.Fprintln(c.out, "\nSetup stopped:", err)
		}
		fmt.Fprint(c.out, "\nPress Enter to close this window. ")
		fmt.Fscanln(c.stdin)
	}
	return err
}

func shellQuote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// installed reports whether this program runs from its installed place,
// rather than as a freshly downloaded file.
func installed() bool {
	self, err := os.Executable()
	if err != nil {
		return true
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	return strings.HasPrefix(self, setup.InstallRoot+"/")
}

// launchWebSetup starts setup with administrator rights (pkexec shows the
// desktop's password window), then opens its page in the web browser and
// waits for it to finish. The page's secret token goes to setup on its
// standard input, out of sight of other programs.
func launchWebSetup(ctx context.Context, pkexec, self string, args []string, dry bool) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	ln.Close()
	token := setup.NewToken()
	who := ""
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	cmdArgs := append([]string{self, "setup", "-web", "-listen", addr, "-for-user", who}, args...)
	var cmd *exec.Cmd
	if dry {
		cmd = exec.CommandContext(ctx, self, cmdArgs[1:]...) // a dry run changes nothing: no rights needed
	} else {
		cmd = exec.CommandContext(ctx, pkexec, cmdArgs...)
	}
	cmd.Stdin = strings.NewReader(token + "\n")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// Wait for the page (the password window may take a while), or for
	// setup to end (the password window was cancelled).
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case err := <-exited:
			return err
		case <-time.After(500 * time.Millisecond):
		}
		if resp, err := client.Get("http://" + addr + "/ping"); err == nil {
			resp.Body.Close()
			break
		}
	}
	openBrowser("http://" + addr + "/start?t=" + token)
	return <-exited
}

// openBrowser opens a page in the desktop's web browser.
func openBrowser(u string) {
	for _, b := range [][]string{{"xdg-open", u}, {"chromium", "--new-window", u}, {"chromium-browser", "--new-window", u}, {"firefox", u}} {
		if p, err := exec.LookPath(b[0]); err == nil {
			if exec.Command(p, b[1:]...).Start() == nil {
				return
			}
		}
	}
}

// webSetup runs the wizard with its questions on a page in the web
// browser, served on this Pi's loopback address, until the page is closed.
func webSetup(ctx context.Context, c *cli, self, listen, forUser string, dry, allowDev bool) error {
	host, _, err := net.SplitHostPort(listen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("setup's page is only served on this Pi's loopback address (127.0.0.1)")
	}
	token, err := c.readLine("")
	if err != nil || len(token) < 32 {
		return errors.New("setup's page needs its token on standard input")
	}
	if !dry && os.Geteuid() != 0 {
		return errors.New("setup needs administrator rights")
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	ui := setup.NewWebUI(token)
	// Everything the page shows also goes to setup's log, to see later what
	// happened (passwords are shown as dots, so they never reach it).
	if !dry {
		if f, err := os.OpenFile(setup.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			defer f.Close()
			fmt.Fprintf(f, "\n######## pi-fleet %s setup, %s\n", version, time.Now().Format(time.RFC1123))
			ui.Log = f
		}
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	go ui.Serve(ctx, ln)
	go func() { <-ctx.Done(); ui.Close() }() // stopped: by another setup, or the Pi shutting down
	go ui.Watch(5 * time.Second)             // its window closed
	// pi-fleet keeps running whatever happens to setup: if setup stops it
	// (to start over, update or move the records) and then ends early, it
	// is started again.
	if !dry && serviceActive() {
		defer func() {
			if !serviceActive() {
				exec.Command("systemctl", "start", "pi-fleet").Run()
			}
		}()
	}
	if others := otherSetups(); len(others) > 0 && !dry {
		ui.Say("Another pi-fleet setup is still running in the background, probably from")
		ui.Say("a setup window closed earlier. Two at once could get in each other's way.")
		ok, err := ui.Confirm("Stop it and continue here?", true)
		if err != nil {
			return err
		}
		if !ok {
			ui.Finish(setup.ErrCancelled)
			<-ui.Closed()
			return setup.ErrCancelled
		}
		stopSetups(others)
		ui.Say("Stopped it.")
	}
	w := &setup.Wizard{
		UI:       ui,
		Sys:      &setup.Real{DryRun: dry, Out: ui, In: ui.Stdin()},
		Self:     self,
		Version:  version,
		AllowDev: allowDev,
		DryRun:   dry,
		SudoUser: forUser,
	}
	done := make(chan error, 1)
	go func() { done <- w.Run() }()
	select {
	case err = <-done:
		ui.Finish(err)
	case <-ui.Closed():
		return setup.ErrCancelled // stopped from the page
	}
	// Keep the page for reading until it's closed (or for half an hour).
	select {
	case <-ui.Closed():
	case <-time.After(30 * time.Minute):
	}
	return err
}

// serviceActive reports whether pi-fleet's service is running.
func serviceActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "pi-fleet").Run() == nil
}

// otherSetups are the process ids of other pi-fleet setups serving a
// page (this one and the one that started it aside).
func otherSetups() []int {
	self, parent := os.Getpid(), os.Getppid()
	dirs, _ := os.ReadDir("/proc")
	var out []int
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid == self || pid == parent {
			continue
		}
		b, err := os.ReadFile("/proc/" + d.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(args) >= 3 && strings.Contains(filepath.Base(args[0]), "pi-fleet") && args[1] == "setup" && slices.Contains(args, "-web") {
			out = append(out, pid)
		}
	}
	return out
}

// stopSetups asks other setups to stop, and makes them if they don't.
func stopSetups(pids []int) {
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	for range 20 {
		time.Sleep(250 * time.Millisecond)
		alive := false
		for _, pid := range pids {
			alive = alive || syscall.Kill(pid, 0) == nil
		}
		if !alive {
			return
		}
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}
