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
# (bash runs it even from a stick.) Or drag the folder onto the Pi's
# desktop, allow this file to run (right-click, Properties, Permissions)
# and double-click it. Any options are passed to setup, such as -dry-run.
set -euo pipefail
SELF=$(readlink -f "$0")
# Double-clicked: there is no terminal to show messages in, so open one.
if [ ! -t 0 ] && [ -z "${PIFLEET_IN_TERMINAL:-}" ] && { [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ]; }; then
  PIFLEET_IN_TERMINAL=1 exec x-terminal-emulator -t "pi-fleet setup" -e "$(printf '%q ' bash "$SELF" "$@")"
fi
# Keep the window open if something goes wrong, so the message can be read.
trap 'status=$?; if [ $status -ne 0 ]; then echo; read -r -p "Press Enter to close this window. " _ || true; fi' EXIT
SRC=$(dirname "$SELF")
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
trap - EXIT
if [ -n "${PIFLEET_IN_TERMINAL:-}" ]; then
  set -- -pause "$@" # this script opened the window: keep it open at the end
fi
exec "$DEST/$(basename "$BIN")" setup "$@"
