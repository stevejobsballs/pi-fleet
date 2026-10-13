package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A standby Pi keeps a live copy of the master Pi on its own external
// drive, at the same place (DataDir), so that taking over is starting
// pi-fleet there as the master. See internal/standby.

// Files a standby keeps in DataDir (as internal/standby names them).
const (
	standbyLink   = "standby.json"
	standbyStatus = "standby-status.json"
)

// StandbyUnit runs a standby Pi: it follows the master into DataDir, on
// its own drive, which it needs mounted.
func StandbyUnit() string {
	return fmt.Sprintf(`[Unit]
Description=pi-fleet standby Pi
After=network-online.target
Wants=network-online.target
RequiresMountsFor=%[1]s

[Service]
User=pifleet
ExecStartPre=/usr/bin/mountpoint -q %[1]s
ExecStart=%[2]s standby -data %[1]s
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=%[1]s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`, DataDir, Binary)
}

// standbyReport is the standby's report on its copy (internal/standby).
type standbyReport struct {
	LastOK time.Time `json:"last_ok"`
	At     int64     `json:"at"`
	Head   int64     `json:"head"`
	Error  string    `json:"error"`
	Since  time.Time `json:"since"`
}

func (w *Wizard) readStandbyReport() (standbyReport, bool) {
	var r standbyReport
	b, err := w.Sys.ReadFile(DataDir + "/" + standbyStatus)
	if err != nil || json.Unmarshal(b, &r) != nil {
		return r, false
	}
	return r, true
}

// standbyMaster is the address of the master a standby follows.
func (w *Wizard) standbyMaster() (string, error) {
	b, err := w.Sys.ReadFile(DataDir + "/" + standbyLink)
	if err != nil {
		return "", fmt.Errorf("this Pi isn't paired as a standby: %w", err)
	}
	var link struct {
		Master string `json:"master"`
	}
	if err := json.Unmarshal(b, &link); err != nil || link.Master == "" {
		return "", errors.New("this standby's pairing can't be read")
	}
	return link.Master, nil
}

func (w *Wizard) newStandby() error {
	u := w.UI
	u.Say("")
	u.Say("A standby Pi keeps a live copy of the master Pi's records, files and")
	u.Say("keys on its own external drive, a few seconds behind. If the master")
	u.Say("fails, run setup on the standby and choose to take over: it becomes the")
	u.Say("master, with the same name, records and certificate.")
	if err := w.dependencies(true); err != nil {
		return err
	}
	if _, err := w.hostname("fleet-standby"); err != nil {
		return err
	}
	u.Step("External drive for the copy")
	if _, _, err := w.dataDrive(); err != nil {
		return err
	}
	if err := w.program(DataDir); err != nil {
		return err
	}
	if err := w.chownData(); err != nil {
		return err
	}
	base, err := w.masterAddress()
	if err != nil {
		return err
	}
	u.Step("Pair with the master")
	u.Say("On the master Pi, open Admin › Pis and press “Make a pairing code” under")
	u.Say("Standby Pi. Type the code it shows here (it works once, for an hour).")
	for {
		code, err := u.Ask("Pairing code", "", nil)
		if err != nil {
			return err
		}
		if err := w.asPifleet("standby-pair", "-data", DataDir, "-master", base, "-ca", MasterCA, "-code", code); err == nil || w.DryRun {
			break
		}
		if again, err := u.Confirm("That didn't work (see above). Try again?", true); err != nil || !again {
			if err != nil {
				return err
			}
			return ErrCancelled
		}
	}
	if err := w.chownData(); err != nil {
		return err
	}
	u.Step("Start copying")
	if err := w.Sys.WriteFile(UnitPath, []byte(StandbyUnit()), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "pi-fleet"}, {"restart", "pi-fleet"}} {
		if err := w.Sys.Run("systemctl", args...); err != nil {
			return err
		}
	}
	if !w.DryRun {
		u.Say("Copying the master's records. This takes a minute or so…")
		ok := false
		for range 60 {
			if r, found := w.readStandbyReport(); found && !r.LastOK.IsZero() {
				ok = true
				break
			}
			u.Progress()
			w.Sleep(3 * time.Second)
		}
		if !ok {
			w.Sys.Run("journalctl", "-u", "pi-fleet", "-n", "15", "--no-pager")
			return errors.New("the standby didn't manage to copy the master's records yet (its log is above); run setup again to see how it is doing")
		}
	}
	u.Step("Done")
	u.Say("This Pi is now the standby for the master Pi at %s. It copies what's", base)
	u.Say("new every few seconds; the master's Pis page shows how it is doing.")
	u.Say("")
	u.Say("If the master fails: switch the master off, then run setup on this Pi")
	u.Say("and choose “Take over as the master”.")
	return nil
}

