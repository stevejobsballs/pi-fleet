# Helpers for pi-fleet's double-click launchers (deploy/tools and
# deploy/maintainer). Sourced, not run.

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
