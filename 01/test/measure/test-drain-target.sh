#!/usr/bin/env bash
# drain target checks (plan A15 drain 1): the target worker must run a koupa
# Pod, no database Pod and no other Pod with an emptyDir (kubectl drain
# without --delete-emptydir-data refuses those); the server dry run of the
# drain must pass. A worker that fails is skipped; none left -> stop.
# Also: after the drain, uncordon and rebalance until every worker runs
# exactly one koupa Pod, giving up after a bounded number of attempts.
. "$(dirname "$0")/lib.sh"
setup_fakes
make_live step-5
DT=$STAGE/scripts/drain-target.sh

# other_pods PY: replace $FAKE/other-pods.json; PY edits list `items`.
other_pods() {
	python3 -I - "$FAKE/other-pods.json" "$1" <<'PY'
import json, sys
path, edit = sys.argv[1:]
items = json.load(open(path))["items"]
def pod(ns, name, node, owner="ReplicaSet", empty=False, labels=None, phase="Running"):
    return {"apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": name, "namespace": ns, "labels": labels or {},
                         "ownerReferences": [{"kind": owner, "name": name + "-o", "controller": True}]},
            "spec": {"nodeName": node, "volumes": [{"name": "d", "emptyDir": {}}] if empty else []},
            "status": {"phase": phase}}
exec(edit)
json.dump({"items": items}, open(path, "w"))
PY
}

# 1. worker-1 has the database Pod: worker-2 is chosen.
out=$("$DT" "$T/t1.json" 2>"$T/t1.err")
rc=$?
check "DB on worker-1: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
check "DB on worker-1: worker-2 chosen (got '$out')" [ "$out" = tkha-worker-2 ]
check "report: worker-1 rejected for the database Pod" python3 -I -c '
import json, sys
r = json.load(open(sys.argv[1]))
w1 = next(c for c in r["candidates"] if c["node"] == "tkha-worker-1")
sys.exit(0 if not w1["ok"] and any("koupa-db-1" in x for x in w1["reasons"]) and r["chosen"] == "tkha-worker-2"
         and r["koupa_pods_affected"] == 1 and any(p["name"].startswith("koupa-abc") for p in
         next(c for c in r["candidates"] if c["node"] == "tkha-worker-2")["pods"]) else 1)' "$T/t1.json"
check "dry run used, no real drain" bash -c 'grep -q "^kubectl drain tkha-worker-2 --ignore-daemonsets --dry-run=server" "$1" && ! grep "^kubectl drain" "$1" | grep -qv dry-run' _ "$FAKE/calls.log"
check "never --delete-emptydir-data" bash -c '! grep -q -- "--delete-emptydir-data" "$1"' _ "$FAKE/calls.log"

# 1b. worker-2 is the infra worker (koupa.dev/infra=true): excluded even
#     though it has no database Pod or emptyDir Pod -> worker-3.
python3 -I -c '
import json, sys
p = sys.argv[1]; n = json.load(open(p))
for x in n["items"]:
    if x["metadata"]["name"] == "tkha-worker-2":
        x["metadata"]["labels"]["koupa.dev/infra"] = "true"
json.dump(n, open(p, "w"))' "$FAKE/nodes.json"
: >"$FAKE/calls.log"
out=$("$DT" "$T/t1b.json" 2>/dev/null)
check "infra label on worker-2: worker-3 chosen (got '$out')" [ "$out" = tkha-worker-3 ]
check "infra label: worker-2 reason says infra worker" python3 -I -c '
import json, sys
r = json.load(open(sys.argv[1]))
w2 = next(c for c in r["candidates"] if c["node"] == "tkha-worker-2")
sys.exit(0 if not w2["ok"] and any("infra worker" in x for x in w2["reasons"]) else 1)' "$T/t1b.json"
check "infra label: no dry run against the infra worker" bash -c '! grep -q "^kubectl drain tkha-worker-2" "$1"' _ "$FAKE/calls.log"
python3 -I -c '
import json, sys
p = sys.argv[1]; n = json.load(open(p))
for x in n["items"]:
    x["metadata"]["labels"].pop("koupa.dev/infra", None)
json.dump(n, open(p, "w"))' "$FAKE/nodes.json"

# 2. worker-2 has a Pod with an emptyDir (metrics-server): worker-3. A
#    DaemonSet's emptyDir (cilium) and a finished Job Pod do not count.
other_pods 'items += [pod("kube-system", "metrics-server-x", "tkha-worker-2", empty=True),
                      pod("koupa", "migrate-x", "tkha-worker-3", owner="Job", empty=True, phase="Succeeded")]'
out=$("$DT" "$T/t2.json" 2>"$T/t2.err")
check "emptyDir on worker-2: worker-3 chosen (got '$out')" [ "$out" = tkha-worker-3 ]
check "worker-2 reason names the emptyDir Pod" grep -q 'metrics-server-x' "$T/t2.json"

