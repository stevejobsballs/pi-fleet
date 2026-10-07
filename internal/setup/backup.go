package setup

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
)

const (
	// BackupDir is where the master Pi's backup drive is mounted.
	BackupDir = "/srv/pi-fleet-backup"
	// BackupLabel labels the backup drive, so setup can reuse it.
	BackupLabel = "PIFLEET-BACKUP"
	// offsiteMarker is written on a registered off-site disk (it matches
	// the backup package's marker file).
	offsiteMarker = ".pi-fleet-disk.json"
)

// partLabelled returns the disk's ext4 partition with the given label.
func (d Disk) partLabelled(label string) (Part, bool) {
	for _, p := range d.Parts {
		if p.Label == label && p.FSType == "ext4" && p.UUID != "" {
			return p, true
		}
	}
	return Part{}, false
}

// backups sets up the master Pi's backups: a backup drive, the keys that
// encrypt them, the first verified backup, and the off-site disks.
func (w *Wizard) backups(port int) error {
	u := w.UI
	u.Step("Backups")
	u.Say("pi-fleet backs up in two places:")
	u.Say("  - a backup drive that stays plugged into this Pi: a verified copy of")
	u.Say("    everything every night, and new records every 5 minutes;")
	u.Say("  - off-site disks, taken in turn to another building, so a fire or theft")
	u.Say("    here can't destroy every copy.")
	u.Say("Every backup is encrypted. Only the holders of the backup keys you make")
	u.Say("next can open them, not even someone who takes the drives.")
	u.Say("")
	u.Say("You need: a second drive for the backups (not the data drive), a USB")
	u.Say("stick to keep the keys on (not left in this Pi), and two more disks to")
	u.Say("rotate off-site. Off-site disks can be added later by running setup again.")
	if err := w.backupDrive(); err != nil {
		return err
	}
	recipients, err := w.backupKeys()
	if err != nil {
		return err
	}
	args := []string{"backup-config", "-data", DataDir, "-dir", BackupDir}
	for _, r := range recipients {
		args = append(args, "-recipient", r)
	}
	if err := w.asPifleet(args...); err != nil {
		return err
	}
	u.Say("Making the first backup and checking it can be restored…")
	if err := w.asPifleet("backup-now", "-data", DataDir); err != nil {
		return fmt.Errorf("the first backup failed (see above): %w", err)
	}
	if err := w.offsiteDisks(); err != nil {
		return err
	}
	// Older installations' service can't write to the backup drive.
	if unit, _ := w.Sys.ReadFile(UnitPath); string(unit) != MasterUnit(port) {
		if err := w.startService(MasterUnit(port), fmt.Sprintf("https://127.0.0.1:%d/login", port)); err != nil {
			return err
		}
	}
	u.Say("")
	u.Say("Backups are set up. If the backup drive is ever unplugged, no backups are")
	u.Say("made and the master's web pages say so.")
	return nil
}

// backupDrive prepares and mounts the backup drive at BackupDir.
func (w *Wizard) backupDrive() error {
	u := w.UI
	u.Say("")
	u.Say("── The backup drive")
	if isMountpoint(w.Sys, BackupDir) {
		u.Say("A backup drive is already open at %s; using it.", BackupDir)
		return w.Sys.Run("chown", "pifleet:pifleet", BackupDir)
	}
	u.Say("Plug the backup drive into another USB port, leaving the data drive in.")
	for {
		if err := u.Pause("Press Enter when the backup drive is plugged in."); err != nil {
			return err
		}
		w.Sleep(2 * time.Second)
		d, err := w.pickDisk("Which drive should hold the backups? (The data drive isn't listed.)")
		if err != nil {
			return err
		}
		if d == nil {
			continue
		}
		var part Part
		if p, ok := d.partLabelled(BackupLabel); ok {
			reuse, err := u.Confirm("This drive already holds pi-fleet backups. Keep them and carry on using it?", true)
			if err != nil {
				return err
			}
			if reuse {
				part = p
			}
		}
		if part.UUID == "" {
			ok, err := w.confirmErase(*d)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			u.Say("Preparing the backup drive…")
			if part, err = EraseAndFormatAs(w.Sys, *d, BackupLabel); err != nil {
				return fmt.Errorf("preparing the backup drive: %w", err)
			}
		}
		if err := mountWith(w.Sys, BackupDir, "# pi-fleet backup drive", FstabLine(part.UUID, BackupDir), true); err != nil {
			return err
		}
		u.Say("The backup drive opens at %s every time the Pi starts.", BackupDir)
		return w.Sys.Run("chown", "pifleet:pifleet", BackupDir)
	}
}

