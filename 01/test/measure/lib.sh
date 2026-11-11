# Shared helpers for the measure.sh script tests. No cluster: fake kubectl,
# podman, curl, collect-timeline and rush-hour are put in front of PATH.
#
#   . "$(dirname "$0")/lib.sh"
#   setup_fakes            # $T (scratch), $FAKE (fake state), PATH, env
#   make_live step-2 'env["GRACEFUL_SHUTDOWN"] = "1"'
#   finish                 # prints PASS/FAIL, exits
set -uo pipefail

STAGE=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REAL_KUBECTL=$(command -v kubectl || true)
# The images measure.sh uses: KOUPA_REPO:KOUPA_V1_TAG / KOUPA_V2_TAG.
# shellcheck source=../../images.env
. "$STAGE/images.env"
IMG_V1=$KOUPA_REPO:$KOUPA_V1_TAG
IMG_V2=$KOUPA_REPO:$KOUPA_V2_TAG
# shellcheck source=../../scripts/lib/image-sed.sh
. "$STAGE/scripts/lib/image-sed.sh"
KOUPA_REPO_RE=$(ere_escape "$KOUPA_REPO")
export KOUPA_REPO KOUPA_V1_TAG KOUPA_V2_TAG IMG_V1 IMG_V2 KOUPA_REPO_RE
FAILS=0
NAME=$(basename "$0" .sh)

fail() {
	echo "  FAIL: $*"
	FAILS=$((FAILS + 1))
}
ok() { echo "  ok: $*"; }

# check DESCRIPTION CMD...: CMD must succeed.
check() {
	local what=$1
	shift
	if "$@"; then ok "$what"; else fail "$what"; fi
}

finish() {
	if [ "$FAILS" -eq 0 ]; then
		echo "PASS $NAME"
		exit 0
	fi
	echo "FAIL $NAME ($FAILS)"
	exit 1
}

