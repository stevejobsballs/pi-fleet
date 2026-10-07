#!/usr/bin/env bash
# Sets up a trial master Pi. See README.md.
set -euo pipefail

. "$(dirname "$0")/lib.sh"

usage() {
  cat <<'USAGE'
usage: sudo ./master-setup.sh --binary PATH [--name HOST] [--ip ADDR] [--port 8443] [--dry-run]

  --binary   signed release binary, e.g. ~/pi-fleet/dist/v0.1.0/pi-fleet_v0.1.0_linux_arm64
  --name     host name employee Pis use to reach this Pi (default: <hostname>.local)
  --ip       this Pi's address for the certificate (default: first address of hostname -I)
  --port     HTTPS port (default 8443)
USAGE
}

BIN="" NAME="$(hostname).local" IP="$(hostname -I 2>/dev/null | awk '{print $1}')" PORT=8443
while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BIN=$2; shift 2 ;;
    --name) NAME=$2; shift 2 ;;
    --ip) IP=$2; shift 2 ;;
    --port) PORT=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die "unknown option $1" ;;
  esac
done
[ -n "$BIN" ] || { usage; die "--binary is required"; }
[ -n "$IP" ] || die "couldn't work out this Pi's IP address; pass --ip"
need_root
check_hostname
DATA=/srv/pi-fleet

echo "== 1. service user and folders"
ensure_user "$DATA"
run mkdir -p "$DATA/releases" /etc/pi-fleet/tls
install_binary "$BIN"
run chown pifleet: "$DATA" "$DATA/releases"
run chmod 0700 "$DATA"

echo "== 2. HTTPS certificate for $NAME ($IP)"
if [ -f /etc/pi-fleet/tls/cert.pem ]; then
  echo "  /etc/pi-fleet/tls/cert.pem exists; keeping it"
else
  run openssl req -x509 -newkey ed25519 -nodes -days 365 -subj "/CN=$NAME" \
    -addext "subjectAltName=DNS:$NAME,IP:$IP" \
    -keyout /etc/pi-fleet/tls/key.pem -out /etc/pi-fleet/tls/cert.pem
  run chown pifleet: /etc/pi-fleet/tls/key.pem
  run chmod 0600 /etc/pi-fleet/tls/key.pem
fi

echo "== 3. database"
if [ -f "$DATA/pi-fleet.db" ]; then
  echo "  $DATA/pi-fleet.db exists; keeping it"
else
  as_pifleet /opt/pi-fleet/current/pi-fleet init -data "$DATA" -role central
fi
# The marker records that the first super user exists, so running the
# script again (for example after a mistyped answer) carries on from here.
MARK="$DATA/.superuser-created"
if [ -f "$MARK" ]; then
  echo "  the first super user exists; keeping it"
elif [ "$DRY_RUN" -eq 1 ]; then
  as_pifleet /opt/pi-fleet/current/pi-fleet bootstrap -data "$DATA" -username "<username>" -name "<legal name>" -email "<email>"
else
  echo
  echo "Now create the first super user (you)."
  LOG=$(mktemp)
  while :; do
    read -r -p "  username (lowercase, e.g. steve or s.jobs): " SU_USER
    if ! [[ $SU_USER =~ ^[a-z][a-z0-9._-]{2,31}$ ]]; then
      echo "  A username is 3-32 lowercase letters, digits, '.', '_' or '-', starting with a letter. Try again."
      continue
    fi
    read -r -p "  legal name (shown on signatures, capitals fine): " SU_NAME
    read -r -p "  work email: " SU_EMAIL
    if [ -z "$SU_NAME" ] || [ -z "$SU_EMAIL" ]; then
      echo "  The legal name and email are both needed. Try again."
      continue
    fi
    echo "  The password needs at least 12 characters and must not contain the username."
    set +e
    sudo -u pifleet /opt/pi-fleet/current/pi-fleet bootstrap -data "$DATA" -username "$SU_USER" -name "$SU_NAME" -email "$SU_EMAIL" 2>&1 | tee "$LOG"
    STATUS=${PIPESTATUS[0]}
    set -e
    if [ "$STATUS" -eq 0 ] || grep -q "users already exist" "$LOG"; then
      break
    fi
    echo "  That didn't work (see the message above). Let's try again."
  done
  rm -f "$LOG"
  sudo -u pifleet touch "$MARK"
fi

echo "== 4. systemd service"
write_file /etc/systemd/system/pi-fleet.service 0644 <<UNIT
[Unit]
Description=pi-fleet master Pi (trial)
After=network-online.target
Wants=network-online.target

[Service]
User=pifleet
ExecStart=/opt/pi-fleet/current/pi-fleet serve -data $DATA -listen :$PORT -tls-cert /etc/pi-fleet/tls/cert.pem -tls-key /etc/pi-fleet/tls/key.pem -releases $DATA/releases
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=$DATA
Restart=on-failure

[Install]
WantedBy=multi-user.target
UNIT
run systemctl daemon-reload
run systemctl enable --now pi-fleet

cat <<DONE

Master Pi is running.
  Web interface:  https://$NAME:$PORT   (the browser warns about the self-signed certificate; accept it for the trial)
  Logs:           journalctl -u pi-fleet -f
  Certificate for employee Pis: /etc/pi-fleet/tls/cert.pem  (copy it to each employee Pi)

Next: sign in, create a site, a location and a user (Users page gives a one-time password),
then run node-setup.sh on the employee Pi.
DONE
