#!/usr/bin/env bash
# measure.sh all (plan A8): control, step-0 .. step-4 interleaved, 5 rounds;
# --dry-run prints the planned sequence and touches nothing.
. "$(dirname "$0")/lib.sh"
setup_fakes

"$STAGE/measure.sh" all --dry-run --duration 40 >"$T/out" 2>&1
rc=$?
check "dry run: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
want=$(for r in 1 2 3 4 5; do for m in control step-0 step-1 step-2 step-3 step-4; do echo "$r $m"; done; done)
got=$(sed -n 's/^round \([0-9]\): measure.sh \([a-z0-9-]*\) --runs 1.*/\1 \2/p' "$T/out")
check "dry run: control, step-0..4 repeated 5 times in order" [ "$got" = "$want" ]
[ "$got" = "$want" ] || sed 's/^/    /' "$T/out"
check "dry run: 30 runs" [ "$(grep -c '^round ' "$T/out")" -eq 30 ]
check "dry run: the options are passed on" bash -c 'grep "^round 1: measure.sh control" "$1" | grep -q -- "--duration 40"' _ "$T/out"
check "dry run: nothing touched" bash -c '! grep -Eq "^(kubectl|rush-hour|go|podman|collect-timeline)" "$1"' _ "$FAKE/calls.log"
check "dry run: no evidence written" bash -c '[ -z "$(ls -A "$1")" ]' _ "$MEASURE_EVIDENCE_DIR"

"$STAGE/measure.sh" step-1 --dry-run >"$T/out2" 2>&1
rc=$?
check "--dry-run only with all: exit 64 (rc=$rc)" [ "$rc" -eq 64 ]
finish
