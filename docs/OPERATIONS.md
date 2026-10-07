# pi-fleet operations

How to install and run pi-fleet on the master Pi and on employee Pis. The
design and its reasons are in [DESIGN.md](DESIGN.md).

**The easy way:** run the release file itself; it is a guided installer
(`pi-fleet setup`, see the README's *Install*). It does sections 1, 2 and 4
below for a master Pi, with its data on the external drive mounted at
`/srv/pi-fleet` (TLS certificate in `/srv/pi-fleet/tls`, port 443), and the
employee and kiosk Pi sections. Backups (section 3) are still set up by
hand. The rest of this document is what setup does, step by step, for
anyone who wants to do it themselves or needs to repair an installation.

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

Back up `/srv/pi-fleet/keys` into the encrypted key escrow now (§8.1). The
fleet CA (`fleet-ca.key`) is created there the first time `serve` runs, so
escrow the directory again after that.

### 3. Backups

Setup does all of this (run it on the master Pi and choose **Set up
backups**): it prepares the backup drive (ext4 labelled `PIFLEET-BACKUP`,
mounted at `/srv/pi-fleet-backup` by UUID), makes the backup keys and saves
each to a USB stick as a standard age key file, configures and tests the
first backup, prepares and registers the off-site disks, and installs the
automatic off-site write below. By hand:

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

`backup-config` marks the directory with `.pi-fleet-backup-drive`. If the
mark is missing (the backup drive is unplugged and only its empty mount
point is left), no backup is written and the web interface warns.

To have central write automatically when a registered disk is plugged in,
mount off-site disks by label and trigger a unit:

```
# /etc/fstab, one line per off-site disk
LABEL=OFFSITE-A  /media/OFFSITE-A  ext4  noauto,nofail,noatime,x-gvfs-hide,x-systemd.device-timeout=10s  0  0
```

```
# /etc/udev/rules.d/90-pi-fleet-offsite.rules
ACTION=="add", SUBSYSTEM=="block", ENV{ID_FS_LABEL}=="OFFSITE-*", \
  TAG+="systemd", ENV{SYSTEMD_WANTS}+="pi-fleet-offsite@%E{ID_FS_LABEL}.service"
```

```ini
# /etc/systemd/system/pi-fleet-offsite@.service
[Unit]
Description=Write a verified pi-fleet backup to off-site disk %i
RequiresMountsFor=/media/%i

[Service]
Type=oneshot
User=pifleet
ExecStart=/opt/pi-fleet/current/pi-fleet offsite-write -data /srv/pi-fleet -disk /media/%i
ExecStopPost=+/usr/bin/umount /media/%i
```

(`%i`, not `%I`: unescaping would turn the dash in `OFFSITE-A` into a
slash. The disk is closed afterwards, so it can be unplugged once it
disappears.)

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

### Kiosk (shared Pi)

A super user creates the kiosk on the master Pi (web *Kiosks* page, or
`pi-fleet kiosk-create -data /srv/pi-fleet -as <you> -name nyc-shop -site NYC`)
and adds members. Then on the shared Pi:

```sh
sudo -u pifleet /opt/pi-fleet/current/pi-fleet activate -data /var/lib/pi-fleet \
    -central https://fleet.example.org -kiosk nyc-shop
```

Confirm the six words on the master Pi's *Pis* page, and run it like any
other Pi. To use the kiosk from tablets on the site network, serve HTTPS with
a certificate from the master Pi:

```sh
pi-fleet run -data /var/lib/pi-fleet -listen 0.0.0.0:8443 -https -tls-names kiosk-nyc.local,192.168.1.20
```

and install the fleet CA (download it from the master Pi's *Pis* page,
`/v1/fleet-ca.pem`) on each tablet once. Certificates renew automatically.

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
