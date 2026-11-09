#!/usr/bin/env bash
# A13: a control run with any unknown (or any failure, or invalid) does not
# pass: measure.sh exits 3 and does not change the image.
. "$(dirname "$0")/lib.sh"
setup_fakes
echo 25 >"$FAKE/rh-backdate"

make_live step-0
scorecard 's["has_unknown"] = True; s["items"]["missing"]["board-2"]["unknown"] = 3'
"$STAGE/measure.sh" control --runs 3 --duration 30 >"$T/out" 2>&1
rc=$?
check "unknown: exit 3 (rc=$rc)" [ "$rc" -eq 3 ]
check "explains the control failed" grep -q '對照組不成立' "$T/out"
check "stops after the first failing run" [ "$(grep -c '^rush-hour run' "$FAKE/calls.log")" -eq 1 ]
check "control never sets the image" bash -c '! grep -q "set image" "$1"' _ "$FAKE/calls.log"

: >"$FAKE/calls.log"
scorecard 's["items"]["first_try_failed"]["confirmed"] = 1'
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
check "one failure: exit 3" [ $? -eq 3 ]

scorecard 's["invalid"] = True; s["invalid_reason"] = "x"; s["invalid_reasons"] = ["x"]'
echo 2 >"$FAKE/rh-rc"
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
check "invalid: exit 3" [ $? -eq 3 ]
rm -f "$FAKE/rh-rc"

scorecard_clean
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
check "clean control: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -20 "$T/out" | sed 's/^/    /'
finish
