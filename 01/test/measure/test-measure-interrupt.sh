#!/usr/bin/env bash
# measure.sh interrupted (TERM, INT) in the middle of a run: it stops the
# collectors, removes the rush-hour container, records the cluster state it
# leaves behind, marks the run invalid and keeps its evidence. It reacts
# promptly even while waiting for the change second.
. "$(dirname "$0")/lib.sh"
setup_fakes

# wait_for FILE PATTERN SECONDS
wait_for() {
	local i
	for i in $(seq 1 $(($3 * 10))); do
		grep -q "$2" "$1" 2>/dev/null && return 0
		sleep 0.1
	done
	return 1
}

# start_measure ARGS...: measure.sh in the background with default SIGINT
# handling (a plain `&` job would ignore SIGINT). Sets mpid.
start_measure() {
	python3 -I -c '
import os, signal, sys
signal.signal(signal.SIGINT, signal.SIG_DFL)
os.execvp(sys.argv[1], sys.argv[1:])' "$STAGE/measure.sh" "$@" >"$T/out" 2>&1 &
	mpid=$!
}

# stop_measure SIGNAL: send it, wait at most 10 s. Sets rc, took (s).
stop_measure() {
	local t=$SECONDS i
	kill -"$1" "$mpid"
	for i in $(seq 1 100); do
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
	took=$((SECONDS - t))
}

check_interrupted_run() {
	local run=$1 sig=$2
	check "$sig: run directory exists" [ -n "$run" ] && [ -d "$run" ]
	check "$sig: collectors stopped" grep -q "collect-timeline stop $run" "$FAKE/calls.log"
	check "$sig: rush-hour not running" bash -c '! kill -0 "$(cat "$1")" 2>/dev/null' _ "$FAKE/rh-pid"
	check "$sig: scorecard line 1 says invalid, interrupted" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*中斷"' _ "$run/scorecard.txt"
	check "$sig: scorecard.json invalid" python3 -I -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if s["invalid"] and any("中斷" in r for r in s["invalid_reasons"]) else 1)' "$run/scorecard.json"
	check "$sig: interrupted.json records the cluster state left" python3 -I -c '
import json, sys
s = json.load(open(sys.argv[1]))
sys.exit(0 if s.get("signal") == sys.argv[2] and s["cluster"]["image"] and "rollout" in s["cluster"] else 1)' "$run/interrupted.json" "$sig"
	check "$sig: evidence index written" [ -f "$run/evidence-index.txt" ]
	check "$sig: no collector or rush-hour child left" bash -c '! pgrep -f "$1/bin/rush-hour run" >/dev/null' _ "$FAKE"
}

# 1. TERM while rush-hour sends (after the change): local binary.
make_live step-1
echo 60 >"$FAKE/rh-sleep"
echo 15 >"$FAKE/rh-backdate" # D=30: the change is due at once
start_measure step-1 --runs 1 --duration 30
wait_for "$FAKE/calls.log" 'set image' 15 || fail "TERM: measure.sh never set the image"
sleep 0.5
stop_measure TERM
check "TERM: exits 143 within 10 s (rc=$rc, ${took}s)" [ "$rc" = 143 ]
[ "$rc" = 143 ] || tail -20 "$T/out" | sed 's/^/    /'
run=$(latest_run)
check_interrupted_run "$run" TERM
check "TERM: cluster image recorded as v2" grep -q 'koupa:v2' "$run/interrupted.json"
check "TERM: says what it left" grep -q 'cluster left' "$T/out"

# 2. INT with rush-hour in a podman container: the container is removed.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
unset RUSH_HOUR_BIN
make_live step-0
echo 15 >"$FAKE/rh-backdate"
start_measure control --runs 1 --duration 30
wait_for "$FAKE/calls.log" '^rush-hour run' 15 || fail "INT: rush-hour never started"
sleep 0.5
stop_measure INT
check "INT: exits 130 within 10 s (rc=$rc, ${took}s)" [ "$rc" = 130 ]
[ "$rc" = 130 ] || tail -20 "$T/out" | sed 's/^/    /'
run=$(latest_run)
check_interrupted_run "$run" INT
check "INT: podman rm -f the rush-hour container" grep -Eq "^podman rm -f( -t [0-9]+)? rush-hour-$(basename "$run")$" "$FAKE/calls.log"
check "INT: no summary of an interrupted invocation as if complete" bash -c '! ls "$1"/summary-* >/dev/null 2>&1' _ "$MEASURE_EVIDENCE_DIR"
export RUSH_HOUR_BIN=$FAKE/bin/rush-hour

# 3. TERM while waiting for the change second: prompt, no image change.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
make_live step-1
echo 0 >"$FAKE/rh-backdate" # the change is 10 s away
start_measure step-1 --runs 1 --duration 30
wait_for "$FAKE/calls.log" '^rush-hour run' 15 || fail "wait: rush-hour never started"
sleep 1
stop_measure TERM
check "wait: exits 143 within 3 s (rc=$rc, ${took}s)" [ "$rc" = 143 ] && [ "$took" -le 3 ]
check "wait: image never changed" bash -c '! grep -q "set image" "$1"' _ "$FAKE/calls.log"
run=$(latest_run)
check_interrupted_run "$run" TERM
check "wait: cluster image recorded as v1" grep -q 'koupa:v1' "$run/interrupted.json"

# 4. A leftover rush-hour container from an earlier run: refuse to start.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
unset RUSH_HOUR_BIN
echo rush-hour-step-0-20260101-000000-r1-dead >"$FAKE/podman-ps"
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
check "leftover container: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "leftover container: names it" grep -q 'rush-hour-step-0-20260101-000000-r1-dead' "$T/out"
check "leftover container: nothing applied" bash -c '! grep -q "^kubectl apply" "$1"' _ "$FAKE/calls.log"
rm -f "$FAKE/podman-ps"
export RUSH_HOUR_BIN=$FAKE/bin/rush-hour
finish
