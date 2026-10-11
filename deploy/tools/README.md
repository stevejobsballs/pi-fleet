# Tools for an installed Pi

Double-click a file in the File Manager (choose **Execute** if asked).
Each opens a terminal window, asks for your password, and waits for Enter
before closing so you can read the result. They work on a master Pi and on
an employee or kiosk Pi. Installing a Pi is done by the release file itself
(`pi-fleet setup`); see the main README.

| File | What it does |
|---|---|
| `Check-Pi.sh` | Shows this Pi's role, version and service state, runs pi-fleet's integrity check, lists the Pis (on a master Pi), and shows any data files not owned by pi-fleet's user (there should be none) and the recent log. |
| `Reset-Pi.sh` | Removes pi-fleet from this Pi so it can be set up again from scratch: the service, the program, its settings and keys, and any records on the SD card. It lists everything first and asks you to type RESET. Drives are never erased: a master's data and backup drives are only unmounted and forgotten, and keep their copy of the records. After resetting a master, reset its employee and kiosk Pis too and set them up against the new master. |
| `Share-Wired-Network.sh` | On a master Pi, for trials where the Pis are joined only by a network switch with no router: shares the master's wired port, so the other Pis get an address from it (and its internet connection). Run it again to stop sharing. |
| `Update-Pi.sh` | Installs the newest signed release: on a master Pi from its release folder (`/srv/pi-fleet/releases`, filled by setup's update), on an employee Pi from the master Pi. The update checks the release's signature, copies the database first, and rolls back if the new version fails its checks. |

Each accepts `--dry-run` when run from a terminal, which shows what they
would do without changing anything.
