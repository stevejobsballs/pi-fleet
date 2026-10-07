package setup

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/release"
)

// Wizard is one run of the installer.
type Wizard struct {
	UI      *UI
	Sys     Sys
	Self    string // this program's file, copied into place
	Version string // this program's version
	// AllowDev lets a development build install (for testing only:
	// development builds can't be updated by signed releases cleanly).
	AllowDev bool
	DryRun   bool
	SudoUser string // who ran sudo, for suggestions
	Now      func() time.Time
	ReadDir  func(string) ([]os.DirEntry, error)
	// Addrs returns this Pi's network addresses (replaced in tests).
	Addrs func() []net.IP
	// Health checks that a freshly started service answers.
	Health func(url string) error
	// Fetch gets the certificate a master Pi presents.
	Fetch func(hostport string) ([]byte, error)
	// Sleep waits between checks (replaced in tests).
	Sleep func(time.Duration)
}

func (w *Wizard) defaults() {
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.ReadDir == nil {
		w.ReadDir = os.ReadDir
	}
	if w.Addrs == nil {
		w.Addrs = localAddrs
	}
	if w.Health == nil {
		w.Health = healthCheck
	}
	if w.Fetch == nil {
		w.Fetch = func(hp string) ([]byte, error) { return FetchCertificate(hp, 8*time.Second) }
	}
	if w.Sleep == nil {
		w.Sleep = time.Sleep
	}
}

// Run guides the user through the whole installation.
func (w *Wizard) Run() error {
	w.defaults()
	u := w.UI
	u.Say("pi-fleet %s setup", w.Version)
	u.Say("")
	u.Say("This sets up pi-fleet on this Raspberry Pi, one step at a time. You can")
	u.Say("stop at any question with Ctrl+C; running setup again carries on from")
	u.Say("where it stopped.")
	if w.DryRun {
		u.Say("")
		u.Say("DRY RUN: nothing will be changed. Commands are shown instead.")
	}
	if err := w.preflight(); err != nil {
		return err
	}
	if unit, err := w.Sys.ReadFile(UnitPath); err == nil {
		switch {
		case strings.Contains(string(unit), " serve "):
			return w.existingMaster(string(unit))
		case strings.Contains(string(unit), " run "):
			return w.existingNode()
		}
	}
	u.Say("")
	role, err := u.Choose("What will this Pi be?", []string{
		"Master Pi: the central Pi that keeps every record for your institution (choose this for the first Pi)",
		"Employee Pi: one person's Pi, which works offline and syncs with the master Pi",
		"Kiosk Pi: a shared Pi in a workshop, used by a group of people",
	})
	if err != nil {
		return err
	}
	if role == 0 {
		return w.newMaster()
	}
	return w.newNode(role == 2)
}

func (w *Wizard) preflight() error {
	if runtime.GOOS != "linux" || !w.Sys.Exists("/run/systemd/system") {
		return errors.New("pi-fleet needs Linux with systemd, such as Raspberry Pi OS")
	}
	if m, err := w.Sys.ReadFile("/proc/device-tree/model"); err == nil {
		w.UI.Say("")
		w.UI.Say("This is a %s.", strings.TrimRight(strings.TrimSpace(string(m)), "\x00"))
	}
	if !releaseVersion(w.Version) && !w.AllowDev {
		return fmt.Errorf("this is a development build (%s); install a signed release (vX.Y.Z)", w.Version)
	}
	return nil
}

var versionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

func releaseVersion(v string) bool { return versionRE.MatchString(v) }

// --- master Pi ---

func (w *Wizard) newMaster() error {
	u := w.UI
	u.Say("")
	u.Say("The master Pi is the framework: the program runs from its SD card, and")
	u.Say("every record (equipment, work orders, inventory, files) lives on an")
	u.Say("external drive. If this Pi ever fails, plug the drive into a new Pi and")
	u.Say("run setup again to carry on where it stopped.")
	if err := w.dependencies(true); err != nil {
		return err
	}
	host, err := w.hostname("fleet-master")
	if err != nil {
		return err
	}
	u.Step("External drive for the data")
	reused, trial, err := w.dataDrive()
	if err != nil {
		return err
	}
	if err := w.program(DataDir); err != nil {
		return err
	}
	if err := w.chownData(); err != nil {
		return err
	}
	if err := w.markTrial(DataDir, trial); err != nil {
		return err
	}
	fp, err := w.certificate(host)
	if err != nil {
		return err
	}
	if err := w.records(reused); err != nil {
		return err
	}
	port := 443
	if err := w.startService(MasterUnit(port), fmt.Sprintf("https://127.0.0.1:%d/login", port)); err != nil {
		return err
	}
	u.Say("")
	now, err := u.Confirm("Set up backups now? (Recommended. You can also run setup again later.)", true)
	if err != nil {
		return err
	}
	if now {
		if err := w.backups(port); err != nil {
			return err
		}
	}
	w.masterDone(host, port, fp, now)
	if trial {
		w.trialWarning()
	}
	return nil
}

