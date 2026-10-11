#!/usr/bin/env bash
# Tests field mode in a real browser: starts a practice copy of pi-fleet
# (the preview), a relay in front of it that can drop connections like a
# lost network, and a hidden Firefox, then takes work, goes offline, saves
# answers, comes back and checks they arrived. Your own Firefox is left
# alone. Prints FIELD MODE OK when everything works.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
TMP=$(mktemp -d)
cleanup() {
  [ -n "${FF:-}" ] && kill "$FF" 2>/dev/null || true
  [ -n "${RELAY:-}" ] && kill "$RELAY" 2>/dev/null || true
  [ -n "${PREVIEW:-}" ] && pkill -P "$PREVIEW" 2>/dev/null || true
  [ -n "${PREVIEW:-}" ] && kill "$PREVIEW" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT
cd "$REPO"
PIFLEET_PREVIEW=127.0.0.1:9083 go test ./internal/web -run '^TestPreview$' -count=1 -timeout 0 >"$TMP/preview.log" 2>&1 &
PREVIEW=$!
for _ in $(seq 120); do grep -q 'preview at' "$TMP/preview.log" && break; sleep 1; done
python3 -I "$HERE/relay.py" 9084 9083 "$TMP/down" >"$TMP/relay.log" 2>&1 &
RELAY=$!
mkdir "$TMP/profile"
firefox --headless --marionette -remote-allow-system-access --no-remote -profile "$TMP/profile" >"$TMP/firefox.log" 2>&1 &
FF=$!
sleep 6
python3 -I "$HERE/fieldtest.py" http://127.0.0.1:9084 "$HERE" "$TMP/down"