# fake_registry DIR: a fake skopeo in DIR (for setup.sh tests): every tag
# exists, except one listed in $FAKE/unpublished (absent: skopeo's exit 2
# "manifest unknown"); $FAKE/registry-down makes every lookup fail.
fake_registry() {
	cat >"$1/skopeo" <<'SKOPEO'
#!/usr/bin/env bash
echo "skopeo $*" >>"$FAKE/calls.log"
ref=${!#}; ref=${ref#docker://}
[ -f "$FAKE/registry-down" ] && { echo 'level=fatal msg="pinging container registry ghcr.io: dial tcp: lookup ghcr.io: no such host"' >&2; exit 1; }
if grep -qxF "$ref" "$FAKE/unpublished" 2>/dev/null; then
	echo "level=fatal msg=\"Error parsing image name \\\"docker://$ref\\\": reading manifest ${ref##*:} in ${ref%:*}: manifest unknown\"" >&2
	exit 2
fi
printf 'sha256:%064d\n' 1
SKOPEO
	chmod +x "$1/skopeo"
}

setup_fakes() {
	T=$(mktemp -d "${TMPDIR:-/tmp}/measure-test.XXXXXX")
	trap 'rm -rf "$T"' EXIT
	FAKE=$T/fake
	mkdir -p "$FAKE/bin" "$FAKE/logs" "$T/evidence"
	export FAKE REAL_KUBECTL
	export MEASURE_EVIDENCE_DIR=$T/evidence
	export COLLECT_TIMELINE=$FAKE/bin/collect-timeline
	export RUSH_HOUR_BIN=$FAKE/bin/rush-hour
	export PATH=$FAKE/bin:$PATH
	# The host-side rush-hour measure.sh builds at start: a fake go copies
	# the fake rush-hour to a scratch path (never the real 01/bin).
	export MEASURE_GO=$FAKE/bin/go
	export MEASURE_HOST_RUSH_HOUR=$T/hostbin/rush-hour
	: >"$FAKE/calls.log"
	echo 172.22.0.201 >"$FAKE/gateway-ip"

	cat >"$FAKE/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
# Fake kubectl: answers from $FAKE, records every call in $FAKE/calls.log.
echo "kubectl $*" >>"$FAKE/calls.log"
args=("$@")
# Drop "-n NS" / "--namespace NS" so dispatch sees the verb first.
clean=()
i=0
while [ $i -lt ${#args[@]} ]; do
	case ${args[$i]} in
	-n | --namespace) i=$((i + 2)); continue ;;
	esac
	clean+=("${args[$i]}")
	i=$((i + 1))
done
set -- "${clean[@]}"
case "$1" in
kustomize) exec "$REAL_KUBECTL" "$@" ;;
apply)
	cat >"$FAKE/applied.yaml"
	# Like a real apply + finished rollout: the Deployment runs the applied image.
	img=$(sed -nE "s#^ *image: ($KOUPA_REPO_RE:v[0-9]+(\.[0-9]+){0,2})\$#\1#p" "$FAKE/applied.yaml" | head -1)
	[ -f "$FAKE/deploy.json" ] && python3 -I -c '
import json, sys
d = json.load(open(sys.argv[1]))
d["spec"]["template"]["spec"]["containers"][0]["image"] = sys.argv[2]
d["status"]["updatedReplicas"] = 3
json.dump(d, open(sys.argv[1], "w"))' "$FAKE/deploy.json" "$img"
	# ... and its Pods run that image's digest again.
	if [ -f "$FAKE/pods.json" ] && [ -n "$img" ] && [ ! -f "$FAKE/apply-stale" ]; then
		digest=$(FAKE_QUIET=1 "$FAKE/bin/skopeo" inspect --format '{{.Digest}}' "docker://$img")
		python3 -I -c '
import json, sys
path, img, digest = sys.argv[1:]
p = json.load(open(path))
for pod in p["items"]:
    pod["spec"]["containers"][0]["image"] = img
    st = pod["status"]["containerStatuses"][0]
    st["image"], st["imageID"] = img, img.split(":")[0] + "@" + digest
json.dump(p, open(path, "w"))' "$FAKE/pods.json" "$img" "$digest"
	fi
	rm -f "$FAKE/set-done"
	exit 0 ;;
rollout) exit 0 ;;
drain)
	# drain NODE --ignore-daemonsets [--dry-run=server] [--timeout=..]
	node=$2
	case " $* " in
	*" --dry-run=server "*)
		if [ -f "$FAKE/drain-dryrun-fail-$node" ]; then
			echo "error: unable to drain node \"$node\": $(cat "$FAKE/drain-dryrun-fail-$node")" >&2
			exit 1
		fi
		echo "node/$node cordoned (server dry run)"
		exit 0 ;;
	esac
	python3 -I "$FAKE/nodes.py" cordon "$node"
	[ -f "$FAKE/drain-sleep" ] && { sleep "$(cat "$FAKE/drain-sleep")" & wait $!; }
	# $FAKE/drain-moves: the evicted koupa Pod lands on another worker.
	[ -f "$FAKE/drain-moves" ] && python3 -I "$FAKE/nodes.py" evict "$node"
	echo "node/$node drained"
	exit "$(cat "$FAKE/drain-rc" 2>/dev/null || echo 0)" ;;
uncordon)
	# $FAKE/uncordon-fail holds how many calls fail before one works.
	if [ "$(cat "$FAKE/uncordon-fail" 2>/dev/null || echo 0)" -gt 0 ]; then
		echo $(($(cat "$FAKE/uncordon-fail") - 1)) >"$FAKE/uncordon-fail"
		echo "error: the server is currently unable to handle the request" >&2
		exit 1
	fi
	python3 -I "$FAKE/nodes.py" uncordon "$2"; echo "node/$2 uncordoned"; exit 0 ;;
