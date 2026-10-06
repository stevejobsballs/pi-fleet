# pi-fleet

Minimal, offline-first equipment management (preventive maintenance,
calibrations, work orders, inventory) for Raspberry Pi. Employee Pis work
offline on a 31-day working set and sync over outbound HTTPS to a central
master Pi that keeps every record indefinitely. No PHI.

**Status:** early development. The design is in [docs/DESIGN.md](docs/DESIGN.md).
Built so far: the signed, append-only event log; users, roles, sites,
equipment, work orders, calibrations, PM schedules and inventory;
activation and sync between employee Pis and the master Pi; Part 11-style
electronic signatures; and the web interface. Not yet: backups and signed
releases.

## Build

Requires Go 1.27+.

```sh
make            # vet, test, build bin/pi-fleet
make build-arm64  # reproducible static linux/arm64 build in dist/
```

## Try it

On the master Pi:

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

## License

Apache-2.0. See [LICENSE](LICENSE).
