#!/usr/bin/env bash
# Second load tool (plan A14 「另一套工具」, design 5.6): k6 against Rush Hour,
# step 0 and step 3, side by side.
#
#   experiments/verify/compare-tools.sh [--runs 5]          measure, then compare
#   experiments/verify/compare-tools.sh compare DIR         only the comparison
#   experiments/verify/compare-tools.sh cleanup-k6-orders RUNID DIR
#                                     delete one k6 run's unfinished orders
#                                     (outcome also into DIR/k6-run.json)
#
# Exit 0 when both tools have 5 valid runs of both steps (a k6 run whose
# process exited non-zero is invalid) and agree on the direction of both
# counts; 1 otherwise: the comparison is incomplete, or investigate (A14).
#
# Each round, in this order: measure.sh step-0 --runs 1 (Rush Hour, the real
# measurement with its evidence pack), k6 step 0, measure.sh step-3 --runs 1,
# k6 step 3. Interleaving keeps both tools on the same cluster state.
#
# A k6 run repeats measure.sh's orchestration for one run (A8 steps 1, 2, 5):
# render the step's overlay with image v1, apply, wait for the rollout and
# the old Pods, scripts/preflight.sh; then k6 (pinned container
# docker.io/grafana/k6:0.55.0, host network) with the same customer: 20/s
# for 60 s, per-attempt timeout 5 s, one immediate retry with the same
# client_ref and body unless a complete 2xx came back (k6-orders.js). At the
# sending start + 10 s the image is set to v2; the rollout must be complete
# at D - 10 s, otherwise the k6 run is invalid. k6 has no boards and no
# kitchen: only the orders are compared.
#
# Because k6 has no kitchen, every order of a k6 run stays 'preparing'.
# After each k6 run its orders (client_ref <run>-c<n>, still 'preparing')
# are deleted with their items and idempotency keys (cleanup-k6-orders;
# DIR/cleanup.txt; a failed cleanup makes that k6 run invalid and the whole
# comparison fail, however many valid runs remain). Order numbers wrap at 9999, so leftovers would put two
# unfinished orders with the same number on the kitchen list. k6's counts
# come from its own result, not the database, so this changes no number.
#
# Compared per run and as median / range: 第一次送出沒有成功回應 and
# 重試後仍沒有成功回應, and the direction from step 0 to step 3 for each tool.
# Output: $VERIFY_EVIDENCE/tools/{rush-hour/,k6/,compare.txt,compare.json}.
# At exit the Deployment is restored to step 5 (v1).
set -uo pipefail
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"
KUBECTL=kubectl
# shellcheck source=../../scripts/lib/kube.sh
. "$stage/scripts/lib/kube.sh"

K6_IMAGE=docker.io/grafana/k6:0.55.0
# shellcheck source=../../images.env
. "$stage/images.env"
IMG_V1=$KOUPA_REPO:$KOUPA_V1_TAG
IMG_V2=$KOUPA_REPO:$KOUPA_V2_TAG
DURATION=60 CHANGE_AT_S=10 MARGIN_S=10
root=$VERIFY_EVIDENCE/tools
K6_NAME=""

verify_cleanup() {
	local rc=$?
	[ -n "$K6_NAME" ] && podman rm -f -t 2 "$K6_NAME" >/dev/null 2>&1
	(exit "$rc")
	crit_cleanup
}
trap verify_cleanup EXIT

# now_ms: Unix milliseconds. Not `date +%s%3N`: uutils date ignores the
# width and prints nanoseconds.
now_ms() { echo $(($(date +%s%N) / 1000000)); }

overlay_of() {
	case $1 in
	step-0) echo "$stage/k8s/base" ;;
	step-[1-5]) echo "$stage/k8s/steps/$1" ;;
	*) return 1 ;;
	esac
}

