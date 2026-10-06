# pi-fleet operations

How to install and run pi-fleet on the master Pi and on employee Pis. The
design and its reasons are in [DESIGN.md](DESIGN.md).

## Master Pi

### 1. Disks

The 1 TB SSD holds all data. Mount it by UUID so the wrong disk can't be
mounted in its place, and never write data to the SD card (DESIGN.md §2.3):

```sh
sudo blkid                       # find the SSD's UUID
sudo mkdir -p /srv/pi-fleet /srv/pi-fleet-backup
# /etc/fstab
UUID=<ssd-uuid>     /srv/pi-fleet         ext4  noatime,nofail  0 2
UUID=<backup-uuid>  /srv/pi-fleet-backup  ext4  noatime,nofail  0 2
```

### 2. Install

```sh
sudo useradd --system --home /srv/pi-fleet --shell /usr/sbin/nologin pifleet
sudo mkdir -p /opt/pi-fleet/releases/v1.0.0
sudo install -m 0755 pi-fleet_v1.0.0_linux_arm64 /opt/pi-fleet/releases/v1.0.0/pi-fleet
sudo ln -sfn /opt/pi-fleet/releases/v1.0.0 /opt/pi-fleet/current
sudo chown pifleet: /srv/pi-fleet /srv/pi-fleet-backup
sudo -u pifleet /opt/pi-fleet/current/pi-fleet init -data /srv/pi-fleet -role central
sudo -u pifleet /opt/pi-fleet/current/pi-fleet bootstrap -data /srv/pi-fleet \
    -username admin -name "Your Legal Name" -email you@example.org
```

Back up `/srv/pi-fleet/keys` into the encrypted key escrow now (§8.1).

### 3. Backups

```sh
pi-fleet backup-keygen      # once per super user, plus one escrow identity; keep identities offline
sudo -u pifleet /opt/pi-fleet/current/pi-fleet backup-config -data /srv/pi-fleet \
    -dir /srv/pi-fleet-backup -recipient age1... -recipient age1...
sudo -u pifleet /opt/pi-fleet/current/pi-fleet backup-now -data /srv/pi-fleet
```

Register each off-site USB disk once (mount it, then):

```sh
sudo -u pifleet /opt/pi-fleet/current/pi-fleet offsite-register -data /srv/pi-fleet \
    -disk /media/OFFSITE-A -label OFFSITE-A
```

To have central write automatically when a registered disk is plugged in,
mount off-site disks by label and trigger a unit:

```
# /etc/udev/rules.d/90-pi-fleet-offsite.rules
ACTION=="add", SUBSYSTEM=="block", ENV{ID_FS_LABEL}=="OFFSITE-*", \
  TAG+="systemd", ENV{SYSTEMD_WANTS}+="pi-fleet-offsite@%E{ID_FS_LABEL}.service"
```

```ini
# /etc/systemd/system/pi-fleet-offsite@.service
[Unit]
Description=Write a verified pi-fleet snapshot to off-site disk %i
Requires=media-%i.mount
After=media-%i.mount

[Service]
Type=oneshot
User=pifleet
ExecStart=/opt/pi-fleet/current/pi-fleet offsite-write -data /srv/pi-fleet -disk /media/%i
```

When the disk reaches the other building:
`pi-fleet offsite-confirm -data /srv/pi-fleet -label OFFSITE-A -as <you>`.

### 4. Service

```ini
# /etc/systemd/system/pi-fleet.service
[Unit]
Description=pi-fleet master Pi
After=network-online.target
RequiresMountsFor=/srv/pi-fleet /srv/pi-fleet-backup

[Service]
User=pifleet
ExecStart=/opt/pi-fleet/current/pi-fleet serve -data /srv/pi-fleet -listen :443 \
    -tls-cert /etc/pi-fleet/tls/cert.pem -tls-key /etc/pi-fleet/tls/key.pem -releases /srv/pi-fleet/releases
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/srv/pi-fleet /srv/pi-fleet-backup
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Employee Pi

```sh
sudo useradd --system --home /var/lib/pi-fleet --shell /usr/sbin/nologin pifleet
sudo mkdir -p /var/lib/pi-fleet && sudo chown pifleet: /var/lib/pi-fleet
# install /opt/pi-fleet as above, then:
sudo -u pifleet /opt/pi-fleet/current/pi-fleet init -data /var/lib/pi-fleet -role node
sudo -u pifleet /opt/pi-fleet/current/pi-fleet activate -data /var/lib/pi-fleet \
    -central https://fleet.example.org -username <you>
```

Read the six words to your super user. Once they confirm, enable
`pi-fleet.service` with
`ExecStart=/opt/pi-fleet/current/pi-fleet run -data /var/lib/pi-fleet`
(and `ReadWritePaths=/var/lib/pi-fleet`), and open http://127.0.0.1:8080.

A Pi 5 RTC battery and full-disk encryption are strongly recommended.

## Updates (both kinds of Pi)

Releases are signed with an offline key and verified against keys compiled
into the binary (DESIGN.md §9). A super user approves a release by copying
its files into the master Pi's releases directory. Employee Pis fetch from
there.

```ini
# /etc/systemd/system/pi-fleet-update.service
[Unit]
Description=Install a signed pi-fleet release

[Service]
Type=oneshot
ExecStartPre=/bin/systemctl stop pi-fleet.service
ExecStart=/opt/pi-fleet/current/pi-fleet update -data /var/lib/pi-fleet -root /opt/pi-fleet
ExecStopPost=/bin/systemctl start pi-fleet.service
```

On the master Pi, add `-from /srv/pi-fleet/releases` and use
`-data /srv/pi-fleet`. Pair it with a timer for the maintenance window. If
the new version's self-check fails, the update puts back the previous
binary and the pre-update database copy.

## Making a release (maintainers)

```sh
pi-fleet release-keygen -out /media/offline/release.key   # once; publish the public key
make release VERSION=v1.0.0 RELEASE_KEYS=<public key line>
# on the offline machine:
pi-fleet release-sign -key /media/offline/release.key -dir dist/v1.0.0 -version v1.0.0
minisign -Vm dist/v1.0.0/manifest.json -P <public key line>   # anyone can check
```

## Disaster recovery

See DESIGN.md §8.4. In short: mount the newest off-site disk, then

```sh
pi-fleet restore -data /srv/pi-fleet -manifest /media/OFFSITE-A/pifleet-....json \
    -identity identity.txt [-events /srv/pi-fleet-backup/events]
```

then restore `/srv/pi-fleet/keys` from escrow and start the service.
Employee Pis re-send everything newer on their next sync.