// dataDrive asks for the external drive, prepares it and mounts it at
// DataDir. It reports whether an earlier pi-fleet data drive was reused.
func (w *Wizard) dataDrive() (reused, trial bool, err error) {
	u := w.UI
	if isMountpoint(w.Sys, DataDir) {
		u.Say("A drive is already open at %s; using it.", DataDir)
		return w.Sys.Exists(DataDir + "/pi-fleet.db"), w.Sys.Exists(DataDir + "/" + TrialMarker), nil
	}
	u.Say("Plug the external drive into one of the Pi's blue USB 3 ports now.")
	u.Say("A USB SSD of 250 GB or more is best. If the drive needs its own power")
	u.Say("supply, plug that in too.")
	for {
		if err := u.Pause("Press Enter when the drive is plugged in."); err != nil {
			return false, false, err
		}
		w.Sleep(2 * time.Second) // let the system notice it
		disk, err := w.pickDisk("Which drive should hold pi-fleet's data?")
		if err != nil {
			return false, false, err
		}
		if disk == nil {
			continue
		}
		trial = disk.stickLike()
		if trial {
			ok, err := w.confirmTrial(*disk)
			if err != nil {
				return false, false, err
			}
			if !ok {
				continue
			}
		}
		if part, ok := disk.DataPart(); ok {
			u.Say("")
			u.Say("This drive was prepared for pi-fleet before (it is labelled %s).", DataLabel)
			reuse, err := u.Confirm("Use it as it is, keeping its records?", true)
			if err != nil {
				return false, false, err
			}
			if reuse {
				if err := MountData(w.Sys, part.UUID, DataDir); err != nil {
					return false, false, err
				}
				has := w.Sys.Exists(DataDir + "/pi-fleet.db")
				if has {
					u.Say("Found the master Pi's records on the drive; they will be kept.")
				}
				return has, trial, nil
			}
		}
		if ok, err := w.confirmErase(*disk); err != nil || !ok {
			if err != nil {
				return false, false, err
			}
			continue
		}
		u.Say("Preparing the drive. This takes a minute or so…")
		part, err := EraseAndFormat(w.Sys, *disk)
		if err != nil {
			return false, false, fmt.Errorf("preparing the drive: %w", err)
		}
		if err := MountData(w.Sys, part.UUID, DataDir); err != nil {
			return false, false, err
		}
		u.Say("The drive is ready and opens at %s every time the Pi starts.", DataDir)
		return false, trial, nil
	}
}

// pickDisk lists the drives and asks which to use; nil means look again.
func (w *Wizard) pickDisk(question string) (*Disk, error) {
	u := w.UI
	out, err := w.Sys.Output("lsblk", LsblkArgs...)
	if err != nil {
		return nil, err
	}
	disks, err := ParseDisks(out)
	if err != nil {
		return nil, err
	}
	protected := w.protectedUUIDs()
	var usable []Disk
	for _, d := range disks {
		if d.Size >= MinDataDrive && !d.holds(DataDir) && !d.holds(BackupDir) && !d.hasUUID(protected) {
			usable = append(usable, d)
		}
	}
	if len(usable) == 0 {
		u.Say("")
		u.Say("No external drive found (drives under %s are left out).", humanSize(MinDataDrive))
		u.Say("Check the cable and power, or try another USB port.")
		return nil, nil
	}
	opts := make([]string, 0, len(usable)+1)
	for _, d := range usable {
		opts = append(opts, d.Describe())
	}
	opts = append(opts, "None of these: look again")
	u.Say("")
	i, err := u.Choose(question, opts)
	if err != nil || i == len(usable) {
		return nil, err
	}
	return &usable[i], nil
}

// confirmErase shows what is on a drive and asks the user to type ERASE.
func (w *Wizard) confirmErase(d Disk) (bool, error) {
	u := w.UI
	u.Say("")
	u.Say("Preparing %s ERASES EVERYTHING ON IT.", d.Describe())
	if c := d.Contents(w.ReadDir); len(c) > 0 {
		u.Say("It holds:")
		for _, l := range c {
			u.Say("  %s", l)
		}
		if strings.Contains(strings.ToLower(strings.Join(c, " ")), "release key") {
			u.Say("")
			u.Say("WARNING: it seems to hold a pi-fleet release signing key. Without that")
			u.Say("key no new version of pi-fleet can be signed. Don't erase it.")
		}
	}
	if d.Size < SmallDataDrive {
		u.Say("")
		u.Say("This drive is small (%s); it may be a USB stick rather than a drive.", humanSize(d.Size))
		u.Say("Make sure it isn't a stick you need, such as one holding keys or backups.")
	}
	a, err := u.line("Type ERASE to erase it, or just press Enter to choose again: ")
	if err != nil {
		return false, err
	}
	return a == "ERASE", nil
}

