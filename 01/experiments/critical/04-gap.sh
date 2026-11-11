#!/usr/bin/env bash
# settings: off on
# Design 5.5 row 4 / plan A12 timing 4: a call commits while the board is
# disconnected.
#   board connects and gets hello -> board closes -> mark one order ready
#   -> board reconnects with Last-Event-ID -> did it get the call?
# Expected: RESUME_FROM_LAST_ID=0 the call is missed; =1 it is backfilled.
. "$(dirname "$0")/lib.sh"
crit_init
exp=04-gap
build_tools || exit 1
fail=0
for setting in off on; do
	stop_bg
	d=$(setting_dir $exp $setting)
	overlay=on
	[ $setting = off ] && overlay=off-resume
	run=crit04$setting$(date +%H%M%S)
	if deploy $overlay "$d" && follow_logs "$d/logs"; then
		run_crit "$d" 120 gap --base "$(gateway_base)" --run "$run"
	else
		orch_fail "$d" "deploy, hook port-forwards or log followers failed"
	fi
	if [ $setting = off ]; then
		check "$d" <<'PY'
seq, ready = R.get("ready_seq"), R.get("ready") or []
ok(bool(seq) and int(R.get("last_event_id") or 1 << 62) < seq, "the call (ready_seq %s) committed after the board's last event id %s" % (seq, R.get("last_event_id")))
ok(R.get("hello_id") is not None, "the board reconnected and got hello (start %s)" % R.get("hello_id"))
ok(seq not in ready, "RESUME_FROM_LAST_ID=0: the call is missed after reconnecting (got %s)" % ready)
PY
	else
		check "$d" <<'PY'
seq, ready = R.get("ready_seq"), R.get("ready") or []
ok(bool(seq) and int(R.get("last_event_id") or 1 << 62) < seq, "the call (ready_seq %s) committed after the board's last event id %s" % (seq, R.get("last_event_id")))
ok(R.get("hello_id") == R.get("last_event_id"), "the reconnect resumed from Last-Event-ID (hello id %s)" % R.get("hello_id"))
ok(ready.count(seq) == 1, "RESUME_FROM_LAST_ID=1: the call is backfilled exactly once (got %s)" % ready)
PY
	fi
	verdict $exp $setting "$d" $? || fail=1
done
exit $fail