# 3. worker-3 fails the server dry run: none left -> exit 1, says why.
echo 'cannot delete Pods not managed by a controller' >"$FAKE/drain-dryrun-fail-tkha-worker-3"
"$DT" "$T/t3.json" >"$T/t3.out" 2>"$T/t3.err"
rc=$?
check "none qualifies: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "none qualifies: nothing on stdout" [ ! -s "$T/t3.out" ]
check "none qualifies: every worker's reason on stderr" bash -c 'grep -q tkha-worker-1 "$1" && grep -q tkha-worker-2 "$1" && grep -q "tkha-worker-3.*not managed" "$1"' _ "$T/t3.err"
check "none qualifies: report chosen null" python3 -I -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["chosen"] is None else 1)' "$T/t3.json"

# 4. measure.sh drain with no worker that qualifies: stops before the
#    change, never drains, run invalid with the reason; exit 1.
: >"$FAKE/calls.log"
"$STAGE/measure.sh" drain --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "measure drain, no target: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "measure drain, no target: no real drain" bash -c '! grep "^kubectl drain" "$1" | grep -qv dry-run' _ "$FAKE/calls.log"
check "measure drain, no target: rush-hour never started" bash -c '! grep -q "^rush-hour run" "$1"' _ "$FAKE/calls.log"
check "measure drain, no target: scorecard says why" bash -c 'head -1 "$1" | grep -q "^本次量測無效：.*drain"' _ "$run/scorecard.txt"
check "measure drain, no target: drain-target.json in the evidence" [ -f "$run/drain-target.json" ]
grep -q '本次量測無效' "$T/out" || tail -20 "$T/out" | sed 's/^/    /'

# 5. A drain whose replacement Pod lands on a worker that already has one:
#    after uncordon the crowded Pod is deleted and the spread verified.
rm -f "$FAKE/drain-dryrun-fail-tkha-worker-3"
make_live step-5
touch "$FAKE/drain-moves"
echo 15 >"$FAKE/rh-backdate"
: >"$FAKE/calls.log"
rm -rf "$MEASURE_EVIDENCE_DIR"/*
"$STAGE/measure.sh" drain --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "rebalance: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -30 "$T/out" | sed 's/^/    /'
check "rebalance: drained worker-2 with --ignore-daemonsets" grep -q '^kubectl drain tkha-worker-2 --ignore-daemonsets --timeout=' "$FAKE/calls.log"
check "rebalance: uncordon after the drain" bash -c 'awk "/^kubectl drain tkha-worker-2 --ignore-daemonsets --timeout/{d=1} d && /^kubectl uncordon tkha-worker-2/{u=1} END{exit !u}" "$1"' _ "$FAKE/calls.log"
check "drain: scorecard notes the completion rule" grep -q 'drain 的「完成」是 kubectl drain' "$run/scorecard.txt"
check "drain: rollout.json completion_rule" grep -q '"completion_rule": "kubectl drain returned' "$run/rollout.json"
check "rebalance: a crowded Pod deleted" grep -q '^kubectl .*delete pod koupa-abc' "$FAKE/calls.log"
check "rebalance: restore.json says balanced" python3 -I -c '
import json, sys
r = json.load(open(sys.argv[1]))
sys.exit(0 if r["ok"] and r["distribution"] == {"tkha-worker-1": 1, "tkha-worker-2": 1, "tkha-worker-3": 1} else 1)' "$run/restore.json"
check "rebalance: worker-1 schedulable again" python3 -I -c '
import json, sys
sys.exit(0 if not any(n["spec"].get("unschedulable") for n in json.load(open(sys.argv[1]))["items"]) else 1)' "$FAKE/nodes.json"

# 6. Rebalancing that never works: gives up after the attempt limit, exit 1.
make_live step-5
touch "$FAKE/drain-moves" "$FAKE/delete-noop"
: >"$FAKE/calls.log"
rm -rf "$MEASURE_EVIDENCE_DIR"/*
MEASURE_REBALANCE_ATTEMPTS=3 "$STAGE/measure.sh" drain --runs 2 --duration 30 >"$T/out" 2>&1
rc=$?
check "rebalance fails: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "rebalance fails: 3 attempts, then stops" [ "$(grep -c '^kubectl .*delete pod' "$FAKE/calls.log")" -eq 3 ]
check "rebalance fails: reported" grep -q '重新平衡.*3 次' "$T/out"
check "rebalance fails: second run never started" [ "$(grep -c '^rush-hour run' "$FAKE/calls.log")" -eq 1 ]
rm -f "$FAKE/drain-moves" "$FAKE/delete-noop"
finish