func (w *Wizard) chownData() error {
	return w.Sys.Run("chown", "pifleet:pifleet", DataDir)
}

// certificate makes the HTTPS certificate on the data drive, unless there
// is one, and returns its fingerprint.
func (w *Wizard) certificate(host string) (string, error) {
	u := w.UI
	u.Step("Secure connection (HTTPS)")
	certPath := DataDir + "/tls/cert.pem"
	if old, err := w.Sys.ReadFile(certPath); err == nil {
		u.Say("Keeping the certificate already on the drive.")
		return Fingerprint(old)
	}
	// An earlier install kept it on the SD card; move it with the data.
	if old, err := w.Sys.ReadFile("/etc/pi-fleet/tls/cert.pem"); err == nil {
		if key, err := w.Sys.ReadFile("/etc/pi-fleet/tls/key.pem"); err == nil && browsersAccept(old) {
			if err := w.writeTLS(old, key); err != nil {
				return "", err
			}
			u.Say("Moved this Pi's existing certificate onto the drive.")
			return Fingerprint(old)
		}
	}
	names := []string{host + ".local", host}
	cert, key, err := NewCertificate(names, w.Addrs(), w.Now())
	if err != nil {
		return "", err
	}
	if err := w.writeTLS(cert, key); err != nil {
		return "", err
	}
	u.Say("Made a certificate for %s.", strings.Join(names, " and "))
	return Fingerprint(cert)
}

func (w *Wizard) writeTLS(cert, key []byte) error {
	dir := DataDir + "/tls"
	if err := w.Sys.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := w.Sys.WriteFile(dir+"/cert.pem", cert, 0o644); err != nil {
		return err
	}
	if err := w.Sys.WriteFile(dir+"/key.pem", key, 0o600); err != nil {
		return err
	}
	return w.Sys.Run("chown", "-R", "pifleet:pifleet", dir)
}

// records creates the master Pi's database and first super user.
func (w *Wizard) records(reused bool) error {
	u := w.UI
	u.Step("Records and the first super user")
	if reused || w.Sys.Exists(DataDir+"/pi-fleet.db") {
		u.Say("The records on the drive are kept.")
	} else if err := w.asPifleet("init", "-data", DataDir, "-role", "central"); err != nil {
		return err
	}
	u.Say("")
	u.Say("The super user runs pi-fleet: they create everyone's accounts and")
	u.Say("approve each new Pi. This is probably you.")
	for {
		if out, err := w.Sys.Output("runuser", "-u", "pifleet", "--", Binary, "bootstrap", "-data", DataDir, "-check"); err == nil && strings.Contains(string(out), "has a super user") {
			u.Say("This master Pi already has a super user.")
			return nil
		}
		user, err := u.Ask("Username (lowercase, e.g. jsmith)", "", domain.CheckUsername)
		if err != nil {
			return err
		}
		name, err := u.Ask("Full legal name (shown on electronic signatures)", "", nil)
		if err != nil {
			return err
		}
		email, err := u.Ask("Work email", "", func(s string) error {
			if a, err := mail.ParseAddress(s); err != nil || a.Address != s {
				return errors.New("that isn't an email address")
			}
			return nil
		})
		if err != nil {
			return err
		}
		u.Say("Now choose a password: at least 12 characters, not containing the username,")
		u.Say("and not one that has appeared in a data breach (pi-fleet checks).")
		if err := w.asPifleet("bootstrap", "-data", DataDir, "-username", user, "-name", name, "-email", email); err == nil {
			return nil
		}
		if w.DryRun {
			return nil
		}
		u.Say("That didn't work (see the message above). Let's try again.")
	}
}

func (w *Wizard) masterDone(host string, port int, fingerprint string, backups bool) {
	u := w.UI
	addr := "https://" + host + ".local"
	if port != 443 {
		addr += ":" + strconv.Itoa(port)
	}
	u.Step("Done")
	u.Say("The master Pi is running. Open this address in a web browser on any")
	u.Say("computer on the same network:")
	u.Say("")
	u.Say("    %s", addr)
	u.Say("")
	u.Say("The browser warns that the connection isn't private the first time,")
	u.Say("because the certificate was made here rather than bought. Choose")
	u.Say("Advanced, then continue.")
	u.Say("")
	u.Say("Certificate fingerprint (employee Pis ask you to check it):")
	u.Say("    %s", fingerprint)
	u.Say("It is also shown on the Pis page of the web interface.")
	u.Say("")
	u.Say("Next, in the web interface: create your sites and locations, then the")
	u.Say("users. To add an employee Pi, run this same setup file on it and choose")
	u.Say("Employee Pi.")
	u.Say("")
	u.Say("The records are on the external drive. Keep it plugged in: pi-fleet")
	u.Say("won't start without it, so nothing is ever written to the SD card.")
	if !backups {
		u.Say("Backups aren't set up yet: run setup again and choose Set up backups.")
	}
}

