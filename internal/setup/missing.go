package setup

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// fstabUUID returns the UUID /etc/fstab mounts at dir, if any.
func fstabUUID(fstab, dir string) string {
	for _, l := range strings.Split(fstab, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[1] == dir && strings.HasPrefix(f[0], "UUID=") {
			return strings.TrimPrefix(f[0], "UUID=")
		}
	}
	return ""
}

// protectedUUIDs are the file systems /etc/fstab gives the master Pi's
// data and backup drives. Setup never offers them for erasing, even when
// they aren't open (pulled out, or failed to mount).
func (w *Wizard) protectedUUIDs() map[string]bool {
	fstab, _ := w.Sys.ReadFile("/etc/fstab")
	p := map[string]bool{}
	for _, dir := range []string{DataDir, BackupDir} {
		if u := fstabUUID(string(fstab), dir); u != "" {
			p[u] = true
		}
	}
	return p
}

func (d Disk) hasUUID(uuids map[string]bool) bool {
	for _, p := range d.Parts {
		if uuids[p.UUID] {
			return true
		}
	}
	return false
}

// sdCopies lists the copies of the records that moves left on the SD
// card, newest first.
func (w *Wizard) sdCopies() []string {
	entries, err := w.ReadDir(filepath0(DataDir))
	if err != nil {
		return nil
	}
	base := DataDir[strings.LastIndexByte(DataDir, '/')+1:]
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base+".on-sd-card-") {
			out = append(out, filepath0(DataDir)+"/"+e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

func filepath0(p string) string { return p[:strings.LastIndexByte(p, '/')] }

// missingDataDrive handles a master Pi whose data drive is in /etc/fstab
// but isn't open. It returns true when the drive is open again and setup
// can carry on as usual.
func (w *Wizard) missingDataDrive(port int, uuid string) (bool, error) {
	u := w.UI
	u.Say("")
	u.Say("The master Pi's data drive isn't open. It holds the records, so")
	u.Say("pi-fleet can't run without it. (Was it unplugged, or did it lose power?)")
	for {
		w.Sys.Run("systemctl", "daemon-reload")
		if w.Sys.Exists("/dev/disk/by-uuid/"+uuid) && w.Sys.Run("mount", DataDir) == nil && isMountpoint(w.Sys, DataDir) {
			u.Say("Found the data drive and opened it.")
			if err := w.restart(fmt.Sprintf("https://127.0.0.1:%d/login", port)); err != nil {
				return false, err
			}
			return true, nil
		}
		if w.Sys.Exists("/dev/disk/by-uuid/" + uuid) {
			u.Say("The data drive is plugged in but couldn't be opened; it may be damaged.")
			u.Say("Setup won't touch it. Shut the Pi down, plug the drive in again and")
			u.Say("start the Pi; if that doesn't help, ask for help before doing anything else.")
			return false, ErrCancelled
		}
		opts := []string{"I've plugged the data drive back in: look again"}
		copies := w.sdCopies()
		for _, c := range copies {
			opts = append(opts, "The data drive is lost: restore the records from the SD card copy "+c)
		}
		opts = append(opts, "Stop")
		i, err := u.Choose("The data drive isn't plugged in. What would you like to do?", opts)
		if err != nil {
			return false, err
		}
		switch {
		case i == 0:
			w.Sleep(2 * time.Second)
			continue
		case i <= len(copies):
			return false, w.restoreSDCopy(port, copies[i-1])
		default:
			return false, ErrCancelled
		}
	}
}

// restoreSDCopy puts the records back from a copy on the SD card (left by
// an earlier move to a drive) and then moves them onto a drive. The copy
// itself is left as it is.
func (w *Wizard) restoreSDCopy(port int, copy string) error {
	u := w.UI
	u.Step("Restore the records from the SD card")
	u.Say("Records added after %s was made are not in it and are lost.", copy)
	ok, err := u.Confirm("Restore the records from it?", false)
	if err != nil || !ok {
		if err == nil {
			err = ErrCancelled
		}
		return err
	}
	w.Sys.Run("systemctl", "stop", "pi-fleet")
	// The lost drive's line goes from /etc/fstab, so the Pi doesn't wait
	// for it at start-up.
	fstab, err := w.Sys.ReadFile("/etc/fstab")
	if err != nil {
		return err
	}
	if err := w.Sys.WriteFile("/etc/fstab", []byte(commentFstab(string(fstab), DataDir)), 0o644); err != nil {
		return err
	}
	w.Sys.Run("systemctl", "daemon-reload")
	if entries, err := w.ReadDir(DataDir); err == nil && len(entries) > 0 {
		aside := fmt.Sprintf("%s.set-aside-%s", DataDir, w.Now().Format("2006-01-02-150405"))
		if err := w.Sys.Rename(DataDir, aside); err != nil {
			return err
		}
	} else if w.Sys.Exists(DataDir) {
		if err := w.Sys.Run("rmdir", DataDir); err != nil {
			return err
		}
	}
	if err := w.Sys.Run("cp", "-a", copy, DataDir); err != nil {
		return err
	}
	u.Say("Checking the restored records…")
	if err := w.Sys.Run("runuser", "-u", "pifleet", "--", Binary, "selfcheck", "-data", DataDir); err != nil {
		return fmt.Errorf("the restored records didn't pass their check (see above); the copy at %s is untouched: %w", copy, err)
	}
	u.Say("The records are back, from %s. Now put them on a drive.", copy)
	return w.moveToDrive(port, false)
}

// commentFstab turns the lines that mount at dir into comments.
func commentFstab(fstab, dir string) string {
	lines := strings.Split(strings.TrimRight(fstab, "\n"), "\n")
	for i, l := range lines {
		f := strings.Fields(l)
		if len(f) >= 2 && !strings.HasPrefix(f[0], "#") && f[1] == dir {
			lines[i] = "# drive lost, removed by pi-fleet setup: " + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
