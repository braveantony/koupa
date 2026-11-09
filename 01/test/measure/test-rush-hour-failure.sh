#!/usr/bin/env bash
# A8: a step that fails makes the run invalid and stops (exit 1), even when
# rush-hour left a scorecard: nonzero rush-hour exit, no SENDING_STARTED,
# collectors that fail to stop.
. "$(dirname "$0")/lib.sh"
setup_fakes
echo 25 >"$FAKE/rh-backdate"

expect_stop() { # WHAT REASON_PATTERN
	local rc=$1 what=$2 pat=$3 run
	run=$(latest_run)
	check "$what: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
	check "$what: only one run" [ "$(grep -c '^rush-hour run' "$FAKE/calls.log")" -eq 1 ]
	check "$what: scorecard line 1 invalid ($pat)" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*$2"' _ "$run/scorecard.txt" "$pat"
	check "$what: scorecard.json invalid" python3 -I -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["invalid"] else 1)' "$run/scorecard.json"
	check "$what: rush-hour scorecard kept for diagnosis" [ -f "$run/scorecard.rush-hour.txt" ]
}

make_live step-0
echo 2 >"$FAKE/rh-rc"
"$STAGE/measure.sh" control --runs 2 --duration 30 >"$T/out" 2>&1
expect_stop $? "rush-hour exit 2" "結束狀態 2"
rm -f "$FAKE/rh-rc"

rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
touch "$FAKE/rh-no-start"
"$STAGE/measure.sh" control --runs 2 --duration 30 >"$T/out" 2>&1
expect_stop $? "no SENDING_STARTED" "SENDING_STARTED"
rm -f "$FAKE/rh-no-start"

rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
touch "$FAKE/collect-stop-fail"
"$STAGE/measure.sh" control --runs 2 --duration 30 >"$T/out" 2>&1
expect_stop $? "collector stop failed" "時間線收集停止失敗"
rm -f "$FAKE/collect-stop-fail"
finish
