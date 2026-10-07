#!/usr/bin/env bash
# Double-click launcher for the release maintainer: signs the newest
# release built with "make release" in this repository, using the release
# key on its USB stick (read in place, never copied). If this Pi is also a
# master Pi, it then offers to install the release and put it in the
# master's release folder for the employee Pis.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/../click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"
REPO=$(cd "$KIT/../.." && pwd)

echo "=== Sign and install a pi-fleet release ==="
echo
DIR=$(ls -d "$REPO"/dist/v*/ 2>/dev/null | sort -V | tail -n 1)
DIR=${DIR%/}
[ -n "$DIR" ] || { echo "No release in $REPO/dist. Build one with: make release VERSION=vX.Y.Z"; exit 1; }
VER=$(basename "$DIR")
BIN="$DIR/pi-fleet_${VER}_linux_$(pi_arch)"
echo "Newest release: $VER ($DIR)"

if [ -f "$DIR/manifest.json.minisig" ]; then
  echo "It is already signed."
else
  # The encrypted release key, read in place on its USB stick: set
  # PIFLEET_RELEASE_KEY to its path, or keep it on a stick as
  # pi-fleet-release.key (any folder up to two levels down).
  KEY=${PIFLEET_RELEASE_KEY:-}
  while [ -z "$KEY" ] || [ ! -f "$KEY" ]; do
    KEY=$(find /media/"$(id -un)" -maxdepth 4 -name pi-fleet-release.key -type f 2>/dev/null | head -n 1)
    [ -n "$KEY" ] && break
    read -r -p "Plug in the USB stick with the release key (pi-fleet-release.key), wait a few seconds, then press Enter. " _
  done
  echo "Release key: $KEY"
  echo "Type the release key's passphrase when asked (it isn't shown as you type)."
  echo
  "$BIN" release-sign -key "$KEY" -dir "$DIR" -version "$VER"
  echo
  echo "Signed. Eject the USB stick in the File Manager, then unplug it and put it away."
fi

echo
if [ ! -f /etc/systemd/system/pi-fleet.service ] || ! grep -q -- "-releases /srv/pi-fleet/releases" /etc/systemd/system/pi-fleet.service; then
  echo "This Pi isn't set up as the master, so the release was only signed."
  exit 0
fi
INSTALLED=$(/opt/pi-fleet/current/pi-fleet version 2>/dev/null | awk '{print $2}')
if [ "$INSTALLED" = "$VER" ]; then
  echo "$VER is already installed on this master Pi; there is nothing more to do."
  exit 0
fi
confirm "Install $VER on this master Pi now (it has ${INSTALLED:-no version})?" y || exit 0
echo "You'll be asked for your sudo password."
sudo install -o pifleet -g pifleet -m 0644 "$DIR"/* /srv/pi-fleet/releases/
sudo bash "$KIT/update.sh"
echo
echo "Employee Pis can now install $VER with Update-Pi.sh."