// --- an existing master Pi ---

var listenRE = regexp.MustCompile(`-listen :(\d+)`)

func (w *Wizard) existingMaster(unit string) error {
	u := w.UI
	port := 443
	if m := listenRE.FindStringSubmatch(unit); m != nil {
		port, _ = strconv.Atoi(m[1])
	}
	onDrive := isMountpoint(w.Sys, DataDir)
	u.Say("")
	u.Say("pi-fleet is already set up here as the master Pi.")
	if fstab, _ := w.Sys.ReadFile("/etc/fstab"); !onDrive {
		if uuid := fstabUUID(string(fstab), DataDir); uuid != "" {
			back, err := w.missingDataDrive(port, uuid)
			if err != nil || !back {
				return err
			}
			onDrive = true
		}
	}
	if d, ok := w.dataDisk(); ok && d.stickLike() && !w.Sys.Exists(DataDir+"/"+TrialMarker) {
		// Set up before trial installations were marked.
		if err := w.markTrial(DataDir, true); err != nil {
			return err
		}
		w.trialWarning()
		u.Say("pi-fleet now shows this on every page; it takes effect when pi-fleet restarts.")
		w.Sys.Run("systemctl", "restart", "pi-fleet")
	}
	type choice struct {
		label string
		do    func() error
	}
	var choices []choice
	if onDrive {
		choices = append(choices, choice{"Move the records to a different drive (for example, a new SSD)", func() error { return w.moveToDrive(port, true) }})
	} else {
		u.Say("Its records are on the SD card, not on an external drive.")
		choices = append(choices, choice{"Move the records to an external drive (recommended)", func() error { return w.moveToDrive(port, false) }})
	}
	if onDrive {
		if w.Sys.Exists(BackupDir + "/.pi-fleet-backup-drive") {
			choices = append(choices,
				choice{"Add off-site disks", func() error { u.Step("Backups"); return w.offsiteDisks() }},
				choice{"Set up backups again (a new backup drive or new backup keys)", func() error { return w.backups(port) }})
		} else {
			choices = append(choices, choice{"Set up backups (backup drive, backup keys, off-site disks)", func() error { return w.backups(port) }})
		}
	}
	if w.newerThanInstalled() {
		choices = append(choices, choice{fmt.Sprintf("Update to this version (%s)", w.Version), func() error {
			return w.update(DataDir, true, fmt.Sprintf("https://127.0.0.1:%d/login", port))
		}})
	}
	choices = append(choices, choice{"Stop", func() error { return nil }})
	labels := make([]string, len(choices))
	for i, c := range choices {
		labels[i] = c.label
	}
	i, err := u.Choose("What would you like to do?", labels)
	if err != nil {
		return err
	}
	return choices[i].do()
}

// newerThanInstalled reports whether this file is a newer release than
// the installed program (or the installed version can't be told).
func (w *Wizard) newerThanInstalled() bool {
	out, err := w.Sys.Output(Binary, "version")
	f := strings.Fields(string(out))
	if err != nil || len(f) != 2 || !releaseVersion(f[1]) {
		return true
	}
	return release.Compare(w.Version, f[1]) > 0
}

// update installs this release over an existing installation with the
// installed program's own update command, which checks the release's
// signature, copies the database first and rolls back if the new version
// fails its checks. The release's manifest.json and manifest.json.minisig
// must be next to this file.
func (w *Wizard) update(dataDir string, master bool, healthURL string) error {
	u := w.UI
	u.Step("Update pi-fleet")
	dir := filepath.Dir(w.Self)
	for _, f := range []string{"manifest.json", "manifest.json.minisig"} {
		if !w.Sys.Exists(filepath.Join(dir, f)) {
			u.Say("To update, put the release's manifest.json and manifest.json.minisig")
			u.Say("files in %s, next to this file, and run setup again.", dir)
			u.Say("They prove the release is genuine.")
			return ErrCancelled
		}
	}
	if err := w.Sys.Run("systemctl", "stop", "pi-fleet"); err != nil {
		return err
	}
	if err := w.Sys.Run(Binary, "update", "-data", dataDir, "-root", InstallRoot, "-from", dir); err != nil {
		w.Sys.Run("systemctl", "start", "pi-fleet")
		return fmt.Errorf("the update didn't install (see above); pi-fleet is running the version it had: %w", err)
	}
	if master {
		// Employee Pis update from the master Pi's copy of the release.
		mirror := dataDir + "/releases"
		if err := w.Sys.MkdirAll(mirror, 0o755); err != nil {
			return err
		}
		entries, _ := w.ReadDir(dir)
		for _, e := range entries {
			n := e.Name()
			if n == "manifest.json" || n == "manifest.json.minisig" || strings.HasPrefix(n, "pi-fleet_"+w.Version+"_") {
				if err := w.Sys.CopyFile(filepath.Join(dir, n), filepath.Join(mirror, n), 0o644); err != nil {
					return err
				}
			}
		}
		if err := w.Sys.Run("chown", "-R", "pifleet:pifleet", mirror); err != nil {
			return err
		}
	}
	return w.restart(healthURL)
}