// backupKeys makes (or takes) the keys backups are encrypted to and
// returns their public halves.
func (w *Wizard) backupKeys() ([]string, error) {
	u := w.UI
	u.Say("")
	u.Say("── Backup keys")
	u.Say("Each key holder can open the backups on their own. Make at least two:")
	u.Say("one for you, and one sealed in an envelope in the other building, in")
	u.Say("case you aren't available. The secret half of a key must never stay on")
	u.Say("this Pi: setup saves it to a USB stick that you then take out and keep")
	u.Say("safe (and preferably also print).")
	holders := []string{"you", "the sealed envelope (escrow)"}
	var recipients []string
	for i := 0; ; i++ {
		if i >= len(holders) {
			more, err := u.Confirm("Add a key for another super user?", false)
			if err != nil {
				return nil, err
			}
			if !more {
				return recipients, nil
			}
			name, err := u.Ask("Whose key is it (a name)", "", nil)
			if err != nil {
				return nil, err
			}
			holders = append(holders, name)
		}
		u.Say("")
		r, err := w.backupKey(holders[i])
		if err != nil {
			return nil, err
		}
		recipients = append(recipients, r)
	}
}

func (w *Wizard) backupKey(holder string) (string, error) {
	u := w.UI
	i, err := u.Choose(fmt.Sprintf("Backup key for %s:", holder), []string{
		"Make a new key and save it on a USB stick",
		"Use a key made before: paste its public half (starts with age1)",
	})
	if err != nil {
		return "", err
	}
	if i == 1 {
		return u.Ask("Public key", "", func(s string) error {
			_, err := age.ParseX25519Recipient(s)
			return err
		})
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	pub := id.Recipient().String()
	// The standard age key file, which pi-fleet restore -identity reads
	// as it is: every line but the key itself is a # comment.
	text := fmt.Sprintf(`# pi-fleet backup key for %s, made %s
#
# KEEP THIS SECRET. Anyone with the AGE-SECRET-KEY line can read the backups.
# Keep this stick (and a printed copy) somewhere safe, never in the master Pi.
# To restore, give this file to: pi-fleet restore -identity <this file>
# (see "Disaster recovery" in pi-fleet's docs/OPERATIONS.md).
#
# public key: %s
%s
`, holder, w.Now().Format("2006-01-02"), pub, id.String())
	dir, err := w.pickStick(holder)
	if err != nil {
		return "", err
	}
	if dir == "" {
		u.Say("")
		u.Say("Write this down exactly, or print it, now. It is not saved anywhere:")
		u.Say("")
		u.Say("    %s", id.String())
		u.Say("")
		ok, err := u.Confirm("Have you written it down or printed it?", false)
		if err != nil || !ok {
			if err == nil {
				err = ErrCancelled
			}
			return "", err
		}
		return pub, nil
	}
	name := "pi-fleet-backup-key-" + slug(holder) + ".txt"
	path := filepath.Join(dir, name)
	for n := 2; w.Sys.Exists(path); n++ {
		path = filepath.Join(dir, fmt.Sprintf("pi-fleet-backup-key-%s-%d.txt", slug(holder), n))
	}
	if err := w.Sys.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	if back, err := w.Sys.ReadFile(path); err == nil && string(back) != text && !w.DryRun {
		return "", fmt.Errorf("the key saved to %s doesn't read back correctly; try another stick", path)
	}
	w.Sys.Run("sync")
	u.Say("Saved the key for %s to %s.", holder, path)
	u.Say("Take the stick out when setup finishes and keep it safe; print the file if you can.")
	return pub, nil
}

// pickStick asks which plugged-in USB stick to save a key to; "" means
// none (the key is shown on screen instead).
func (w *Wizard) pickStick(holder string) (string, error) {
	u := w.UI
	for {
		out, err := w.Sys.Output("lsblk", LsblkArgs...)
		if err != nil {
			return "", err
		}
		disks, err := ParseDisks(out)
		if err != nil {
			return "", err
		}
		var dirs, opts []string
		for _, d := range disks {
			if d.holds(DataDir) || d.holds(BackupDir) {
				continue
			}
			for _, p := range d.Parts {
				for _, m := range p.Mounts {
					if strings.HasPrefix(m, "/media/") && !strings.HasPrefix(m, "/media/OFFSITE-") {
						dirs = append(dirs, m)
						opts = append(opts, fmt.Sprintf("%s (%s, %s)", m, d.Model, humanSize(d.Size)))
					}
				}
			}
		}
		opts = append(opts, "Plug a stick in now, then look again", "No stick: show the key on screen to write down")
		i, err := u.Choose(fmt.Sprintf("Where should the key for %s be saved?", holder), opts)
		if err != nil {
			return "", err
		}
		switch {
		case i < len(dirs):
			return dirs[i], nil
		case i == len(dirs):
			if err := u.Pause("Plug the stick in, wait for it to open, then press Enter."); err != nil {
				return "", err
			}
			continue
		default:
			return "", nil
		}
	}
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// OffsiteFstabLine mounts an off-site disk by its label when the backup
// service needs it, and hides it from the desktop.
func OffsiteFstabLine(label string) string {
	return fmt.Sprintf("LABEL=%[1]s  /media/%[1]s  ext4  noauto,nofail,noatime,x-gvfs-hide,x-systemd.device-timeout=10s  0  0", label)
}

// offsiteDisks prepares and registers the off-site disks, writes the first
// backup to each, and installs the automatic write on plug-in.
func (w *Wizard) offsiteDisks() error {
	u := w.UI
	u.Say("")
	u.Say("── Off-site disks")
	u.Say("Off-site disks take turns: one is plugged in here to receive a backup,")
	u.Say("then goes to the other building, and the one there comes back. Two disks")
	u.Say("(OFFSITE-A and OFFSITE-B) are the minimum. Each needs to be at least as")
	u.Say("big as the data drive.")
	a, err := u.Ask("How many off-site disks to prepare now (0 to do it later)", "2", func(s string) error {
		if n, err := strconv.Atoi(s); err != nil || n < 0 || n > 9 {
			return fmt.Errorf("type a number from 0 to 9")
		}
		return nil
	})
	if err != nil {
		return err
	}
	n, _ := strconv.Atoi(a)
	if n == 0 {
		return w.installOffsiteTrigger()
	}
	// While disks are being prepared, plugging one in mustn't start a write.
	w.Sys.Run("rm", "-f", OffsiteRulePath)
	w.Sys.Run("udevadm", "control", "--reload-rules")
	fstab, _ := w.Sys.ReadFile("/etc/fstab")
	next := 0 // continue after disks prepared before
	for next < 26 && strings.Contains(string(fstab), OffsiteFstabLine(offsiteLabel(next))) {
		next++
	}
	for i := 0; i < n; i++ {
		if next+i >= 26 {
			return fmt.Errorf("pi-fleet setup names off-site disks OFFSITE-A to OFFSITE-Z")
		}
		if err := w.offsiteDisk(offsiteLabel(next + i)); err != nil {
			return err
		}
	}
	if err := w.installOffsiteTrigger(); err != nil {
		return err
	}
	u.Say("")
	u.Say("How the rotation works:")
	u.Say("  1. Take one off-site disk to the other building now.")
	u.Say("  2. When a disk reaches the other building, record it on the master Pi:")
	u.Say("       sudo -u pifleet %s offsite-confirm -data %s -label OFFSITE-A -as <your username>", Binary, DataDir)
	u.Say("  3. Every week, bring the other disk back and plug it into this Pi. pi-fleet")
	u.Say("     writes a fresh, checked backup to it by itself, then closes it; when it")
	u.Say("     disappears from the desktop (a few minutes), unplug it and swap again.")
	u.Say("The master's web pages warn when an off-site copy is overdue.")
	return nil
}

func offsiteLabel(i int) string { return "OFFSITE-" + string(rune('A'+i)) }

func (w *Wizard) offsiteDisk(label string) error {
	u := w.UI
	u.Say("")
	u.Say("Plug in the disk to become %s (write %s on it with a marker).", label, label)
	for {
		if err := u.Pause("Press Enter when it is plugged in."); err != nil {
			return err
		}
		w.Sleep(2 * time.Second)
		d, err := w.pickDisk(fmt.Sprintf("Which disk is %s?", label))
		if err != nil {
			return err
		}
		if d == nil {
			continue
		}
		dir := "/media/" + label
		_, prepared := d.partLabelled(label)
		if !prepared {
			ok, err := w.confirmErase(*d)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			u.Say("Preparing %s…", label)
			if _, err := EraseAndFormatAs(w.Sys, *d, label); err != nil {
				return fmt.Errorf("preparing %s: %w", label, err)
			}
		} else {
			for _, p := range d.Parts {
				for _, m := range p.Mounts {
					if m != dir {
						w.Sys.Run("umount", m)
					}
				}
			}
		}
		if err := mountWith(w.Sys, dir, "# pi-fleet off-site disk "+label, OffsiteFstabLine(label), true); err != nil {
			return err
		}
		if err := w.Sys.Run("chown", "pifleet:pifleet", dir); err != nil {
			return err
		}
		if !w.Sys.Exists(dir + "/" + offsiteMarker) {
			if err := w.asPifleet("offsite-register", "-data", DataDir, "-disk", dir, "-label", label); err != nil {
				return err
			}
		}
		u.Say("Writing the first backup to %s and checking it…", label)
		if err := w.asPifleet("offsite-write", "-data", DataDir, "-disk", dir); err != nil {
			w.Sys.Run("umount", dir)
			return fmt.Errorf("writing to %s: %w", label, err)
		}
		if err := w.Sys.Run("umount", dir); err != nil {
			return err
		}
		u.Say("%s is ready; unplug it.", label)
		return nil
	}
}

func (w *Wizard) installOffsiteTrigger() error {
	if err := w.Sys.WriteFile(OffsiteUnitPath, []byte(OffsiteUnit()), 0o644); err != nil {
		return err
	}
	if err := w.Sys.WriteFile(OffsiteRulePath, []byte(OffsiteRule), 0o644); err != nil {
		return err
	}
	if err := w.Sys.Run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return w.Sys.Run("udevadm", "control", "--reload-rules")
}