# k6_run STEP N: one k6 measurement; DIR/k6-run.json says valid or why not.
k6_run() {
	local step=$1 n=$2 dir runid base t0="" invalid="" i
	runid=k6-$step-$(date +%Y%m%d-%H%M%S)-r$n
	dir=$root/k6/$runid
	mkdir -p "$dir"
	log "k6 run $runid"
	kubectl kustomize "$(overlay_of "$step")" | sed -E "s#$KOUPA_REPO:v[0-9]+(\.[0-9]+){0,2}#$IMG_V1#" >"$dir/applied.yaml"
	kubectl apply -f "$dir/applied.yaml" >"$dir/apply.log" 2>&1 || invalid="kubectl apply failed"
	if [ -z "$invalid" ]; then
		timeout 320 kubectl -n "$NS" rollout status deploy/koupa --timeout=300s >>"$dir/apply.log" 2>&1 || invalid="rollout to v1 not complete"
	fi
	local deadline=$(($(date +%s) + 180))
	until [ -n "$invalid" ] || old_pods_gone; do
		[ "$(date +%s)" -lt "$deadline" ] || invalid="old Pods not gone in 180 s"
		sleep 1
	done
	deadline=$(($(date +%s) + 60))
	until [ -n "$invalid" ] || app_started || [ "$(date +%s)" -ge "$deadline" ]; do sleep 1; done
	if [ -z "$invalid" ] && ! "$stage/scripts/preflight.sh" "$step" --image "$IMG_V1" >"$dir/preflight.txt" 2>&1; then
		invalid="preflight: $(grep '^FAIL' "$dir/preflight.txt" | paste -sd';' -)"
	fi
	base=$(gateway_base) || invalid="${invalid:+$invalid; }no Gateway address"
	if [ -n "$invalid" ]; then
		log "k6 run $runid not started: $invalid"
		printf '{"run": "%s", "step": "%s", "valid": false, "invalid_reason": %s}\n' "$runid" "$step" "$(python3 -I -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$invalid")" >"$dir/k6-run.json"
		return 1
	fi
	K6_NAME=verify-k6-$$-$n
	podman run --rm --name "$K6_NAME" --network host --userns=keep-id --user "$(id -u):$(id -g)" \
		-v "$verify_dir/k6-orders.js:/scripts/k6-orders.js:ro,z" -v "$dir:/out:z" \
		-e BASE_URL="$base" -e RUN_ID="$runid" -e RATE=20 -e DURATION="${DURATION}s" -e TIMEOUT=5s -e RETRIES=1 -e OUT_DIR=/out \
		"$K6_IMAGE" run --quiet --no-color /scripts/k6-orders.js >"$dir/k6.stdout" 2>"$dir/k6.stderr" &
	local kpid=$!
	for i in $(seq 1 600); do
		t0=$(sed -n 's/.*msg="SENDING_STARTED \([0-9]*\)".*/\1/p' "$dir/k6.stderr" | head -1)
		[ -n "$t0" ] && break
		kill -0 "$kpid" 2>/dev/null || break
		sleep 0.1
	done
	local change_at="" done_at="" status=""
	if [ -z "$t0" ]; then
		invalid="k6 did not start sending"
	else
		local at_ms=$((t0 + CHANGE_AT_S * 1000)) dl_ms=$((t0 + (DURATION - MARGIN_S) * 1000)) d
		d=$((at_ms - $(now_ms)))
		[ "$d" -gt 0 ] && sleep "$(awk -v d="$d" 'BEGIN{printf "%.3f", d/1000}')"
		change_at=$(now_ms)
		log "second $CHANGE_AT_S: set image $IMG_V2"
		if kubectl -n "$NS" set image deploy/koupa "koupa=$IMG_V2" >>"$dir/apply.log" 2>&1; then
			while [ "$(now_ms)" -lt "$dl_ms" ]; do
				status=$(deploy_status)
				if [ "$status" = done ]; then
					done_at=$(now_ms)
					break
				fi
				sleep 0.5
			done
			[ -n "$done_at" ] || invalid="rollout not complete at D - $MARGIN_S s ($status)"
		else
			invalid="set image failed"
		fi
	fi
	timeout 400 tail --pid="$kpid" -f /dev/null
	wait "$kpid"
	local krc=$?
	K6_NAME=""
	grep '^ATTEMPT\|msg="ATTEMPT ' "$dir/k6.stderr" | sed -n 's/.*msg="ATTEMPT \(.*\)" source=console.*/\1/p' | sed 's/\\"/"/g' >"$dir/attempts.jsonl"
	[ -f "$dir/result.json" ] || invalid="${invalid:+$invalid; }k6 wrote no result"
	# A k6 process that did not end successfully (thresholds, script
	# errors, interruption) invalidates the run whatever it wrote.
	[ "$krc" = 0 ] || invalid="${invalid:+$invalid; }k6 exited $krc"
	python3 -I - "$dir/k6-run.json" "$runid" "$step" "$t0" "$change_at" "$done_at" "$invalid" "$krc" "$dir/result.json" "$(podman image inspect --format '{{.Id}}' "$K6_IMAGE" 2>/dev/null)" <<'PY'