// moveToDrive copies a master Pi's records onto a new external drive,
// from the SD card or (fromDrive) from the drive they are on now, checks
// the copy, and switches over. The old copy is kept: on the SD card it is
// renamed; an old drive is left as it was, relabelled so setup never
// mistakes it for the current one.
func (w *Wizard) moveToDrive(port int, fromDrive bool) error {
	u := w.UI
	if !w.DryRun && !w.Sys.Exists(DataDir+"/pi-fleet.db") {
		return fmt.Errorf("there are no records in %s to move; nothing was changed", DataDir)
	}
	if err := w.dependencies(true); err != nil {
		return err
	}
	u.Step("External drive for the data")
	from := "the SD card"
	if fromDrive {
		from = "the drive they are on now"
	}
	u.Say("The records will be copied from %s to the new drive,", from)
	u.Say("and checked before anything switches over. pi-fleet is stopped while")
	u.Say("they are copied, usually for a minute or two. The old copy is kept.")
	if fromDrive {
		u.Say("Plug the new drive in alongside the current one. (The current one isn't")
		u.Say("offered below.)")
	}
	var disk *Disk
	for disk == nil {
		if err := u.Pause("Plug the drive in, then press Enter."); err != nil {
			return err
		}
		w.Sleep(2 * time.Second)
		d, err := w.pickDisk("Which drive should hold pi-fleet's data?")
		if err != nil {
			return err
		}
		if d == nil {
			continue
		}
		if d.stickLike() {
			ok, err := w.confirmTrial(*d)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
		}
		if _, ok := d.DataPart(); ok {
			u.Say("That drive already holds pi-fleet data. Erasing it destroys those records.")
		}
		ok, err := w.confirmErase(*d)
		if err != nil {
			return err
		}
		if ok {
			disk = d
		}
	}
	trial := disk.stickLike()
	oldPart := ""
	if fromDrive {
		out, err := w.Sys.Output("findmnt", "-n", "-o", "SOURCE", DataDir)
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return fmt.Errorf("couldn't tell which drive holds the records now: %v", err)
		}
		oldPart = strings.TrimSpace(string(out))
	}
	oldFstab, err := w.Sys.ReadFile("/etc/fstab")
	if err != nil {
		return err
	}
	u.Say("Preparing the drive…")
	part, err := EraseAndFormat(w.Sys, *disk)
	if err != nil {
		return fmt.Errorf("preparing the drive: %w", err)
	}
	tmp := "/mnt/pi-fleet-move"
	if err := w.Sys.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if err := w.Sys.Run("mount", part.Path, tmp); err != nil {
		return err
	}
	u.Say("Stopping pi-fleet and copying the records…")
	if err := w.Sys.Run("systemctl", "stop", "pi-fleet"); err != nil {
		return err
	}
	undo := func(cause error) error {
		w.Sys.Run("umount", tmp)
		w.Sys.Run("systemctl", "start", "pi-fleet")
		return fmt.Errorf("%w; nothing was switched over and pi-fleet is running from %s as before", cause, from)
	}
	if err := w.Sys.Run("cp", "-a", DataDir+"/.", tmp+"/"); err != nil {
		return undo(err)
	}
	if !w.Sys.Exists(tmp+"/tls/cert.pem") && w.Sys.Exists("/etc/pi-fleet/tls/cert.pem") {
		if err := w.Sys.Run("cp", "-a", "/etc/pi-fleet/tls", tmp+"/tls"); err != nil {
			return undo(err)
		}
	}
	if err := w.Sys.Run("chown", "-R", "pifleet:pifleet", tmp); err != nil {
		return undo(err)
	}
	if err := w.markTrial(tmp, trial); err != nil {
		return undo(err)
	}
	u.Say("Checking the copy…")
	if err := w.Sys.Run("runuser", "-u", "pifleet", "--", Binary, "selfcheck", "-data", tmp); err != nil {
		return undo(fmt.Errorf("the copy didn't pass its check: %w", err))
	}
	if err := w.Sys.Run("umount", tmp); err != nil {
		return undo(err)
	}
	// Switch over. If the new drive won't open, go back to the old copy.
	var old string
	var back func()
	if fromDrive {
		if err := w.Sys.Run("umount", DataDir); err != nil {
			return undo(err)
		}
		back = func() {
			w.Sys.WriteFile("/etc/fstab", oldFstab, 0o644)
			w.Sys.Run("systemctl", "daemon-reload")
			w.Sys.Run("mount", DataDir)
			w.Sys.Run("systemctl", "start", "pi-fleet")
		}
	} else {
		old = fmt.Sprintf("%s.on-sd-card-%s", DataDir, w.Now().Format("2006-01-02"))
		if w.Sys.Exists(old) { // an earlier copy from today is kept too
			old = fmt.Sprintf("%s.on-sd-card-%s", DataDir, w.Now().Format("2006-01-02-150405"))
		}
		if err := w.Sys.Rename(DataDir, old); err != nil {
			w.Sys.Run("systemctl", "start", "pi-fleet")
			return err
		}
		back = func() {
			w.Sys.WriteFile("/etc/fstab", oldFstab, 0o644)
			w.Sys.Run("systemctl", "daemon-reload")
			w.Sys.Rename(old, DataDir)
			w.Sys.Run("systemctl", "start", "pi-fleet")
		}
	}
	if err := MountData(w.Sys, part.UUID, DataDir); err != nil {
		back()
		return fmt.Errorf("%w; switched back to %s", err, from)
	}
	if fromDrive {
		// The old drive keeps its copy, but must never be taken for the
		// current one (setup reuses a drive labelled PIFLEET-DATA).
		w.Sys.Run("e2label", oldPart, "PIFLEET-OLD")
	}
	if err := w.chownData(); err != nil {
		return err
	}
	if err := w.startService(MasterUnit(port), fmt.Sprintf("https://127.0.0.1:%d/login", port)); err != nil {
		return err
	}
	u.Step("Done")
	u.Say("The records now live on the new drive, and pi-fleet is running from it.")
	if trial {
		w.trialWarning()
	} else {
		u.Say("It is no longer a trial installation.")
	}
	if fromDrive {
		u.Say("The old drive still holds a copy as of the move, renamed PIFLEET-OLD so")
		u.Say("it is never mistaken for the current one. Once you're happy everything")
		u.Say("works, unplug it (shut the Pi down first) and reuse or wipe it.")
	} else {
		u.Say("The old copy on the SD card is kept at:")
		u.Say("    %s", old)
		u.Say("Once you're happy everything works (say, after a week), delete it with:")
		u.Say("    sudo rm -r %s", old)
	}
	return nil
}

