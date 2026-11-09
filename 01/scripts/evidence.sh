#!/usr/bin/env bash
# Complete the evidence pack of one measurement run (design 5.4).
#
#   scripts/evidence.sh <run-dir>
#
# measure.sh has already put into <run-dir>: params.json, manifest/applied.yaml,
# rollout.json, the collector's raw records (collect-timeline.sh: app/,
# ingress.log, pods.jsonl, ..., missing.jsonl, timeline.txt) and Rush Hour's
# attempts.jsonl, boards.jsonl and scorecard.{json,txt}. This adds:
#   manifest/kustomize.yaml   `kubectl kustomize` of the overlay, image set as applied
#   manifest/live.yaml        `kubectl get -o yaml` of the koupa objects now
#   versions.txt              git commit, kubectl/cluster, podman, Cilium, CNPG, kernel
#   images.json               image digests: every koupa Pod seen, Rush Hour
#   audit/                    /api/audit: what the scorecard used, and the
#                             queries run again now (queries.txt says which)
#   failures.json             failed and unsettled attempts from the scorecard
#   evidence-index.txt        every file with its size; MISSING lines for
#                             required pieces that are absent
# Exit 0 when every required piece is present, 1 otherwise, 2 usage.
set -uo pipefail
stage=$(cd "$(dirname "$0")/.." && pwd)
NS=${KOUPA_NS:-koupa}
KUBECTL=${KUBECTL:-kubectl}

[ $# -eq 1 ] && [ -d "$1" ] || {
	echo "usage: $0 <run-dir>" >&2
	exit 2
}
run=$(realpath -- "$1")
[ -f "$run/params.json" ] || {
	echo "evidence: $run/params.json missing (not a measure.sh run directory)" >&2
	exit 2
}
mkdir -p "$run/manifest" "$run/audit"

param() { python3 -I -c 'import json,sys; v=json.load(open(sys.argv[1])); [v := v.get(k, {}) if isinstance(v, dict) else "" for k in sys.argv[2].split(".")]; print(v if isinstance(v,(str,int,float)) else "")' "$run/params.json" "$1"; }
run_id=$(param run_id)
overlay=$(param overlay)
image=$(param image_before)
base=$(param base_url)

case $overlay in
step-0 | control | base) dir=$stage/k8s/base ;;
step-[1-4]) dir=$stage/k8s/steps/$overlay ;;
*) dir=$stage/k8s/$overlay ;;
esac

# Manifests.
if "$KUBECTL" kustomize "$dir" >"$run/manifest/kustomize.raw.yaml" 2>"$run/manifest/kustomize.err"; then
	sed "s#ghcr.io/braveantony/koupa:v[0-9][0-9]*#${image:-ghcr.io/braveantony/koupa:v1}#" \
		"$run/manifest/kustomize.raw.yaml" >"$run/manifest/kustomize.yaml"
	rm -f "$run/manifest/kustomize.raw.yaml" "$run/manifest/kustomize.err"
fi
{
	"$KUBECTL" -n "$NS" get deploy,rs,pods,svc,endpointslices -o yaml
	echo "---"
	"$KUBECTL" -n "$NS" get gateway,httproute -o yaml
	echo "---"
	"$KUBECTL" get gatewayclass koupa -o yaml
	echo "---"
	"$KUBECTL" -n "$NS" get ciliumgatewayclassconfig -o yaml
} >"$run/manifest/live.yaml" 2>"$run/manifest/live.err"
[ -s "$run/manifest/live.err" ] || rm -f "$run/manifest/live.err"

# Versions.
{
	echo "collected_at=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
	echo "git_commit=$(git -C "$stage" rev-parse HEAD 2>/dev/null || echo unknown)"
	echo "git_dirty_files=$(git -C "$stage" status --porcelain 2>/dev/null | wc -l)"
	echo "kubectl_context=$("$KUBECTL" config current-context 2>/dev/null || echo unknown)"
	"$KUBECTL" version 2>/dev/null | sed 's/^/kubectl_version: /'
	echo "cilium_image=$("$KUBECTL" -n kube-system get ds cilium -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo unknown)"
	echo "cnpg_image=$("$KUBECTL" -n cnpg-system get deploy cnpg-controller-manager -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo unknown)"
	echo "postgres_image=$("$KUBECTL" -n "$NS" get cluster koupa-db -o jsonpath='{.spec.imageName}' 2>/dev/null || echo unknown)"
	echo "podman=$(podman --version 2>/dev/null || echo unknown)"
	echo "kernel=$(uname -sr)"
} >"$run/versions.txt"

