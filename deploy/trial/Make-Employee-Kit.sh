#!/usr/bin/env bash
# Double-click launcher, on the master: puts everything an employee Pi
# needs (this kit, the release binary, the master's certificate) in one
# folder, ready to copy to the employee Pi by USB stick or over the network.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"
DRY=0; [ "${1:-}" = --dry-run ] && DRY=1

echo "=== Make the employee Pi kit ==="
echo
CERT=/etc/pi-fleet/tls/cert.pem
[ -f "$CERT" ] || [ $DRY -eq 1 ] || { echo "No $CERT: set up the master first (Setup-Master-Pi.sh)."; exit 1; }
BIN=$(find_release_binary "$KIT")
[ -n "$BIN" ] || ask BIN "Couldn't find a pi-fleet release binary. Type its full path"
OUT="$HOME/Desktop/pi-fleet-employee-kit"
[ -d "$HOME/Desktop" ] || OUT="$HOME/pi-fleet-employee-kit"
PORT=$(grep -o -- '-listen :[0-9]*' /etc/systemd/system/pi-fleet.service 2>/dev/null | cut -d: -f2 || true)
URL="https://$(hostname).local:${PORT:-8443}"

if [ $DRY -eq 1 ]; then
  echo "+ would copy the kit, $BIN and $CERT to $OUT, and note $URL in it"
  exit 0
fi
rm -rf "$OUT"
mkdir -p "$OUT"
cp "$KIT"/*.sh "$KIT/README.md" "$OUT/"
rm -f "$OUT/Sign-And-Install-Release.sh" "$OUT/Preview-Website.sh"   # only for the development Pi
cp "$BIN" "$OUT/"
cp "$CERT" "$OUT/cert.pem"
echo "$URL" > "$OUT/master-url.txt"
chmod +x "$OUT"/*.sh "$OUT/$(basename "$BIN")"
echo "Made $OUT"
echo "  master address: $URL"
echo
echo "Copy the whole folder to the employee Pi, either on a USB stick or"
echo "over the network (that needs SSH turned on on the employee Pi)."
echo
if confirm "Send it over the network now?" n; then
  ask DEST "Employee Pi login, e.g. tess@fleet-tess.local"
  scp -r "$OUT" "$DEST:"
  echo "Sent. On the employee Pi, open the pi-fleet-employee-kit folder in"
  echo "its home folder and double-click Setup-Employee-Pi.sh."
else
  echo "On the employee Pi, open the copied folder and double-click"
  echo "Setup-Employee-Pi.sh."
fi
