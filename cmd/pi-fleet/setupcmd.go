package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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
	if err := fs.Parse(args); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, isFile := c.stdin.(*os.File)
	interactive := isFile && term.IsTerminal(int(in.Fd()))
	// Double-clicked: there is no terminal to ask questions in, so open one.
	if !interactive && (os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "") {
		if t, err := exec.LookPath("x-terminal-emulator"); err == nil {
			cmd := shellQuote(append([]string{self, "setup", "-pause"}, args...))
			return syscall.Exec(t, []string{t, "-t", "pi-fleet setup", "-e", cmd}, os.Environ())
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
