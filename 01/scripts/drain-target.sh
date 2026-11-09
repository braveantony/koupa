#!/usr/bin/env bash
# Choose the worker measure.sh drain empties (plan A15 drain step 1).
#
#   scripts/drain-target.sh OUT_JSON
#
# Every worker (no control-plane role, schedulable), in name order, must:
#   - run a koupa Pod (namespace koupa, app.kubernetes.io/name=koupa);
#   - run no database Pod (label cnpg.io/cluster);
#   - run no other Pod with an emptyDir volume, except DaemonSet Pods (left
#     alone by --ignore-daemonsets) and finished Pods: kubectl drain without
#     --delete-emptydir-data refuses those, and the data may matter;
#   - pass `kubectl drain NODE --ignore-daemonsets --dry-run=server`
#     (PodDisruptionBudgets, unmanaged Pods, ...).
# The first worker that passes is printed on stdout. OUT_JSON lists every
# worker with its Pods and the reasons it was rejected, the chosen one and
# the number of koupa Pods the drain affects.
# Exit 0 a worker was chosen; 1 none qualifies (reasons on stderr); 2 usage
# or the cluster could not be read.
set -uo pipefail
NS=${KOUPA_NS:-koupa}
KUBECTL=${KUBECTL:-kubectl}
[ $# -eq 1 ] || { echo "usage: $0 OUT_JSON" >&2; exit 2; }
out=$1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

"$KUBECTL" get nodes -o json >"$tmp/nodes.json" 2>"$tmp/err" &&
	"$KUBECTL" get pods -A -o json >"$tmp/pods.json" 2>>"$tmp/err" || {
	echo "drain-target: cannot read nodes or Pods: $(head -c 300 "$tmp/err")" >&2
	exit 2
}

# Static checks; prints the workers that passed them.
python3 -I - "$tmp" "$NS" <<'PY' >"$tmp/static-ok" || exit 2
import json, os, sys
tmp, ns = sys.argv[1:]
nodes = json.load(open(os.path.join(tmp, "nodes.json")))["items"]
pods = json.load(open(os.path.join(tmp, "pods.json")))["items"]
workers = sorted(n["metadata"]["name"] for n in nodes
                 if "node-role.kubernetes.io/control-plane" not in n["metadata"].get("labels", {}))
unsched = {n["metadata"]["name"] for n in nodes if n.get("spec", {}).get("unschedulable")}
def kind(p):
    return next((o.get("kind") for o in p["metadata"].get("ownerReferences", []) if o.get("controller")), None)
def is_koupa(p):
    return (p["metadata"].get("namespace") == ns and not p["metadata"].get("deletionTimestamp")
            and p["metadata"].get("labels", {}).get("app.kubernetes.io/name") == "koupa")
cands = []
for w in workers:
    on = [p for p in pods if p.get("spec", {}).get("nodeName") == w]
    reasons, koupa = [], [p for p in on if is_koupa(p)]
    if w in unsched:
        reasons.append("cordoned (unschedulable)")
    if not koupa:
        reasons.append("no koupa Pod")
    for p in on:
        name = "%s/%s" % (p["metadata"].get("namespace"), p["metadata"]["name"])
        if "cnpg.io/cluster" in p["metadata"].get("labels", {}):
            reasons.append("database Pod %s" % name)
            continue
        if kind(p) == "DaemonSet" or p.get("status", {}).get("phase") in ("Succeeded", "Failed"):
            continue
        ed = [v["name"] for v in p.get("spec", {}).get("volumes", []) or [] if "emptyDir" in v]
        if ed:
            reasons.append("Pod %s uses emptyDir %s (drain would need --delete-emptydir-data)" % (name, ",".join(ed)))
    cands.append({"node": w, "ok": not reasons, "reasons": reasons, "koupa_pods": len(koupa),
                  "pods": [{"namespace": p["metadata"].get("namespace"), "name": p["metadata"]["name"],
                            "owner": kind(p), "phase": p.get("status", {}).get("phase"),
                            "emptydir": [v["name"] for v in p.get("spec", {}).get("volumes", []) or [] if "emptyDir" in v]}
                           for p in on]})
json.dump({"candidates": cands}, open(os.path.join(tmp, "cands.json"), "w"))
for c in cands:
    if c["ok"]:
        print(c["node"])
PY

# Server dry run for the workers that passed, in order, until one passes.
chosen=""
: >"$tmp/dry.tsv"
while read -r w; do
	[ -n "$w" ] || continue
	if "$KUBECTL" drain "$w" --ignore-daemonsets --dry-run=server >"$tmp/dry.out" 2>&1; then
		chosen=$w
		break
	fi
	printf '%s\t%s\n' "$w" "$(grep -v '^node/' "$tmp/dry.out" | tr '\n' ' ' | head -c 400)" >>"$tmp/dry.tsv"
done <"$tmp/static-ok"

python3 -I - "$tmp" "$out" "$chosen" <<'PY'
import datetime, json, os, sys
tmp, out, chosen = sys.argv[1:]
c = json.load(open(os.path.join(tmp, "cands.json")))["candidates"]
for line in open(os.path.join(tmp, "dry.tsv")):
    w, why = line.rstrip("\n").split("\t", 1)
    for x in c:
        if x["node"] == w:
            x["ok"] = False
            x["reasons"].append("kubectl drain --dry-run=server failed: " + why.strip())
ch = next((x for x in c if x["node"] == chosen), None)
json.dump({"checked_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
           "rules": "koupa Pod present; no database Pod; no non-DaemonSet running Pod with emptyDir; "
                    "kubectl drain --ignore-daemonsets --dry-run=server passes (A15)",
           "chosen": chosen or None, "koupa_pods_affected": ch["koupa_pods"] if ch else None,
           "candidates": c}, open(out, "w"), indent=2)
if not chosen:
    for x in c:
        print("drain-target: %s: %s" % (x["node"], "; ".join(x["reasons"]) or "?"), file=sys.stderr)
PY
[ -n "$chosen" ] || { echo "drain-target: no worker satisfies the A15 conditions" >&2; exit 1; }
echo "$chosen"
