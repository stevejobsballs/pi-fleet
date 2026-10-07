#!/usr/bin/env bash
# Double-click launcher, on the development Pi: runs a practice copy of the
# web interface from the code in this repo, with sample records, and opens
# it in Firefox. For trying out changes to the look (internal/web/static/
# style.css) before making a release. Nothing is kept; the real master is
# not touched.
set -euo pipefail
KIT=$(dirname "$(readlink -f "$0")")
. "$KIT/../click-lib.sh"
open_terminal "$(readlink -f "$0")" "$@"
REPO=$(cd "$KIT/../.." && pwd)
export PATH="$HOME/.local/go/bin:/usr/local/go/bin:$PATH"
command -v go >/dev/null || { echo "Go isn't installed on this Pi, so the preview can't run here."; exit 1; }
ADDR=127.0.0.1:9080

echo "=== pi-fleet website preview ==="
echo
echo "Sign in at http://$ADDR as:"
echo "  admin  password tumbleweed-gasket-42   (super user)"
echo "  mona   password brass-kettle-orchard-7 (mid-tier user)"
echo "  tess   password brass-kettle-orchard-7 (user)"
echo
echo "After editing style.css, close this window and double-click this file"
echo "again: the preview picks up changes when it starts. Close this window"
echo "to stop the preview."
echo
( sleep 4; xdg-open "http://$ADDR" >/dev/null 2>&1 || true ) &
cd "$REPO"
PIFLEET_PREVIEW=$ADDR go test ./internal/web -run '^TestPreview$' -count=1 -v -timeout 0 2>&1 | grep -v '^=== RUN' || true
