package setup

import (
	"path"
	"strings"
)

// Opening pi-fleet on an employee or kiosk Pi: an entry in the Pi's menu,
// an icon on the desktop of whoever set it up, and on a kiosk Pi, the
// browser full screen on pi-fleet whenever the Pi starts.

const (
	LauncherPath  = "/opt/pi-fleet/open-pi-fleet"
	MenuEntryPath = "/usr/share/applications/pi-fleet.desktop"
)

// launcherScript opens pi-fleet in a browser once it answers. With
// --kiosk it fills the screen (Alt+F4 closes it).
const launcherScript = `#!/bin/bash
# Opens pi-fleet in the web browser (written by pi-fleet setup).
# With --kiosk: full screen, for a shared kiosk Pi; Alt+F4 closes it.
url=http://127.0.0.1:8080/
# At start-up, wait until pi-fleet answers (up to 2 minutes).
for _ in $(seq 1 60); do
  (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null && break
  sleep 2
done
kiosk=0
[ "${1:-}" = "--kiosk" ] && kiosk=1
for b in chromium chromium-browser firefox; do
  command -v "$b" >/dev/null || continue
  if [ "$kiosk" = 1 ]; then
    case "$b" in
      firefox) exec firefox --kiosk "$url" ;;
      *) exec "$b" --kiosk --noerrdialogs --disable-session-crashed-bubble --no-first-run "$url" ;;
    esac
  fi
  exec "$b" --new-window "$url"
done
exec xdg-open "$url"
`

func desktopEntry(kiosk bool) string {
	exec, comment := LauncherPath, "Equipment and maintenance records"
	if kiosk {
		exec += " --kiosk"
	}
	return "[Desktop Entry]\nType=Application\nName=pi-fleet\nComment=" + comment + "\nExec=" + exec +
		"\nIcon=web-browser\nTerminal=false\nCategories=Office;\n"
}

// launchers installs the menu entry and the desktop icon, and on a kiosk
// Pi opens pi-fleet at start-up. Failing here doesn't stop setup: pi-fleet
// still works by typing its address.
func (w *Wizard) launchers(kiosk bool) {
	u := w.UI
	if err := w.Sys.WriteFile(LauncherPath, []byte(launcherScript), 0o755); err != nil {
		u.Say("Couldn't add the pi-fleet icon (%v). Open http://127.0.0.1:8080 in the web browser instead.", err)
		return
	}
	if err := w.Sys.WriteFile(MenuEntryPath, []byte(desktopEntry(false)), 0o644); err != nil {
		u.Say("Couldn't add pi-fleet to the menu: %v", err)
	}
	home := w.userHome()
	if home == "" {
		return // no desktop user known (setup run as root)
	}
	owner := w.SudoUser + ":"
	if desk := home + "/Desktop"; w.Sys.Exists(desk) {
		p := desk + "/pi-fleet.desktop"
		if err := w.Sys.WriteFile(p, []byte(desktopEntry(false)), 0o755); err == nil {
			w.Sys.Run("chown", owner, p)
			u.Say("Added a pi-fleet icon to the desktop: double-click it to open pi-fleet.")
		}
	}
	if !kiosk {
		return
	}
	dir := home + "/.config/autostart"
	for _, d := range []string{home + "/.config", dir} {
		if !w.Sys.Exists(d) {
			if err := w.Sys.MkdirAll(d, 0o755); err != nil {
				u.Say("Couldn't set pi-fleet to open at start-up: %v", err)
				return
			}
			w.Sys.Run("chown", owner, d)
		}
	}
	p := dir + "/pi-fleet.desktop"
	if err := w.Sys.WriteFile(p, []byte(desktopEntry(true)), 0o644); err != nil {
		u.Say("Couldn't set pi-fleet to open at start-up: %v", err)
		return
	}
	w.Sys.Run("chown", owner, p)
	u.Say("When this Pi starts, pi-fleet opens full screen (Alt+F4 closes it).")
}

// userHome is the home folder of whoever ran setup with sudo ("" if
// unknown).
func (w *Wizard) userHome() string {
	if w.SudoUser == "" || w.SudoUser == "root" {
		return ""
	}
	out, err := w.Sys.Output("getent", "passwd", w.SudoUser)
	if err != nil {
		return ""
	}
	f := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(f) < 6 || !path.IsAbs(f[5]) {
		return ""
	}
	return path.Clean(f[5])
}
