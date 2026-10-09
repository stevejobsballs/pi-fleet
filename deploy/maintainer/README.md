# Tools for the release maintainer

For whoever builds and signs pi-fleet releases, on their development Pi.
Institutions that only install pi-fleet don't need these.

| File | What it does |
|---|---|
| `Sign-And-Install-Release.sh` | Signs the newest release built with `make release VERSION=vX.Y.Z` in this repository, using the release key on its USB stick. It finds `pi-fleet-release.key` on any plugged-in stick (or set `PIFLEET_RELEASE_KEY` to its path), reads it in place and asks for its passphrase; the key is never copied. If the Pi is also a master Pi, it then offers to install the release there and put it in the master's release folder for the employee Pis. |
| `Make-Setup-Folders.sh` | Makes `pi-fleet-master-setup` and `pi-fleet-employee-setup` on the Desktop from the newest signed release: everything needed to set up a Pi from a USB stick (the release, `Set-up-this-Pi.sh`, the tools and a `READ ME FIRST.txt`; the master's also has the IT network guide). Run on the master Pi, the employee folder's read-me names the master's address and certificate fingerprint. Sign-And-Install-Release runs it after signing. |
| `Preview-Website.sh` | Runs a practice copy of the web interface from this repository, with sample records, at http://127.0.0.1:9080, for trying out changes to its look (`internal/web/static/style.css`) before a release. Needs Go. |

The release key is made once with `pi-fleet release-keygen` on an offline
machine and kept on removable media; its public half is built into every
release (`release-keys.txt`, `make RELEASE_KEYS=...`). Keep a second copy
of the key on another stick, in another place: without it no further
release can be signed.