import json, os, sys
out, run, step, t0, ch, done, invalid, krc, res, iid = sys.argv[1:]
r = json.load(open(res)) if os.path.exists(res) else {}
if r.get("dropped_iterations"):
    invalid = (invalid + "; " if invalid else "") + "k6 dropped %d iterations (not enough VUs)" % r["dropped_iterations"]
json.dump({"run": run, "step": step, "tool": "k6", "image_id": iid, "valid": not invalid, "invalid_reason": invalid or None,
           "k6_exit": int(krc), "sending_started_ms": int(t0) if t0 else None,
           "change_offset_s": (int(ch) - int(t0)) / 1000 if t0 and ch else None,
           "rollout_completed_offset_s": (int(done) - int(t0)) / 1000 if t0 and done else None,
           "result": r}, open(out, "w"), indent=2, ensure_ascii=False)
PY
	log "k6 run $runid: $(cat "$dir/k6.stdout" 2>/dev/null | tail -1) ${invalid:+(invalid: $invalid)}"
	# A run whose orders could not be deleted is not valid: the comparison
	# must not succeed while it leaves unfinished orders behind.
	cleanup_and_record "$runid" "$dir"
}

# cleanup_and_record RUNID DIR: cleanup_k6_orders, then the outcome into
# DIR/k6-run.json (when it exists). Returns the cleanup's status.
cleanup_and_record() {
	local cleanup=ok rc=0
	cleanup_k6_orders "$1" "$2" || { rc=1; cleanup="failed: $(tail -1 "$2/cleanup.txt" 2>/dev/null)"; }
	[ ! -f "$2/k6-run.json" ] || set_cleanup_status "$2/k6-run.json" "$cleanup" || rc=1
	return $rc
}

# set_cleanup_status FILE STATUS: record the order cleanup in k6-run.json
# ("ok" or "failed: ..."); compare treats anything but "ok" as invalid.
set_cleanup_status() {
	python3 -I - "$1" "$2" <<'PY'
import json, sys
f, st = sys.argv[1:]
k = json.load(open(f))
k["cleanup"] = st
json.dump(k, open(f, "w"), indent=2, ensure_ascii=False)
PY
}

