#!/usr/bin/env bash
# Run every critical-timing experiment (plan A12, design 5.5, A10's pause
# point must-tests) on the live cluster, then restore step 5 (image v1, no
# TEST_HOOKS, no experiment pull secret or test Pods). Evidence:
# evidence/critical-<ts>/<experiment>/<setting>/. Exits non-zero when any
# experiment FAILs or the restore fails. Interrupted (INT/TERM) or not, the
# EXIT trap stops the running experiment (its own trap releases hooks and
# ends its port-forwards and followers) and then restores.
#
#   experiments/critical/run.sh [NN ...]   e.g. run.sh 01 05; default all
#
# Before the first run (and after changing the App or slow-board):
#   experiments/critical/build-images.sh
#
# Test seams (experiments/critical/test): CRIT_EXP_DIR (where NN-*.sh are,
# default this directory) and CRIT_RESTORE (command run instead of the
# step-5 restore).
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
stage=$(cd "$here/../.." && pwd)
export CRIT_EVIDENCE=${CRIT_EVIDENCE:-$stage/evidence/critical-$(date -u +%Y%m%dT%H%M%SZ)}
export CRIT_SUMMARY=$CRIT_EVIDENCE/summary.txt
mkdir -p "$CRIT_EVIDENCE"
: >"$CRIT_SUMMARY"
child=""
exp_dir=${CRIT_EXP_DIR:-$here}

parent_cleanup() {
	local rc=$?
	trap - EXIT
	# Ignore further signals: a second INT/TERM must not cut the cleanup short.
	trap "" INT TERM
	if [ -n "$child" ] && kill -0 "$child" 2>/dev/null; then
		# The experiment runs in its own process group (setsid): TERM all of
		# it (crit, kubectl, the script), let its EXIT trap run.
		kill -TERM -- "-$child" 2>/dev/null
		local i
		for i in $(seq 1 60); do kill -0 "$child" 2>/dev/null || break; sleep 1; done
		kill -KILL -- "-$child" 2>/dev/null
		echo "FAIL interrupted $(basename "${cur:-?}")" >>"$CRIT_SUMMARY"
		rc=1
	fi
	# Restore once (lib.sh's restore_step5: step 5, v1, no TEST_HOOKS, test
	# Pods and the pull secret deleted).
	if [ -n "${CRIT_RESTORE:-}" ]; then
		restore_ok() { $CRIT_RESTORE; }
	else
		restore_ok() { (CRIT_NO_RESTORE=1 && . "$here/lib.sh" && restore_step5); }
	fi
	if ! restore_ok 2>>"$CRIT_EVIDENCE/run.log"; then
		echo "FAIL restore" >>"$CRIT_SUMMARY"
		rc=1
	fi
	echo "--- summary ($CRIT_EVIDENCE)"
	cat "$CRIT_SUMMARY"
	grep -q '^FAIL' "$CRIT_SUMMARY" && rc=1
	exit $rc
}
trap parent_cleanup EXIT
trap 'exit 130' INT TERM

sel=("$@")
[ ${#sel[@]} -gt 0 ] || sel=(01 02 03 04 05 06 07)
for n in "${sel[@]}"; do
	cur=$(ls "$exp_dir/$n"-*.sh 2>/dev/null | head -1)
	[ -n "$cur" ] || { echo "FAIL no-experiment-$n" >>"$CRIT_SUMMARY"; continue; }
	echo "=== $(basename "$cur")" >&2
	CRIT_NO_RESTORE=1 setsid timeout 1800 "$cur" 2>>"$CRIT_EVIDENCE/run.log" &
	child=$!
	wait "$child"
	code=$?
	child=""
	exp=$(basename "$cur" .sh)
	# A non-zero exit (124: timeout) fails the experiment even when it
	# already wrote PASS for some setting; so does any expected setting
	# (its "# settings:" line) without a verdict line.
	[ "$code" = 0 ] || echo "FAIL $exp - (exited $code; see run.log)" >>"$CRIT_SUMMARY"
	settings=$(sed -n 's/^# settings: *//p' "$cur" | head -1)
	[ -n "$settings" ] || echo "FAIL $exp - (no \"# settings:\" line)" >>"$CRIT_SUMMARY"
	for st in $settings; do
		grep -qE "^(PASS|FAIL) $exp $st " "$CRIT_SUMMARY" ||
			echo "FAIL $exp $st (no verdict; see run.log)" >>"$CRIT_SUMMARY"
	done
done
