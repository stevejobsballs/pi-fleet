# Helpers for the double-click launchers (Setup-Master-Pi.sh and friends).
# Sourced, not run. The launchers gather answers with prompts and then run
# the ordinary trial scripts with sudo.

# open_terminal "$0" "$@": a double-clicked script has no terminal to ask
# questions in, so it reopens itself in a terminal window.
open_terminal() {
  local self=$1; shift
  if [ ! -t 0 ] && [ -z "${PIFLEET_IN_TERMINAL:-}" ]; then
    local cmd
    cmd=$(printf '%q ' bash "$self" "$@")
    PIFLEET_IN_TERMINAL=1 exec x-terminal-emulator -t "pi-fleet" -e "$cmd"
  fi
  # Keep the window open at the end, also after an error, so the output
  # can be read (and sent if something went wrong).
  trap 'status=$?; echo; if [ $status -eq 0 ]; then echo "Finished."; else echo "Stopped with an error (see above)."; fi; read -r -p "Press Enter to close this window. " _; exit $status' EXIT
}

# ask VAR "Question" [default]: reads an answer, offering a default.
ask() {
  local __var=$1 question=$2 default=${3:-} answer
  while :; do
    if [ -n "$default" ]; then
      read -r -p "$question [$default]: " answer
      answer=${answer:-$default}
    else
      read -r -p "$question: " answer
    fi
    [ -n "$answer" ] && break
  done
  printf -v "$__var" '%s' "$answer"
}

# confirm "Question" default(y|n): returns 0 for yes.
confirm() {
  local answer hint="[y/N]"
  [ "$2" = y ] && hint="[Y/n]"
  read -r -p "$1 $hint " answer
  answer=${answer:-$2}
  [ "$answer" = y ] || [ "$answer" = Y ]
}

# pi_arch prints the release architecture of this Pi (arm64 or amd64).
pi_arch() {
  case "$(uname -m)" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64) echo amd64 ;;
    *) uname -m ;;
  esac
}

# find_newest PATTERN DIR...: prints the newest-versioned file matching
# PATTERN in the given folders, or nothing.
find_newest() {
  local pattern=$1; shift
  local d
  for d in "$@"; do
    [ -d "$d" ] && find "$d" -maxdepth 1 -type f -name "$pattern" 2>/dev/null
  done | awk -F/ '{print $NF "\t" $0}' | sort -V | tail -n 1 | cut -f2
}

# find_release_binary KIT_DIR: the newest signed pi-fleet binary for this Pi.
find_release_binary() {
  local kit=$1
  find_newest "pi-fleet_v*_linux_$(pi_arch)" "$kit" "$HOME" "$HOME/Downloads" "$HOME/Desktop" \
    $(ls -d "$HOME"/pi-fleet/dist/v* 2>/dev/null)
}

# offer_rename DEFAULT: offers to rename a Pi still called raspberrypi.
# Renaming needs a restart, after which the launcher is run again.
offer_rename() {
  [ "$(hostname)" = raspberrypi ] || return 0
  echo "This Pi is still called 'raspberrypi'. Each Pi needs its own name so"
  echo "the others can find it on the network."
  if confirm "Rename it now?" y; then
    local name
    ask name "New name (letters, digits and dashes)" "$1"
    sudo raspi-config nonint do_hostname "$name"
    echo
    echo "Renamed to $name. The Pi must restart for this to take effect."
    echo "After it restarts, double-click this file again."
    if confirm "Restart now?" y; then sudo reboot; fi
    exit 0
  fi
}
