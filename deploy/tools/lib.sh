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
