#!/usr/bin/env bash
# Fault injection against the measurement tool (plan A14 實際注入, design 5.6).
#
#   experiments/verify/mutation.sh [drop-ready] [duplicate-order]   (default: both)
#   experiments/verify/mutation.sh check SETTING DIR   re-evaluate saved evidence
#
# For each setting: deploy experiments/verify/overlays/<setting> (step 5 +
# TEST_HOOKS + TEST_MUTATION, experiment image), run one measurement and
# compare the App log's injection records with the scorecard:
#
#   TEST_MUTATION counts per App instance (each Pod has its own counter):
#   injected is counted from the App log lines and must be n x the Pods that
#   logged injections; detected (scorecard) must equal injected. Both are
#   printed.
#
#   drop-ready=3       each Pod's broadcaster skips the next 3 call numbers
#                      for its first board connection and logs
#                      mutation_drop_ready (ready_seq). Injected and finally
#                      missed are counted apart: a dropped number the board
#                      got later (after a reconnect) is not missed, so
#                      detected = injected - recovered. Every injected
#                      number not recovered must be a confirmed miss of
#                      exactly one board on that Pod, the board misses
#                      nothing else, and nothing is unknown.
#   duplicate-order=2  each Pod creates one extra order for the next 2
#                      client_refs (bypassing idempotency_keys) and logs
#                      mutation_duplicate_order with the outcome. Detected
#                      (scorecard confirmed) = the committed injections;
#                      commit_unknown ones must show up as unknown.
#
# measure.sh is not used: its preflight refuses TEST_MUTATION (A14: never in
# a real measurement). The run is measure.sh's steps 3, 4 and 7 without the
# change: collect-timeline.sh start, rush-hour run (host binary built from
# the working tree, D = VERIFY_DURATION, default 30 s, 20/s, 3 boards, retry
# 1), collect-timeline.sh stop. These scorecards are 測試注入，不可用於比較.
#
# VERIFY_SKIP_DEPLOY=1 runs the checks against the cluster as it is (no
# TEST_MUTATION): they must FAIL. At exit the Deployment is restored to
# step 5 (v1, no TEST_*, no pull secret).
set -uo pipefail
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"
crit_init # its EXIT trap is replaced by verify_cleanup below

DURATION=${VERIFY_DURATION:-30}
COLLECTING=""
verify_cleanup() {
	local rc=$?
	if [ -n "$COLLECTING" ]; then
		RUSH_HOUR=$RH_BIN timeout 120 "$stage/scripts/collect-timeline.sh" stop "$COLLECTING" >/dev/null 2>&1
	fi
	(exit "$rc")
	crit_cleanup
}
trap verify_cleanup EXIT

# measure DIR RUN: one measurement without a change.
measure() {
	local dir=$1 runid=$2 base rc
	base=$(gateway_base) || { orch_fail "$dir" "Gateway koupa has no address"; return 1; }
	if ! RUSH_HOUR=$RH_BIN timeout 120 "$stage/scripts/collect-timeline.sh" start "$dir" >"$dir/collect.log" 2>&1; then
		orch_fail "$dir" "collect-timeline.sh start failed"
		return 1
	fi
	COLLECTING=$dir
	log "rush-hour run $runid against $base (D=${DURATION}s)"
	timeout $((DURATION + 400)) "$RH_BIN" run --base "$base" --run "$runid" --duration "${DURATION}s" --rate 20 --boards 3 \
		--out "$dir" --ingress-log "$dir/ingress.log" --app-logs "$dir/app" >"$dir/rush-hour.stdout" 2>"$dir/rush-hour.stderr"
	rc=$?
	log "rush-hour exited $rc"
	RUSH_HOUR=$RH_BIN timeout 120 "$stage/scripts/collect-timeline.sh" stop "$dir" >>"$dir/collect.log" 2>&1 ||
		orch_fail "$dir" "collect-timeline.sh stop failed"
	COLLECTING=""
	if [ ! -f "$dir/scorecard.json" ]; then
		orch_fail "$dir" "rush-hour wrote no scorecard (exit $rc)"
		return 1
	fi
	# A14: never comparable.
	{ echo "測試注入（TEST_MUTATION），不可用於比較"; cat "$dir/scorecard.txt"; } >"$dir/.sc" && mv "$dir/.sc" "$dir/scorecard.txt"
	kubectl -n "$NS" get deploy koupa -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
		>"$dir/deployment-env.txt" 2>/dev/null
}

