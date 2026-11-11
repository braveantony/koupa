#!/usr/bin/env bash
# settings: off on
# Design 5.5 row 2 / plan A12 timing 2: SIGTERM while a request is being
# handled.
#   arm request.mid -> send -> wait for the report -> watch, then
#   kubectl delete pod <reported Pod> --wait=false, then
#   GRACEFUL_SHUTDOWN=0: no SIGTERM wait, no release; once the container
#     ended or the Pod is gone, the client must not have a full success;
#   GRACEFUL_SHUTDOWN=1: wait for the Pod's SIGTERM line -> 2s -> release
#     -> the client gets its success response.
. "$(dirname "$0")/lib.sh"
crit_init
exp=02-sigterm-mid
build_tools || exit 1
fail=0
for setting in off on; do
	stop_bg
	d=$(setting_dir $exp $setting)
	overlay=on g=1
	[ $setting = off ] && overlay=off-graceful g=0
	run=crit02$setting$(date +%H%M%S)
	if deploy $overlay "$d" && hooks_up && follow_logs "$d/logs"; then
		kubectl -n "$NS" get pods -l "$APP_SELECTOR" -w -o json --output-watch-events >"$d/pods-watch.jsonl" 2>/dev/null &
		BG_PIDS+=($!)
		run_crit "$d" 200 sigterm --base "$(gateway_base)" --run "$run" --hooks "$HOOKS" --graceful $g --logs "$d/logs"
		sleep 2
	else
		orch_fail "$d" "deploy, hook port-forwards or log followers failed"
	fi
	if [ $setting = off ]; then
		check "$d" <<'PY'
pod, att = get(R, "report", "pod_name"), get(R, "report", "attempt")
ok(bool(pod) and bool(att), "a request paused at request.mid (%s on %s)" % (att, pod))
ok(R.get("pod_gone") is True or bool(R.get("finished_at")), "the container ended or the Pod is gone (finishedAt %r; missing is allowed)" % R.get("finished_at"))
ok(get(R, "client", "full_success") is not True, "GRACEFUL_SHUTDOWN=0: the client did not get a full success response (%s)" %
   ({k: get(R, "client", k) for k in ("status", "full_response", "err")}))
L = lines(pod) if pod else []
ok(not any(l.get("event") == "hook_released" for l in L), "no release was sent to the paused request")
ok(not any(l.get("event") == "request_end" and l.get("attempt") == att for l in L), "the request never ended in the App")
PY
	else
		check "$d" <<'PY'
pod, att, uid = get(R, "report", "pod_name"), get(R, "report", "attempt"), get(R, "report", "pod_uid")
ok(bool(pod) and bool(att), "a request paused at request.mid (%s on %s)" % (att, pod))
L = lines(pod) if pod else []
ev = [(l.get("event"), l.get("attempt")) for l in L]
sig = [i for i, l in enumerate(L) if l.get("event") == "sigterm"]
rel = [i for i, l in enumerate(L) if l.get("event") == "hook_released" and l.get("attempt") == att]
end = [l for l in L if l.get("event") == "request_end" and l.get("attempt") == att]
ok(bool(sig) and bool(rel) and sig[0] < rel[0], "the same Pod logged SIGTERM before hook_released for the same attempt")
ok(all(l.get("pod_uid") == uid for l in L if l.get("event") in ("sigterm", "hook_released")), "SIGTERM and release belong to the reported Pod UID")
ok(len(end) == 1 and end[0].get("status") == 201, "the App ended the request with 201 after SIGTERM")
ok(get(R, "client", "full_success") is True and get(R, "client", "status") == 201, "GRACEFUL_SHUTDOWN=1: the client got the success response")
PY
	fi
	verdict $exp $setting "$d" $? || fail=1
done
exit $fail
