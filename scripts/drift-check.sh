#!/usr/bin/env bash
# Checks whether upstream Switchyard behaviour has moved since the pinned revision.
#
# Re-pins to the latest upstream main, re-runs the oracle against the SAME
# recorded golden, and reports the behavioural diff. A diff is the answer to
# "how much did upstream move?" — the number that decides whether owning routing
# outside Rust stays cheap.
#
# Exit codes: 0 up to date or no behavioural change; 1 drift detected (or the
# newer revision could not be evaluated).
#
# Usage: scripts/drift-check.sh [report-file]
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

UPSTREAM="${UPSTREAM:-https://github.com/NVIDIA-NeMo/Switchyard.git}"
REPORT="${1:-drift-report.txt}"
PIN_FILE="testdata/corpus/PIN"

pinned="$(tr -d '[:space:]' < "$PIN_FILE")"
latest="$(git ls-remote "$UPSTREAM" refs/heads/main | cut -f1)"

say() { echo "$*" | tee -a "$REPORT"; }

: > "$REPORT"
say "# Switchyard drift report"
say ""
say "pinned: $pinned"
say "latest: $latest"
say ""

if [ -z "$latest" ]; then
  say "could not resolve upstream main; nothing to compare"
  exit 1
fi

if [ "$pinned" = "$latest" ]; then
  say "up to date — no newer revision to evaluate"
  exit 0
fi

say "re-pinning to $latest and re-running the oracle against the recorded golden"
say ""

python3 - "$pinned" "$latest" <<'PY'
import pathlib, sys
pinned, latest = sys.argv[1], sys.argv[2]
path = pathlib.Path("Cargo.toml")
text = path.read_text()
if pinned not in text:
    sys.exit(f"pinned revision {pinned} not found in Cargo.toml")
path.write_text(text.replace(pinned, latest))
PY
say "Cargo.toml re-pinned."

if ! cargo update -p switchyard-libsy -p switchyard-protocol >/dev/null 2>&1; then
  say ""
  say "cargo could not resolve the newer revision — upstream may have changed shape."
  exit 1
fi
say "dependencies resolved."
say ""
say '```'

cargo run -q -p libsy-conform -- --mode verify 2>&1 | tee -a "$REPORT"
status=${PIPESTATUS[0]}

say '```'
say ""

if [ "$status" -eq 0 ]; then
  say "no behavioural change: the corpus still matches at $latest"
  exit 0
fi

say "BEHAVIOURAL DRIFT DETECTED — the diff above is exactly what moved upstream."
exit 1
