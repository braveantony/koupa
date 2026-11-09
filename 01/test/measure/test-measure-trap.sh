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
make_live step-4
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
make_live step-4
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
make_live step-4
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
finish
