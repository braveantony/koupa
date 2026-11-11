#!/usr/bin/env bash
# settings: off on
# Design 5.5 row 3 / plan A12 timing 3: a board has its hello, the App has
# not started the backfill yet, and a new call commits.
#   arm sse.after_hello -> board connects -> wait for the report -> mark one
#   order ready -> release -> the board must get that call exactly once.
# Expected with RESUME_FROM_LAST_ID off and on: no miss, no duplicate (this
# tests the backfill / live hand-over of design 4.2, not step 4).
. "$(dirname "$0")/lib.sh"
crit_init
exp=03-handoff
build_tools || exit 1
fail=0
for setting in off on; do
	stop_bg
	d=$(setting_dir $exp $setting)
	overlay=on
	[ $setting = off ] && overlay=off-resume
	run=crit03$setting$(date +%H%M%S)
	if deploy $overlay "$d" && hooks_up && follow_logs "$d/logs"; then
		run_crit "$d" 120 handoff --base "$(gateway_base)" --run "$run" --hooks "$HOOKS"
	else
		orch_fail "$d" "deploy, hook port-forwards or log followers failed"
	fi
	check "$d" <<'PY'
pod = get(R, "report", "pod_name")
ok(bool(pod) and (get(R, "report", "attempt") or "").endswith("-b1"), "the board paused at sse.after_hello (%s)" % pod)
seq, ready = R.get("ready_seq"), R.get("ready") or []
hello = int(R.get("hello_id") or -1)
ok(bool(seq) and hello >= 0 and hello < seq, "the call (ready_seq %s) committed after the board's start point %s" % (seq, hello))
L = lines(pod) if pod else []
paused = [i for i, l in enumerate(L) if l.get("event") == "hook_paused" and l.get("hook") == "sse.after_hello"]
ends = [l for l in all_lines() if l.get("event") == "request_end" and l.get("ready_seq") == seq]
ok(bool(paused) and bool(ends), "the call was assigned while the board was paused (hook_paused, then ready request_end)")
att = get(R, "report", "attempt")
ok(any(l.get("event") == "hook_released" and l.get("attempt") == att and l.get("hook") == "sse.after_hello" for l in L),
   "the reported board attempt %s was released (hook_released)" % att)
ok(ready.count(seq) == 1, "the board got ready_seq %s exactly once (got %s)" % (seq, ready))
ok(len(ready) == len(set(ready)) and ready == sorted(ready), "no duplicate and increasing: %s" % ready)
PY
	verdict $exp $setting "$d" $? || fail=1
done
exit $fail
