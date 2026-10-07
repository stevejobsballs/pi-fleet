#!/usr/bin/env bash
# Double-click launcher: installs the newest signed release on this Pi
# (master or employee) and checks the result.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/../click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"

echo "=== Update pi-fleet on this Pi ==="
echo "You'll be asked for your sudo password."
echo
if [ "${1:-}" = --dry-run ]; then
  bash "$KIT/update.sh" --dry-run
else
  sudo bash "$KIT/update.sh"
fi