check_drop_ready() {
	vcheck "$1" <<'PY'
inj = sorted({(m["_uid"], int(m["ready_seq"])) for m in app_lines() if m.get("event") == "mutation_drop_ready"})
conns, recv, start = {}, collections.defaultdict(set), {}
for e in boards():
    if e.get("kind") == "hello":
        conns[(e["board"], e["conn"])] = e.get("pod_uid")
        start.setdefault(e["board"], e["start"])
    elif e.get("kind") == "ready":
        recv[e["board"]].add(e["ready_seq"])
pods_with_board = set(conns.values())
per_pod = collections.Counter(u for u, _ in inj)
ok(len(inj) > 0, "the App logged injected drops (mutation_drop_ready): %d" % len(inj))
# TEST_MUTATION counts per App instance: every Pod that logged injections
# logged n of them, so injected == n x those Pods.
N = 3
ok(len(inj) == N * len(per_pod), "injected = n x Pods that logged injections: %d = %d x %d" % (len(inj), N, len(per_pod)))
for u in sorted(per_pod):
    ok(u in pods_with_board, "Pod %s dropped numbers only while a board was connected to it" % u[:8])
C = audit("cursor")
ready = {r["ready_seq"] for r in (audit("ready") or [])}
ok(C is not None and ready, "DB audit (cursor %s, %d call numbers) is in the evidence" % (C, len(ready)))
missing = {b: {s for s in ready if start[b] < s <= C and s not in recv[b]} for b in start}
recovered, final = [], []
for u, s in inj:
    bs = sorted({b for (b, c), uu in conns.items() if uu == u})
    lacking = [b for b in bs if s not in recv[b]]
    if not lacking:
        recovered.append((u, s))
        continue
    ok(len(lacking) == 1, "injected %d on Pod %s is missing on exactly one board: %s" % (s, u[:8], lacking))
    final.append((lacking[0], s))
want = collections.Counter(b for b, _ in final)
items = S.get("items", {}).get("missing", {})
for b in sorted(start):
    mine = {s for bb, s in final if bb == b}
    ok(missing[b] == mine, "%s misses exactly the injected numbers not recovered: missed %s, injected %s" % (b, sorted(missing[b]), sorted(mine)))
    sc = items.get(b, {})
    ok(sc.get("confirmed") == want[b] and sc.get("unknown") == 0,
       "scorecard %s：已確認 %s（want %d）、未知 %s（want 0）" % (b, sc.get("confirmed"), want[b], sc.get("unknown")))
detected = sum(v.get("confirmed", 0) for v in items.values())
ok(detected == len(inj) - len(recovered),
   "detected (scorecard confirmed misses) = injected - recovered: %d = %d - %d" % (detected, len(inj), len(recovered)))
table = {"setting": "drop-ready=3", "injected": len(inj), "injected_expected": N * len(per_pod), "detected": detected,
         "injected_by_pod": dict(per_pod),
         "recovered_after_reconnect": len(recovered), "finally_missed_expected": len(final),
         "scorecard_confirmed": sum(v.get("confirmed", 0) for v in items.values()),
         "scorecard_unknown": sum(v.get("unknown", 0) for v in items.values()),
         "per_board": {b: {"expected": want[b], "scorecard": items.get(b)} for b in sorted(start)},
         "injected_list": inj}
table["pods"] = len(per_pod)
json.dump(table, open(os.path.join(D, "mutation-table.json"), "w"), indent=2)
print("table: injected %(injected)d (= 3 x %(pods)d Pods), detected %(detected)d, recovered %(recovered_after_reconnect)d, finally missed %(finally_missed_expected)d, "
      "scorecard confirmed %(scorecard_confirmed)d unknown %(scorecard_unknown)d" % table)
PY
}

