#!/usr/bin/env bash
# measure.sh step-1: the rollout to v2 has not finished at D - 10 s ->
# the run is invalid (scorecard says so on line 1, json invalid), exit 2.
. "$(dirname "$0")/lib.sh"
setup_fakes

make_live step-1
touch "$FAKE/rollout-never"
echo 15 >"$FAKE/rh-backdate" # D=30: the change is due now, D-10 in ~5 s
"$STAGE/measure.sh" step-1 --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
check "exit status 2 (rc=$rc)" [ "$rc" -eq 2 ]
run=$(latest_run)
check "run directory exists" [ -n "$run" ]
check "set image to v2 was called" grep -q "set image deploy/koupa koupa=$IMG_V2\$" "$FAKE/calls.log"
check "scorecard line 1 says invalid" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*rollout"' _ "$run/scorecard.txt"
check "scorecard.json invalid" python3 -I -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if s["invalid"] and any("rollout" in r for r in s["invalid_reasons"]) else 1)' "$run/scorecard.json"
check "no main scores printed" bash -c '! grep -q "【主要成績】" "$1"' _ "$run/scorecard.txt"
check "collector stopped" grep -q "collect-timeline stop $run" "$FAKE/calls.log"
check "rollout result recorded" grep -q '"completed": false' "$run/rollout.json"

# Same, but the rollout finishes: valid, exit 0.
rm -f "$FAKE/rollout-never"
make_live step-1
"$STAGE/measure.sh" step-1 --runs 1 --duration 30 >"$T/out2" 2>&1
rc=$?
check "rollout in time: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -20 "$T/out2" | sed 's/^/    /'
run=$(latest_run)
check "valid scorecard keeps its scores" grep -q "【主要成績】" "$run/scorecard.txt"
finish
