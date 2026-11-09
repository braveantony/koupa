#!/usr/bin/env bash
# Plan A10 must-test: with A12 pause points, make one attempt in each of
# the states 「送到了，App 沒有結束這個請求」 (reached_not_ended) and 「App
# 回報寫入和 flush 成功」 (write_flush_ok), and classify both with
# verdict.State over the timeline collector's App logs (A11 coverage).
# Overlay off-graceful (GRACEFUL_SHUTDOWN=0):
#   write_flush_ok: arm order.after_commit -> a raw client sends straight to
#     Pod Q's IP (no ingress) and never reads -> report -> release
#     -> the App writes and flushes, logs request_end -> the client closes
#     without having read anything.
#   reached_not_ended: arm request.mid -> send straight to Pod P -> report
#     -> kubectl delete pod P --wait=false -> the program logs SIGTERM and
#     exits at once; the request never ends.
# Also A10's companion case: the same evidence with P's SIGTERM line
# deleted (coverage recomputed by `rush-hour coverage`) must be 「未知」.
. "$(dirname "$0")/lib.sh"
exp=07-state-samples
build_tools || exit 1
COLLECT=$stage/scripts/collect-timeline.sh
setting=off-graceful
d=$(setting_dir $exp $setting)
r=$d/run
mkdir -p "$r"
collector_on=0
stop_collector() {
	[ "$collector_on" = 1 ] || return 0
	collector_on=0
	RUSH_HOUR=$RH_BIN timeout 300 "$COLLECT" stop "$r" >>"$d/collector.log" 2>&1 || log "collector stop failed"
}
trap 'rc=$?; stop_collector; (exit $rc); crit_cleanup' EXIT
run07() {
	deploy off-graceful "$d" && hooks_up || return 1
	RUSH_HOUR=$RH_BIN timeout 120 "$COLLECT" start "$r" >>"$d/collector.log" 2>&1 || return 1
	collector_on=1
	follow_logs "$d/logs" || return 1
	local pods p q
	pods=($(koupa_pods))
	q=${pods[0]} p=${pods[1]}
	# Straight to the Pod IPs (routable from this host), no ingress and no
	# port-forward: a port-forward reports the Pod's connection end about
	# a second late, after the log collection of the exited Pod has ended.
	local qip pip
	qip=$(kubectl -n "$NS" get pod "$q" -o jsonpath='{.status.podIP}')
	pip=$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.status.podIP}')
	[ -n "$qip" ] && [ -n "$pip" ] || return 1
	local run=crit07$(date +%H%M%S)
	timeout 90 "$CRIT_BIN" flushok --run "${run}f" --hooks "$HOOKS" --logs "$d/logs" --pod-addr "$q=$qip:8080" \
		>"$d/flushok.json" 2>>"$d/crit.log" || return 1
	timeout 150 "$CRIT_BIN" reached --run "${run}r" --hooks "$HOOKS" --logs "$d/logs" --pod-addr "$p=$pip:8080" \
		>"$d/reached.json" 2>>"$d/crit.log" || return 1
	timeout 90 kubectl -n "$NS" wait --for=delete "pod/$p" --timeout=80s >&2 || return 1
	sleep 3
}
run07 || orch_fail "$d" "a 07 step failed or timed out"
sed 's/^/    | /' "$d/crit.log" >&2
stop_collector
# The two client attempts, as the client saw them.
python3 -I - "$d" <<'PY' >"$d/attempts.jsonl"
import json, os, sys
for f in ("flushok.json", "reached.json"):
    try:
        print(json.dumps(json.load(open(os.path.join(sys.argv[1], f)))["client"]))
    except Exception:
        pass
PY
"$CRIT_BIN" classify --app-logs "$r/app" --attempts "$d/attempts.jsonl" >"$d/classify.json" 2>>"$d/crit.log"
# Companion: delete the SIGTERM line of P, recompute coverage.
rm -rf "$d/run-no-sigterm"
cp -a "$r" "$d/run-no-sigterm"
puid=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["report"]["pod_uid"])' "$d/reached.json" 2>/dev/null)
if [ -n "$puid" ] && [ -f "$d/run-no-sigterm/app/$puid.log" ]; then
	grep -v '"event":"sigterm"' "$r/app/$puid.log" >"$d/run-no-sigterm/app/$puid.log"
	"$RH_BIN" coverage "$d/run-no-sigterm" 2>>"$d/crit.log"
	"$CRIT_BIN" classify --app-logs "$d/run-no-sigterm/app" --attempts "$d/attempts.jsonl" >"$d/classify-no-sigterm.json" 2>>"$d/crit.log"
fi
check "$d" <<'PY'
def load(f):
    try:
        return json.load(open(os.path.join(D, f)))
    except Exception:
        return None
fo, re_ = load("flushok.json") or {}, load("reached.json") or {}
cl = {c["attempt"]: c for c in load("classify.json") or []}
cn = {c["attempt"]: c for c in load("classify-no-sigterm.json") or []}
fa, ra = get(fo, "client", "attempt"), get(re_, "client", "attempt")
ok(get(fo, "report", "pod_name") == fo.get("pod") and get(fo, "client", "full_response") is False,
   "write_flush_ok sample: paused on %s, the client never got a full response" % fo.get("pod"))
ok(get(re_, "report", "pod_name") == re_.get("pod") and get(re_, "client", "full_success") is not True and bool(re_.get("sigterm_at")),
   "reached_not_ended sample: paused on %s, Pod deleted, SIGTERM logged, no success for the client (%s)" % (re_.get("pod"), get(re_, "client", "err")))
FL = lines(fo.get("pod") or "-")
ok(any(l.get("event") == "hook_released" and l.get("attempt") == fa for l in FL), "write_flush_ok attempt %s was released (hook_released)" % fa)
ok(get(cl, fa, "log_covered") is True and get(cl, ra, "log_covered") is True, "the collector covers both attempts (A11): %s / %s" % (get(cl, fa, "pod_coverage"), get(cl, ra, "pod_coverage")))
ok(get(cl, fa, "state") == "write_flush_ok", "verdict.State = write_flush_ok (「App 回報寫入和 flush 成功」): %s" % get(cl, fa, "state"))
ok(get(cl, ra, "state") == "reached_not_ended", "verdict.State = reached_not_ended (「送到了，App 沒有結束這個請求」): %s" % get(cl, ra, "state"))
ok(get(cn, ra, "state") == "unknown", "with P's SIGTERM line deleted: unknown (「未知」): %s" % get(cn, ra, "state"))
PY
verdict $exp $setting "$d" $?
