#!/usr/bin/env bash
# Tests signing out in a real browser: starts a practice copy of pi-fleet
# (the preview) and a hidden Firefox, signs in, signs out, presses Back
# and Forward, and leaves a page open after its sign-in ends. Your own
# Firefox is left alone. Prints SIGN OUT OK when everything works.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
TMP=$(mktemp -d)
cleanup() {
  [ -n "${FF:-}" ] && kill "$FF" 2>/dev/null || true
  [ -n "${PREVIEW:-}" ] && pkill -P "$PREVIEW" 2>/dev/null || true
  [ -n "${PREVIEW:-}" ] && kill "$PREVIEW" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT
cd "$REPO"
PIFLEET_PREVIEW=127.0.0.1:9085 go test ./internal/web -run '^TestPreview$' -count=1 -timeout 0 >"$TMP/preview.log" 2>&1 &
PREVIEW=$!
for _ in $(seq 120); do grep -q 'preview at' "$TMP/preview.log" && break; sleep 1; done
mkdir "$TMP/profile"
firefox --headless --marionette -remote-allow-system-access --no-remote -profile "$TMP/profile" >"$TMP/firefox.log" 2>&1 &
FF=$!
sleep 6
python3 -I "$HERE/signouttest.py" http://127.0.0.1:9085 "$HERE"