scale)
	# scale deploy/koupa --replicas=N; $FAKE/scale-fail: how many
	# scale-backs to 3 fail before one works.
	n=${3#--replicas=}
	if [ "$n" = 3 ] && [ "$(cat "$FAKE/scale-fail" 2>/dev/null || echo 0)" -gt 0 ]; then
		echo $(($(cat "$FAKE/scale-fail") - 1)) >"$FAKE/scale-fail"
		echo "error: the server is currently unable to handle the request" >&2
		exit 1
	fi
	python3 -I - "$FAKE/deploy.json" "$n" <<'PY'
import json, sys
path, n = sys.argv[1], int(sys.argv[2])
d = json.load(open(path))
d["spec"]["replicas"] = n
d["status"].update({"replicas": n, "updatedReplicas": n, "readyReplicas": n, "availableReplicas": n})
json.dump(d, open(path, "w"))
PY
	exit 0 ;;
delete)
	# delete pod NAME [--wait=false]: rescheduled onto the emptiest worker
	# ($FAKE/delete-noop: back onto the same node).
	[ -f "$FAKE/delete-noop" ] || python3 -I "$FAKE/nodes.py" resched "$3"
	exit 0 ;;
version) echo "Client Version: fake"; echo "Server Version: fake"; exit 0 ;;
config) echo fake-context; exit 0 ;;
set)
	# set image deploy/koupa koupa=IMAGE
	img=${4#koupa=}
	python3 -I - "$FAKE/deploy.json" "$img" "$FAKE" <<'PY'
import json, os, sys
path, img, fake = sys.argv[1:]
d = json.load(open(path))
d["spec"]["template"]["spec"]["containers"][0]["image"] = img
d["metadata"]["generation"] += 1
if os.path.exists(os.path.join(fake, "rollout-never")):
    d["status"]["updatedReplicas"] = 1
else:
    d["status"]["observedGeneration"] = d["metadata"]["generation"]
json.dump(d, open(path, "w"))
PY
	digest=$(FAKE_QUIET=1 "$FAKE/bin/skopeo" inspect --format '{{.Digest}}' "docker://$img")
	[ -f "$FAKE/v2-stale" ] && digest="sha256:$(printf '%064d' 9)"
	python3 -I - "$FAKE/pods.json" "$img" "$digest" <<'PY'
import json, sys
path, img, digest = sys.argv[1:]
p = json.load(open(path))
for pod in p["items"]:
    pod["spec"]["containers"][0]["image"] = img
    st = pod["status"]["containerStatuses"][0]
    st["image"] = img
    st["imageID"] = img.split(":")[0] + "@" + digest
json.dump(p, open(path, "w"))
PY
	touch "$FAKE/set-done"
	exit 0 ;;
logs)
	pod=$2
	cat "$FAKE/logs/$pod.log" 2>/dev/null || { echo "no logs for $pod" >&2; exit 1; }
	exit 0 ;;
get)
	what=$2
	case "$what" in
	deploy | deployment | deploy/koupa | deployment/koupa | koupa) ;;
	esac
	case " $* " in
	*" gateway "*) cat "$FAKE/gateway-ip"; exit 0 ;;
	*" deploy koupa "* | *" deployment koupa "* | *" deploy/koupa "*)
		case " $* " in
		*" -o json "*)
			# $FAKE/get-sleep: after set image, each read of the Deployment
			# takes that long (a slow API server near the deadline).
			[ -f "$FAKE/set-done" ] && [ -f "$FAKE/get-sleep" ] && sleep "$(cat "$FAKE/get-sleep")"
			cat "$FAKE/deploy.json" ;;
		*" -o yaml "*) cat "$FAKE/deploy.json" ;;
		*) python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); s=d["status"]; print(d["metadata"]["generation"], s.get("observedGeneration",0), d["spec"]["replicas"], s.get("replicas",0), s.get("updatedReplicas",0), s.get("availableReplicas",0))' "$FAKE/deploy.json" ;;
		esac
		exit 0 ;;
	*" rs "* | *" replicasets "* | *" replicaset "*) cat "$FAKE/rs.json"; exit 0 ;;
	*" nodes "* | *" node "*) cat "$FAKE/nodes.json"; exit 0 ;;
	*" pods"*" -A "* | *" pods"*" --all-namespaces "*) python3 -I "$FAKE/nodes.py" allpods; exit 0 ;;
	*" pods "*) cat "$FAKE/pods.json"; exit 0 ;;
	*" ds "* | *" daemonset "*) echo "quay.io/cilium/cilium:fake"; exit 0 ;;
	*) echo '{"apiVersion":"v1","kind":"List","items":[]}'; exit 0 ;;
	esac ;;