check_duplicate_order() {
	vcheck "$1" <<'PY'
inj = sorted({(m["_uid"], m["client_ref"], m.get("outcome")) for m in app_lines() if m.get("event") == "mutation_duplicate_order"})
per_pod = collections.Counter(u for u, _, _ in inj)
ok(len(inj) > 0, "the App logged injected duplicates (mutation_duplicate_order): %d" % len(inj))
ok(len({r for _, r, _ in inj}) == len(inj), "every injection is a different client_ref")
# TEST_MUTATION counts per App instance: injected == n x Pods that logged.
N = 2
ok(len(inj) == N * len(per_pod), "injected = n x Pods that logged injections: %d = %d x %d" % (len(inj), N, len(per_pod)))
# Committed injections are certain duplicates; commit_unknown ones may be.
refs = {r for _, r, o in inj if o == "committed"}
unk_refs = {r for _, r, o in inj if o != "committed"}
orders = audit("orders") or {}
ok(bool(orders), "DB audit of orders is in the evidence (%d client_refs)" % len(orders))
dup = {r for r, ids in orders.items() if len(ids) > 1}
ok(refs <= dup <= refs | unk_refs, "client_refs with more than one order in the DB = the injected ones (DB %d, injected committed %d, commit_unknown %d)" % (len(dup), len(refs), len(unk_refs)))
ok(all(len(orders.get(r, [])) == 2 for r in refs), "each injected client_ref has exactly 2 orders")
it = S.get("items", {})
dc, de = it.get("duplicate_client_refs", {}), it.get("duplicate_extra_orders", {})
ok(dc.get("confirmed") == len(refs) and dc.get("unknown") == len(unk_refs),
   "detected = injected (committed): scorecard 重複訂單（client_ref 數）已確認 %s = %d、未知 %s = %d（commit_unknown）" % (dc.get("confirmed"), len(refs), dc.get("unknown"), len(unk_refs)))
ok(de.get("confirmed") == len(refs), "scorecard 多出的訂單張數：已確認 %s（want %d）" % (de.get("confirmed"), len(refs)))
table = {"setting": "duplicate-order=2", "injected": len(inj), "injected_expected": N * len(per_pod), "pods": len(per_pod),
         "injected_committed": len(refs), "detected": dc.get("confirmed"), "injected_by_pod": dict(per_pod),
         "outcomes": dict(collections.Counter(o for _, _, o in inj)), "db_duplicate_client_refs": len(dup),
         "scorecard_duplicate_client_refs": dc, "scorecard_extra_orders": de, "injected_list": inj}
json.dump(table, open(os.path.join(D, "mutation-table.json"), "w"), indent=2)
print("table: injected %d (= 2 x %d Pods; committed %d), detected %s, DB duplicates %d, scorecard unknown %s" % (len(inj), len(per_pod), len(refs), dc.get("confirmed"), len(dup), dc.get("unknown")))
PY
}

if [ "${1:-}" = check ]; then
	# Re-evaluate saved evidence: check SETTING DIR.
	[ $# -eq 3 ] || { echo "usage: $0 check drop-ready|duplicate-order DIR" >&2; exit 64; }
	CRIT_NO_RESTORE=1
	rm -f "$3/checks.py"
	case $2 in
	drop-ready) check_drop_ready "$3" ;;
	duplicate-order) check_duplicate_order "$3" ;;
	*) exit 64 ;;
	esac
	exit $?
fi
settings=("$@")
[ "${#settings[@]}" -gt 0 ] || settings=(drop-ready duplicate-order)
build_rush_hour || { log "cannot build rush-hour"; exit 1; }
fail=0
for s in "${settings[@]}"; do
	case $s in drop-ready | duplicate-order) ;; *) log "unknown setting $s"; exit 64 ;; esac
	dir=$(setting_dir mutation "$s")
	if [ "${VERIFY_SKIP_DEPLOY:-0}" = 1 ]; then dir=$(setting_dir mutation-red "$s"); fi
	runid=verify-mut-$s-$(date -u +%H%M%S)
	if verify_deploy "$s" "$dir"; then
		measure "$dir" "$runid"
	else
		orch_fail "$dir" "deploy verify/$s failed"
	fi
	rc=0
	case $s in
	drop-ready) check_drop_ready "$dir" || rc=1 ;;
	duplicate-order) check_duplicate_order "$dir" || rc=1 ;;
	esac
	verdict mutation "$s" "$dir" "$rc" || fail=1
done
exit "$fail"
