#!/usr/bin/env bash
# Removes pi-fleet from this Pi (master or employee) so it can be set up
# again from scratch: the service, the program, its settings and keys, and
# the records kept on the SD card. Drives are never erased or changed: a
# master's data and backup drives are only unmounted and forgotten, so
# they still hold their copy of the records. See README.md.
set -euo pipefail

. "$(dirname "$0")/lib.sh"

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) echo "usage: sudo ./reset.sh [--dry-run]"; exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done
need_root

FSTAB=/etc/fstab
# pi_fleet_fstab prints fstab without pi-fleet's drives and comments.
pi_fleet_fstab() {
  awk '$2 == "/srv/pi-fleet" || $2 == "/srv/pi-fleet-backup" { next }
       /^# pi-fleet / || /^# drive lost, removed by pi-fleet setup/ { next }
       { print }' "$FSTAB"
}

ROLE="not set up"
if is_master; then ROLE="master Pi"; elif [ -f /etc/systemd/system/pi-fleet.service ]; then ROLE="employee or kiosk Pi"; fi
HOMEDIR=$(getent passwd "${SUDO_USER:-root}" | cut -d: -f6)

echo "This Pi: $ROLE ($(/opt/pi-fleet/current/pi-fleet version 2>/dev/null || echo "no pi-fleet program"))"
echo
echo "This removes pi-fleet from this Pi so it can be set up again from scratch:"
echo "  - the pi-fleet service and program (/opt/pi-fleet), its settings and keys (/etc/pi-fleet)"
[ -d /var/lib/pi-fleet ] && echo "  - this Pi's own records (/var/lib/pi-fleet): anything not yet synced to the master is lost"
for d in /srv/pi-fleet /srv/pi-fleet-backup; do
  if mountpoint -q "$d"; then
    echo "  - $d is unmounted and removed from /etc/fstab: the drive itself is NOT erased"
  elif [ -e "$d" ]; then
    echo "  - $d (on the SD card)"
  fi
done
for d in /srv/pi-fleet.on-sd-card-*; do [ -e "$d" ] && echo "  - $d (an old copy of the records on the SD card)"; done
[ -e /etc/systemd/system/pi-fleet-offsite@.service ] && echo "  - the off-site backup rule"
[ -d "$HOMEDIR/pi-fleet-install" ] && echo "  - $HOMEDIR/pi-fleet-install (setup's copy of the release)"
id pifleet >/dev/null 2>&1 && echo "  - the pifleet system user"
if ! diff -q "$FSTAB" <(pi_fleet_fstab) >/dev/null; then
  echo "  - pi-fleet's lines in /etc/fstab (a copy of the file is kept)"
fi
echo
echo "Kept: this Pi's name, the network settings, every drive and USB stick and"
echo "what's on it, and the setup folders."
if is_master; then
  echo
  echo "After this, employee and kiosk Pis can't connect to this master any more:"
  echo "reset them too and set them up again against the new master."
fi
echo
if [ "$DRY_RUN" -eq 0 ]; then
  read -r -p "Type RESET to remove pi-fleet from this Pi (anything else stops): " answer
  [ "$answer" = RESET ] || { echo "Stopped; nothing was changed."; exit 1; }
fi

echo
echo "== stopping pi-fleet"
if systemctl list-unit-files pi-fleet.service >/dev/null 2>&1; then
  run systemctl disable --now pi-fleet || true
fi
run rm -f /etc/systemd/system/pi-fleet.service /etc/systemd/system/pi-fleet-offsite@.service /etc/udev/rules.d/90-pi-fleet-offsite.rules

echo "== forgetting pi-fleet's drives (they are not erased)"
for d in /srv/pi-fleet-backup /srv/pi-fleet; do
  if mountpoint -q "$d"; then run umount "$d"; fi
  if [ "$DRY_RUN" -eq 0 ] && mountpoint -q "$d"; then
    die "$d is still in use, so nothing on it was removed. Restart the Pi and run this again."
  fi
done
if ! diff -q "$FSTAB" <(pi_fleet_fstab) >/dev/null; then
  saved="$FSTAB.before-pi-fleet-reset-$(date +%Y%m%d-%H%M%S)"
  run cp -p "$FSTAB" "$saved"
  if [ "$DRY_RUN" -eq 0 ]; then
    pi_fleet_fstab >"$FSTAB.new" && mv "$FSTAB.new" "$FSTAB"
  else
    diff "$FSTAB" <(pi_fleet_fstab) || true
  fi
fi
run systemctl daemon-reload
command -v udevadm >/dev/null && run udevadm control --reload || true

echo "== removing the program, settings and SD card records"
run rm -rf /opt/pi-fleet /etc/pi-fleet /var/lib/pi-fleet /srv/pi-fleet /srv/pi-fleet-backup
for d in /srv/pi-fleet.on-sd-card-*; do [ -e "$d" ] && run rm -rf "$d"; done
[ -d "$HOMEDIR/pi-fleet-install" ] && run rm -rf "$HOMEDIR/pi-fleet-install"
if id pifleet >/dev/null 2>&1; then run userdel pifleet; fi

echo
echo "pi-fleet is removed. To set this Pi up again, run Set-up-this-Pi.sh from"
echo "the setup folder (pi-fleet-master-setup or pi-fleet-employee-setup)."
