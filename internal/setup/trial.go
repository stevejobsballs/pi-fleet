package setup

import "strings"

// TrialMarker on the data drive marks a trial installation: the records
// are on a USB stick, which is for testing only. pi-fleet then shows a
// warning on every page until the records move to a proper drive.
const TrialMarker = ".pi-fleet-trial"

// stickLike reports whether a drive looks like a USB stick rather than a
// drive fit for daily records: sticks say they are removable, and are
// small. Sticks wear out under a database's constant small writes and are
// easily pulled out.
func (d Disk) stickLike() bool {
	return d.Removable || d.Size < SmallDataDrive
}

// confirmTrial explains why a USB stick is only for testing and asks the
// user to accept a trial installation.
func (w *Wizard) confirmTrial(d Disk) (bool, error) {
	u := w.UI
	u.Say("")
	u.Say("%s looks like a USB stick.", d.Describe())
	u.Say("")
	u.Say("  Don't use a USB stick for actual work. A database on a USB stick is")
	u.Say("  only for testing: sticks wear out under constant small writes, lose")
	u.Say("  data when pulled out, and fail without warning.")
	u.Say("")
	u.Say("You can use it to try pi-fleet out. Every page will then say it is a")
	u.Say("trial, until the records are moved to an SSD (run setup again and")
	u.Say("choose \"Move the records to a different drive\").")
	return u.Confirm("Use this USB stick for a trial installation?", false)
}

// markTrial records on the data drive (open at dir) whether it is a trial.
func (w *Wizard) markTrial(dir string, trial bool) error {
	path := dir + "/" + TrialMarker
	if !trial {
		return w.Sys.Run("rm", "-f", path)
	}
	if err := w.Sys.WriteFile(path, []byte("Trial installation: the records are on a USB stick, which is only for testing.\n"), 0o644); err != nil {
		return err
	}
	return w.Sys.Run("chown", "pifleet:pifleet", path)
}

// dataDisk returns the drive pi-fleet's records are on now, if any.
func (w *Wizard) dataDisk() (Disk, bool) {
	out, err := w.Sys.Output("lsblk", LsblkArgs...)
	if err != nil {
		return Disk{}, false
	}
	disks, err := ParseDisks(out)
	if err != nil {
		return Disk{}, false
	}
	for _, d := range disks {
		if d.holds(DataDir) {
			return d, true
		}
	}
	return Disk{}, false
}

func (w *Wizard) trialWarning() {
	u := w.UI
	u.Say("")
	u.Say(strings.Repeat("!", 72))
	u.Say("  TRIAL INSTALLATION: the records are on a USB stick. Don't use it for")
	u.Say("  actual work. Before real use, move them to an SSD: run setup again and")
	u.Say("  choose \"Move the records to a different drive\".")
	u.Say(strings.Repeat("!", 72))
}
