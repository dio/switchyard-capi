#!/usr/bin/env bash
# Measures how far this repo has fallen behind upstream Switchyard.
#
# It answers two different questions, and they are not the same question:
#
#   A. Has upstream behaviour moved?   oracle@latest  vs  golden@pinned
#   B. Has OUR port drifted?           port@here      vs  golden@latest
#
# A is the news. B is the actionable number: it says which turns libsygo would
# now get wrong, which is what decides whether owning routing outside Rust stays
# cheap. Reporting only A would tell you upstream moved without telling you
# whether it matters to us; reporting only B would hide movement in families the
# port has not implemented yet.
#
# Section C is the granular view: which recorded turns changed, side by side.
#
# Exit codes: 0 nothing to do; 1 drift detected, or the revision could not be
# evaluated at all.
#
# Usage: scripts/drift-check.sh [report-file]
#
# Environment:
#   UPSTREAM   git URL to compare against (default: the upstream repo)
#   DRIFT_REV  evaluate this exact revision instead of upstream main. Useful for
#              testing the job, or for evaluating a specific PR revision.
#   SKIP_PORT  set to 1 to measure only upstream movement (no Go toolchain).
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

UPSTREAM="${UPSTREAM:-https://github.com/NVIDIA-NeMo/Switchyard.git}"
REPORT="${1:-drift-report.txt}"

# The corpus and golden are canonical in libsygo; the harness only reads them.
LIBSYGO_TESTDATA="${LIBSYGO_TESTDATA:-../libsygo/testdata}"
PIN_FILE="$LIBSYGO_TESTDATA/corpus/PIN"

pinned="$(tr -d '[:space:]' < "$PIN_FILE")"
if [ -n "${DRIFT_REV:-}" ]; then
  latest="$DRIFT_REV"
else
  latest="$(git ls-remote "$UPSTREAM" refs/heads/main | cut -f1)"
fi

say() { echo "$*" | tee -a "$REPORT"; }

: > "$REPORT"

# Re-pinning is a local experiment, not a commit: put the manifests back even when
# we exit early, so a local run leaves no trace.
cleanup() {
  git checkout -- Cargo.toml Cargo.lock 2>/dev/null || true
}
trap cleanup EXIT

say "# Switchyard drift report"
say ""
say "pinned: $pinned"
say "latest: $latest"
say "port:   libsygo $(cd go && go list -m -f '{{.Version}}' github.com/dio/libsygo 2>/dev/null || echo unknown)"
say ""

if [ -z "$latest" ]; then
  say "could not resolve upstream main; nothing to compare"
  exit 1
fi

if [ "$pinned" = "$latest" ]; then
  say "up to date — no newer revision to evaluate"
  exit 0
fi

scratch="$(mktemp -d)"
trap 'cleanup; rm -rf "$scratch"' EXIT

say "re-pinning to $latest"
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

if ! cargo update -p switchyard-libsy -p switchyard-protocol -p switchyard-translation >/dev/null 2>&1; then
  say ""
  say "cargo could not resolve the newer revision — upstream may have changed shape."
  exit 1
fi
say "dependencies resolved."
say ""

# ---------------------------------------------------------------------------
# A. Did upstream behaviour move? The oracle at the new pin, against the golden
# recorded at the old one.
# ---------------------------------------------------------------------------

say "## A. Upstream behaviour vs the recorded golden"
say ""
say '```'

set +e
output="$(cargo run -q -p libsy-conform -- --mode verify 2>&1)"
status=$?
set -e
printf '%s\n' "$output" >> "$REPORT"

say '```'
say ""

# A compile failure is API movement, not behavioural drift: the harness targets
# types and variants that no longer exist (or do not exist yet). Worth reporting
# separately — it means the corpus and port must be updated first, and neither
# question below can be answered until they are.
if printf '%s' "$output" | grep -qE '^error(\[|:)'; then
  say "REVISION CANNOT BE EVALUATED — it does not compile against this harness."
  say "Upstream changed API shape (types or variants added, renamed, or removed),"
  say "not merely behaviour. Update the harness before a behavioural diff is meaningful."
  exit 1
fi

upstream_moved=0
if [ "$status" -ne 0 ]; then
  upstream_moved=1
  say "Upstream moved: the diff above is exactly what changed."
