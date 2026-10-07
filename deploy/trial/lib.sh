# Shared helpers for the pi-fleet trial scripts. Sourced, not run.

DRY_RUN=0

# run prints a command and runs it, or only prints it with --dry-run.
run() {
  printf '+ %s\n' "$*"
  if [ "$DRY_RUN" -eq 0 ]; then "$@"; fi
}

# write_file PATH MODE < content: writes a file (or shows it with --dry-run).
write_file() {
  local path=$1 mode=$2 content
  content=$(cat)
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '+ write %s (mode %s):\n%s\n' "$path" "$mode" "$content" | sed 's/^/    /'
    return
  fi
  printf '%s\n' "$content" > "$path"
  chmod "$mode" "$path"
}

die() { echo "error: $*" >&2; exit 1; }

need_root() {
  if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
    die "run this with sudo"
  fi
}

ensure_user() {
  local home=$1
  if id pifleet >/dev/null 2>&1; then
    echo "  user pifleet exists"
    if [ "$(getent passwd pifleet | cut -d: -f7)" != /usr/sbin/nologin ]; then
      run usermod --shell /usr/sbin/nologin pifleet
    fi
  else
    run useradd --system --home "$home" --shell /usr/sbin/nologin pifleet
  fi
}

# check_hostname warns about the default host name: two Pis both called
# raspberrypi can't tell each other apart on the network.
check_hostname() {
  if [ "$(hostname)" = raspberrypi ]; then
    echo "WARNING: this Pi is still called 'raspberrypi'. Give each Pi its own name first:"
    echo "  sudo raspi-config nonint do_hostname <new-name> && sudo reboot"
    if [ "$DRY_RUN" -eq 0 ]; then
      read -r -p "Continue anyway? [y/N] " ok
      [ "$ok" = y ] || [ "$ok" = Y ] || exit 1
    fi
  fi
}

# install_binary BINARY: installs a release binary under /opt/pi-fleet.
install_binary() {
  local bin=$1 version
  [ -f "$bin" ] || die "release binary not found: $bin"
  version=$("$bin" version | awk '{print $2}')
  case "$version" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) die "$bin reports version '$version'; install a signed release (vX.Y.Z), not a development build" ;;
  esac
  run mkdir -p "/opt/pi-fleet/releases/$version"
  run install -m 0755 "$bin" "/opt/pi-fleet/releases/$version/pi-fleet"
  run ln -sfn "/opt/pi-fleet/releases/$version" /opt/pi-fleet/current
  echo "  installed pi-fleet $version"
}

as_pifleet() { run sudo -u pifleet "$@"; }
