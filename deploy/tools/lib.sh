# Shared helpers for check.sh and update.sh. Sourced, not run.

DRY_RUN=0

# run prints a command and runs it, or only prints it with --dry-run.
run() {
  printf '+ %s\n' "$*"
  if [ "$DRY_RUN" -eq 0 ]; then "$@"; fi
}

die() { echo "error: $*" >&2; exit 1; }

need_root() {
  if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
    die "run this with sudo"
  fi
}

# is_master: the service file says which kind of Pi this is (a master Pi
# runs "serve"), and anyone can read it.
is_master() { grep -q " serve " /etc/systemd/system/pi-fleet.service 2>/dev/null; }

# data_drive_missing: a master Pi whose data drive (in /etc/fstab) isn't
# open. pi-fleet doesn't run without it, so nothing can be installed.
data_drive_missing() {
  is_master && grep -Eq '^[^#]+[[:space:]]/srv/pi-fleet[[:space:]]' /etc/fstab && ! mountpoint -q /srv/pi-fleet
}

DATA_DRIVE_HELP="The master Pi's data drive isn't connected, so pi-fleet isn't running.
Shut the Pi down, plug the data drive (labelled PIFLEET-DATA) back in, and
start the Pi again; pi-fleet then starts by itself. Then try again."