esac
echo "fake kubectl: unhandled: $*" >&2
exit 0
EOF

	cat >"$FAKE/bin/podman" <<'EOF'
#!/usr/bin/env bash
echo "podman $*" >>"$FAKE/calls.log"
case "$1" in
image)
	# podman image inspect --format F REF: {{.Id}} -> the fake image ID.
	case "$*" in
	*.Id*) echo "fa4e1d0000000000000000000000000000000000000000000000000000000001" ;;
	*) echo "sha256:fakedigest" ;;
	esac ;;
ps) cat "$FAKE/podman-ps" 2>/dev/null ;;
version | --version) echo "podman version fake" ;;
run)
	# podman run [opts] IMAGE ARGS...: run the fake rush-hour with ARGS.
	shift
	while [ $# -gt 0 ] && [ "$1" != "${RUSH_HOUR_IMAGE:-ghcr.io/braveantony/rush-hour:0.1.0}" ] &&
		[ "$1" != fa4e1d0000000000000000000000000000000000000000000000000000000001 ]; do shift; done
	shift
	exec "$FAKE/bin/rush-hour" "$@" ;;
esac
exit 0
EOF

	# Fake skopeo inspect --format '{{.Digest}}' docker://IMAGE: the digest
	# in $FAKE/digest-<tag> (default: the v1 line = make_live's imageID,
	# others ...02);
	# $FAKE/skopeo-fail makes it fail; $FAKE/unpublished-<tag>: the tag is
	# not on the registry (skopeo's exit 2, "manifest unknown").
	cat >"$FAKE/bin/skopeo" <<'EOF'
#!/usr/bin/env bash
[ -n "${FAKE_QUIET:-}" ] || echo "skopeo $*" >>"$FAKE/calls.log"
[ -f "$FAKE/skopeo-fail" ] && { echo "fake skopeo: registry unreachable" >&2; exit 1; }
ref=${!#}
ref=${ref#docker://}
tag=${ref##*:}
if [ -f "$FAKE/unpublished-$tag" ]; then
	echo "level=fatal msg=\"Error parsing image name \\\"docker://$ref\\\": reading manifest $tag in ${ref%:*}: manifest unknown\"" >&2
	exit 2
fi
if [ -f "$FAKE/digest-$tag" ]; then cat "$FAKE/digest-$tag"; exit 0; fi
case $tag in
v1 | v1.*) printf 'sha256:%064d\n' 1 ;;
*) printf 'sha256:%064d\n' 2 ;;
esac
EOF

	cat >"$FAKE/bin/curl" <<'EOF'
#!/usr/bin/env bash
# Fake curl [-sS] [--max-time N] [-o FILE] [-w FMT] URL: /api/audit
# answers per mode; $FAKE/curl-fail fails like an unreachable host,
# $FAKE/curl-500 answers HTTP 500.
echo "curl $*" >>"$FAKE/calls.log"
out=/dev/stdout w="" url=""
while [ $# -gt 0 ]; do
	case $1 in
	-o) out=$2; shift ;;
	-w) w=$2; shift ;;
	--max-time) shift ;;
	-*) ;;
	*) url=$1 ;;
	esac
	shift
done
if [ -f "$FAKE/curl-fail" ]; then
	echo "curl: (7) Failed to connect" >&2
	[ -n "$w" ] && printf '000'
	exit 7
fi
code=200
if [ -f "$FAKE/curl-500" ]; then
	code=500
	echo '{"error":"database unavailable"}' >"$out"
