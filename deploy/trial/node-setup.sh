#!/usr/bin/env bash
# Sets up a trial employee Pi (or a kiosk). See README.md.
set -euo pipefail

. "$(dirname "$0")/lib.sh"

usage() {
  cat <<'USAGE'
usage: sudo ./node-setup.sh --binary PATH --master URL --ca CERT (--username NAME | --kiosk NAME) [--dry-run]

  --binary    signed release binary copied from the master, e.g. ~/pi-fleet_v0.1.0_linux_arm64
  --master    the master Pi's URL, e.g. https://fleet-master.local:8443
  --ca        the master's certificate (/etc/pi-fleet/tls/cert.pem on the master), copied here
  --username  your username, from the super user
  --kiosk     instead of --username: set this Pi up as the named shared kiosk
USAGE
}

BIN="" MASTER="" CA="" USERNAME="" KIOSK=""
while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BIN=$2; shift 2 ;;
    --master) MASTER=$2; shift 2 ;;
    --ca) CA=$2; shift 2 ;;
    --username) USERNAME=$2; shift 2 ;;
    --kiosk) KIOSK=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die "unknown option $1" ;;
  esac
done
[ -n "$BIN" ] && [ -n "$MASTER" ] && [ -n "$CA" ] || { usage; die "--binary, --master and --ca are required"; }
[ -n "$USERNAME" ] || [ -n "$KIOSK" ] || { usage; die "give --username or --kiosk"; }
[ -f "$CA" ] || [ "$DRY_RUN" -eq 1 ] || die "certificate not found: $CA"
need_root
check_hostname
DATA=/var/lib/pi-fleet
PF=/opt/pi-fleet/current/pi-fleet

echo "== 1. service user and folders"
ensure_user "$DATA"
run mkdir -p "$DATA" /etc/pi-fleet
install_binary "$BIN"
run install -m 0644 "$CA" /etc/pi-fleet/master.pem
run chown pifleet: "$DATA"
run chmod 0700 "$DATA"

echo "== 2. activation"
if [ -f "$DATA/pi-fleet.db" ]; then
  echo "  $DATA/pi-fleet.db exists; skipping init and activation"
else
  as_pifleet "$PF" init -data "$DATA" -role node
  if [ -n "$KIOSK" ]; then
    as_pifleet "$PF" activate -data "$DATA" -central "$MASTER" -kiosk "$KIOSK" -ca /etc/pi-fleet/master.pem
  else
    as_pifleet "$PF" activate -data "$DATA" -central "$MASTER" -username "$USERNAME" -ca /etc/pi-fleet/master.pem
  fi
fi

echo "== 3. waiting for a super user to confirm this Pi on the master's Pis page"
if [ "$DRY_RUN" -eq 0 ]; then
  until [ "$(sudo -u pifleet "$PF" activation-status -data "$DATA" 2>/dev/null)" = "active" ]; do
    printf '.'; sleep 10
  done
  echo " confirmed"
else
  echo "+ (poll activation-status until active)"
fi
as_pifleet "$PF" sync -data "$DATA"

echo "== 4. systemd service"
write_file /etc/systemd/system/pi-fleet.service 0644 <<UNIT
[Unit]
Description=pi-fleet employee Pi (trial)
After=network-online.target
Wants=network-online.target

[Service]
User=pifleet
ExecStart=$PF run -data $DATA
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

Employee Pi is running.
  Web interface (on this Pi's own screen): http://127.0.0.1:8080
  Logs: journalctl -u pi-fleet -f
  It syncs with the master every 5 minutes.
DONE
