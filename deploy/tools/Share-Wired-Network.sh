#!/usr/bin/env bash
# Double-click launcher, on a master Pi: shares its wired network port, so
# Pis plugged into the same switch get an address from the master (and its
# internet connection) when there is no router on the switch. For trials;
# on a proper network, choose "Stop sharing". Uses NetworkManager.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/../click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"

CONN=$(nmcli -t -f NAME,TYPE connection show | awk -F: '$2=="802-3-ethernet"{print $1; exit}')
[ -n "$CONN" ] || { echo "No wired network connection found on this Pi."; exit 1; }
METHOD=$(nmcli -g ipv4.method connection show "$CONN")
echo "=== Wired network: $CONN (now: $METHOD) ==="
echo
if [ "$METHOD" = shared ]; then
  echo "This Pi already shares its wired port with Pis on the switch."
  if confirm "Stop sharing (for when the Pis are on a normal network with a router)?" n; then
    sudo nmcli connection modify "$CONN" ipv4.method auto
    sudo nmcli connection up "$CONN" || true
    echo "Stopped sharing; the wired port now gets its address from the network."
  fi
  exit 0
fi
echo "Pis plugged into the same switch as this one get no address unless"
echo "something hands them out. This Pi can: they then get an address from it"
echo "and use its internet connection."
confirm "Share this Pi's wired port?" y || exit 0
echo "You'll be asked for your sudo password."
sudo nmcli connection modify "$CONN" ipv4.method shared
sudo nmcli connection up "$CONN"
echo
ip -br addr show "$(nmcli -g connection.interface-name connection show "$CONN" 2>/dev/null || echo eth0)" 2>/dev/null || true
echo "Shared. Plug the other Pis into the switch; they get an address by themselves."
