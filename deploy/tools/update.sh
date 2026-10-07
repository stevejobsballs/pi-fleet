#!/usr/bin/env bash
# Installs a signed release on a trial Pi (either role) and checks the
# result, especially file ownership after updating as root. See README.md.
set -euo pipefail

. "$(dirname "$0")/lib.sh"

FROM=""
while [ $# -gt 0 ]; do
  case "$1" in
    --from) FROM=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) echo "usage: sudo ./update.sh [--from RELEASE_DIR] [--dry-run]   (--from is needed on the master)"; exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done
need_root
if is_master; then
  DATA=/srv/pi-fleet
  [ -n "$FROM" ] || FROM=/srv/pi-fleet/releases
else
  DATA=/var/lib/pi-fleet
fi
PF=/opt/pi-fleet/current/pi-fleet
echo "== updating $DATA (currently $($PF version 2>/dev/null || echo unknown))"

run systemctl stop pi-fleet
set +e
if [ -n "$FROM" ]; then
  run "$PF" update -data "$DATA" -root /opt/pi-fleet -from "$FROM"
else
  run "$PF" update -data "$DATA" -root /opt/pi-fleet
fi
STATUS=$?
set -e
run systemctl start pi-fleet

echo "== now running $($PF version 2>/dev/null || echo unknown)"
echo "== files in $DATA not owned by pifleet (there should be none):"
if [ "$DRY_RUN" -eq 0 ]; then
  BAD=$(find "$DATA" ! -user pifleet -printf '  %u %p\n')
  if [ -n "$BAD" ]; then echo "$BAD"; echo "PROBLEM: report these"; else echo "  none"; fi
fi
exit $STATUS
