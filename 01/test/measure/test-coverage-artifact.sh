#!/usr/bin/env bash
# The periodic `rush-hour coverage` runs with the same identified Rush Hour
# artifact as the run (image mode: podman run of the recorded image ID),
# and its failures are surfaced (coverage.err, a warning), not dropped.
. "$(dirname "$0")/lib.sh"
setup_fakes
unset RUSH_HOUR_BIN
echo 25 >"$FAKE/rh-backdate"
echo 5 >"$FAKE/rh-sleep"
make_live step-0
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
check "coverage via podman run of the image ID" grep -Eq "^podman run .* fa4e1d0+1 coverage $run$" "$FAKE/calls.log"
check "coverage container has its own name" grep -Eq "^podman run .*--name rush-hour-$(basename "$run")-cov " "$FAKE/calls.log"

rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
touch "$FAKE/coverage-fail"
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
run=$(latest_run)
check "failure kept in coverage.err" grep -q 'fake coverage: cannot read' "$run/coverage.err"
check "failure surfaced as a warning" grep -q 'rush-hour coverage failed' "$T/out"
rm -f "$FAKE/coverage-fail"
export RUSH_HOUR_BIN=$FAKE/bin/rush-hour
finish
