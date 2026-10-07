# pi-fleet

Minimal, offline-first equipment management (preventive maintenance,
calibrations, work orders, inventory) for Raspberry Pi. Employee Pis work
offline on a 31-day working set and sync over outbound HTTPS to a central
master Pi that keeps every record indefinitely. No PHI.

**Status:** early development. The design is in [docs/DESIGN.md](docs/DESIGN.md).
Built so far: the signed, append-only event log; users, roles, sites,
equipment, work orders, calibrations, PM schedules and inventory;
activation and sync between employee Pis and the master Pi; Part 11-style
electronic signatures; the web interface; verified, encrypted backups with
off-site USB rotation; signed releases with automatic rollback;
attachments (certificates and photos); and kiosk mode for shared Pis.
Installation and day-to-day operation: [docs/OPERATIONS.md](docs/OPERATIONS.md).

## Install

You need Raspberry Pis (4 or 5) running 64-bit Raspberry Pi OS, on one
network. The first one becomes the **master Pi**, which keeps every record
on an **external drive** (a USB SSD of 250 GB or more). Every other Pi is an
**employee Pi** (one person's) or a **kiosk Pi** (shared in a workshop).

The release file *is* the installer: one file with everything in it. On
each Pi:

1. Download the release's three files into one folder:
   `pi-fleet_<version>_linux_arm64`, `manifest.json` and
   `manifest.json.minisig`.
2. Optionally check they are genuine (see *Release signing key* below).
3. Make the program runnable: in the File Manager right-click it,
   **Properties → Permissions**, and allow executing it. (Or in a terminal:
   `chmod +x pi-fleet_*_linux_arm64`.)
4. Double-click it and choose **Execute** (or run `./pi-fleet_*_linux_arm64`
   in a terminal). Setup opens in a terminal window, asks for your password
   (administrator rights), and guides you from there.

Setup installs whatever else the Pi needs, names the Pi, and then:

- **Master Pi:** asks you to plug in the external drive, prepares it
  (showing what is on it and asking you to type ERASE first) and mounts it
  so that all records, files and keys live on it. pi-fleet will not start
  without it, so nothing is ever written to the SD card. It makes the HTTPS
  certificate and the first super user, and starts pi-fleet. If the Pi
  fails, plug the drive into a new Pi and run setup there: it finds the
  drive and carries on. A master set up before the drive step existed is
  offered a move of its records onto a drive (copied, checked, then
  switched over; the SD card copy is kept). Records already on a drive can
  be moved the same way to a different one, for example from a temporary
  USB stick to an SSD; the old drive keeps its copy, relabelled
  `PIFLEET-OLD`.
- **Employee or kiosk Pi:** asks for the master Pi's name, shows the
  master's certificate fingerprint to compare with the master's **Pis**
  page, then activates the Pi with the one-time password from the super
  user. The super user approves it by typing the six words it shows.

Running setup again on an installed Pi offers to update it to the file's
version, through the verified update with rollback. `-dry-run` shows what
setup would do without changing anything.

## Build

Requires Go 1.27+.

```sh
make            # vet, test, build bin/pi-fleet
make build-arm64  # reproducible static linux/arm64 build in dist/
```

## Try it by hand

The commands setup runs, for development (data in a scratch directory). On
the master Pi:

```sh
pi-fleet init -data /var/lib/pi-fleet -role central
pi-fleet bootstrap -data /var/lib/pi-fleet -username admin -name "Your Name" -email you@example.org
pi-fleet user-create -data /var/lib/pi-fleet -as admin -username tess -name "Tess Tech" \
    -email tess@example.org -verified "in person, badge 4411"     # prints a one-time password
pi-fleet serve -data /var/lib/pi-fleet -tls-cert cert.pem -tls-key key.pem
# web interface at https://<master-pi>:8443/
```

On an employee's Pi:

```sh
pi-fleet init -data /var/lib/pi-fleet -role node
pi-fleet activate -data /var/lib/pi-fleet -central https://fleet.example.org:8443 -username tess
# shows six pairing words; the super user confirms them on the master Pi:
#   pi-fleet nodes -data /var/lib/pi-fleet -status pending_confirmation
#   pi-fleet node-confirm -data /var/lib/pi-fleet -node <id> -as admin
pi-fleet run -data /var/lib/pi-fleet     # web interface at http://127.0.0.1:8080, syncs every 5 minutes
```

## Release signing key

Releases are signed with this minisign key, which every build trusts (see
`release-keys.txt`). Check a release yourself with
`minisign -Vm manifest.json -P RWQAisnE7wGQwd/uCF+DUmXqqMY3QC3F2TPYRxyIBqZJGgvB4xR+U5XM`.

```
untrusted comment: minisign public key C19001EFC4C98A00
RWQAisnE7wGQwd/uCF+DUmXqqMY3QC3F2TPYRxyIBqZJGgvB4xR+U5XM
```

## License

Apache-2.0. See [LICENSE](LICENSE).