# cleanup_k6_orders RUNID DIR: delete the run's orders that are still
# 'preparing' (client_ref RUNID-c<n>), their items and idempotency keys, in
# one transaction on the CNPG primary; the count goes to DIR/cleanup.txt.
cleanup_k6_orders() {
	local runid=$1 dir=$2 pod
	[[ $runid =~ ^k6-[A-Za-z0-9-]+$ ]] || { log "cleanup: not a k6 run id: $runid"; return 1; }
	pod=$(kubectl -n "$NS" get pod -l cnpg.io/cluster=koupa-db,cnpg.io/instanceRole=primary \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
	[ -n "$pod" ] || { log "cleanup: no database primary"; echo "cleanup failed: no database primary" >"$dir/cleanup.txt"; return 1; }
	if ! kubectl -n "$NS" exec -i "$pod" -c postgres -- psql -X -q -tA -v ON_ERROR_STOP=1 -U postgres -d koupa \
		-v ref="$runid-c%" >"$dir/cleanup.txt" 2>&1 <<'SQL'; then
BEGIN;
CREATE TEMP TABLE k6_gone ON COMMIT DROP AS
  SELECT id FROM orders WHERE client_ref LIKE :'ref' AND status = 'preparing';
DELETE FROM idempotency_keys WHERE order_id IN (SELECT id FROM k6_gone);
DELETE FROM order_items WHERE order_id IN (SELECT id FROM k6_gone);
DELETE FROM orders WHERE id IN (SELECT id FROM k6_gone);
SELECT 'deleted_orders=' || count(*) FROM k6_gone;
COMMIT;
SQL
		log "cleanup of $runid failed: $(tail -1 "$dir/cleanup.txt")"
		return 1
	fi
	log "cleanup $runid: $(cat "$dir/cleanup.txt")"
}

compare() {
	local dir=$1
	python3 -I - "$dir" <<'PY'
import collections, glob, json, os, statistics, sys
root = sys.argv[1]
rows = []
CLEANUP_FAILED = []  # k6 runs whose unfinished orders were not deleted
def numeric(x):
    return isinstance(x, (int, float)) and not isinstance(x, bool)
def item(it, key):
    v = it.get(key)
    return v.get("confirmed") if isinstance(v, dict) else None
def item_unknown(it, key):
    v = it.get(key)
    return v.get("unknown") if isinstance(v, dict) else None
REQUIRED_FIELDS = ("orders", "first", "after")
def require_fields(r):
    """A run is valid only when every required result field is present
    and numeric."""
    bad = [k for k in REQUIRED_FIELDS if not numeric(r.get(k))]
    if bad and r["valid"]:
        r["valid"] = False
        r["invalid_reason"] = "結果欄位缺少或不是數字：%s" % ", ".join(bad)
    return r
for f in sorted(glob.glob(os.path.join(root, "rush-hour", "step-*", "scorecard.json"))):
    s = json.load(open(f))
    d = os.path.dirname(f)
    step = "step-" + os.path.basename(d).split("-")[1]
    it = s.get("items") or {}
    rows.append(require_fields({"tool": "rush-hour", "step": step, "run": os.path.basename(d), "valid": not s.get("invalid"),
                 "invalid_reason": s.get("invalid_reason") or None, "orders": item(it, "orders"),
                 "first": item(it, "first_try_failed"), "first_unknown": item_unknown(it, "first_try_failed"),
                 "after": item(it, "failed_after_retry"), "after_unknown": item_unknown(it, "failed_after_retry"),
                 "mtime": os.path.getmtime(f)}))
for f in sorted(glob.glob(os.path.join(root, "k6", "*", "k6-run.json"))):
    k = json.load(open(f))
    r = k.get("result") or {}
    valid, why = k["valid"], k.get("invalid_reason")
    if k.get("k6_exit") != 0:
        valid, why = False, "; ".join(x for x in (why, "k6 exited %s" % k.get("k6_exit")) if x)
    if not r:
        valid, why = False, "; ".join(x for x in (why, "no k6 result") if x)
    # Evidence from before the cleanup existed has no "cleanup" key.
    if "cleanup" in k and k["cleanup"] != "ok":
        CLEANUP_FAILED.append("%s（%s）" % (k["run"], k["cleanup"]))
        valid, why = False, "; ".join(x for x in (why, "未完成訂單沒有清掉（%s）" % k["cleanup"]) if x)
    rows.append(require_fields({"tool": "k6", "step": k["step"], "run": k["run"], "valid": valid, "invalid_reason": why,
                 "orders": r.get("orders"), "first": r.get("first_try_failed"), "first_unknown": 0,
                 "after": r.get("failed_after_retry"), "after_unknown": 0, "mtime": os.path.getmtime(f),
                 "rollout_completed_offset_s": k.get("rollout_completed_offset_s")}))
def kinds(path, tool):
    """First attempts without a complete 2xx, by what the client saw:
    5xx (the ingress answered, e.g. 503 UF / UC) or no response
    (timeout, reset, refused)."""
    c = collections.Counter()
    try:
        for l in open(path):
            a = json.loads(l)
            if a.get("try") != 1 or (tool == "rush-hour" and a.get("kind") != "order"):
                continue
            st = a.get("status") or 0
            if tool == "rush-hour":
                good = a.get("full_response") and 200 <= st < 300 and not a.get("err_kind")
            else:
                good = 200 <= st < 300 and not a.get("error_code")
            if not good:
                c["5xx" if st >= 500 else "no_response" if not st else "other"] += 1
    except OSError:
        return None
    return dict(c)
for r in rows:
    d = os.path.join(root, "rush-hour" if r["tool"] == "rush-hour" else "k6", r["run"], "attempts.jsonl")
    r["first_kinds"] = kinds(d, r["tool"])
rows.sort(key=lambda r: r["mtime"])
out = []
P = lambda *a: out.append(" ".join(str(x) for x in a))
P("k6 對 Rush Hour：送出沒有成功回應（A14 另一套工具）")
P("每張 20/s × 60 秒、逾時 5 秒、失敗立刻重試 1 次（同 client_ref、同內容）；第 10 秒 set image v2")
P("")
P("%-10s %-7s %-42s %6s %8s %8s  %-18s %s" % ("工具", "步驟", "量測", "訂單", "第一次", "重試後", "第一次：5xx／沒回應", "備註"))
for r in rows:
    note = "" if r["valid"] else "無效：%s" % r["invalid_reason"]
    unk = ""
    if r["first_unknown"] or r["after_unknown"]:
        unk = "（未知 %s／%s）" % (r["first_unknown"], r["after_unknown"])
    k = r["first_kinds"] or {}
    P("%-10s %-7s %-42s %6s %8s %8s  %-18s %s%s" % (r["tool"], r["step"], r["run"], r["orders"], r["first"], r["after"],
      "%d／%d" % (k.get("5xx", 0), k.get("no_response", 0)), unk, note))
P("")
summary = {}
for tool in ("rush-hour", "k6"):
    for step in ("step-0", "step-3"):
        v = [r for r in rows if r["tool"] == tool and r["step"] == step and r["valid"]]
        s = {"n": len(v)}
        for key in ("first", "after"):
            xs = [r[key] for r in v]  # every valid run has numeric fields
            s[key] = {"median": statistics.median(xs) if xs else None, "min": min(xs) if xs else None, "max": max(xs) if xs else None, "values": xs}
        summary[(tool, step)] = s
P("%-10s %-7s %3s  %-26s %-26s" % ("工具", "步驟", "次數", "第一次送出沒有成功回應", "重試後仍沒有成功回應"))
fmt = lambda x: "中位數 %s，範圍 %s～%s" % (x["median"], x["min"], x["max"]) if x["median"] is not None else "沒有有效資料"
REQUIRED = 5  # plan A14: five valid runs per tool and step
BAD = []
for (tool, step), s in summary.items():
    if s["n"] < REQUIRED:
        BAD.append("%s %s：有效 %d 次，不足 %d 次" % (tool, step, s["n"], REQUIRED))
    P("%-10s %-7s %3d  %-26s %-26s" % (tool, step, s["n"], fmt(s["first"]), fmt(s["after"])))
P("")
P("第一次送出沒有成功回應，依客人端看到的結果（中位數，範圍）：")
for tool in ("rush-hour", "k6"):
    for step in ("step-0", "step-3"):
        v = [r["first_kinds"] or {} for r in rows if r["tool"] == tool and r["step"] == step and r["valid"]]
        parts = []
        for key, label in (("5xx", "入口回 5xx"), ("no_response", "沒有回應（逾時／斷線）")):
            xs = [k.get(key, 0) for k in v]
            if xs:
                parts.append("%s 中位數 %s（%s～%s）" % (label, statistics.median(xs), min(xs), max(xs)))
        P("  %-10s %-7s %s" % (tool, step, "；".join(parts)))
P("")
def direction(a, b):
    if a is None or b is None:
        return "無法判斷"
    return "減少" if b < a else "增加" if b > a else "持平"
dirs = {}
for key, label in (("first", "第一次送出沒有成功回應"), ("after", "重試後仍沒有成功回應")):
    d = {t: direction(summary[(t, "step-0")][key]["median"], summary[(t, "step-3")][key]["median"]) for t in ("rush-hour", "k6")}
    dirs[key] = d
    if "無法判斷" in d.values():
        verdict = "無法比較（缺有效資料）"
        BAD.append(label)
    elif d["rush-hour"] == d["k6"]:
        verdict = "方向一致"
    else:
        verdict = "方向不一致，要調查"
        BAD.append(label)
    P("%s：步驟 0 → 步驟 3，Rush Hour %s、k6 %s → %s" % (label, d["rush-hour"], d["k6"], verdict))
    for step in ("step-0", "step-3"):
        a, b = summary[("rush-hour", step)][key], summary[("k6", step)][key]
        if a["median"] is None or b["median"] is None:
            continue
        overlap = not (a["max"] < b["min"] or b["max"] < a["min"])
        P("  %s：Rush Hour %s～%s，k6 %s～%s（範圍%s）" % (step, a["min"], a["max"], b["min"], b["max"], "重疊" if overlap else "不重疊"))
P("")
P("比較不完整：" + "；".join(b for b in BAD if "次，不足" in b) if any("次，不足" in b for b in BAD) else "每個工具、每個步驟都有 %d 次有效量測" % REQUIRED)
# Any failed cleanup fails the whole comparison, however many valid runs
# remain: unfinished orders were left in the database.
if CLEANUP_FAILED:
    BAD.append("cleanup")
    P("比較失敗：未完成訂單沒有清掉：" + "；".join(CLEANUP_FAILED))
text = "\n".join(out) + "\n"
open(os.path.join(root, "compare.txt"), "w").write(text)
json.dump({"rows": rows, "summary": {"%s %s" % k: v for k, v in summary.items()}, "direction": dirs},
          open(os.path.join(root, "compare.json"), "w"), indent=2, ensure_ascii=False)
print(text, end="")
sys.exit(1 if BAD else 0)
PY
}

if [ "${1:-}" = cleanup-k6-orders ]; then
	[ -n "${3:-}" ] || { echo "usage: $0 cleanup-k6-orders RUNID DIR" >&2; exit 64; }
	CRIT_NO_RESTORE=1
	mkdir -p "$3"
	cleanup_and_record "$2" "$3"
	exit $?
fi
if [ "${1:-}" = compare ]; then
	[ -n "${2:-}" ] || { echo "usage: $0 compare DIR" >&2; exit 64; }
	CRIT_NO_RESTORE=1
	root=$2
	compare "$root"
	exit $?
fi
runs=5
[ "${1:-}" = --runs ] && { runs=$2; shift 2; }
[[ $runs =~ ^[1-9][0-9]*$ ]] || { echo "--runs must be a positive integer" >&2; exit 64; }
mkdir -p "$root/rush-hour" "$root/k6"
echo "runs=$runs started=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$root/plan.txt"
for n in $(seq 1 "$runs"); do
	for step in step-0 step-3; do
		log "round $n/$runs: measure.sh $step --runs 1 (Rush Hour)"
		MEASURE_EVIDENCE_DIR=$root/rush-hour timeout 1200 "$stage/measure.sh" "$step" --runs 1 >"$root/rush-hour/measure-$step-r$n.log" 2>&1
		log "measure.sh $step exited $?"
		k6_run "$step" "$n"
	done
done
compare "$root"
