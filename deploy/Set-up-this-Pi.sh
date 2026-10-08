#!/usr/bin/env bash
# Starts pi-fleet's setup on this Pi from a folder holding a release
# (pi-fleet_<version>_linux_arm64, manifest.json, manifest.json.minisig),
# even when the folder is on a USB stick. Programs can't run straight from
# most USB sticks (their FAT format can't mark a file as runnable: running
# it gives "Permission denied"), so this copies the release onto the Pi
# first. Put it in the folder with the release, then in a terminal:
#
#     bash /media/$USER/*/<folder>/Set-up-this-Pi.sh
#
# (bash runs it even from a stick.) Any options are passed to setup, such
# as -dry-run.
set -euo pipefail
SRC=$(dirname "$(readlink -f "$0")")
DEST="$HOME/pi-fleet-install"

if [ "$(uname -m)" != aarch64 ]; then
  echo "This Pi runs the 32-bit version of Raspberry Pi OS ($(uname -m))."
  echo "pi-fleet needs the 64-bit version. Put Raspberry Pi OS (64-bit) on it"
  echo "with Raspberry Pi Imager, then run this again."
  exit 1
fi
BIN=$(ls "$SRC"/pi-fleet_v*_linux_arm64 2>/dev/null | sort -V | tail -n 1 || true)
if [ -z "$BIN" ]; then
  echo "No pi-fleet release (pi-fleet_v…_linux_arm64) found next to this script in $SRC."
  echo "Download the release's files into the same folder, then run this again."
  exit 1
fi
if [ "$SRC" != "$DEST" ]; then
  echo "Copying the release onto this Pi ($DEST)..."
  rm -rf "$DEST"
  mkdir -p "$DEST"
  cp -r "$SRC"/. "$DEST"/
  sync
  echo "Copied. Eject the USB stick before taking it out."
fi
chmod +x "$DEST/$(basename "$BIN")"
[ -d "$DEST/tools" ] && chmod +x "$DEST"/tools/*.sh
echo
exec "$DEST/$(basename "$BIN")" setup "$@"
