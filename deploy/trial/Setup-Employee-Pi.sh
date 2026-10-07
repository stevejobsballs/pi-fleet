#!/usr/bin/env bash
# Double-click launcher: sets up this Pi as a trial employee Pi (or a
# kiosk). Expects the folder made by Make-Employee-Kit.sh on the master.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"
DRY=(); [ "${1:-}" = --dry-run ] && DRY=(--dry-run)

echo "=== Set up a pi-fleet employee Pi ==="
echo
[ ${#DRY[@]} -gt 0 ] || offer_rename "fleet-$(id -un)"

BIN=$(find_release_binary "$KIT")
if [ -n "$BIN" ]; then
  echo "Release binary: $BIN"
else
  ask BIN "Couldn't find a pi-fleet release binary. Type its full path"
fi
CERT=$(find_newest cert.pem "$KIT" "$HOME" "$HOME/Downloads")
if [ -n "$CERT" ]; then
  echo "Master certificate: $CERT"
else
  ask CERT "Couldn't find the master's cert.pem. Type its full path"
fi
URL=""
[ -f "$KIT/master-url.txt" ] && URL=$(head -n 1 "$KIT/master-url.txt")
echo
ask URL "Master Pi address" "${URL:-https://fleet-master.local:8443}"

echo
WHO=()
if confirm "Is this a shared kiosk Pi (rather than one person's own Pi)?" n; then
  ask NAME "Kiosk name, as the super user created it"
  WHO=(--kiosk "$NAME")
else
  ask NAME "Your username, from the super user"
  WHO=(--username "$NAME")
fi
echo
echo "You'll be asked for your sudo password, then the one-time password."
echo "When six words appear, a super user types them on the master's Pis page."
echo
if [ ${#DRY[@]} -gt 0 ]; then
  bash "$KIT/node-setup.sh" --binary "$BIN" --ca "$CERT" --master "$URL" "${WHO[@]}" "${DRY[@]}"
else
  sudo bash "$KIT/node-setup.sh" --binary "$BIN" --ca "$CERT" --master "$URL" "${WHO[@]}"
fi
echo
echo "pi-fleet is running: open http://127.0.0.1:8080 in the web browser."
