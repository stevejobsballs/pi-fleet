package setup

import "fmt"

const (
	InstallRoot = "/opt/pi-fleet"
	Binary      = InstallRoot + "/current/pi-fleet"
	UnitPath    = "/etc/systemd/system/pi-fleet.service"
	NodeDataDir = "/var/lib/pi-fleet"
	MasterCA    = "/etc/pi-fleet/master.pem"
)

// MasterUnit runs the master Pi. Its data lives on the external drive:
// the service needs the drive mounted and checks it really is, so it can
// never start writing records to the SD card instead.
func MasterUnit(port int) string {
	return fmt.Sprintf(`[Unit]
Description=pi-fleet master Pi
After=network-online.target
Wants=network-online.target
RequiresMountsFor=%[1]s

[Service]
User=pifleet
ExecStartPre=/usr/bin/mountpoint -q %[1]s
ExecStart=%[2]s serve -data %[1]s -listen :%[3]d -tls-cert %[1]s/tls/cert.pem -tls-key %[1]s/tls/key.pem -releases %[1]s/releases
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=%[1]s -%[4]s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, DataDir, Binary, port, BackupDir)
}

// NodeUnit runs an employee or kiosk Pi.
func NodeUnit() string {
	return fmt.Sprintf(`[Unit]
Description=pi-fleet employee Pi
After=network-online.target
Wants=network-online.target

[Service]
User=pifleet
ExecStart=%[2]s run -data %[1]s
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=%[1]s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, NodeDataDir, Binary)
}

// OffsiteRule starts an off-site write whenever a disk labelled OFFSITE-…
// is plugged in.
const OffsiteRule = `# pi-fleet: write a verified backup to an off-site disk when it is plugged in
ACTION=="add", SUBSYSTEM=="block", ENV{ID_FS_LABEL}=="OFFSITE-*", TAG+="systemd", ENV{SYSTEMD_WANTS}+="pi-fleet-offsite@%E{ID_FS_LABEL}.service"
`

const (
	OffsiteRulePath = "/etc/udev/rules.d/90-pi-fleet-offsite.rules"
	OffsiteUnitPath = "/etc/systemd/system/pi-fleet-offsite@.service"
)

// OffsiteUnit writes the backup. %i is the disk's label as is (labels are
// letters, digits and dashes; %I would turn the dashes into slashes). The
// disk is closed afterwards, so it can be unplugged once it disappears.
func OffsiteUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Write a verified pi-fleet backup to off-site disk %%i
RequiresMountsFor=/media/%%i

[Service]
Type=oneshot
User=pifleet
ExecStart=%[1]s offsite-write -data %[2]s -disk /media/%%i
ExecStopPost=+/usr/bin/umount /media/%%i
`, Binary, DataDir)
}