else
	case $url in
	*mode=cursor*) echo '{"cursor":5}' >"$out" ;;
	*mode=orders*)
		# $FAKE/audit-orders overrides; @RUN@ is replaced by the run id.
		run=${url#*run=}; run=${run%%&*}
		if [ -f "$FAKE/audit-orders" ]; then sed "s/@RUN@/$run/g" "$FAKE/audit-orders" >"$out"
		else printf '{"%s-c1":[1],"%s-c2":[2]}\n' "$run" "$run" >"$out"; fi ;;
	*mode=ready*) echo '[]' >"$out" ;;
	*) echo '{"fake":true}' >"$out" ;;
	esac
fi
[ -n "$w" ] && printf '%s' "$code"
exit 0
EOF

	cat >"$FAKE/bin/collect-timeline" <<'EOF'
#!/usr/bin/env bash
# Fake collect-timeline.sh start|stop DIR
echo "collect-timeline $*" >>"$FAKE/calls.log"
echo "RUSH_HOUR=${RUSH_HOUR:-}" >>"$FAKE/collect-env"
dir=$2
mkdir -p "$dir/app"
case $1 in
start) echo '{"started_at":"2026-01-01T00:00:00Z"}' >"$dir/collect.json"; : >"$dir/ingress.log" ;;
stop)
	[ -f "$FAKE/collect-stop-fail" ] && { echo "fake collect-timeline: stop failed" >&2; exit 1; }
	: >"$dir/missing.jsonl"
	echo "fake timeline" >"$dir/timeline.txt"
	: >"$dir/timeline.jsonl"
	echo '{"items":[]}' >"$dir/pods-at-stop.json"
	: >"$dir/host.jsonl"
	: >"$dir/envoy-stats-start.txt"
	: >"$dir/envoy-stats-end.txt"
	;;
esac
EOF

	# Fake rush-hour. run: prints SENDING_STARTED backdated by
	# $FAKE/rh-backdate seconds (so tests need not wait out D), writes
	# scorecard.{json,txt} from $FAKE/scorecard.json, exits $FAKE/rh-rc.
	cat >"$FAKE/bin/rush-hour" <<'EOF'
#!/usr/bin/env bash
echo "rush-hour $*" >>"$FAKE/calls.log"
cmd=$1
shift
case $cmd in
run)
	out="" run=""
	while [ $# -gt 0 ]; do
		case $1 in
		--out) out=$2; shift ;;
		--run) run=$2; shift ;;
		esac
		shift
	done
	back=$(cat "$FAKE/rh-backdate" 2>/dev/null || echo 0)
	echo $$ >"$FAKE/rh-pid"
	trap 'echo "rush-hour got a signal" >>"$FAKE/calls.log"; exit 130' INT TERM
	[ -f "$FAKE/rh-no-start" ] || echo "SENDING_STARTED $(( $(date +%s%N) - back * 1000000000 ))"
	sleep "$(cat "$FAKE/rh-sleep" 2>/dev/null || echo 1)" &
	wait $!
	mkdir -p "$out"
	: >"$out/attempts.jsonl"
	: >"$out/boards.jsonl"
	sed "s/@RUN@/$run/g" "$FAKE/scorecard.json" >"$out/scorecard.json"
	printf 'Rush Hour 成績單　量測編號 %s\n\n【主要成績】\n訂單：100 張\n' "$run" >"$out/scorecard.txt"
	cat "$out/scorecard.txt"
	exit "$(cat "$FAKE/rh-rc" 2>/dev/null || echo 0)" ;;
summarize) echo "彙整表（fake）" ;;
probe)
	# probe --base U --run ID --duration D --out DIR: runs until TERM.
	out=""
	while [ $# -gt 0 ]; do case $1 in --out) out=$2; shift ;; esac; shift; done
	echo $$ >"$FAKE/probe-pid"
	trap 'echo "{\"kind\":\"probe\",\"pod_uid\":\"uid-0\"}" >>"$out/probes.jsonl"; echo "probe stopped" >>"$FAKE/calls.log"; exit 0' INT TERM
	: >"$out/probes.jsonl"
	sleep 300 &
	wait $! ;;
calibrate)
	# calibrate [--prestop S] --json FILE DIR...: $FAKE/calib-valid-per-run
	# valid samples per DIR (default 2), candidate 3.2 s when >= 5.
	json="" pre=20s dirs=()
	while [ $# -gt 0 ]; do
		case $1 in --json) json=$2; shift ;; --prestop) pre=$2; shift ;; *) dirs+=("$1") ;; esac
		shift
	done
	python3 -I - "$json" "$pre" "$(cat "$FAKE/calib-valid-per-run" 2>/dev/null || echo 2)" "${dirs[@]}" <<'PY'
