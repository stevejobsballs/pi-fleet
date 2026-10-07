// Package setup is the guided installer behind `pi-fleet setup`: it puts
// pi-fleet on a Raspberry Pi as a master Pi (with its data on an external
// drive) or as an employee or kiosk Pi, asking one question at a time.
package setup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Sys is everything the installer does to the machine. The real one runs
// commands and writes files; with DryRun it only says what it would do,
// and tests use a fake.
type Sys interface {
	// Run runs a command with the installer's terminal, for commands that
	// talk to the user (password prompts) or whose output they should see.
	Run(name string, args ...string) error
	// Output runs a command that changes nothing and returns its output.
	// It runs even in a dry run, so questions can be asked accurately.
	Output(name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte, mode os.FileMode) error
	ReadFile(path string) ([]byte, error)
	Exists(path string) bool
	MkdirAll(path string, mode os.FileMode) error
	Rename(from, to string) error
	Symlink(target, link string) error
	CopyFile(from, to string, mode os.FileMode) error
}

// Real is the machine itself.
type Real struct {
	DryRun bool
	Out    io.Writer
	In     io.Reader
}

func (r *Real) say(format string, args ...any) {
	fmt.Fprintf(r.Out, "    + "+format+"\n", args...)
}

func (r *Real) Run(name string, args ...string) error {
	if r.DryRun {
		r.say("%s", shellJoin(append([]string{name}, args...)))
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.In, r.Out, r.Out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (r *Real) Output(name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (r *Real) WriteFile(path string, data []byte, mode os.FileMode) error {
	if r.DryRun {
		r.say("write %s (%d bytes)", path, len(data))
		return nil
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *Real) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (r *Real) Exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func (r *Real) MkdirAll(path string, mode os.FileMode) error {
	if r.DryRun {
		r.say("mkdir -p %s", path)
		return nil
	}
	return os.MkdirAll(path, mode)
}

func (r *Real) Rename(from, to string) error {
	if r.DryRun {
		r.say("mv %s %s", from, to)
		return nil
	}
	return os.Rename(from, to)
}

func (r *Real) Symlink(target, link string) error {
	if r.DryRun {
		r.say("ln -sfn %s %s", target, link)
		return nil
	}
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

func (r *Real) CopyFile(from, to string, mode os.FileMode) error {
	if r.DryRun {
		r.say("install -m %o %s %s", mode, from, to)
		return nil
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return r.WriteFile(to, b, mode)
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t'\"$`\\|&;<>()*?[]#~") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}
