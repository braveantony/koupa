# Shared helpers of the critical-timing experiments (plan A12). Sourced by
# every NN-*.sh; not executable on its own.
#
# Environment:
#   CRIT_EVIDENCE   evidence root (default: evidence/critical-<ts>, set by run.sh)
#   CRIT_SUMMARY    file every PASS/FAIL line is appended to (run.sh)
#   CRIT_NO_RESTORE 1 = leave the Deployment as it is at exit (run.sh restores once)
#   CRIT_SKIP_DEPLOY 1 = do not apply the overlay (only to show the checks
#                   failing against the cluster as it is)
#   KOUPA_NS        namespace (default koupa)
# Image tags: experiments/critical/images.env (written by build-images.sh).

set -uo pipefail

crit_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
stage=$(cd "$crit_dir/../.." && pwd)
NS=${KOUPA_NS:-koupa}
# shellcheck source=/dev/null
. "$crit_dir/images.env"
CRIT_EVIDENCE=${CRIT_EVIDENCE:-$stage/evidence/critical-$(date -u +%Y%m%dT%H%M%SZ)}
CRIT_BIN=$stage/bin/crit
RH_BIN=$stage/bin/rush-hour
V1_IMAGE=ghcr.io/braveantony/koupa:v1
HOOK_NAMES=(order.before_commit order.after_commit request.mid sse.after_hello)
APP_SELECTOR=app.kubernetes.io/name=koupa
TEST_POD_SELECTOR=app.kubernetes.io/name=crit-slow-board
PULL_SECRET=ghcr-experiments

log() { printf '%s %s\n' "$(date -u +%H:%M:%S.%3N)" "$*" >&2; }
now_rfc() { date -u +%Y-%m-%dT%H:%M:%S.%NZ; }

# ------------------------------------------------------------ state, cleanup

HOOKS=""         # comma-separated hook control addresses (port-forwards)
BG_PIDS=()       # port-forwards and log followers
TEST_PODS=()     # Pods this script created (deleted at exit)

crit_cleanup() {
	local rc=$?
	trap - EXIT INT TERM
	if [ -n "$HOOKS" ] && [ -x "$CRIT_BIN" ]; then
		for h in "${HOOK_NAMES[@]}"; do
			timeout 15 "$CRIT_BIN" release "$h" --hooks "$HOOKS" >/dev/null 2>&1 || true
		done
	fi
	for p in "${TEST_PODS[@]}"; do
		timeout 30 kubectl -n "$NS" delete pod "$p" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	done
	for p in "${BG_PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null
	if [ "${CRIT_NO_RESTORE:-0}" != 1 ]; then
		restore_step4 || rc=1
	fi
	exit "$rc"
}
trap crit_cleanup EXIT
trap 'exit 130' INT TERM

# stop_bg: end port-forwards and followers of the previous setting.
stop_bg() {
	for p in "${BG_PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null
	BG_PIDS=() HOOKS=""
}

# ------------------------------------------------------------ build

build_tools() {
	mkdir -p "$stage/bin"
	(cd "$stage" && CGO_ENABLED=0 timeout 300 scripts/go build -o "$CRIT_BIN" ./experiments/critical/cmd/crit &&
		CGO_ENABLED=0 timeout 300 scripts/go build -o "$RH_BIN" ./rush-hour) >&2
}

# ------------------------------------------------------------ deployment

koupa_pods() {
	kubectl -n "$NS" get pods -l "$APP_SELECTOR" -o json | python3 -I -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    if p["status"].get("phase") == "Running" and not p["metadata"].get("deletionTimestamp"):
        print(p["metadata"]["name"])'
}

