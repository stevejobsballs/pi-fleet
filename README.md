# pi-fleet

Minimal, offline-first equipment management (preventive maintenance,
calibrations, work orders, inventory) for Raspberry Pi. Employee Pis work
offline on a 31-day working set and sync over outbound HTTPS to a central
master Pi that keeps every record indefinitely. No PHI.

**Status:** early development. The design is in [docs/DESIGN.md](docs/DESIGN.md).
Built so far: the signed, hash-chained, append-only event log that every
other feature is built on.

## Build

Requires Go 1.27+.

```sh
make            # vet, test, build bin/pi-fleet
make build-arm64  # reproducible static linux/arm64 build in dist/
```

## Try it

```sh
bin/pi-fleet init -data ./demo     # create node keys, database, genesis event
bin/pi-fleet verify -data ./demo   # re-check signatures, hashes and chain links
```

## License

Apache-2.0. See [LICENSE](LICENSE).
