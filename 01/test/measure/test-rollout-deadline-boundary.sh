#!/usr/bin/env bash
# The rollout counts only when its completion is OBSERVED before D - 10 s:
# a "done" read that returns after the deadline makes the run invalid.
# After the rollout every Pod of the new ReplicaSet must run the v2 digest
# recorded at the start of the run; otherwise the run is invalid.
. "$(dirname "$0")/lib.sh"
setup_fakes

# D = 30, sending started 19.5 s ago: change due now, deadline in ~0.5 s;
# the first read of the Deployment after set image takes 2 s.
make_live step-1
echo 19 >"$FAKE/rh-backdate"
echo 2 >"$FAKE/get-sleep"
"$STAGE/measure.sh" step-1 --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "done seen after D-10: exit 2 (rc=$rc)" [ "$rc" -eq 2 ]
check "rollout.json completed false" grep -q '"completed": false' "$run/rollout.json"
check "scorecard invalid: rollout" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*rollout"' _ "$run/scorecard.txt"
rm -f "$FAKE/get-sleep" "$FAKE/set-done"

# Rollout in time, and the new Pods run the v2 digest: valid.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
make_live step-1
"$STAGE/measure.sh" step-1 --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "in time, v2 digest: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -12 "$T/out" | sed 's/^/    /'
check "rollout.json records the v2 digest check" python3 -I -c '
import json, sys
r = json.load(open(sys.argv[1]))
sys.exit(0 if r.get("v2_digest_check", {}).get("ok") is True else 1)' "$run/rollout.json"
rm -f "$FAKE/set-done"

# The new Pods run another digest under the v2 tag: invalid.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
make_live step-1
touch "$FAKE/v2-stale"
"$STAGE/measure.sh" step-1 --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "v2 digest mismatch: exit 2 (rc=$rc)" [ "$rc" -eq 2 ]
check "scorecard invalid: digest" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*digest"' _ "$run/scorecard.txt"
check "names the Pod" grep -q 'koupa-abc-0' "$run/rollout.json"
rm -f "$FAKE/v2-stale" "$FAKE/set-done"
finish