// --- employee and kiosk Pis ---

func (w *Wizard) newNode(kiosk bool) error {
	u := w.UI
	if err := w.dependencies(false); err != nil {
		return err
	}
	suggest := "fleet-pi"
	if kiosk {
		suggest = "fleet-kiosk"
	} else if w.SudoUser != "" && w.SudoUser != "root" {
		suggest = "fleet-" + w.SudoUser
	}
	if _, err := w.hostname(suggest); err != nil {
		return err
	}
	base, err := w.masterAddress()
	if err != nil {
		return err
	}
	if err := w.program(NodeDataDir); err != nil {
		return err
	}
	if err := w.activate(base, kiosk); err != nil {
		return err
	}
	if err := w.startService(NodeUnit(), "http://127.0.0.1:8080/login"); err != nil {
		return err
	}
	u.Step("Done")
	u.Say("This Pi is running. Open http://127.0.0.1:8080 in its web browser.")
	u.Say("It works without a network connection and syncs with the master Pi")
	u.Say("every 5 minutes when it has one.")
	return nil
}

// masterAddress finds the master Pi and trusts its certificate once the
// user has compared fingerprints.
func (w *Wizard) masterAddress() (string, error) {
	u := w.UI
	u.Step("Find the master Pi")
	u.Say("Type the master Pi's name as shown at the end of its setup, for example")
	u.Say("fleet-master.local, or its address in the web browser.")
	for {
		a, err := u.Ask("Master Pi", "fleet-master.local", nil)
		if err != nil {
			return "", err
		}
		host, ports := parseMaster(a)
		var cert []byte
		var port string
		for _, p := range ports {
			if cert, err = w.Fetch(net.JoinHostPort(host, p)); err == nil {
				port = p
				break
			}
		}
		if cert == nil {
			u.Say("Couldn't reach %s (%v). Check the name, and that both Pis are on the same network.", host, err)
			continue
		}
		fp, err := Fingerprint(cert)
		if err != nil {
			return "", err
		}
		u.Say("")
		u.Say("The master Pi's certificate fingerprint is:")
		u.Say("    %s", fp)
		u.Say("Compare it with the one on the master's Pis page (or at the end of its")
		u.Say("setup). If they differ, something is pretending to be the master Pi.")
		ok, err := u.Confirm("Do they match?", false)
		if err != nil {
			return "", err
		}
		if !ok {
			u.Say("Not trusted. Check the name and try again, or ask your super user.")
			continue
		}
		if err := w.Sys.MkdirAll(filepath.Dir(MasterCA), 0o755); err != nil {
			return "", err
		}
		if err := w.Sys.WriteFile(MasterCA, cert, 0o644); err != nil {
			return "", err
		}
		base := "https://" + host
		if port != "443" {
			base += ":" + port
		}
		return base, nil
	}
}