func (w *Wizard) existingStandby() error {
	u := w.UI
	master, err := w.standbyMaster()
	if err != nil {
		return err
	}
	u.Say("")
	u.Say("This Pi is the standby for the master Pi at %s.", master)
	if r, ok := w.readStandbyReport(); ok {
		switch {
		case r.Error != "":
			u.Say("Its copy isn't up to date: %s (since %s).", r.Error, r.Since.Local().Format("2 January 15:04"))
		case !r.LastOK.IsZero():
			u.Say("Its copy was last brought up to date at %s.", r.LastOK.Local().Format("2 January 15:04:05"))
		}
	}
	type choice struct {
		label string
		do    func() error
	}
	choices := []choice{{"Take over as the master (only if the master has failed)", w.takeOver}}
	if w.newerThanInstalled() {
		choices = append(choices, choice{fmt.Sprintf("Update to this version (%s)", w.Version), func() error {
			return w.update(DataDir, false, "")
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

// takeOver makes the standby the master: only when the master doesn't
// answer, since two masters would split the records.
func (w *Wizard) takeOver() error {
	u := w.UI
	u.Step("Take over as the master")
	master, err := w.standbyMaster()
	if err != nil {
		return err
	}
	m, err := url.Parse(master)
	if err != nil {
		return err
	}
	host, port := m.Hostname(), m.Port()
	if port == "" {
		port = "443"
	}
	if _, err := w.Fetch(net.JoinHostPort(host, port)); err == nil {
		u.Say("The master Pi at %s is still answering. Only take over when it has", master)
		u.Say("failed: switch it off (or unplug its network cable) first, then run")
		u.Say("setup here again. Two masters at once would split the records.")
		return nil
	}
	if r, ok := w.readStandbyReport(); ok && !r.LastOK.IsZero() {
		u.Say("The copy here was last brought up to date at %s; anything recorded on", r.LastOK.Local().Format("2 January 15:04:05"))
		u.Say("the master after that is lost.")
	}
	u.Say("")
	u.Say("This Pi becomes the master: it takes the name %s, and serves the same", host)
	u.Say("records with the same certificate, so employee Pis, phones and QR")
	u.Say("labels carry on. The old master must not be switched back on as it is.")
	a, err := u.Line("Type TAKE OVER to go ahead (anything else stops): ")
	if err != nil {
		return err
	}
	if strings.TrimSpace(a) != "TAKE OVER" {
		u.Say("Stopped; nothing was changed.")
		return nil
	}
	if err := w.Sys.Run("systemctl", "stop", "pi-fleet"); err != nil {
		return err
	}
	if name, ok := strings.CutSuffix(host, ".local"); ok && hostRE.MatchString(name) {
		if err := w.setHostname(name); err != nil {
			return err
		}
	} else {
		u.Say("Ask your IT department to point %s to this Pi's address.", host)
	}
	for _, f := range []string{standbyLink, standbyStatus} {
		w.Sys.Run("rm", "-f", DataDir+"/"+f)
	}
	if err := w.chownData(); err != nil {
		return err
	}
	if err := w.mirrorRelease(DataDir, filepath.Dir(w.Self)); err != nil {
		return err
	}
	p, _ := strconv.Atoi(port)
	if err := w.startService(MasterUnit(p), fmt.Sprintf("https://127.0.0.1:%d/login", p)); err != nil {
		return err
	}
	cert, err := w.Sys.ReadFile(DataDir + "/tls/cert.pem")
	if err != nil {
		return err
	}
	fp, err := Fingerprint(cert)
	if err != nil {
		return err
	}
	w.masterDone(host, p, fp, w.Sys.Exists(BackupDir+"/.pi-fleet-backup-drive"))
	u.Say("")
	u.Say("Don't switch the old master back on as it is: two masters would split")
	u.Say("the records. Set it up again as the new standby (Reset-Pi, then setup,")
	u.Say("Standby Pi), or keep it switched off. Set up backups on this Pi too.")
	return nil
}
