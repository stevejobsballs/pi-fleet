#!/usr/bin/env bash
# Double-click launcher: removes pi-fleet from this Pi (master or
# employee) so it can be set up again from scratch. On a master Pi it asks
# whether to keep the records (the database) on the data drive or delete
# them too. It lists what it will remove and asks you to confirm first.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/../click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"

echo "=== Reset pi-fleet on this Pi ==="
echo "You'll be asked for your sudo password."
echo
if [[ " $* " == *" --dry-run "* ]]; then
  bash "$KIT/reset.sh" "$@"
else
  sudo bash "$KIT/reset.sh" "$@"
fi
