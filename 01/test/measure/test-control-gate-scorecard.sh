#!/usr/bin/env bash
# A13 control gate on incomplete or malformed scorecards: every main item
# and all three boards must be present with integer confirmed/unknown, and
# has_unknown/invalid must be explicit booleans. Anything else fails.
. "$(dirname "$0")/lib.sh"
setup_fakes
SC=$STAGE/scripts/lib/scorecard.py
d=$T/run
mkdir -p "$d"

gate() { # EDIT_PY -> exit status of control-ok, output in $T/why
	scorecard "$1"
	sed 's/@RUN@/r1/g' "$FAKE/scorecard.json" >"$d/scorecard.json"
	python3 -I "$SC" "$d" control-ok >"$T/why" 2>&1
}

gate ''
check "clean scorecard passes" [ $? -eq 0 ]
while IFS='|' read -r what edit; do
	gate "$edit"
	rc=$?
	check "$what fails (rc=$rc: $(cat "$T/why"))" [ "$rc" -ne 0 ]
done <<'CASES'
board-3 missing|del s["items"]["missing"]["board-3"]
missing boards empty|s["items"]["missing"] = {}
extra unknown board only|s["items"]["missing"] = {"board-1": {"confirmed": 0, "unknown": 0}}
first_try_failed absent|del s["items"]["first_try_failed"]
failed_after_retry None|s["items"]["failed_after_retry"] = None
duplicate confirmed missing|del s["items"]["duplicate_client_refs"]["confirmed"]
unknown not an int|s["items"]["duplicate_extra_orders"]["unknown"] = "0"
confirmed is bool|s["items"]["first_try_failed"]["confirmed"] = False
items None|s["items"] = None
has_unknown absent|del s["has_unknown"]
invalid absent|del s["invalid"]
orders absent|del s["items"]["orders"]
board unknown|s["items"]["missing"]["board-2"]["unknown"] = 1
CASES

# step-0 "problem observed" and "is the run invalid" take bools/non-ints
# for nothing: a true count is no observation, a missing/odd invalid flag
# is invalid.
check_action() { # WHAT EDIT ACTION WANT_RC
	scorecard "$2"
	sed 's/@RUN@/r1/g' "$FAKE/scorecard.json" >"$d/scorecard.json"
	python3 -I "$SC" "$d" "$3" >"$T/why" 2>&1
	local rc=$?
	check "$1 (rc=$rc, want $4)" [ "$rc" -eq "$4" ]
}
check_action "problem: confirmed true is not an observation" 's["items"]["first_try_failed"]["confirmed"] = True' problem 1
check_action "problem: confirmed 1.0 is not an observation" 's["items"]["missing"]["board-1"]["confirmed"] = 1.0' problem 1
check_action "problem: confirmed 3 is" 's["items"]["first_try_failed"]["confirmed"] = 3' problem 0
check_action "problem: invalid 1 (not false) is not a valid run" 's["items"]["first_try_failed"]["confirmed"] = 3; s["invalid"] = 1' problem 1
check_action "is-invalid: invalid absent counts as invalid" 'del s["invalid"]' is-invalid 0
check_action "is-invalid: invalid 0 (not false) counts as invalid" 's["invalid"] = 0' is-invalid 0
check_action "is-invalid: invalid false is valid" '' is-invalid 1

printf '{"run": ' >"$d/scorecard.json"
python3 -I "$SC" "$d" control-ok >"$T/why" 2>&1
check "truncated json fails" [ $? -ne 0 ]
finish
