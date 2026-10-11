package setup

import (
	"fmt"
	"strings"
	"time"
)

// DataDir is where a master Pi's data drive is mounted.
const DataDir = "/srv/pi-fleet"

// partitionPath names a disk's first partition (sda → sda1, nvme0n1 →
// nvme0n1p1, mmcblk0 → mmcblk0p1).
func partitionPath(disk string) string {
	last := disk[len(disk)-1]
	if last >= '0' && last <= '9' {
		return disk + "p1"
	}
	return disk + "1"
}

// EraseAndFormat wipes a disk and makes one ext4 partition labelled
// DataLabel on it, returning the partition and its UUID.
func EraseAndFormat(sys Sys, d Disk) (Part, error) { return EraseAndFormatAs(sys, d, DataLabel) }

// EraseAndFormatAs is EraseAndFormat with another label.
func EraseAndFormatAs(sys Sys, d Disk, label string) (Part, error) {
	for _, p := range d.Parts {
		for _, m := range p.Mounts {
			if err := sys.Run("umount", m); err != nil {
				return Part{}, fmt.Errorf("couldn't close %s (is a program using it?): %w", m, err)
			}
		}
	}
	// Lines in /etc/fstab that mount the old file systems would point at
	// nothing once the disk is erased (and make the Pi wait for it at boot).
	if fstab, err := sys.ReadFile("/etc/fstab"); err == nil {
		if next := forgetErased(string(fstab), d); next != string(fstab) {
			if err := sys.WriteFile("/etc/fstab", []byte(next), 0o644); err != nil {
				return Part{}, err
			}
		}
	}
	steps := [][]string{
		{"wipefs", "--all", "--quiet", d.Path},
		{"parted", "--script", d.Path, "mklabel", "gpt", "mkpart", "pifleet", "ext4", "1MiB", "100%"},
		{"partprobe", d.Path},
		{"udevadm", "settle"},
	}
	for _, s := range steps {
		if err := sys.Run(s[0], s[1:]...); err != nil {
			return Part{}, err
		}
	}
	part := partitionPath(d.Path)
	if err := sys.Run("mkfs.ext4", "-q", "-F", "-L", label, "-m", "1", part); err != nil {
		return Part{}, err
	}
	uuid, err := blkidUUID(sys, part)
	if err != nil {
		return Part{}, err
	}
	return Part{Path: part, FSType: "ext4", Label: label, UUID: uuid}, nil
}

// forgetErased comments out the fstab lines that mount one of d's file
// systems, by UUID, label or device name.
func forgetErased(fstab string, d Disk) string {
	names := map[string]bool{}
	for _, p := range d.Parts {
		if p.UUID != "" {
			names["UUID="+p.UUID] = true
		}
		if p.Label != "" {
			names["LABEL="+p.Label] = true
		}
		if p.Path != "" {
			names[p.Path] = true
		}
	}
	lines := strings.Split(fstab, "\n")
	for i, l := range lines {
		if f := strings.Fields(l); len(f) >= 2 && !strings.HasPrefix(f[0], "#") && names[f[0]] {
			lines[i] = "# drive erased by pi-fleet setup: " + l
		}
	}
	return strings.Join(lines, "\n")
}

func blkidUUID(sys Sys, part string) (string, error) {
	if r, ok := sys.(*Real); ok && r.DryRun {
		return "<new-uuid>", nil
	}
	var last error
	for i := 0; i < 10; i++ { // udev may take a moment to see the new file system
		out, err := sys.Output("blkid", "-s", "UUID", "-o", "value", part)
		if u := strings.TrimSpace(string(out)); err == nil && u != "" {
			return u, nil
		}
		last = err
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("couldn't read the new file system's ID: %v", last)
}

// FstabLine mounts the data drive at boot by its UUID, so another drive
// can't be mistaken for it. nofail lets the Pi start without the drive;
// pi-fleet's service then refuses to start rather than write to the SD card.
func FstabLine(uuid, dir string) string {
	return fmt.Sprintf("UUID=%s  %s  ext4  defaults,noatime,nofail,x-systemd.device-timeout=20s  0  2", uuid, dir)
}

// UpdateFstab returns fstab with dir mounted from uuid, replacing (as a
// comment, so nothing is lost) any earlier line for dir.
func UpdateFstab(fstab, uuid, dir string) string {
	return replaceFstab(fstab, dir, "# pi-fleet master Pi data drive", FstabLine(uuid, dir))
}

func replaceFstab(fstab, dir, comment, line string) string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(fstab, "\n"), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && !strings.HasPrefix(f[0], "#") && f[1] == dir {
			l = "# replaced by pi-fleet setup: " + l
		}
		out = append(out, l)
	}
	out = append(out, comment, line)
	return strings.Join(out, "\n") + "\n"
}

// MountData adds the drive to /etc/fstab and mounts it at dir.
func MountData(sys Sys, uuid, dir string) error {
	return mountWith(sys, dir, "# pi-fleet master Pi data drive", FstabLine(uuid, dir), true)
}

// mountWith puts line in /etc/fstab for dir and, if mount, mounts it.
func mountWith(sys Sys, dir, comment, line string, mount bool) error {
	fstab, err := sys.ReadFile("/etc/fstab")
	if err != nil {
		return err
	}
	if !strings.Contains(string(fstab), line) {
		if err := sys.WriteFile("/etc/fstab", []byte(replaceFstab(string(fstab), dir, comment, line)), 0o644); err != nil {
			return err
		}
	}
	if err := sys.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := sys.Run("systemctl", "daemon-reload"); err != nil { // fstab changes become mount units
		return err
	}
	if !mount || isMountpoint(sys, dir) {
		return nil
	}
	return sys.Run("mount", dir)
}

func isMountpoint(sys Sys, dir string) bool {
	_, err := sys.Output("mountpoint", "-q", dir)
	return err == nil
}
