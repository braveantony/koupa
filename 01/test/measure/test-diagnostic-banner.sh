#!/usr/bin/env bash
# A13: step 0 that observes none of the predefined problems -> exit 3; with
# --diagnostic it goes on and every scorecard starts with the banner.
. "$(dirname "$0")/lib.sh"
setup_fakes
echo 15 >"$FAKE/rh-backdate"
banner='診斷模式：對照組不成立，不可用於比較'

make_live step-0
"$STAGE/measure.sh" step-0 --runs 2 --duration 30 >"$T/out" 2>&1
rc=$?
check "no problem observed: exit 3 (rc=$rc)" [ "$rc" -eq 3 ]
check "both runs were measured" [ "$(grep -c '^rush-hour run' "$FAKE/calls.log")" -eq 2 ]
for d in "$MEASURE_EVIDENCE_DIR"/step-0-*/; do
	check "no banner without --diagnostic ($(basename "$d"))" bash -c '! grep -q "$2" "$1"' _ "$d/scorecard.txt" "$banner"
done

rm -rf "$MEASURE_EVIDENCE_DIR"/*
make_live step-0
"$STAGE/measure.sh" step-0 --runs 2 --duration 30 --diagnostic >"$T/out" 2>&1
rc=$?
check "--diagnostic: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
n=0
for d in "$MEASURE_EVIDENCE_DIR"/step-0-*/; do
	n=$((n + 1))
	check "banner on line 1 ($(basename "$d"))" [ "$(head -1 "$d/scorecard.txt")" = "$banner" ]
	check "scorecard.json marked diagnostic" python3 -I -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get("diagnostic") else 1)' "$d/scorecard.json"
done
check "two scorecards" [ "$n" -eq 2 ]
check "summary has the banner" bash -c 'head -1 "$1" | grep -q "$2"' _ "$(ls "$MEASURE_EVIDENCE_DIR"/summary-step-0-*.txt | head -1)" "$banner"

# A problem was observed: the gate holds, no banner even with --diagnostic.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
make_live step-0
scorecard 's["items"]["first_try_failed"]["confirmed"] = 4'
"$STAGE/measure.sh" step-0 --runs 1 --duration 30 --diagnostic >"$T/out" 2>&1
check "problem observed: exit 0" [ $? -eq 0 ]
d=$(latest_run)
check "no banner when the gate holds" bash -c '! grep -q "$2" "$1"' _ "$d/scorecard.txt" "$banner"

# Control failing with --diagnostic: continues, banner, exit 0.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
scorecard 's["has_unknown"] = True'
"$STAGE/measure.sh" control --runs 2 --duration 30 --diagnostic >"$T/out" 2>&1
check "control --diagnostic: exit 0" [ $? -eq 0 ]
check "control --diagnostic runs all" [ "$(grep -c '^rush-hour run' "$FAKE/calls.log")" -eq 2 ]
for d in "$MEASURE_EVIDENCE_DIR"/control-*/; do
	check "control banner ($(basename "$d"))" [ "$(head -1 "$d/scorecard.txt")" = "$banner" ]
done
finish
