#!/usr/bin/env bash
# Stale-binary guard: measure.sh builds the host-side rush-hour (timeline
# collector, calibrate analysis) from the working tree before anything
# else, records its sha256 and the git commit in each run's evidence, and
# refuses to run when the build fails.
. "$(dirname "$0")/lib.sh"
setup_fakes
make_live step-0

touch "$FAKE/go-fail"
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
check "build fails: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "build fails: says so" grep -q 'rush-hour.*build' "$T/out"
check "build fails: nothing applied" bash -c '! grep -q "^kubectl apply" "$1"' _ "$FAKE/calls.log"
rm -f "$FAKE/go-fail"

echo 15 >"$FAKE/rh-backdate"
: >"$FAKE/calls.log"
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "build ok: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -20 "$T/out" | sed 's/^/    /'
check "build ok: go build ./rush-hour first" bash -c 'head -1 "$1" | grep -q "^go build -o $2 ./rush-hour"' _ "$FAKE/calls.log" "$MEASURE_HOST_RUSH_HOUR"
check "tooling.json: sha256, git commit and dirty flag" python3 -I -c '
import hashlib, json, subprocess, sys
t = json.load(open(sys.argv[1]))
sha = hashlib.sha256(open(sys.argv[2], "rb").read()).hexdigest()
head = subprocess.run(["git", "-C", sys.argv[3], "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
h = t["host_rush_hour"]
sys.exit(0 if h["sha256"] == sha and h["path"] == sys.argv[2] and t["git"]["head"] == head
         and isinstance(t["git"]["dirty"], bool) else 1)' "$run/tooling.json" "$MEASURE_HOST_RUSH_HOUR" "$STAGE"
check "collectors use the fresh build (RUSH_HOUR)" grep -qx "RUSH_HOUR=$MEASURE_HOST_RUSH_HOUR" "$FAKE/collect-env"
finish
