#!/usr/bin/env bash
# Double-click launcher: sets up this Pi as the trial master.
# Runs master-setup.sh with the newest release binary it can find.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"
DRY=(); [ "${1:-}" = --dry-run ] && DRY=(--dry-run)

echo "=== Set up the pi-fleet master Pi ==="
echo
[ ${#DRY[@]} -gt 0 ] || offer_rename fleet-master

BIN=$(find_release_binary "$KIT")
if [ -n "$BIN" ]; then
  echo "Release binary: $BIN"
else
  ask BIN "Couldn't find a pi-fleet release binary. Type its full path"
fi
echo
echo "You'll be asked for your sudo password, then to choose the first"
echo "super user's name and password."
echo
if [ ${#DRY[@]} -gt 0 ]; then
  bash "$KIT/master-setup.sh" --binary "$BIN" "${DRY[@]}"
else
  sudo bash "$KIT/master-setup.sh" --binary "$BIN"
fi
