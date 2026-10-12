#!/usr/bin/env bash
# Starts pi-fleet's setup on this Pi from a folder holding a release
# (pi-fleet_<version>_linux_arm64, manifest.json, manifest.json.minisig),
# even when the folder is on a USB stick. Programs can't run straight from
# most USB sticks (their FAT format can't mark a file as runnable: running
# it gives "Permission denied"), so this copies the release onto the Pi
# first. Put it in the folder with the release, drag the folder onto the
# Pi's desktop, allow this file to run (right-click, Properties,
# Permissions) and double-click it: setup opens in the web browser, after
# the desktop asks for the administrator password. From a terminal:
#
#     bash /media/$USER/*/<folder>/Set-up-this-Pi.sh
#
# (bash runs it even from a stick.) Any options are passed to setup, such
# as -dry-run.
set -euo pipefail
SELF=$(readlink -f "$0")
SRC=$(dirname "$SELF")
DEST="$HOME/pi-fleet-install"

# Double-clicked: no terminal. Problems are shown in a message window, and
# setup itself opens in the web browser after the desktop's password
# window. Typed in a terminal, everything stays there.
desktop=0
if [ ! -t 0 ] && { [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ]; }; then desktop=1; fi
tell() { # tell "message": shows it, in a window when double-clicked
  if [ "$desktop" = 1 ] && command -v zenity >/dev/null; then
    zenity --error --title "pi-fleet setup" --width 480 --text "$1" 2>/dev/null || true
  elif [ "$desktop" = 1 ] && command -v xmessage >/dev/null; then
    xmessage -center "$1" 2>/dev/null || true
  else
    echo "$1"
  fi
}

if [ "$(uname -m)" != aarch64 ]; then
  tell "This Pi runs the 32-bit version of Raspberry Pi OS ($(uname -m)). pi-fleet needs the 64-bit version: put Raspberry Pi OS (64-bit) on it with Raspberry Pi Imager, then try again."
  exit 1
fi
BIN=$(ls "$SRC"/pi-fleet_v*_linux_arm64 2>/dev/null | sort -V | tail -n 1 || true)
if [ -z "$BIN" ]; then
  tell "No pi-fleet release (pi-fleet_v…_linux_arm64) was found next to this file, in $SRC. Download the release's files into the same folder, then try again."
  exit 1
fi
if [ "$SRC" != "$DEST" ]; then
  # Programs can't run straight from most USB sticks, so copy it first.
  rm -rf "$DEST"
  mkdir -p "$DEST"
  if ! cp -r "$SRC"/. "$DEST"/ || ! sync; then
    tell "The release couldn't be copied onto this Pi. If it's on a USB stick, take the stick out properly (eject it) on the computer you copied from, copy the files again, and try once more."
    exit 1
  fi
fi
chmod +x "$DEST/$(basename "$BIN")"
[ -d "$DEST/tools" ] && chmod +x "$DEST"/tools/*.sh
# Any options are passed to setup, such as -dry-run.
exec "$DEST/$(basename "$BIN")" setup "$@"
