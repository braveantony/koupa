#!/usr/bin/env bash
# Interruption of drain and scale-down (plan A15: on the end of a run or a
# failure halfway, uncordon / restore 3 replicas): SIGINT after the change
# still uncordons the worker (or scales back to 3), stops the collectors
# and the probe, and marks the run invalid.
. "$(dirname "$0")/lib.sh"
setup_fakes

wait_for() {
	local i
	for i in $(seq 1 $(($3 * 10))); do
		grep -q "$2" "$1" 2>/dev/null && return 0
		sleep 0.1
	done
	return 1
}
start_measure() {
	python3 -I -c '
import os, signal, sys
signal.signal(signal.SIGINT, signal.SIG_DFL)
os.execvp(sys.argv[1], sys.argv[1:])' "$STAGE/measure.sh" "$@" >"$T/out" 2>&1 &
	mpid=$!
}
stop_measure() {
	local i
	kill -"$1" "$mpid"
	for i in $(seq 1 150); do
		kill -0 "$mpid" 2>/dev/null || break
		sleep 0.1
	done
	if kill -0 "$mpid" 2>/dev/null; then
		kill -9 "$mpid"
		rc=timeout
	else
		wait "$mpid"
		rc=$?
	fi
}
# after PATTERN_A PATTERN_B: a calls.log line matching B comes after one matching A.
after() { awk -v a="$1" -v b="$2" '$0 ~ a {s=1} s && $0 ~ b {f=1} END {exit !f}' "$FAKE/calls.log"; }

common_checks() {
	local what=$1 run
	run=$(latest_run)
	check "$what: exit 130 (rc=$rc)" [ "$rc" = 130 ]
	[ "$rc" = 130 ] || tail -20 "$T/out" | sed 's/^/    /'
	check "$what: collectors stopped" grep -q "^collect-timeline stop $run" "$FAKE/calls.log"
	check "$what: run invalid, interrupted" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*中斷"' _ "$run/scorecard.txt"
	check "$what: interrupted.json says what was restored" grep -q '"restored"' "$run/interrupted.json"
	check "$what: rush-hour not running" bash -c '! kill -0 "$(cat "$1")" 2>/dev/null' _ "$FAKE/rh-pid"
}

# 1. SIGINT after the drain: uncordon, collectors stopped.
make_live step-5
echo 60 >"$FAKE/rh-sleep"
echo 15 >"$FAKE/rh-backdate"
start_measure drain --runs 1 --duration 30
wait_for "$FAKE/calls.log" '^kubectl drain tkha-worker-2 --ignore-daemonsets --timeout' 20 || fail "drain: never drained"
sleep 0.5
stop_measure INT
common_checks "drain"
check "drain: uncordon after the drain" after '^kubectl drain tkha-worker-2 --ignore-daemonsets --timeout' '^kubectl uncordon tkha-worker-2'
check "drain: node schedulable again" python3 -I -c '
import json, sys
sys.exit(0 if not any(n["spec"].get("unschedulable") for n in json.load(open(sys.argv[1]))["items"]) else 1)' "$FAKE/nodes.json"

# 2. SIGINT while kubectl drain is still running: uncordon too.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
make_live step-5
echo 30 >"$FAKE/drain-sleep"
start_measure drain --runs 1 --duration 30
wait_for "$FAKE/calls.log" '^kubectl drain tkha-worker-2 --ignore-daemonsets --timeout' 20 || fail "mid-drain: never drained"
sleep 0.5
stop_measure INT
common_checks "mid-drain"
check "mid-drain: uncordon" after '^kubectl drain tkha-worker-2 --ignore-daemonsets --timeout' '^kubectl uncordon tkha-worker-2'
check "mid-drain: no kubectl drain left" bash -c '! pgrep -f "$1/bin/kubectl drain" >/dev/null' _ "$FAKE"
rm -f "$FAKE/drain-sleep"

# 3. SIGINT after scaling to 2: back to 3 replicas.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
make_live step-5
start_measure scale-down --runs 1 --duration 30
wait_for "$FAKE/calls.log" '^kubectl .*scale deploy/koupa --replicas=2' 20 || fail "scale-down: never scaled"
sleep 0.5
stop_measure INT
common_checks "scale-down"
check "scale-down: replicas restored to 3" after 'scale deploy/koupa --replicas=2' 'scale deploy/koupa --replicas=3'
check "scale-down: Deployment spec back to 3" python3 -I -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["spec"]["replicas"] == 3 else 1)' "$FAKE/deploy.json"

