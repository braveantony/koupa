#!/usr/bin/env bash
# Plan A10 must-test "the App commits after the client timed out", with a
# real Rush Hour run (ingress, timeline collectors, wind-down, scorecard),
# all mechanisms on.
#
# a) after-timeout: arm order.before_commit with hold=1 (the pause outlives
#    the client) -> Rush Hour's only order pauses -> its first attempt
#    times out (5s) -> release -> the transaction commits as usual:
#    request_end committed, the wind-down does not count it as "not
#    created", the reconciliation finds the order.
# b) past-limit: the second order (sent at 29s of D=30s) pauses with
#    hold=1 and is released only after Rush Hour has finished its
#    wind-down and written the scorecard: the scorecard must show
#    「伺服器端結果未知」, never a definite 0, with the wind-down having
#    waited its full server-side limit (steps 3 and 4) before evaluating.
#    The timeline collector runs over the whole experiment. A Pod that is
#    still running has no final log coverage until the collector stops
#    (A11: meta written at stop), so the live scorecard's reason is "Pod
#    log incomplete"; after the collector stopped (still before release)
#    the same scorecard is recomputed from the stored facts and the final
#    evidence (crit rescore): with complete coverage the attempt must
#    still be unknown, now for the deadline reason "received, no request
#    end, Pod program not known to have exited". Then release: the order
#    commits and a later audit finds it.
#    Test-only: Rush Hour runs with --tx-timeout 5s, so its server-side
#    wait limit is 5s + 10s instead of 70s. The database's real
#    transaction_timeout stays 60s; without the shorter limit the held
#    transaction would be killed by the database before the wind-down
#    gave up. Rush Hour uses tx-timeout only for that limit and for
#    commit_unknown attempts (none here).
. "$(dirname "$0")/lib.sh"
exp=06-late-commit
build_tools || exit 1
COLLECT=$stage/scripts/collect-timeline.sh
fail=0
collector_dir=""
stop_collector() {
	[ -n "$collector_dir" ] || return 0
	RUSH_HOUR=$RH_BIN timeout 300 "$COLLECT" stop "$collector_dir" >>"$collector_dir/../collector.log" 2>&1 || log "collector stop failed"
	collector_dir=""
}
trap 'rc=$?; stop_collector; (exit $rc); crit_cleanup' EXIT

# rh DIR RUN RATE [ARGS]: Rush Hour in the background; sets rh_pid.
rh() {
	local dir=$1 run=$2 rate=$3
	shift 3
	timeout 600 "$RH_BIN" run --base "$(gateway_base)" --run "$run" --duration 30s --rate "$rate" --boards 3 \
		--out "$dir" --ingress-log "$dir/ingress.log" --app-logs "$dir/app" --verbose "$@" \
		>"$dir/rush-hour.stdout" 2>"$dir/rush-hour.stderr" &
	rh_pid=$!
	BG_PIDS+=($rh_pid)
}

wait_pid() { # PID SECONDS
	local deadline=$(($(date +%s) + $2))
	while kill -0 "$1" 2>/dev/null; do
		[ "$(date +%s)" -lt "$deadline" ] || { log "pid $1 still running after ${2}s"; return 1; }
		sleep 0.5
	done
}

