package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// DataLabel is the file system label of a master Pi's data drive. The
// installer looks for it to reuse a drive (for example after moving it to
// a new Pi) instead of erasing it.
const DataLabel = "PIFLEET-DATA"

// Disk is a whole drive and its partitions, as lsblk reports them.
type Disk struct {
	Name, Path, Model, Serial, Tran string
	Size                            int64
	Removable                       bool
	Parts                           []Part
}

// Part is a partition (or a file system on the whole disk).
type Part struct {
	Path, FSType, Label, UUID string
	Size                      int64
	Mounts                    []string
}

type lsblkDev struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Type        string     `json:"type"`
	Size        int64      `json:"size"`
	Model       *string    `json:"model"`
	Serial      *string    `json:"serial"`
	Tran        *string    `json:"tran"`
	RM          bool       `json:"rm"`
	FSType      *string    `json:"fstype"`
	Label       *string    `json:"label"`
	UUID        *string    `json:"uuid"`
	Mountpoints []*string  `json:"mountpoints"`
	Children    []lsblkDev `json:"children"`
}

// LsblkArgs lists drives in the form ParseDisks reads.
var LsblkArgs = []string{"-J", "-b", "-o", "NAME,PATH,TYPE,SIZE,MODEL,SERIAL,TRAN,RM,FSTYPE,LABEL,UUID,MOUNTPOINTS"}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func (d lsblkDev) part() Part {
	p := Part{Path: d.Path, FSType: str(d.FSType), Label: str(d.Label), UUID: str(d.UUID), Size: d.Size}
	for _, m := range d.Mountpoints {
		if m != nil && *m != "" {
			p.Mounts = append(p.Mounts, *m)
		}
	}
	return p
}

// ParseDisks reads lsblk's JSON and returns the drives that could hold
// pi-fleet's data: whole disks, not the one this Pi runs from.
func ParseDisks(lsblkJSON []byte) ([]Disk, error) {
	var out struct {
		Devices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(lsblkJSON, &out); err != nil {
		return nil, fmt.Errorf("reading the list of drives: %w", err)
	}
	var disks []Disk
	for _, d := range out.Devices {
		if d.Type != "disk" {
			continue // loop devices, zram swap, optical drives
		}
		disk := Disk{Name: d.Name, Path: d.Path, Model: str(d.Model), Serial: str(d.Serial), Tran: str(d.Tran), Size: d.Size, Removable: d.RM}
		if str(d.FSType) != "" { // a file system on the whole disk
			disk.Parts = append(disk.Parts, d.part())
		}
		for _, c := range d.Children {
			disk.Parts = append(disk.Parts, c.part())
		}
		if disk.system() {
			continue
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

// system reports whether the Pi runs from this disk.
func (d Disk) system() bool {
	for _, p := range d.Parts {
		for _, m := range p.Mounts {
			if m == "/" || m == "/boot" || m == "/boot/firmware" || m == "[SWAP]" || strings.HasPrefix(m, "/usr") || strings.HasPrefix(m, "/var") {
				return true
			}
		}
	}
	return false
}

// holds reports whether one of the disk's partitions is open at dir: the
// drive pi-fleet's records are on now is never offered for erasing.
func (d Disk) holds(dir string) bool {
	for _, p := range d.Parts {
		if slices.Contains(p.Mounts, dir) {
			return true
		}
	}
	return false
}

// DataPart returns the partition of an earlier pi-fleet data drive, if
// this is one.
func (d Disk) DataPart() (Part, bool) {
	for _, p := range d.Parts {
		if p.Label == DataLabel && p.FSType == "ext4" && p.UUID != "" {
			return p, true
		}
	}
	return Part{}, false
}

// Describe is how a drive is shown to the user.
func (d Disk) Describe() string {
	name := d.Model
	if name == "" {
		name = "unnamed drive"
	}
	via := map[string]string{"usb": "USB", "nvme": "NVMe", "sata": "SATA", "mmc": "SD card"}[d.Tran]
	if via == "" {
		via = d.Tran
	}
	s := fmt.Sprintf("%s, %s", name, humanSize(d.Size))
	if via != "" {
		s += ", " + via
	}
	var parts []string
	for _, p := range d.Parts {
		desc := p.FSType
		if desc == "" {
			desc = "unformatted"
		}
		if p.Label != "" {
			desc = fmt.Sprintf("%q (%s)", p.Label, desc)
		}
		if len(p.Mounts) > 0 {
			desc += " open at " + strings.Join(p.Mounts, ", ")
		}
		parts = append(parts, desc)
	}
	if len(parts) == 0 {
		parts = append(parts, "empty")
	}
	return s + " — " + strings.Join(parts, "; ")
}

// Contents lists a few top-level names on each open partition, so the
// user can see what erasing the drive would destroy.
func (d Disk) Contents(readDir func(string) ([]os.DirEntry, error)) []string {
	var out []string
	for _, p := range d.Parts {
		for _, m := range p.Mounts {
			entries, err := readDir(m)
			if err != nil {
				continue
			}
			names := []string{}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".") || e.Name() == "System Volume Information" || e.Name() == "lost+found" {
					continue
				}
				names = append(names, e.Name())
			}
			slices.Sort(names)
			if len(names) > 8 {
				names = append(names[:8], fmt.Sprintf("… and %d more", len(names)-8))
			}
			if len(names) == 0 {
				names = []string{"(no files)"}
			}
			out = append(out, m+": "+strings.Join(names, ", "))
		}
	}
	return out
}

// MinDataDrive is the smallest drive accepted for a master Pi's data, and
// SmallDataDrive the size below which the installer asks the user to make
// sure (a USB stick rather than an SSD, say).
const (
	MinDataDrive   = 16e9
	SmallDataDrive = 120e9
)

// humanSize uses the decimal units printed on drives.
func humanSize(n int64) string {
	switch {
	case n >= 1e12:
		return fmt.Sprintf("%.1f TB", float64(n)/1e12)
	case n >= 1e9:
		return fmt.Sprintf("%.0f GB", float64(n)/1e9)
	default:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
}
