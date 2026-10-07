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
ReadWritePaths=%[1]s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, DataDir, Binary, port)
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