for setting in after-timeout past-limit; do
	stop_bg
	d=$(setting_dir $exp $setting)
	run=crit06$([ $setting = after-timeout ] && echo a || echo b)$(date +%H%M%S)
	r=$d/run
	mkdir -p "$r"
	echo "{\"run\":\"$run\"}" >"$d/meta.json"
	run06() {
		deploy on "$d" && hooks_up || return 1
		collector_dir=$r
		RUSH_HOUR=$RH_BIN timeout 120 "$COLLECT" start "$r" >>"$d/collector.log" 2>&1 || return 1
		if [ $setting = after-timeout ]; then
			"$CRIT_BIN" arm order.before_commit --hold --hooks "$HOOKS" 2>>"$d/crit.log" || return 1
			rh "$r" "$run" 0.033
			"$CRIT_BIN" wait order.before_commit --hooks "$HOOKS" --timeout 30s >"$d/report.json" 2>>"$d/crit.log" || return 1
			# Attempt 1's record is written when it ends (client timeout 5s).
			wait_grep "$r/attempts.jsonl" "\"attempt\":\"$run-a1-t1\"" 30 || return 1
			log "attempt 1 timed out at the client; release"
			now_rfc >"$d/released_at"
			"$CRIT_BIN" release order.before_commit --hooks "$HOOKS" 2>>"$d/crit.log" || return 1
			wait_pid "$rh_pid" 400 || return 1
			sleep 3
			stop_collector
		else
			rh "$r" "$run" 0.0345 --tx-timeout 5s
			# Order 1 (t=0) goes through untouched; then arm for order 2 (t=29s).
			wait_grep "$r/attempts.jsonl" "\"attempt\":\"$run-a1-t1\"" 30 || return 1
			"$CRIT_BIN" arm order.before_commit --hold --hooks "$HOOKS" 2>>"$d/crit.log" || return 1
			"$CRIT_BIN" wait order.before_commit --hooks "$HOOKS" --timeout 60s >"$d/report.json" 2>>"$d/crit.log" || return 1
			log "order 2 held; waiting for Rush Hour to finish its wind-down and scorecard"
			wait_pid "$rh_pid" 400 || return 1
			now_rfc >"$d/rush_hour_done_at"
			stop_collector
			"$CRIT_BIN" rescore --dir "$r" >"$d/rescored.json" 2>>"$d/crit.log" || return 1
			now_rfc >"$d/released_at"
			"$CRIT_BIN" release order.before_commit --hooks "$HOOKS" 2>>"$d/crit.log" || return 1
			local pod
			pod=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["pod_name"])' "$d/report.json")
			local deadline=$(($(date +%s) + 30))
			until kubectl -n "$NS" logs "$pod" >"$d/pod-after-release.log" 2>/dev/null &&
				grep -c "\"event\":\"request_end\".*\"attempt\":\"$run-a2-t1\"" "$d/pod-after-release.log" >/dev/null; do
				[ "$(date +%s)" -lt "$deadline" ] || { log "no request_end for $run-a2-t1 after release"; return 1; }
				sleep 0.5
			done
			curl -s -m 10 "$(gateway_base)/api/audit?mode=orders&run=$run" >"$d/audit-after-release.json" || return 1
		fi
		return 0
	}
	run06 || orch_fail "$d" "a 06 $setting step failed or timed out"
	stop_collector
	"$CRIT_BIN" release order.before_commit --hooks "$HOOKS" >/dev/null 2>&1
	export RUN=$run
	if [ $setting = after-timeout ]; then
		check "$d" <<'PY'
run = os.environ["RUN"]
sc = json.load(open(os.path.join(D, "run", "scorecard.json")))
att = {}
for l in open(os.path.join(D, "run", "attempts.jsonl")):
    a = json.loads(l); att[a["attempt"]] = a
a1 = att.get(run + "-a1-t1", {})
ok(a1.get("err_kind") == "timeout" and not a1.get("full_response"), "attempt 1 timed out at the client (%s)" % a1.get("err_kind"))
rel = open(os.path.join(D, "released_at")).read().strip()
ok(T(a1.get("responded_at")) is not None and T(a1["responded_at"]) < T(rel), "release came after the client timeout")
app = []
for f in glob.glob(os.path.join(D, "run", "app", "*.log")):
    for l in open(f):
        i = l.find("{")
        if i >= 0:
            try:
                m = json.loads(l[i:])
            except ValueError:
                continue
            if m.get("attempt") == run + "-a1-t1": app.append(m)
ends = [m for m in app if m.get("event") == "request_end"]
rels = [m for m in app if m.get("event") == "hook_released" and m.get("hook") == "order.before_commit"]
ok(len(rels) == 1 and T(rels[0]["ts"]) > T(a1["responded_at"]), "the held attempt was released (hook_released %s) after the client timeout" % (rels and rels[0]["ts"]))
ok(len(ends) == 1 and ends[0].get("tx_outcome") == "committed", "request_end of attempt 1: committed (%s)" % [e.get("tx_outcome") for e in ends])
ok(bool(ends) and bool(rels) and T(ends[0]["commit_sent_at"]) >= T(rels[0]["ts"]) and T(ends[0]["commit_sent_at"]) > T(a1["responded_at"]),
   "COMMIT was sent after the release and after the client gave up (%s)" % (ends and ends[0]["commit_sent_at"]))
orders = get(sc, "facts", "audit", "orders") or {}
ok(len(orders.get(run + "-c1", [])) == 1, "the reconciliation finds the order (%s)" % orders.get(run + "-c1"))
ok(not any(u["client_ref"] == run + "-c1" for u in get(sc, "diagnostics", "unsettled_attempts") or []), "the wind-down settled it (not unknown)")
ok(get(sc, "items", "duplicate_client_refs") == {"confirmed": 0, "unknown": 0}, "no duplicate, decided: %s" % get(sc, "items", "duplicate_client_refs"))
ok(get(sc, "items", "orders", "confirmed") == 1, "the scorecard counts the order as created (orders %s)" % get(sc, "items", "orders"))
ok(not sc.get("invalid"), "the run is valid (%s)" % sc.get("invalid_reasons"))
PY
	else
		check "$d" <<'PY'