# Image digests.
rh_kind=$(param rush_hour.kind)
rh_ref=$(param rush_hour.ref)
rh_digest=unknown
rh_id=$(param rush_hour.image_id)
case $rh_kind in
image) rh_digest=$(podman image inspect --format '{{.Digest}}' "${rh_id:-$rh_ref}" 2>/dev/null || echo unknown) ;;
bin) rh_digest=sha256:$(sha256sum "$rh_ref" 2>/dev/null | cut -d' ' -f1) ;;
esac
"$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name=koupa -o json >"$run/.pods-now.json" 2>/dev/null || echo '{"items":[]}' >"$run/.pods-now.json"
python3 -I - "$run" "$rh_kind" "$rh_ref" "$rh_digest" "$rh_id" <<'PY'
import json, os, sys
run, kind, ref, digest, iid = sys.argv[1:]
pods = {}
def add(items, source):
    for p in items:
        for c in p.get("status", {}).get("containerStatuses", []) or []:
            if c.get("name") != "koupa":
                continue
            uid = p["metadata"].get("uid")
            e = pods.setdefault(uid, {"pod": p["metadata"].get("name"), "uid": uid, "seen_in": []})
            e["image"] = c.get("image")
            if c.get("imageID"):
                e["image_id"] = c.get("imageID")
            e["seen_in"].append(source)
# Every Pod of the run: watch events, the state at stop, the state now.
path = os.path.join(run, "pods.jsonl")
if os.path.exists(path):
    for line in open(path, encoding="utf-8", errors="replace"):
        i = line.find("{")
        if i < 0:
            continue
        try:
            ev = json.loads(line[i:])
        except ValueError:
            continue
        obj = ev.get("object", ev)
        if obj.get("metadata", {}).get("labels", {}).get("app.kubernetes.io/name") == "koupa":
            add([obj], "pods.jsonl")
for name in ("pods-at-stop.json", ".pods-now.json"):
    try:
        add(json.load(open(os.path.join(run, name))).get("items", []), name.strip("."))
    except (OSError, ValueError):
        pass
for e in pods.values():
    e["seen_in"] = sorted(set(e["seen_in"]))
out = {"koupa_pods": sorted(pods.values(), key=lambda e: e["pod"] or ""),
       "rush_hour": {"kind": kind, "ref": ref, "image_id": iid or None, "digest": digest}}
# Each Pod's digest against what its tag resolved to at the start of the run.
try:
    reg = json.load(open(os.path.join(run, "image-digests.json"))).get("koupa", {})
except (OSError, ValueError):
    reg = {}
for e in out["koupa_pods"]:
    want = (reg.get(e.get("image") or "") or {}).get("digest")
    have = (e.get("image_id") or "").rsplit("@", 1)[-1] or None
    e["registry_digest"] = want
    e["digest_matches_registry"] = (have == want) if want and have else None
json.dump(out, open(os.path.join(run, "images.json"), "w"), indent=2)
PY
rm -f "$run/.pods-now.json"

# /api/audit: what the scorecard used, and the same queries again now.
python3 -I - "$run" <<'PY'
import json, os, sys
run = sys.argv[1]
try:
    s = json.load(open(os.path.join(run, "scorecard.json")))
except (OSError, ValueError):
    sys.exit(0)
json.dump(s.get("facts", {}).get("audit"), open(os.path.join(run, "audit", "used-by-scorecard.json"), "w"), indent=2, ensure_ascii=False)
d = s.get("diagnostics") or {}
json.dump({"failed_attempts": d.get("failed_attempts") or [],
           "unsettled_attempts": d.get("unsettled_attempts") or [],
           "client_in_flight": d.get("client_in_flight") or []},
          open(os.path.join(run, "failures.json"), "w"), indent=2, ensure_ascii=False)
PY
: >"$run/audit/queries.txt"
for mode in cursor orders ready; do
	url="$base/api/audit?run=$run_id&mode=$mode"
	echo "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ) GET $url (after the run; the scorecard used audit/used-by-scorecard.json)" >>"$run/audit/queries.txt"
	curl -sS --max-time 10 "$url" >"$run/audit/$mode.json" 2>>"$run/audit/queries.txt" ||
		echo "  failed (curl rc=$?)" >>"$run/audit/queries.txt"
done

# collect-timeline.sh writes missing.jsonl only when a source is missing;
# once it has run, an absent file means none was.
[ -f "$run/collect.json" ] && [ ! -e "$run/missing.jsonl" ] && : >"$run/missing.jsonl"

# Index.
required=(params.json image-digests.json manifest/applied.yaml manifest/kustomize.yaml manifest/live.yaml versions.txt
	images.json rollout.json scorecard.json scorecard.txt attempts.jsonl boards.jsonl ingress.log app
	missing.jsonl timeline.txt audit/used-by-scorecard.json failures.json)
missing=0
{
	echo "# evidence pack $run_id ($(date -u +%Y-%m-%dT%H:%M:%SZ))"
	for f in "${required[@]}"; do
		if [ -e "$run/$f" ]; then
			echo "OK      $f"
		else
			echo "MISSING $f"
			missing=$((missing + 1))
		fi
	done
	echo "# all files"
	(cd "$run" && find . -path ./evidence-index.txt -prune -o -type f -printf '%10s  %P\n' | sort -k2)
} >"$run/evidence-index.txt"
if [ "$missing" -gt 0 ]; then
	echo "evidence: $missing required piece(s) missing in $run (see evidence-index.txt)" >&2
	exit 1
fi
echo "evidence: $run complete"
