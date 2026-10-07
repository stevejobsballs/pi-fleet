# Tools for an installed Pi

Double-click either file in the File Manager (choose **Execute** if asked).
Each opens a terminal window, asks for your password, and waits for Enter
before closing so you can read the result. They work on a master Pi and on
an employee or kiosk Pi. Installing a Pi is done by the release file itself
(`pi-fleet setup`); see the main README.

| File | What it does |
|---|---|
| `Check-Pi.sh` | Shows this Pi's role, version and service state, runs pi-fleet's integrity check, lists the Pis (on a master Pi), and shows any data files not owned by pi-fleet's user (there should be none) and the recent log. |
| `Update-Pi.sh` | Installs the newest signed release: on a master Pi from its release folder (`/srv/pi-fleet/releases`, filled by setup's update), on an employee Pi from the master Pi. The update checks the release's signature, copies the database first, and rolls back if the new version fails its checks. |

Both accept `--dry-run` when run from a terminal, which shows what they
would do without changing anything.
