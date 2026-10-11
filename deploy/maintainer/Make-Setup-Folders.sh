#!/usr/bin/env bash
# Double-click launcher for the release maintainer: makes the two folders
# people copy onto a USB stick to install pi-fleet, on the Desktop, from
# the newest signed release in this repository:
#
#   pi-fleet-master-setup     for the master Pi
#   pi-fleet-employee-setup   for employee and kiosk Pis
#
# Each holds the release, Set-up-this-Pi.sh, the tools for an installed
# Pi and a READ ME FIRST.txt. Run on the master Pi, the employee folder's
# read-me also names the master's address and certificate fingerprint.
# Any previous copies of the two folders are replaced.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/../click-lib.sh"
if [ -z "${PIFLEET_NO_TERMINAL:-}" ]; then
  open_terminal "$(readlink -f "$0")" "$@"
fi
DEPLOY=$(cd "$KIT/.." && pwd)
REPO=$(cd "$DEPLOY/.." && pwd)
OUT=${1:-$HOME/Desktop}

DIR=$(ls -d "$REPO"/dist/v*/ 2>/dev/null | sort -V | tail -n 1)
DIR=${DIR%/}
[ -n "$DIR" ] || { echo "No release in $REPO/dist. Build one with: make release VERSION=vX.Y.Z"; exit 1; }
VER=$(basename "$DIR")
[ -f "$DIR/manifest.json.minisig" ] || { echo "$VER isn't signed yet: run Sign-And-Install-Release.sh first."; exit 1; }
BIN=pi-fleet_${VER}_linux_arm64

# On the master Pi, fill in its address and certificate fingerprint.
MASTER="(the master's address, shown at the end of its setup and on its Pis page)"
FINGERPRINT="(shown at the end of the master's setup and on its Pis page)"
if grep -qs -- "-releases /srv/pi-fleet/releases" /etc/systemd/system/pi-fleet.service; then
  fp=$(echo | timeout 10 openssl s_client -connect 127.0.0.1:8443 2>/dev/null | openssl x509 -outform DER 2>/dev/null | sha256sum | cut -c1-64 || true)
  if [ ${#fp} -eq 64 ] && [ "$fp" != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" ]; then
    FINGERPRINT=$(echo "$fp" | tr a-f A-F | sed 's/..../& /g; s/ $//')
    MASTER="$(hostname).local:8443"
  fi
fi

# make_folder NAME TEMPLATE TOOL...: one setup folder.
make_folder() {
  local name=$1 template=$2 dest="$OUT/$1" tool
  shift 2
  rm -rf "$dest"
  mkdir -p "$dest/tools"
  cp "$DIR/$BIN" "$DIR/manifest.json" "$DIR/manifest.json.minisig" "$dest/"
  cp "$DEPLOY/Set-up-this-Pi.sh" "$DEPLOY/click-lib.sh" "$dest/"
  for tool in "$@"; do cp "$DEPLOY/tools/$tool" "$dest/tools/"; done
  chmod +x "$dest/$BIN" "$dest/Set-up-this-Pi.sh" "$dest"/tools/*.sh
  sed -e "s|{{VERSION}}|$VER|g" -e "s|{{MASTER}}|$MASTER|g" -e "s|{{FINGERPRINT}}|$FINGERPRINT|g" \
    "$DEPLOY/setup-folders/$template" >"$dest/READ ME FIRST.txt"
  echo "  $dest"
}

echo "Making the setup folders for $VER in $OUT:"
make_folder pi-fleet-master-setup master-READ-ME-FIRST.txt \
  Check-Pi.sh check.sh Update-Pi.sh update.sh Reset-Pi.sh reset.sh lib.sh Share-Wired-Network.sh
cp "$REPO/docs/NETWORK.md" "$OUT/pi-fleet-master-setup/NETWORK-guide-for-IT.md"
make_folder pi-fleet-employee-setup employee-READ-ME-FIRST.txt \
  Check-Pi.sh check.sh Update-Pi.sh update.sh Reset-Pi.sh reset.sh lib.sh
echo
echo "Copy a folder onto a USB stick (drag it), eject the stick, and follow"
echo "the READ ME FIRST.txt inside on the Pi being set up."