# settled: 3 Running koupa Pods, none terminating, all from the current
# ReplicaSet, every one has logged tx_timeout_check (started).
settled() {
	local want hash n=0 p
	hash=$(kubectl -n "$NS" get rs -l "$APP_SELECTOR" -o json | python3 -I -c '
import json, sys
d = json.load(sys.stdin)
rs = [r for r in d["items"] if r["spec"].get("replicas", 0) > 0]
print(rs[0]["metadata"]["labels"]["pod-template-hash"] if len(rs) == 1 else "")')
	[ -n "$hash" ] || return 1
	want=$(kubectl -n "$NS" get pods -l "$APP_SELECTOR" -o json | python3 -I -c '
import json, sys
d = json.load(sys.stdin)
print(len(d["items"]), sum(1 for p in d["items"] if p["status"].get("phase") == "Running"
      and not p["metadata"].get("deletionTimestamp") and p["metadata"]["labels"].get("pod-template-hash") == sys.argv[1]))' "$hash")
	[ "$want" = "3 3" ] || return 1
	for p in $(koupa_pods); do
		kubectl -n "$NS" logs "$p" 2>/dev/null | grep -c '"event":"tx_timeout_check"' >/dev/null || return 1
		n=$((n + 1))
	done
	[ "$n" = 3 ]
}

wait_settled() {
	local deadline=$(($(date +%s) + ${1:-240}))
	until settled; do
		[ "$(date +%s)" -lt "$deadline" ] || return 1
		sleep 2
	done
}

# apply_overlay DIR IMAGE OUT: render, set the koupa image, apply, wait.
apply_overlay() {
	local dir=$1 image=$2 out=$3
	kubectl kustomize "$dir" >"$out.rendered" || return 1
	sed "s#ghcr.io/braveantony/koupa:v[0-9][0-9]*#$image#" "$out.rendered" >"$out"
	rm -f "$out.rendered"
	timeout 60 kubectl apply -f "$out" >&2 || return 1
	timeout 300 kubectl -n "$NS" rollout status deploy/koupa --timeout=280s >&2 || return 1
	wait_settled 240
}

current_overlay=""
# deploy NAME: experiments/critical/overlays/NAME with the experiment image.
# Skipped when this process already deployed it and the Pods are settled.
deploy() {
	local name=$1 edir=$2
	if [ "${CRIT_SKIP_DEPLOY:-0}" = 1 ]; then
		log "CRIT_SKIP_DEPLOY=1: using the cluster as it is"
		return 0
	fi
	local mark=$CRIT_EVIDENCE/.deployed
	if [ "$(cat "$mark" 2>/dev/null)" = "$name $CRIT_APP_IMAGE" ] && settled; then
		log "overlay $name already applied"
		return 0
	fi
	log "applying overlay $name ($CRIT_APP_IMAGE)"
	rm -f "$mark"
	pull_secret_up || return 1
	apply_overlay "$crit_dir/overlays/$name" "$CRIT_APP_IMAGE" "$edir/manifest.yaml" || { log "apply $name failed"; return 1; }
	echo "$name $CRIT_APP_IMAGE" >"$mark"
}

# restore_step4: what measure.sh applies for step 4 (image v1), and no
# TEST_HOOKS anywhere in the Deployment or its Pods.
restore_step4() {
	log "restoring step-4 (image $V1_IMAGE, no TEST_HOOKS)"
	mkdir -p "$CRIT_EVIDENCE"
	rm -f "$CRIT_EVIDENCE/.deployed"
	apply_overlay "$stage/k8s/steps/step-4" "$V1_IMAGE" "$CRIT_EVIDENCE/restore-manifest.yaml" || { log "restore failed"; return 1; }
	if kubectl -n "$NS" get deploy koupa -o json | grep -c 'TEST_HOOKS\|TEST_MUTATION\|TEST_SSE\|ghcr-experiments\|koupa-experiments' >/dev/null ||
		kubectl -n "$NS" get pods -l "$APP_SELECTOR" -o json | grep -c 'TEST_HOOKS\|TEST_MUTATION\|TEST_SSE' >/dev/null; then
		log "restore: test settings still present"
		return 1
	fi
	timeout 60 kubectl -n "$NS" delete pod -l "$TEST_POD_SELECTOR" --ignore-not-found --wait=false >&2 || return 1
	pull_secret_down || return 1
	log "restored: step-4, $V1_IMAGE, no test settings, no test Pods, no $PULL_SECRET secret"
}

# ------------------------------------------------------------ pull secret

# The experiment images are pulled by the kubelet (imagePullPolicy Always
# in the overlays) through the Secret $PULL_SECRET, referenced only by the
# experiment overlays and the slow-board Pod. The credential goes from the
# host's podman auth file to kubectl through a pipe, never through a
# command line or a file. When the images can be pulled anonymously (the
# package was made public), no Secret is created.
images_public() {
	local img
	for img in "$CRIT_APP_IMAGE" "$CRIT_SLOW_BOARD_IMAGE"; do
		timeout 60 skopeo inspect --no-creds --format '{{.Digest}}' "docker://$img" >/dev/null 2>&1 || return 1
	done
}

pull_secret_up() {
	if images_public; then
		log "experiment images are public: no pull secret"
		pull_secret_down
		return
	fi
	python3 -I - "$NS" "$PULL_SECRET" "${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/containers/auth.json" <<'PY' |
import base64, json, sys
ns, name, path = sys.argv[1:]
auth = json.load(open(path))["auths"]["ghcr.io"]["auth"]
cfg = json.dumps({"auths": {"ghcr.io": {"auth": auth}}}).encode()
print(json.dumps({"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
    "metadata": {"name": name, "namespace": ns, "labels": {"app.kubernetes.io/name": "crit-experiments"}},
    "data": {".dockerconfigjson": base64.b64encode(cfg).decode()}}))
PY
		timeout 30 kubectl apply -f - >/dev/null || { log "cannot create pull secret $PULL_SECRET"; return 1; }
	log "pull secret $PULL_SECRET created (images are private)"
}

pull_secret_down() {
	timeout 30 kubectl -n "$NS" delete secret "$PULL_SECRET" --ignore-not-found >/dev/null
}

# ------------------------------------------------------------ port-forwards, logs

# pf POD REMOTE_PORT: start a port-forward; sets PF_PORT. Not to be called
# in $(...): the port-forward PID must reach BG_PIDS of this shell.
pf() {
	local pod=$1 rport=$2 f lp="" i
	f=$(mktemp)
	kubectl -n "$NS" port-forward "pod/$pod" ":$rport" >"$f" 2>&1 &
	BG_PIDS+=($!)
	for i in $(seq 1 100); do
		lp=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$f" | head -1)
		[ -n "$lp" ] && break
		sleep 0.1
	done
	rm -f "$f"
	PF_PORT=$lp
	[ -n "$lp" ]
}

# hooks_up: a port-forward to 9090 of every koupa Pod; sets HOOKS.
hooks_up() {
	local p lp addrs=()
	for p in $(koupa_pods); do
		pf "$p" 9090 || { log "port-forward 9090 to $p failed"; return 1; }
		lp=$PF_PORT
		# The control API answers a zero wait with 408.
		local code="" i
		for i in $(seq 1 50); do
			code=$(curl -s -o /dev/null -w '%{http_code}' -m 2 "http://127.0.0.1:$lp/hooks/request.mid/wait?timeout=0s")
			[ "$code" = 408 ] && break
			sleep 0.2
		done
		[ "$code" = 408 ] || { log "hook control on $p not answering (HTTP $code)"; return 1; }
		addrs+=("127.0.0.1:$lp")
	done
	[ "${#addrs[@]}" = 3 ] || { log "expected 3 koupa Pods with hooks, got ${#addrs[@]}"; return 1; }
	HOOKS=$(IFS=,; echo "${addrs[*]}")
}

# follow_logs DIR: kubectl logs -f per koupa Pod into DIR/<pod>.log.
follow_logs() {
	local dir=$1 p
	mkdir -p "$dir"
	for p in $(koupa_pods); do
		kubectl -n "$NS" logs -f --timestamps "$p" >"$dir/$p.log" 2>"$dir/$p.err" &
		BG_PIDS+=($!)
	done
	sleep 1
}

gateway_base() {
	local ip
	ip=$(kubectl -n "$NS" get gateway koupa -o jsonpath='{.status.addresses[0].value}')
	[ -n "$ip" ] && echo "http://$ip"
}

# ------------------------------------------------------------ verdicts

# setting_dir EXP SETTING: create and print the evidence directory.
setting_dir() {
	local d=$CRIT_EVIDENCE/$1/$2
	mkdir -p "$d"
	echo "$d"
}

# check DIR: run the python expectations on stdin with helpers:
#   R               result.json of the setting (crit's output; {} if none)
#   lines(pod)      the Pod's followed log as a list of dicts
#   all_lines()     every followed log line, with "_pod"
#   ok(cond, text)  record one expectation
# Writes DIR/checks.txt; exit 0 only if every expectation holds.
check() {
	local dir=$1
	{
		cat <<'PY'
import glob, json, os, sys
D = sys.argv[1]
try:
    R = json.load(open(os.path.join(D, "result.json")))
except Exception as e:
    R = {}
    pass
def lines(pod):
    out = []
    try:
        for l in open(os.path.join(D, "logs", pod + ".log"), errors="replace"):
            i = l.find("{")
            if i < 0:
                continue
            try:
                m = json.loads(l[i:])
            except ValueError:
                continue
            m["_pod"] = pod
            out.append(m)
    except OSError:
        pass
    return out
def all_lines():
    out = []
    for f in sorted(glob.glob(os.path.join(D, "logs", "*.log"))):
        out += lines(os.path.basename(f)[:-4])
    return out
FAILED = []
def ok(cond, text):
    print(("ok   " if cond else "FAIL ") + text)
    if not cond:
        FAILED.append(text)
_of = os.path.join(D, "orchestration.failed")
ok(not os.path.exists(_of), "every orchestration step completed in time" +
   (": " + open(_of).read().strip().replace("\n", "; ") if os.path.exists(_of) else ""))
import datetime, re
def T(s):
    """RFC 3339 (any offset, up to nanoseconds) -> aware datetime; None if empty."""
    if not s:
        return None
    m = re.match(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$", s.strip())
    if not m:
        return None
    frac = (m.group(2) or "0")[:6].ljust(6, "0")
    tz = "+00:00" if m.group(3) == "Z" else m.group(3)
    return datetime.datetime.fromisoformat(m.group(1) + "." + frac + tz)
def get(d, *path):
    for p in path:
        if not isinstance(d, dict):
            return None
        d = d.get(p)
    return d
PY
		echo 'try:'
		sed 's/^/    /'
		echo 'except Exception as e:'
		echo '    ok(False, "expectations could not be evaluated: %r" % e)'
		echo 'sys.exit(1 if FAILED else 0)'
	} >"$dir/checks.py"
	python3 -I "$dir/checks.py" "$dir" >"$dir/checks.txt" 2>&1
	local rc=$?
	sed 's/^/    /' "$dir/checks.txt" >&2
	return $rc
}

# verdict EXP SETTING DIR RC: print and record PASS/FAIL.
verdict() {
	local line
	if [ "$4" = 0 ]; then line="PASS $1 $2 $3"; else line="FAIL $1 $2 $3"; fi
	echo "$line"
	echo "$line" >"$3/verdict.txt"
	[ -z "${CRIT_SUMMARY:-}" ] || echo "$line" >>"$CRIT_SUMMARY"
	[ "$4" = 0 ]
}

# run_crit DIR TIMEOUT ARGS...: crit with a hard timeout; result.json and
# crit.log (its step log) in DIR.
run_crit() {
	local dir=$1 t=$2
	shift 2
	timeout "$t" "$CRIT_BIN" "$@" >"$dir/result.json" 2>"$dir/crit.log"
	local rc=$?
	sed 's/^/    | /' "$dir/crit.log" >&2
	if [ "$rc" != 0 ]; then
		orch_fail "$dir" "crit $1 exited $rc (124 = timeout)"
		return 1
	fi
}

# orch_fail DIR WHY: an orchestration step failed or timed out; check()
# then fails the setting whatever the expectations say.
orch_fail() {
	log "orchestration failed: $2"
	echo "$2" >>"$1/orchestration.failed"
}

mkdir -p "$CRIT_EVIDENCE"

# wait_grep FILE ERE SECONDS: wait until FILE has a line matching ERE.
wait_grep() {
	local deadline=$(($(date +%s) + $3))
	until grep -qE "$2" "$1" 2>/dev/null; do
		[ "$(date +%s)" -lt "$deadline" ] || { log "timeout (${3}s) waiting for /$2/ in $(basename "$1")"; return 1; }
		sleep 0.1
	done
}
