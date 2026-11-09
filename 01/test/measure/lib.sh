# Shared helpers for the measure.sh script tests. No cluster: fake kubectl,
# podman, curl, collect-timeline and rush-hour are put in front of PATH.
#
#   . "$(dirname "$0")/lib.sh"
#   setup_fakes            # $T (scratch), $FAKE (fake state), PATH, env
#   make_live step-1 'env["GRACEFUL_SHUTDOWN"] = "1"'
#   finish                 # prints PASS/FAIL, exits
set -uo pipefail

STAGE=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REAL_KUBECTL=$(command -v kubectl || true)
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
	img=$(grep -o 'ghcr.io/braveantony/koupa:v[0-9]*' "$FAKE/applied.yaml" | head -1)
	[ -f "$FAKE/deploy.json" ] && python3 -I -c '
import json, sys
d = json.load(open(sys.argv[1]))
d["spec"]["template"]["spec"]["containers"][0]["image"] = sys.argv[2]
d["status"]["updatedReplicas"] = 3
json.dump(d, open(sys.argv[1], "w"))' "$FAKE/deploy.json" "$img"
	exit 0 ;;
rollout) exit 0 ;;
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
		*" -o json "*) cat "$FAKE/deploy.json" ;;
		*" -o yaml "*) cat "$FAKE/deploy.json" ;;
		*) python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); s=d["status"]; print(d["metadata"]["generation"], s.get("observedGeneration",0), d["spec"]["replicas"], s.get("replicas",0), s.get("updatedReplicas",0), s.get("availableReplicas",0))' "$FAKE/deploy.json" ;;
		esac
		exit 0 ;;
	*" rs "* | *" replicasets "* | *" replicaset "*) cat "$FAKE/rs.json"; exit 0 ;;
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
image) echo "sha256:fakedigest" ;;
ps) cat "$FAKE/podman-ps" 2>/dev/null ;;
version | --version) echo "podman version fake" ;;
run)
	# podman run [opts] IMAGE ARGS...: run the fake rush-hour with ARGS.
	shift
	while [ $# -gt 0 ] && [ "$1" != "${RUSH_HOUR_IMAGE:-ghcr.io/braveantony/rush-hour:0.1.0}" ]; do shift; done
	shift
	exec "$FAKE/bin/rush-hour" "$@" ;;
esac
exit 0
EOF

	cat >"$FAKE/bin/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >>"$FAKE/calls.log"
echo '{"fake":true}'
EOF

	cat >"$FAKE/bin/collect-timeline" <<'EOF'
#!/usr/bin/env bash
# Fake collect-timeline.sh start|stop DIR
echo "collect-timeline $*" >>"$FAKE/calls.log"
dir=$2
mkdir -p "$dir/app"
case $1 in
start) echo '{"started_at":"2026-01-01T00:00:00Z"}' >"$dir/collect.json"; : >"$dir/ingress.log" ;;
stop)
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
	echo "SENDING_STARTED $(( $(date +%s%N) - back * 1000000000 ))"
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
coverage | timeline) ;;
esac
exit 0
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
    "facts": {"run": "@RUN@", "audit": {"cursor": 5, "orders": {}, "ready": []}},
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
	step-[1-4]) dir=$STAGE/k8s/steps/$overlay ;;
	*) dir=$STAGE/k8s/$overlay ;;
	esac
	"$REAL_KUBECTL" kustomize "$dir" >"$FAKE/rendered.yaml" || return 1
	python3 -I - "$FAKE" "$edit" <<'PY'
import json, sys, yaml
fake, edit = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(open(fake + "/rendered.yaml")) if d]
dep = [d for d in docs if d["kind"] == "Deployment"][0]
pod = dep["spec"]["template"]["spec"]
c = pod["containers"][0]
c["image"] = "ghcr.io/braveantony/koupa:v1"
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
        "spec": json.loads(json.dumps(pod)),
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
json.dump({"kind": "List", "items": [rs, old]}, open(fake + "/rs.json", "w"))
json.dump({"kind": "List", "items": pods}, open(fake + "/pods.json", "w"))
PY
}

# latest_run: the newest evidence/<run-id> directory.
latest_run() { ls -1dt "$MEASURE_EVIDENCE_DIR"/*/ 2>/dev/null | head -1 | sed 's:/$::'; }
