#!/usr/bin/env bash
# Design 5.5 row 1 / plan A12 timing 1: the order is committed, the
# response not yet sent, the customer gives up and resends.
#   arm order.after_commit -> send -> wait for the report -> the client
#   closes -> release -> resend the same client_ref -> count its orders.
# Expected: IDEMPOTENCY_KEY=0 duplicate orders; =1 the same ticket back.
. "$(dirname "$0")/lib.sh"
exp=01-retry-after-commit
build_tools || exit 1
fail=0
for setting in off on; do
	stop_bg
	d=$(setting_dir $exp $setting)
	overlay=on
	[ $setting = off ] && overlay=off-idempotency
	run=crit01$setting$(date +%H%M%S)
	if deploy $overlay "$d" && hooks_up && follow_logs "$d/logs"; then
		run_crit "$d" 120 retry --base "$(gateway_base)" --run "$run" --hooks "$HOOKS"
		sleep 2
	fi
	if [ $setting = off ]; then
		check "$d" <<'PY'
ok(get(R, "report", "attempt") == R.get("client_ref", "-c1").rsplit("-c1", 1)[0] + "-a1-t1", "the first attempt paused at order.after_commit")
ok(get(R, "first", "full_response") is False, "the client closed before any response")
ends = [l for l in all_lines() if l.get("event") == "request_end" and l.get("attempt", "").endswith("-a1-t1")]
ok(len(ends) == 1 and ends[0].get("tx_outcome") == "committed", "attempt 1 committed (request_end committed): %s" % [e.get("tx_outcome") for e in ends])
ok(get(R, "retry", "full_success") is True, "the resend got a success response")
ok(len(R.get("orders") or []) == 2, "IDEMPOTENCY_KEY=0: duplicate order, 2 orders for the client_ref (got %s)" % R.get("orders"))
PY
	else
		check "$d" <<'PY'
ok(get(R, "report", "attempt") == R.get("client_ref", "-c1").rsplit("-c1", 1)[0] + "-a1-t1", "the first attempt paused at order.after_commit")
ok(get(R, "first", "full_response") is False, "the client closed before any response")
ends = [l for l in all_lines() if l.get("event") == "request_end" and l.get("attempt", "").endswith("-a1-t1")]
ok(len(ends) == 1 and ends[0].get("tx_outcome") == "committed", "attempt 1 committed (request_end committed): %s" % [e.get("tx_outcome") for e in ends])
ids = R.get("orders") or []
ok(len(ids) == 1, "IDEMPOTENCY_KEY=1: exactly one order for the client_ref (got %s)" % ids)
ok(get(R, "retry", "status") == 200 and get(R, "retry", "body", "order_id") == (ids[0] if ids else None),
   "the resend got the same ticket back (200 replay, order_id %s)" % get(R, "retry", "body", "order_id"))
PY
	fi
	verdict $exp $setting "$d" $? || fail=1
done
exit $fail