run = os.environ["RUN"]
A2 = run + "-a2-t1"
sc = json.load(open(os.path.join(D, "run", "scorecard.json")))
rs = json.load(open(os.path.join(D, "rescored.json")))
txt = open(os.path.join(D, "run", "scorecard.txt")).read()
rep = json.load(open(os.path.join(D, "report.json")))
rel = T(open(os.path.join(D, "released_at")).read().strip())
ok(rep.get("attempt") == A2, "order 2's first attempt was held at order.before_commit (%s)" % rep.get("attempt"))
# Timing: the wind-down waited its whole server-side limit, twice, and
# evaluated before the release.
limit_s = get(sc, "facts", "tx_timeout_ms") / 1000 + 10
tl = [(T(e["at"]), e["what"]) for e in get(sc, "facts", "timeline") or []]
def gave_up(stepname):
    i = next((k for k, (_, w) in enumerate(tl) if w.startswith(stepname)), None)
    if i is None or i == 0:
        return None
    return (tl[i][0] - tl[i - 1][0]).total_seconds(), tl[i][1]
for stepname in ("第 3 步", "第 4 步"):
    g = gave_up(stepname)
    ok(g is not None and "等待到上限" in g[1] and g[0] >= limit_s - 0.2,
       "%s waited the full server-side limit %.0fs before giving up (%s)" % (stepname, limit_s, g and "%.1fs: %s" % g))
ok(T(get(sc, "facts", "evaluated_at")) < rel, "the scorecard was evaluated (%s) before the release (%s)" % (get(sc, "facts", "evaluated_at"), rel))
live = [u for u in get(sc, "diagnostics", "unsettled_attempts") or [] if u["attempt"] == A2]
ok(len(live) == 1 and live[0]["reason"] in ("received without end, but Pod log incomplete", "received without end; Pod program not known to have exited"),
   "live scorecard: %s server-side unknown, received without request end (%s)" % (A2, live and live[0]["reason"]))
dup = get(sc, "items", "duplicate_client_refs") or {}
ok(dup.get("confirmed") == 0 and dup.get("unknown", 0) >= 1, "duplicates are not a definite 0: %s" % dup)
ok("伺服器端結果未知" in txt, "scorecard.txt says 「伺服器端結果未知」")
ok(sc.get("has_unknown") is True, "has_unknown is set")
# With the collector stopped, coverage of the held Pod is final.
pod = [p for p in get(rs, "diagnostics", "pods") or [] if p["uid"] == rep.get("pod_uid")]
ok(len(pod) == 1 and pod[0]["log_covered"] is True, "after collection: the held Pod's log is complete (%s)" % (pod and pod[0]["coverage"]))
fin = [u for u in get(rs, "diagnostics", "unsettled_attempts") or [] if u["attempt"] == A2]
ok(len(fin) == 1 and fin[0]["reason"] == "received without end; Pod program not known to have exited",
   "with complete logs it is still unknown for the deadline reason (%s)" % (fin and fin[0]["reason"]))
rdup = get(rs, "items", "duplicate_client_refs") or {}
ok(rdup.get("confirmed") == 0 and rdup.get("unknown", 0) >= 1, "recomputed duplicates still not a definite 0: %s" % rdup)
L = []
for l in open(os.path.join(D, "pod-after-release.log")):
    i = l.find("{")
    if i >= 0:
        try:
            m = json.loads(l[i:])
        except ValueError:
            continue
        if m.get("attempt") == A2: L.append(m)
rels = [m for m in L if m.get("event") == "hook_released"]
ends = [m for m in L if m.get("event") == "request_end"]
ok(len(rels) == 1 and T(rels[0]["ts"]) > rel, "the held attempt was released only now (hook_released %s)" % (rels and rels[0]["ts"]))
ok(len(ends) == 1 and ends[0].get("tx_outcome") == "committed" and T(ends[0]["ts"]) > rel,
   "after release the held attempt committed (%s)" % [(e.get("tx_outcome"), e.get("ts")) for e in ends])
try:
    later = json.load(open(os.path.join(D, "audit-after-release.json")))
except Exception:
    later = {}
ok(len(later.get(run + "-c2", [])) == 1, "a later audit finds order 2 (%s)" % later.get(run + "-c2"))
PY
	fi
	verdict $exp $setting "$d" $? || fail=1
done
exit $fail