// parseMaster splits what the user typed into a host and the ports to
// try: theirs, or the usual ones.
func parseMaster(s string) (string, []string) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if h, p, err := net.SplitHostPort(s); err == nil {
		return h, []string{p}
	}
	return s, []string{"443", "8443"}
}

func (w *Wizard) activate(base string, kiosk bool) error {
	u := w.UI
	u.Step("Connect this Pi to the master Pi")
	if !w.Sys.Exists(NodeDataDir + "/pi-fleet.db") {
		if err := w.asPifleet("init", "-data", NodeDataDir, "-role", "node"); err != nil {
			return err
		}
	}
	status := func() string {
		out, _ := w.Sys.Output("runuser", "-u", "pifleet", "--", Binary, "activation-status", "-data", NodeDataDir)
		return strings.TrimSpace(string(out))
	}
	if s := status(); s != "active" && !strings.HasPrefix(s, "pending") {
		var args []string
		if kiosk {
			u.Say("A super user creates the kiosk on the master Pi's Kiosks page, which")
			u.Say("shows its name and a one-time password.")
			name, err := u.Ask("Kiosk name", "", nil)
			if err != nil {
				return err
			}
			args = []string{"-kiosk", name}
		} else {
			u.Say("Your super user creates your account and gives you a username and a")
			u.Say("one-time password.")
			name, err := u.Ask("Your username", "", domain.CheckUsername)
			if err != nil {
				return err
			}
			args = []string{"-username", name}
		}
		for {
			err := w.asPifleet(append([]string{"activate", "-data", NodeDataDir, "-central", base, "-ca", MasterCA}, args...)...)
			if err == nil || w.DryRun {
				break
			}
			if again, cerr := u.Confirm("That didn't work (see above). Try again?", true); cerr != nil || !again {
				if cerr != nil {
					return cerr
				}
				return ErrCancelled
			}
		}
	}
	if w.DryRun {
		return nil
	}
	u.Say("")
	u.Say("Waiting for a super user to approve this Pi: on the master Pi's Pis page,")
	u.Say("they type the six words shown above and press Confirm.")
	for status() != "active" {
		fmt.Fprint(u.out, ".")
		w.Sleep(5 * time.Second)
	}
	u.Say(" approved.")
	return w.asPifleet("sync", "-data", NodeDataDir)
}

func (w *Wizard) existingNode() error {
	u := w.UI
	u.Say("")
	u.Say("pi-fleet is already set up here as an employee or kiosk Pi.")
	if !w.newerThanInstalled() {
		u.Say("It runs this version (%s) or a newer one. Nothing to do.", w.Version)
		return nil
	}
	u.Say("Employee Pis normally update themselves from the master Pi.")
	i, err := u.Choose("What would you like to do?", []string{fmt.Sprintf("Update to this version (%s) now", w.Version), "Stop"})
	if err != nil || i == 1 {
		return err
	}
	return w.update(NodeDataDir, false, "http://127.0.0.1:8080/login")
}

// --- shared steps ---

