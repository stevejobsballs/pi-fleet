#!/usr/bin/env bash
# Reports the state of a trial Pi: service, version, data checks.
set -uo pipefail

. "$(dirname "$0")/lib.sh"
[ "${1:-}" = "--dry-run" ] && DRY_RUN=1
need_root
if [ -f /srv/pi-fleet/pi-fleet.db ]; then DATA=/srv/pi-fleet; ROLE=master; else DATA=/var/lib/pi-fleet; ROLE=employee; fi
PF=/opt/pi-fleet/current/pi-fleet
echo "role:     $ROLE ($DATA)"
echo "version:  $($PF version 2>/dev/null)"
echo "service:  $(systemctl is-active pi-fleet)"
echo "selfcheck:"
run sudo -u pifleet "$PF" selfcheck -data "$DATA"
if [ "$ROLE" = master ]; then
  echo "Pis:"
  run sudo -u pifleet "$PF" nodes -data "$DATA"
fi
echo "files not owned by pifleet:"
find "$DATA" ! -user pifleet -printf '  %u %p\n' | grep . || echo "  none"
echo "recent log:"
journalctl -u pi-fleet -n 10 --no-pager 2>/dev/null | sed 's/^/  /'
