#!/usr/bin/env bash
# Run every critical-timing experiment (plan A12, design 5.5, A10's pause
# point must-tests) on the live cluster, then restore step 4 (image v1, no
# TEST_HOOKS). Evidence: evidence/critical-<ts>/<experiment>/<setting>/.
# Exits non-zero when any experiment FAILs (or the restore fails).
#
#   experiments/critical/run.sh [NN ...]   e.g. run.sh 01 05; default all
#
# Before the first run (and after changing the App or slow-board):
#   experiments/critical/build-images.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
stage=$(cd "$here/../.." && pwd)
export CRIT_EVIDENCE=${CRIT_EVIDENCE:-$stage/evidence/critical-$(date -u +%Y%m%dT%H%M%SZ)}
export CRIT_SUMMARY=$CRIT_EVIDENCE/summary.txt
mkdir -p "$CRIT_EVIDENCE"
: >"$CRIT_SUMMARY"
sel=("$@")
[ ${#sel[@]} -gt 0 ] || sel=(01 02 03 04 05 06 07)
rc=0
for n in "${sel[@]}"; do
	s=$(ls "$here/$n"-*.sh 2>/dev/null | head -1)
	[ -n "$s" ] || { echo "no experiment $n" >&2; rc=1; continue; }
	echo "=== $(basename "$s")" >&2
	CRIT_NO_RESTORE=1 timeout 1800 "$s" 2> >(tee -a "$CRIT_EVIDENCE/run.log" >&2) || rc=1
	grep -q "$(basename "$s" .sh)" "$CRIT_SUMMARY" || { echo "FAIL $(basename "$s" .sh) - (no verdict; see run.log)" >>"$CRIT_SUMMARY"; rc=1; }
done
# Restore once at the end (lib.sh's restore_step4).
(CRIT_NO_RESTORE=1; . "$here/lib.sh"; restore_step4) 2> >(tee -a "$CRIT_EVIDENCE/run.log" >&2) || { echo "FAIL restore" >>"$CRIT_SUMMARY"; rc=1; }
echo "--- summary ($CRIT_EVIDENCE)"
cat "$CRIT_SUMMARY"
grep -q '^FAIL' "$CRIT_SUMMARY" && rc=1
exit $rc