func (w *Wizard) dependencies(master bool) error {
	u := w.UI
	u.Step("Check what this Pi needs")
	pkgs := MissingPackages(master)
	if len(pkgs) == 0 {
		u.Say("Everything needed is already installed.")
		return nil
	}
	u.Say("Downloading and installing: %s", strings.Join(pkgs, ", "))
	u.Say("(This needs an internet connection.)")
	return InstallPackages(w.Sys, pkgs)
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// hostname gives the Pi its own name on the network.
func (w *Wizard) hostname(suggest string) (string, error) {
	u := w.UI
	u.Step("Name this Pi")
	cur, _ := w.Sys.Output("hostname")
	current := strings.TrimSpace(string(cur))
	if current != "" && current != "raspberrypi" && current != "localhost" {
		suggest = current
	}
	u.Say("Other Pis and browsers find this Pi by its name, so each needs its own.")
	name, err := u.Ask("Name (lowercase letters, digits and dashes)", suggest, func(s string) error {
		if !hostRE.MatchString(s) {
			return errors.New("use lowercase letters, digits and dashes, starting and ending with a letter or digit")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if name == current {
		return name, nil
	}
	if err := w.Sys.Run("hostnamectl", "set-hostname", name); err != nil {
		return "", err
	}
	if hosts, err := w.Sys.ReadFile("/etc/hosts"); err == nil {
		if err := w.Sys.WriteFile("/etc/hosts", []byte(UpdateHosts(string(hosts), name)), 0o644); err != nil {
			return "", err
		}
	}
	w.Sys.Run("systemctl", "restart", "avahi-daemon") // announce the new name
	u.Say("This Pi is now called %s (%s.local on the network).", name, name)
	return name, nil
}

// UpdateHosts points the 127.0.1.1 line of /etc/hosts at the new name,
// as Raspberry Pi OS does.
func UpdateHosts(hosts, name string) string {
	lines := strings.Split(strings.TrimRight(hosts, "\n"), "\n")
	found := false
	for i, l := range lines {
		if f := strings.Fields(l); len(f) >= 1 && f[0] == "127.0.1.1" {
			lines[i] = "127.0.1.1\t" + name
			found = true
		}
	}
	if !found {
		lines = append(lines, "127.0.1.1\t"+name)
	}
	return strings.Join(lines, "\n") + "\n"
}

// program creates pi-fleet's user and installs this file as the program.
func (w *Wizard) program(home string) error {
	u := w.UI
	u.Step("Install the program")
	if _, err := w.Sys.Output("id", "pifleet"); err != nil {
		if err := w.Sys.Run("useradd", "--system", "--home", home, "--shell", "/usr/sbin/nologin", "pifleet"); err != nil {
			return err
		}
	}
	if err := w.Sys.MkdirAll(home, 0o755); err != nil {
		return err
	}
	if home == NodeDataDir {
		if err := w.Sys.Run("chown", "pifleet:pifleet", home); err != nil {
			return err
		}
		if err := w.Sys.Run("chmod", "0700", home); err != nil {
			return err
		}
	}
	dir := filepath.Join(InstallRoot, "releases", w.Version)
	if self, err := filepath.EvalSymlinks(w.Self); err == nil && filepath.Dir(self) == dir {
		u.Say("pi-fleet %s is installed.", w.Version)
		return nil
	}
	if out, err := w.Sys.Output(Binary, "version"); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 && releaseVersion(f[1]) && release.Compare(f[1], w.Version) > 0 {
			return fmt.Errorf("pi-fleet %s is already installed, which is newer than this file (%s); download the newest release", f[1], w.Version)
		}
	}
	if err := w.Sys.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := w.Sys.CopyFile(w.Self, dir+"/pi-fleet", 0o755); err != nil {
		return err
	}
	if err := w.Sys.Symlink(dir, InstallRoot+"/current"); err != nil {
		return err
	}
	u.Say("Installed pi-fleet %s in %s.", w.Version, InstallRoot)
	return nil
}

func (w *Wizard) asPifleet(args ...string) error {
	return w.Sys.Run("runuser", append([]string{"-u", "pifleet", "--", Binary}, args...)...)
}

// startService installs and starts the systemd service, then waits for
// it to answer.
func (w *Wizard) startService(unit, healthURL string) error {
	u := w.UI
	u.Step("Start pi-fleet")
	if err := w.Sys.WriteFile(UnitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := w.Sys.Run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := w.Sys.Run("systemctl", "enable", "pi-fleet"); err != nil {
		return err
	}
	return w.restart(healthURL)
}

func (w *Wizard) restart(healthURL string) error {
	if err := w.Sys.Run("systemctl", "restart", "pi-fleet"); err != nil {
		return err
	}
	if w.DryRun {
		return nil
	}
	var err error
	for i := 0; i < 30; i++ {
		if err = w.Health(healthURL); err == nil {
			w.UI.Say("pi-fleet is running and starts by itself whenever the Pi starts.")
			return nil
		}
		w.Sleep(time.Second)
	}
	w.Sys.Run("journalctl", "-u", "pi-fleet", "-n", "15", "--no-pager")
	return fmt.Errorf("pi-fleet didn't start (%v); its log is above", err)
}

func healthCheck(url string) error {
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // our own service, on this Pi
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func localAddrs() []net.IP {
	var ips []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			ips = append(ips, n.IP)
		}
	}
	return ips
}
