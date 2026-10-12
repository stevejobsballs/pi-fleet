package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrCancelled is returned when the user stops the installer.
var ErrCancelled = errors.New("setup stopped; nothing more was changed. Run it again to carry on")

// Prompter is how the wizard talks to the person setting up: the terminal
// (UI) or a page in the web browser (WebUI).
type Prompter interface {
	Say(format string, args ...any)
	// Step starts a numbered section.
	Step(title string)
	// Ask reads an answer, offering def, until check accepts it.
	Ask(question, def string, check func(string) error) (string, error)
	// Confirm asks a yes/no question.
	Confirm(question string, def bool) (bool, error)
	// Choose shows options and returns the index picked.
	Choose(question string, options []string) (int, error)
	// Pause waits until the person carries on.
	Pause(prompt string) error
	// Line reads a free answer, which may be empty.
	Line(prompt string) (string, error)
	// Progress shows that a wait goes on (a dot on the terminal).
	Progress()
}

// UI asks questions on the terminal, one at a time.
type UI struct {
	in   *bufio.Reader
	out  io.Writer
	step int
}

func NewUI(in io.Reader, out io.Writer) *UI {
	return &UI{in: bufio.NewReader(in), out: out}
}

func (u *UI) Say(format string, args ...any) { fmt.Fprintf(u.out, format+"\n", args...) }

// Step starts a numbered section.
func (u *UI) Step(title string) {
	u.step++
	fmt.Fprintf(u.out, "\n── Step %d: %s %s\n\n", u.step, title, strings.Repeat("─", max(3, 60-len(title))))
}

// Line reads a free answer, which may be empty.
func (u *UI) Line(prompt string) (string, error) {
	fmt.Fprint(u.out, prompt)
	s, err := u.in.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", ErrCancelled
		}
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// Ask reads an answer, offering def when the user just presses Enter, and
// asks again until check accepts it (check may be nil).
func (u *UI) Ask(question, def string, check func(string) error) (string, error) {
	for {
		prompt := question + ": "
		if def != "" {
			prompt = fmt.Sprintf("%s [%s]: ", question, def)
		}
		a, err := u.Line(prompt)
		if err != nil {
			return "", err
		}
		if a == "" {
			a = def
		}
		if a == "" {
			u.Say("  Please type an answer.")
			continue
		}
		if check != nil {
			if err := check(a); err != nil {
				u.Say("  %v", err)
				continue
			}
		}
		return a, nil
	}
}

// Confirm asks a yes/no question.
func (u *UI) Confirm(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		a, err := u.Line(fmt.Sprintf("%s [%s]: ", question, hint))
		if err != nil {
			return false, err
		}
		switch strings.ToLower(a) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		u.Say("  Please answer y or n.")
	}
}

// Choose shows numbered options and returns the index picked.
func (u *UI) Choose(question string, options []string) (int, error) {
	u.Say("%s", question)
	for i, o := range options {
		u.Say("  %d) %s", i+1, o)
	}
	for {
		a, err := u.Line(fmt.Sprintf("Choose 1-%d: ", len(options)))
		if err != nil {
			return 0, err
		}
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		u.Say("  Please type a number from 1 to %d.", len(options))
	}
}

// Progress prints a dot while a wait goes on.
func (u *UI) Progress() { fmt.Fprint(u.out, ".") }

// Pause waits for Enter.
func (u *UI) Pause(prompt string) error {
	_, err := u.Line(prompt + " ")
	return err
}