import json, os, sys
out, pre, per, dirs = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4:]
samples = []
for d in dirs:
    for i in range(3):
        ok = i < per
        samples.append({"run": os.path.basename(d), "pod_name": "koupa-abc-%d" % i, "pod_uid": "uid-%d" % i,
                        "lower": "2026-01-01T00:00:01Z", "upper": "2026-01-01T00:00:03Z" if ok else None,
                        "upper_kind": "model" if ok else "none", "p": 0.33, "k": 18, "n": 160,
                        "u_minus_deletion_s": 3.2 if ok else None, "valid": ok,
                        "invalid_reason": "" if ok else "no_upper_bound"})
valid = sum(s["valid"] for s in samples)
rep = {"prestop_s": float(pre.rstrip("s")), "upper_bound_method": "model", "samples": samples,
       "valid_samples": valid, "candidate_s": 3.2 if valid >= 5 else None, "probes_without_pod_uid": 0}
json.dump(rep, open(out, "w"))
print("fake calibrate table: %d valid" % valid)
print(("candidate: 3.200 s" if valid >= 5 else "no candidate: %d" % valid))
PY
	;;
coverage)
	[ -f "$FAKE/coverage-fail" ] && { echo "fake coverage: cannot read $1" >&2; exit 1; } ;;
timeline) ;;
esac
exit 0
EOF
	# Fake go build -o OUT ./rush-hour: copies the fake rush-hour;
	# $FAKE/go-fail makes the build fail.
	cat >"$FAKE/bin/go" <<'EOF'
#!/usr/bin/env bash
echo "go $*" >>"$FAKE/calls.log"
[ -f "$FAKE/go-fail" ] && { echo "fake go: build failed: syntax error" >&2; exit 1; }
out=""
while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift ;; esac; shift; done
mkdir -p "$(dirname "$out")"
cp "$FAKE/bin/rush-hour" "$out"
EOF

	# Nodes and Pods across the cluster: nodes.json (3 workers, 1 control
	# plane), the koupa Pods of pods.json plus $FAKE/other-pods.json.
	cat >"$FAKE/nodes.py" <<'EOF'
import json, os, sys
fake = os.environ["FAKE"]
def load(n, default):
    try:
        return json.load(open(os.path.join(fake, n)))
    except (OSError, ValueError):
        return default
def save(n, v):
    json.dump(v, open(os.path.join(fake, n), "w"))
nodes, pods = load("nodes.json", {"items": []}), load("pods.json", {"items": []})
workers = [n["metadata"]["name"] for n in nodes["items"]
           if "node-role.kubernetes.io/control-plane" not in n["metadata"].get("labels", {})]
def per_node():
    c = {w: 0 for w in workers}
    for p in pods["items"]:
        c[p["spec"].get("nodeName")] = c.get(p["spec"].get("nodeName"), 0) + 1
    return c
def unsched(w):
    return any(n["metadata"]["name"] == w and n["spec"].get("unschedulable") for n in nodes["items"])
cmd = sys.argv[1]
if cmd in ("cordon", "uncordon"):
    for n in nodes["items"]:
        if n["metadata"]["name"] == sys.argv[2]:
            n["spec"]["unschedulable"] = cmd == "cordon"
    save("nodes.json", nodes)
elif cmd == "evict":
    for p in pods["items"]:
        if p["spec"].get("nodeName") == sys.argv[2]:
            c = per_node()
            p["spec"]["nodeName"] = min((w for w in workers if w != sys.argv[2] and not unsched(w)), key=lambda w: c[w])
    save("pods.json", pods)
