# Cluster helpers for measure.sh (sourced). Use $KUBECTL and $NS.

# deploy_status: "done" when the rollout is complete (kubectl rollout
# status rules), else a short description.
deploy_status() {
	"$KUBECTL" -n "$NS" get deploy koupa -o json 2>/dev/null | python3 -I -c '
import json, sys
try:
    d = json.load(sys.stdin)
except ValueError:
    print("Deployment unreadable"); sys.exit()
s, want = d.get("status", {}), d["spec"].get("replicas", 1)
gen, obs = d["metadata"].get("generation", 0), s.get("observedGeneration", 0)
up, rep, av = s.get("updatedReplicas", 0), s.get("replicas", 0), s.get("availableReplicas", 0)
if obs >= gen and up == want and rep == up and av == up:
    print("done")
else:
    print("generation %s/%s updated %s replicas %s available %s of %s" % (obs, gen, up, rep, av, want))
'
}

# old_pods_gone: only Pods of the current ReplicaSet are left.
old_pods_gone() {
	local hash
	hash=$("$KUBECTL" -n "$NS" get rs -l app.kubernetes.io/name=koupa -o json 2>/dev/null | python3 -I -c '
import json, sys
rev = sys.argv[1]
for r in json.load(sys.stdin).get("items", []):
    if r["metadata"].get("annotations", {}).get("deployment.kubernetes.io/revision") == rev:
        print(r["metadata"].get("labels", {}).get("pod-template-hash", ""))
' "$("$KUBECTL" -n "$NS" get deploy koupa -o json 2>/dev/null | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["metadata"].get("annotations",{}).get("deployment.kubernetes.io/revision",""))' 2>/dev/null)" 2>/dev/null)
	[ -n "$hash" ] || return 1
	"$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name=koupa -o json 2>/dev/null | python3 -I -c '
import json, sys
h = sys.argv[1]
items = json.load(sys.stdin).get("items", [])
sys.exit(0 if items and all(p["metadata"].get("labels", {}).get("pod-template-hash") == h and not p["metadata"].get("deletionTimestamp") for p in items) else 1)
' "$hash"
}

# app_started: every koupa Pod's log has its tx_timeout_check line, the
# app's own startup verdict (there is no readinessProbe, design 4.9, so a
# Pod is Ready before the app has opened its database pool).
app_started() {
	local pods pod
	pods=$("$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name=koupa -o json 2>/dev/null |
		python3 -I -c 'import json,sys; print(" ".join(p["metadata"]["name"] for p in json.load(sys.stdin)["items"]))' 2>/dev/null) || return 1
	[ -n "$pods" ] || return 1
	for pod in $pods; do
		# Not a pipe: grep -q stops early, kubectl logs then dies of SIGPIPE
		# and pipefail would turn a found line into a failure.
		grep -q '"event": *"tx_timeout_check"' < <("$KUBECTL" -n "$NS" logs "$pod" -c koupa 2>/dev/null) || return 1
	done
}

# pods_digest_check DIGEST OUT_JSON: every Pod of the Deployment's current
# ReplicaSet (not terminating) runs DIGEST and there are spec.replicas of
# them. Writes {ok, want, pods, bad} to OUT_JSON; returns 0 when ok.
pods_digest_check() {
	local want=$1 out=$2 tmp
	tmp=$(mktemp -d)
	"$KUBECTL" -n "$NS" get deploy koupa -o json >"$tmp/deploy.json" 2>/dev/null
	"$KUBECTL" -n "$NS" get rs -l app.kubernetes.io/name=koupa -o json >"$tmp/rs.json" 2>/dev/null
	"$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name=koupa -o json >"$tmp/pods.json" 2>/dev/null
	python3 -I - "$tmp" "$want" "$out" <<'PY'
import json, os, sys
tmp, want, out = sys.argv[1:]
def load(n):
    try:
        return json.load(open(os.path.join(tmp, n)))
    except (OSError, ValueError):
        return None
dep, rss, pods = load("deploy.json"), load("rs.json"), load("pods.json")
res = {"ok": False, "want": want, "pods": [], "bad": []}
if not (dep and rss and pods):
    res["bad"].append("cannot read the Deployment, ReplicaSets or Pods")
else:
    rev = dep["metadata"].get("annotations", {}).get("deployment.kubernetes.io/revision")
    hashes = [r["metadata"].get("labels", {}).get("pod-template-hash") for r in rss.get("items", [])
              if r["metadata"].get("annotations", {}).get("deployment.kubernetes.io/revision") == rev]
    if len(hashes) != 1:
        res["bad"].append("no single ReplicaSet with revision %s" % rev)
    else:
        cur = [p for p in pods.get("items", [])
               if p["metadata"].get("labels", {}).get("pod-template-hash") == hashes[0]
               and not p["metadata"].get("deletionTimestamp")]
        for p in cur:
            st = next((c for c in p.get("status", {}).get("containerStatuses", []) or []
                       if c.get("name") == "koupa"), {})
            iid = st.get("imageID") or ""
            have = iid.rsplit("@", 1)[-1] if "@" in iid else None
            res["pods"].append({"pod": p["metadata"]["name"], "image_id": iid or None})
            if have != want:
                res["bad"].append("%s runs %s" % (p["metadata"]["name"], iid or "no imageID"))
        n = dep["spec"].get("replicas", 1)
        if len(cur) != n:
            res["bad"].append("%d Pods of the current ReplicaSet, want %d" % (len(cur), n))
    res["ok"] = not res["bad"] and want.startswith("sha256:")
json.dump(res, open(out, "w"), indent=2)
sys.exit(0 if res["ok"] else 1)
PY
	local rc=$?
	rm -rf "$tmp"
	return "$rc"
}