# 4. SIGINT during calibrate: the probe is stopped as well.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
make_live calibrate
start_measure calibrate --runs 1 --duration 30
wait_for "$FAKE/calls.log" 'set image' 20 || fail "calibrate: never set the image"
sleep 0.5
stop_measure INT
run=$(latest_run)
check "calibrate: exit 130 (rc=$rc)" [ "$rc" = 130 ]
check "calibrate: probe stopped" bash -c '! kill -0 "$(cat "$1")" 2>/dev/null' _ "$FAKE/probe-pid"
check "calibrate: collectors stopped" grep -q "^collect-timeline stop $run" "$FAKE/calls.log"
check "calibrate: no uncordon or scale (nothing to restore)" bash -c '! grep -Eq "uncordon|scale deploy" "$1"' _ "$FAKE/calls.log"
# 5. Restoration that fails: retried; when it never works the run is
#    invalid, restore.json says so, the invocation exits 1, and the
#    pending restoration is retried once more on exit.
export MEASURE_RESTORE_RETRY_SLEEP_S=0
run_mode() { # MODE: one run, sets rc and run
	rm -rf "$MEASURE_EVIDENCE_DIR"/*
	: >"$FAKE/calls.log"
	"$STAGE/measure.sh" "$1" --runs 1 --duration 30 >"$T/out" 2>&1
	rc=$?
	run=$(latest_run)
}
echo 1 >"$FAKE/rh-sleep"
make_live step-5
echo 2 >"$FAKE/uncordon-fail"
run_mode drain
check "uncordon fails twice: retried, exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
check "uncordon fails twice: 3 uncordon calls" [ "$(grep -c '^kubectl uncordon' "$FAKE/calls.log")" -eq 3 ]
check "uncordon fails twice: restore ok" python3 -I -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["ok"] else 1)' "$run/restore.json"

make_live step-5
echo 99 >"$FAKE/uncordon-fail"
run_mode drain
check "uncordon never works: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "uncordon never works: run invalid, says restoration" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*uncordon"' _ "$run/scorecard.txt"
check "uncordon never works: scorecard.json invalid" python3 -I -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["invalid"] else 1)' "$run/scorecard.json"
check "uncordon never works: restore.json not ok, node still cordoned" python3 -I -c '
import json, sys
r = json.load(open(sys.argv[1]))
sys.exit(0 if not r["ok"] and "tkha-worker-2" in r["unschedulable"] and r["pending"] == ["uncordon tkha-worker-2"] else 1)' "$run/restore.json"
check "uncordon never works: tried again on exit" [ "$(grep -c '^kubectl uncordon' "$FAKE/calls.log")" -ge 4 ]
check "uncordon never works: reported" grep -q 'tkha-worker-2.*uncordon\|uncordon.*tkha-worker-2' "$T/out"
rm -f "$FAKE/uncordon-fail"

make_live step-5
echo 99 >"$FAKE/scale-fail"
run_mode scale-down
check "scale-back never works: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "scale-back never works: run invalid" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*scale"' _ "$run/scorecard.txt"
check "scale-back never works: restore.json not ok, pending scale" python3 -I -c '
import json, sys
r = json.load(open(sys.argv[1]))
sys.exit(0 if not r["ok"] and r["pending"] == ["scale deploy/koupa --replicas=3"] else 1)' "$run/restore.json"
rm -f "$FAKE/scale-fail"

# 6. SIGINT after the drain while uncordon keeps failing: still exits 130
#    and interrupted.json shows the restoration did not succeed.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
make_live step-5
echo 60 >"$FAKE/rh-sleep"
echo 99 >"$FAKE/uncordon-fail"
start_measure drain --runs 1 --duration 30
wait_for "$FAKE/calls.log" '^kubectl drain tkha-worker-2 --ignore-daemonsets --timeout' 20 || fail "trap/uncordon-fail: never drained"
sleep 0.5
stop_measure INT
run=$(latest_run)
check "trap, uncordon fails: exit 130 (rc=$rc)" [ "$rc" = 130 ]
check "trap, uncordon fails: interrupted.json restored.ok false" python3 -I -c '
import json, sys
r = json.load(open(sys.argv[1]))["restored"]
sys.exit(0 if r and not r["ok"] and r["pending"] else 1)' "$run/interrupted.json"
rm -f "$FAKE/uncordon-fail"
finish
