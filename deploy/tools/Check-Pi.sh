#!/usr/bin/env bash
# Double-click launcher: shows the state of pi-fleet on this Pi.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/../click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"

echo "=== pi-fleet on $(hostname) ==="
echo "You'll be asked for your sudo password."
echo
if [ "${1:-}" = --dry-run ]; then
  bash "$KIT/check.sh" --dry-run
else
  sudo bash "$KIT/check.sh"
fi