elif cmd == "resched":
    for p in pods["items"]:
        if p["metadata"]["name"] == sys.argv[2]:
            p["spec"]["nodeName"] = None
            c = per_node()
            p["spec"]["nodeName"] = min((w for w in workers if not unsched(w)), key=lambda w: c[w])
            p["metadata"]["name"] += "x"
    save("pods.json", pods)
elif cmd == "allpods":
    other = load("other-pods.json", {"items": []})
    print(json.dumps({"kind": "List", "items": pods["items"] + other["items"]}))
EOF
	chmod +x "$FAKE"/bin/*
	scorecard_clean
}

# scorecard ITEMS_PY: write $FAKE/scorecard.json; ITEMS_PY edits dict `s`.
scorecard() {
	python3 -I - "$FAKE/scorecard.json" "${1:-}" <<'PY'
import json, sys
path, edit = sys.argv[1], sys.argv[2]
z = lambda: {"confirmed": 0, "unknown": 0}
s = {
    "run": "@RUN@", "invalid": False, "invalid_reason": "", "invalid_reasons": None,
    "has_unknown": False, "no_ingress": False,
    "items": {"orders": {"confirmed": 1200, "unknown": 0}, "first_try_failed": z(),
              "failed_after_retry": z(), "duplicate_client_refs": z(),
              "duplicate_extra_orders": z(),
              "missing": {"board-1": z(), "board-2": z(), "board-3": z()}},
    "diagnostics": {"failed_attempts": [], "unsettled_attempts": []},
    "facts": {"run": "@RUN@", "audit": {"cursor": 5, "orders": {"@RUN@-c1": [1], "@RUN@-c2": [2]}, "ready": []}},
}
exec(edit)
json.dump(s, open(path, "w"), ensure_ascii=False)
PY
}
scorecard_clean() { scorecard ""; }

# make_live OVERLAY [EDIT_PY]: the cluster state after a finished rollout
# of OVERLAY with image v1. EDIT_PY may change `c` (container), `env`
# (name -> value), `pod` (pod spec) before the objects are written.
make_live() {
	local overlay=$1 edit=${2:-} dir
	case $overlay in
	step-0 | control | base) dir=$STAGE/k8s/base ;;
	step-[1-5]) dir=$STAGE/k8s/steps/$overlay ;;
	*) dir=$STAGE/k8s/$overlay ;;
	esac
	"$REAL_KUBECTL" kustomize "$dir" >"$FAKE/rendered.yaml" || return 1
	python3 -I - "$FAKE" "$edit" <<'PY'
import json, os, sys, yaml
fake, edit = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(open(fake + "/rendered.yaml")) if d]
dep = [d for d in docs if d["kind"] == "Deployment"][0]
pod = dep["spec"]["template"]["spec"]
c = pod["containers"][0]
c["image"] = os.environ["IMG_V1"]
pod.setdefault("terminationGracePeriodSeconds", 30)
for e in c.get("env", []):
    fr = e.get("valueFrom", {}).get("fieldRef")
    if fr:
        fr["apiVersion"] = "v1"
env = {e["name"]: e.get("value") for e in c.get("env", []) if "value" in e}
exec(edit)
# Re-apply env edits (value entries only; valueFrom entries stay).
kept = [e for e in c.get("env", []) if "value" not in e]
c["env"] = kept + [{"name": k, "value": v} for k, v in env.items()]
dep["metadata"].update({"generation": 2, "uid": "dep-uid",
    "annotations": {"deployment.kubernetes.io/revision": "2"}})
dep["status"] = {"observedGeneration": 2, "replicas": 3, "updatedReplicas": 3,
                 "readyReplicas": 3, "availableReplicas": 3}
rs = {"apiVersion": "apps/v1", "kind": "ReplicaSet",
      "metadata": {"name": "koupa-abc", "namespace": "koupa",
                   "labels": {"app.kubernetes.io/name": "koupa", "pod-template-hash": "abc"},
                   "annotations": {"deployment.kubernetes.io/revision": "2"},
                   "ownerReferences": [{"kind": "Deployment", "name": "koupa", "controller": True}]},
      "spec": {"replicas": 3}, "status": {"replicas": 3}}
old = json.loads(json.dumps(rs))
old["metadata"].update({"name": "koupa-old", "annotations": {"deployment.kubernetes.io/revision": "1"}})
old["spec"]["replicas"] = 0
pods = []
for i in range(3):
    pods.append({"apiVersion": "v1", "kind": "Pod",
        "metadata": {"name": "koupa-abc-%d" % i, "namespace": "koupa", "uid": "uid-%d" % i,
                     "labels": {"app.kubernetes.io/name": "koupa", "pod-template-hash": "abc"},
                     "ownerReferences": [{"kind": "ReplicaSet", "name": "koupa-abc", "controller": True}]},
        "spec": dict(json.loads(json.dumps(pod)), nodeName="tkha-worker-%d" % (i + 1)),
        "status": {"phase": "Running",
                   "conditions": [{"type": "Ready", "status": "True"}],
                   "containerStatuses": [{"name": "koupa", "ready": True, "image": c["image"],
                       "imageID": "ghcr.io/braveantony/koupa@sha256:%064d" % 1,
                       "state": {"running": {}}}]}})
    graceful = env.get("GRACEFUL_SHUTDOWN") == "1"
    th = env.get("TEST_HOOKS") == "1"
    with open("%s/logs/koupa-abc-%d.log" % (fake, i), "w") as f:
        f.write(json.dumps({"ts": "2026-01-01T00:00:00Z", "event": "startup", "pod_uid": "uid-%d" % i,
            "graceful_shutdown": graceful, "test_hooks": th,
            "test_mutation_drop_ready": 0, "test_mutation_duplicate_order": 0}) + "\n")
        f.write(json.dumps({"ts": "2026-01-01T00:00:01Z", "event": "tx_timeout_check", "pod_uid": "uid-%d" % i,
            "ok": True, "got": "1min"}) + "\n")
json.dump(dep, open(fake + "/deploy.json", "w"))
node = lambda n, cp=False: {"apiVersion": "v1", "kind": "Node", "metadata": {"name": n,
    "labels": {"node-role.kubernetes.io/control-plane": ""} if cp else {}}, "spec": {}}
json.dump({"kind": "List", "items": [node("tkha-control-plane-1", True)] +
           [node("tkha-worker-%d" % i) for i in (1, 2, 3)]}, open(fake + "/nodes.json", "w"))
db = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "koupa-db-1", "namespace": "koupa",
      "labels": {"cnpg.io/cluster": "koupa-db"}, "ownerReferences": [{"kind": "Cluster", "name": "koupa-db", "controller": True}]},
      "spec": {"nodeName": "tkha-worker-1", "volumes": [{"name": "scratch-data", "emptyDir": {}}]},
      "status": {"phase": "Running"}}
cil = lambda w: {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "cilium-" + w[-1], "namespace": "kube-system",
      "ownerReferences": [{"kind": "DaemonSet", "name": "cilium", "controller": True}]},
      "spec": {"nodeName": w, "volumes": [{"name": "tmp", "emptyDir": {}}]}, "status": {"phase": "Running"}}
if not os.path.exists(fake + "/other-pods.json.keep"):
    json.dump({"kind": "List", "items": [db] + [cil("tkha-worker-%d" % i) for i in (1, 2, 3)]},
              open(fake + "/other-pods.json", "w"))
json.dump({"kind": "List", "items": [rs, old]}, open(fake + "/rs.json", "w"))
json.dump({"kind": "List", "items": pods}, open(fake + "/pods.json", "w"))
PY
}

# latest_run: the newest evidence/<run-id> directory.
latest_run() { ls -1dt "$MEASURE_EVIDENCE_DIR"/*/ 2>/dev/null | head -1 | sed 's:/$::'; }
