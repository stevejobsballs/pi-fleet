package setup

import (
	"os/exec"
	"slices"
)

// Dependency is a program the installer or pi-fleet needs, and the
// Debian package that provides it.
type Dependency struct {
	Program, Package, Why string
	MasterOnly            bool
}

// Dependencies lists everything outside pi-fleet itself. Raspberry Pi OS
// has most of them already; the installer adds what is missing.
var Dependencies = []Dependency{
	{"systemctl", "systemd", "runs pi-fleet in the background", false},
	{"runuser", "util-linux", "runs pi-fleet as its own user", false},
	{"lsblk", "util-linux", "finds the external drive", true},
	{"wipefs", "util-linux", "prepares the external drive", true},
	{"parted", "parted", "prepares the external drive", true},
	{"partprobe", "parted", "prepares the external drive", true},
	{"mkfs.ext4", "e2fsprogs", "formats the external drive", true},
	{"avahi-daemon", "avahi-daemon", "lets other Pis find this one by name (name.local)", false},
}

// lookPath is replaced in tests.
var lookPath = exec.LookPath

// MissingPackages returns the packages to install for a role.
func MissingPackages(master bool) []string {
	var pkgs []string
	for _, d := range Dependencies {
		if d.MasterOnly && !master {
			continue
		}
		if _, err := lookPath(d.Program); err == nil {
			continue
		}
		if !slices.Contains(pkgs, d.Package) {
			pkgs = append(pkgs, d.Package)
		}
	}
	return pkgs
}

// InstallPackages downloads and installs packages with apt.
func InstallPackages(sys Sys, pkgs []string) error {
	if len(pkgs) == 0 {
		return nil
	}
	if err := sys.Run("apt-get", "update"); err != nil {
		return err
	}
	return sys.Run("apt-get", append([]string{"install", "-y", "--no-install-recommends"}, pkgs...)...)
}