else
  say "Upstream did not move: the corpus still matches at $latest."
fi
say ""

# ---------------------------------------------------------------------------
# B. Has our port drifted? The port against the golden recorded at the NEW pin.
#
# This is the number that decides whether to act. Upstream moving is only news;
# the port disagreeing is the failure.
# ---------------------------------------------------------------------------

if [ "${SKIP_PORT:-0}" = "1" ]; then
  say "## B. Port drift"
  say ""
  say "skipped (SKIP_PORT=1)"
  [ "$upstream_moved" -eq 1 ] && exit 1
  exit 0
fi

say "recording a golden at $latest, to diff the port against the new truth"
say ""

if ! cargo run -q -p libsy-conform -- --mode record --golden "$scratch/golden-latest" >/dev/null 2>&1; then
  say "could not record a golden at $latest; port drift cannot be measured"
  [ "$upstream_moved" -eq 1 ] && exit 1
  exit 0
fi

# The Rust harness shells out to the Go binary, so it must exist and be current.
if ! make build-go >/dev/null 2>&1; then
  say "could not build the Go port; port drift cannot be measured"
  [ "$upstream_moved" -eq 1 ] && exit 1
  exit 0
fi

say "## B. Port drift — libsygo vs upstream at $latest"
say ""
say '```'

set +e
port_output="$(LIBSY_CONFORM_BACKEND=port cargo run -q -p libsy-conform -- --mode impl --tier 1 --golden "$scratch/golden-latest" 2>&1)"
port_status=$?
set -e
printf '%s\n' "$port_output" >> "$REPORT"

say '```'
say ""

port_drifted=0
if [ "$port_status" -ne 0 ]; then
  port_drifted=1
  say "PORT DRIFT DETECTED — the diff above is where libsygo and upstream disagree."
  say "Each line is a turn the port would now route differently. The port is the"
  say "thing to fix; the golden is the truth."
else
  say "The port agrees with upstream at $latest across the tier-1 surface."
fi
say ""

# ---------------------------------------------------------------------------
# C. Which recorded turns moved, and how.
# ---------------------------------------------------------------------------

say "## C. Turn-level movement, golden@pinned vs golden@latest"
say ""
say '```'

python3 - "$scratch/golden-latest" "$LIBSYGO_TESTDATA/golden" <<'PY' | tee -a "$REPORT"
import json, pathlib, sys

def load(directory):
    turns = {}
    for path in sorted(pathlib.Path(directory).glob("*.jsonl")):
        for line in path.read_text().splitlines():
            line = line.strip()
            if not line:
                continue
            record = json.loads(line)
            turns[(record["case_id"], record["turn"])] = record
    return turns

newer, older = load(sys.argv[1]), load(sys.argv[2])
moved = [
    key for key in sorted(older)
    if key in newer and older[key] != newer[key]
]
added = sorted(set(newer) - set(older))
removed = sorted(set(older) - set(newer))

# Only the fields worth naming. A whole-record diff buries the change in hashes
# and digests, which are consequences rather than causes.
FIELDS = ["selected_model_id", "evidence", "steps", "error", "rewritten_model"]

if not (moved or added or removed):
    print("no recorded turn changed")
else:
    for key in moved:
        old, new = older[key], newer[key]
        fields = [f for f in FIELDS if old.get(f) != new.get(f)]
        print(f"{key[0]} (turn {key[1]}): {', '.join(fields) or 'unchanged fields'}")
        for field in fields:
            print(f"    {field}: {json.dumps(old.get(field))} -> {json.dumps(new.get(field))}")
    for key in added:
        print(f"{key[0]} (turn {key[1]}): added upstream")
    for key in removed:
        print(f"{key[0]} (turn {key[1]}): no longer produced upstream")
PY

say '```'
say ""

if [ "$port_drifted" -eq 1 ]; then
  say "RESULT: the port has drifted. Fix libsygo, or pin the revision deliberately."
  exit 1
fi

if [ "$upstream_moved" -eq 1 ]; then
  say "RESULT: upstream moved and the port still agrees. Consider advancing the pin."
  say "The port needs no change: the corpus is the only thing that moved."
  exit 1
fi

say "RESULT: nothing to do."
exit 0
