#!/usr/bin/env bash
# Koupa measurement (plan A8, A13; design 5.1, 5.4).
#
#   ./measure.sh <mode> [--runs 5] [--duration 60] [--rate 20] [--verbose] [--diagnostic]
#   ./measure.sh all [--runs 5] [--dry-run] [...]
#
# mode: step-0 .. step-5, control, calibrate, rollback, drain, scale-down, all.
#
# One run, every time from A8 step 1 (plan A8):
#   1. render the step's overlay, set the image to v1, kubectl apply
#   2. wait for the rollout and for old Pods to be gone, then
#      scripts/preflight.sh (seven checks, incl. every Pod's image digest =
#      what the v1 tag resolves to now); any mismatch stops: no measurement
#   3. start the timeline collectors (scripts/collect-timeline.sh), read the
#      Gateway IP
#   4. start rush-hour (podman, host network); it sends once the 3 boards
#      got hello and prints SENDING_STARTED <unix-nano>
#   5. at SENDING_STARTED + 10 s: set image v2 (control: nothing). The
#      rollout must be complete at D - 10 s, otherwise the run is invalid
#   6. at D rush-hour winds down by itself (A9) and writes its scorecard
#   7. stop the collectors, complete the evidence pack (scripts/evidence.sh)
#      in evidence/<run-id>/
#   8. label the scorecard's preStop: calibrated, 「preStop 未經校正」 or
#      unknown, from the koupa.dev/prestop-calibrated annotation of the
#      Deployment read after preflight (deployment-at-start.json); runs
#      without preStop get no label
#
# Other modes (plan A8 table, A11, A15):
#   calibrate   k8s/calibrate (step 0 + preStop 20 s, grace 45 s); a
#               `rush-hour probe` (50/s, new connection each) runs beside the
#               load from before sending until the old Pods are gone; after
#               each run `rush-hour calibrate` estimates every terminated Pod
#               (calib.Estimate). Runs repeat until 5 valid samples or
#               --runs; samples and candidate in evidence/calibrate-<time>/.
#   rollback    step-5 settings applied with v2; the change sets v1.
#   drain       step-5; before the run the Pods must be spread one per worker
#               and scripts/drain-target.sh picks the worker (A15 checks, or
#               the run stops); the change is `kubectl drain W
#               --ignore-daemonsets` (never --delete-emptydir-data), complete
#               when it returns before D - 10 s. Afterwards: uncordon, the
#               node must be schedulable, 3 available Pods, then rebalance
#               (delete a Pod on a crowded worker, re-check; at most
#               MEASURE_REBALANCE_ATTEMPTS=5 times, else stop and report).
#   scale-down  step-5; the change scales to 2 replicas; afterwards back to 3,
#               all available.
#   all         rounds (--runs, default 5) of control, step-0 .. step-5, one
#               run each (measure.sh MODE --runs 1); --dry-run prints the
#               sequence only. The step-0 gate is judged over all rounds.
#
# Host-side rush-hour (timeline collector, calibrate analysis): built from
# the working tree at start (scripts/go build -o bin/rush-hour); its sha256
# and the git commit (+ dirty flag) go into every run's tooling.json. A
# failed build stops measure.sh: no stale binary is ever used.
#
# A13 gate: control must be 0 failures, 0 missed calls, 0 duplicates with no
# unknown and not invalid; step-0 must observe at least one predefined
# problem over its runs. Otherwise exit 3, unless --diagnostic: then the
# runs go on and every scorecard of this invocation starts with
# 「診斷模式：對照組不成立，不可用於比較」. With --diagnostic on step-1..5 (a
# comparison continued after a failed control) every scorecard gets it too.
#
# Interruption (EXIT, INT, TERM): rush-hour, the probe and the collectors
# are stopped, a drained worker is uncordoned and a scaled Deployment set
# back to 3 replicas (both confirmed), the run is marked invalid.
#
# Exit: 0 ok; 1 a step failed or timed out (run marked invalid, stopped), or
# the cluster could not be restored; 2 at least one run invalid, or
# calibrate ended with fewer than 5 valid samples; 3 the A13 gate failed;
# 64 usage.
#
# Environment: RUSH_HOUR_IMAGE (default ghcr.io/braveantony/rush-hour:0.1.0);
# RUSH_HOUR_BIN=<path> runs a local rush-hour binary instead of podman (tests,
# development); MEASURE_EVIDENCE_DIR (default ./evidence); COLLECT_TIMELINE
# (default scripts/collect-timeline.sh); KUBECTL; KOUPA_NS;
# MEASURE_HOST_RUSH_HOUR (default ./bin/rush-hour) and MEASURE_GO (default
# scripts/go): where the host-side rush-hour is built and with what.
set -uo pipefail
stage=$(cd "$(dirname "$0")" && pwd)
NS=${KOUPA_NS:-koupa}
KUBECTL=${KUBECTL:-kubectl}
EVIDENCE=${MEASURE_EVIDENCE_DIR:-$stage/evidence}
COLLECT=${COLLECT_TIMELINE:-$stage/scripts/collect-timeline.sh}
RUSH_HOUR_IMAGE=${RUSH_HOUR_IMAGE:-ghcr.io/braveantony/rush-hour:0.1.0}
IMG_V1=ghcr.io/braveantony/koupa:v1
IMG_V2=ghcr.io/braveantony/koupa:v2
BANNER='診斷模式：對照組不成立，不可用於比較'
CHANGE_AT_S=10 # the change is triggered at SENDING_STARTED + 10 s
ROLLOUT_MARGIN_S=10 # the rollout must be complete at D - 10 s
APPLY_ROLLOUT_TIMEOUT_S=300
OLD_PODS_TIMEOUT_S=180
SENDING_TIMEOUT_S=90
APP_START_TIMEOUT_S=${MEASURE_APP_START_TIMEOUT_S:-60}
RESTORE_TIMEOUT_S=${MEASURE_RESTORE_TIMEOUT_S:-240}
REBALANCE_ATTEMPTS=${MEASURE_REBALANCE_ATTEMPTS:-5}
CALIB_MIN_VALID=5 # plan A11 step 6
PRESTOP_BUFFER_S=5 # provisional buffer on top of the calibrated candidate (step 2)
HOST_RH=${MEASURE_HOST_RUSH_HOUR:-$stage/bin/rush-hour}
GO=${MEASURE_GO:-$stage/scripts/go}

usage() {
	sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 64
}
log() { printf '[measure %s] %s\n' "$(date +%H:%M:%S)" "$*"; }
warn() { printf '[measure %s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
now_ns() { date +%s%N; }
rfc() { date -u -d "@$(($1 / 1000000000)).$(printf '%09d' $(($1 % 1000000000)))" +%Y-%m-%dT%H:%M:%S.%NZ; }

# bg_wait CMD...: run CMD in the background and wait for it, so a TERM or
# INT is handled at once instead of after CMD ends (bash runs traps only
# between foreground commands). Returns CMD's status.
fg_pid=""
bg_wait() {
	"$@" &
	fg_pid=$!
	local rc=0
	wait "$fg_pid" || rc=$?
	fg_pid=""
	return "$rc"
}

# sleep_until NS: sleep until the Unix time NS (nanoseconds); no-op if past.
sleep_until() {
	local d=$(($1 - $(now_ns)))
	[ "$d" -gt 0 ] && bg_wait sleep "$(awk -v d="$d" 'BEGIN{printf "%.3f", d/1e9}')"
	return 0
}

# ---------------------------------------------------------------- arguments
mode=${1:-}
[ $# -ge 1 ] && shift
runs=5 duration=60 rate=20 verbose=0 diagnostic=0 dry_run=0
while [ $# -gt 0 ]; do
	case $1 in
	--runs) [ $# -ge 2 ] || usage; runs=$2; shift 2 ;;
	--duration) [ $# -ge 2 ] || usage; duration=${2%s}; shift 2 ;;
	--rate) [ $# -ge 2 ] || usage; rate=$2; shift 2 ;;
	--verbose) verbose=1; shift ;;
	--diagnostic) diagnostic=1; shift ;;
	--dry-run) dry_run=1; shift ;;
	-h | --help) usage ;;
	*) warn "unknown argument: $1"; usage ;;
	esac
done
# overlay: the settings applied (also preflight's step); change: what A8 step 5
# does (image: set image img_after; none; drain; scale).
img_before=$IMG_V1 img_after=$IMG_V2 change=image overlay=$mode
case $mode in
step-0) overlay_dir=$stage/k8s/base ;;
control) overlay_dir=$stage/k8s/base img_after="" change=none ;;
step-[1-5]) overlay_dir=$stage/k8s/steps/$mode ;;
calibrate) overlay_dir=$stage/k8s/calibrate ;;
rollback) overlay=step-5 overlay_dir=$stage/k8s/steps/step-5 img_before=$IMG_V2 img_after=$IMG_V1 ;;
drain) overlay=step-5 overlay_dir=$stage/k8s/steps/step-5 img_after="" change=drain ;;
scale-down) overlay=step-5 overlay_dir=$stage/k8s/steps/step-5 img_after="" change=scale ;;
all) overlay="" overlay_dir="" ;;
*) usage ;;
esac
[ "$dry_run" = 0 ] || [ "$mode" = all ] || { warn "--dry-run is only for all"; exit 64; }
[[ $runs =~ ^[1-9][0-9]*$ ]] || { warn "--runs must be a positive integer"; exit 64; }
[[ $duration =~ ^[0-9]+$ ]] && [ "$duration" -ge 30 ] || { warn "--duration must be whole seconds, at least 30 (plan A8)"; exit 64; }
[[ $rate =~ ^[0-9]+([.][0-9]+)?$ ]] || { warn "--rate must be a positive number"; exit 64; }

ALL_SEQUENCE=(control step-0 step-1 step-2 step-3 step-4 step-5)
all_opts=(--duration "$duration" --rate "$rate")
[ "$verbose" = 1 ] && all_opts+=(--verbose)
[ "$diagnostic" = 1 ] && all_opts+=(--diagnostic)
if [ "$mode" = all ] && [ "$dry_run" = 1 ]; then
	echo "all: $runs round(s) of ${ALL_SEQUENCE[*]} (plan A8), one run each:"
	for r in $(seq 1 "$runs"); do
		for m in "${ALL_SEQUENCE[@]}"; do echo "round $r: measure.sh $m --runs 1 ${all_opts[*]}"; done
	done
	exit 0
fi

if [ -n "${RUSH_HOUR_BIN:-}" ]; then
	rh_kind=bin rh_ref=$RUSH_HOUR_BIN
else
	rh_kind=image rh_ref=$RUSH_HOUR_IMAGE
	command -v podman >/dev/null || { warn "podman not found (or set RUSH_HOUR_BIN)"; exit 1; }
fi
mkdir -p "$EVIDENCE"
EVIDENCE=$(realpath -- "$EVIDENCE")

# Leftovers of an earlier, killed invocation would disturb this one (two
# load generators, collectors writing into an old run): refuse to start.
leftover=""
if [ "$rh_kind" = image ]; then
	leftover=$(podman ps -a --filter 'name=^rush-hour-' --format '{{.Names}}' 2>/dev/null | paste -sd' ' -)
	[ -z "$leftover" ] || leftover="rush-hour container(s): $leftover (podman rm -f <name>)"
fi
if pids=$(pgrep -f 'collect-timeline\.sh _child' 2>/dev/null); then
	leftover="${leftover:+$leftover; }timeline collectors still running: pid $(echo "$pids" | paste -sd' ' -) (scripts/collect-timeline.sh stop <run-dir>)"
fi
if [ -n "$leftover" ]; then
	warn "leftovers of an earlier measurement: $leftover"
	exit 1
fi

# ---------------------------------------------------------------- host rush-hour
# Build the host-side rush-hour from the working tree, so the collectors
# and the calibrate analysis never run a stale binary; record what ran.
# Under `all` the parent builds once and the children reuse its record
# (MEASURE_TOOLING_FILE) after checking the binary's sha256.
sha_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }
tooling="" own_tooling=0 all_child=""
if [ -n "${MEASURE_TOOLING_FILE:-}" ] && [ -f "$MEASURE_TOOLING_FILE" ] &&
	[ "$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["host_rush_hour"]["sha256"])' "$MEASURE_TOOLING_FILE" 2>/dev/null)" = "$(sha_of "$HOST_RH")" ]; then
	tooling=$MEASURE_TOOLING_FILE
else
	mkdir -p "$(dirname "$HOST_RH")"
	build_log=$EVIDENCE/.build-rush-hour-$$.log
	log "building the host-side rush-hour from the working tree ($HOST_RH)"
	if ! (cd "$stage" && CGO_ENABLED=0 "$GO" build -o "$HOST_RH" ./rush-hour) >"$build_log" 2>&1; then
		sed 's/^/  /' "$build_log" | tail -20 >&2
		warn "cannot build the host-side rush-hour (go build ./rush-hour failed, see $build_log): not measuring with a stale binary"
		exit 1
	fi
	rm -f "$build_log"
	tooling=$EVIDENCE/.tooling-$$.json own_tooling=1
	head=$(git -C "$stage" rev-parse HEAD 2>/dev/null || echo unknown)
	dirty_n=$(git -C "$stage" status --porcelain 2>/dev/null | wc -l)
	python3 -I - "$tooling" "$HOST_RH" "$(sha_of "$HOST_RH")" "$head" "$dirty_n" "$GO" <<'PY'
import datetime, json, sys
out, path, sha, head, dirty_n, go = sys.argv[1:]
json.dump({"host_rush_hour": {"path": path, "sha256": sha,
                              "built_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                              "build": "cd 01 && CGO_ENABLED=0 %s build -o %s ./rush-hour" % (go, path)},
           "git": {"head": head, "dirty": int(dirty_n) > 0, "dirty_files": int(dirty_n)}},
          open(out, "w"), indent=2)
PY
fi
# collect-timeline.sh uses this binary and does not build its own.
export RUSH_HOUR=$HOST_RH

# rush_hour [--exec] [--name NAME] MOUNT_DIR ARGS...: run rush-hour (the
# identified artifact: RUSH_HOUR_BIN, or the image by its recorded ID) with
# MOUNT_DIR visible at the same path. --exec replaces the (background)
# subshell, so $! is the rush-hour (or podman) process itself and a signal
# to it reaches it. NAME is the container name (default rush-hour-<run>).
rush_hour() {
	local ex="" name="rush-hour-$cur_id"
	[ "$1" = --exec ] && { ex=exec; shift; }
	[ "$1" = --name ] && { name=$2; shift 2; }
	local mount=$1
	shift
	if [ "$rh_kind" = bin ]; then
		$ex "$RUSH_HOUR_BIN" "$@"
	else
		$ex podman run --rm --name "$name" --network host --userns=keep-id --user "$(id -u):$(id -g)" \
			-v "$mount:$mount:Z" "${rh_image_id:-$RUSH_HOUR_IMAGE}" "$@"
	fi
}

# ---------------------------------------------------------------- scorecards
# scorecard_py DIR ACTION [ARG]: the scorecard edits and A13 checks.
scorecard_py() { python3 -I "$stage/scripts/lib/scorecard.py" "$@"; }

. "$stage/scripts/lib/kube.sh"

# ---------------------------------------------------------------- one run
cur_id="" cur_dir="" rh_pid="" cov_pid="" collecting=0 rh_image_id=""
probe_pid="" drain_pid="" drained="" scaled=0 drain_target="" restore_failed=""

write_params() {
	python3 -I - "$cur_dir/params.json" <<PY
import json, sys
json.dump({
    "run_id": "$cur_id", "mode": "$mode", "overlay": "$overlay", "run_number": $1, "runs": $runs,
    "image_before": "$img_before", "image_after": "$img_after" or None,
    "change": "$change", "drain_target": "${drain_target:-}" or None, "probe": "$mode" == "calibrate",
    "duration_s": $duration, "change_at_s": $CHANGE_AT_S, "rollout_deadline_s": $((duration - ROLLOUT_MARGIN_S)),
    "rate": $rate, "boards": 3, "retries": 1, "tx_timeout": "60s", "verbose": bool($verbose),
    "diagnostic": bool($diagnostic), "base_url": "${base_url:-}", "namespace": "$NS",
    "rush_hour": {"kind": "$rh_kind", "ref": "$rh_ref", "image_id": "${rh_image_id:-}" or None},
    "started_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)", "host": "$(hostname)",
}, open(sys.argv[1], "w"), indent=2)
PY
}

# stop_rush_hour: end rush-hour now. With podman the container is removed
# (podman rm -f), whether or not the podman client is still attached.
stop_rush_hour() {
	if [ "$rh_kind" = image ] && [ -n "$cur_id" ]; then
		podman rm -f -t 5 "rush-hour-$cur_id" >/dev/null 2>&1
	fi
	[ -n "$rh_pid" ] || return 0
	if kill -0 "$rh_pid" 2>/dev/null; then
		kill -TERM "$rh_pid" 2>/dev/null
		local i
		for i in $(seq 1 50); do
			kill -0 "$rh_pid" 2>/dev/null || break
			sleep 0.1
		done
		kill -KILL "$rh_pid" 2>/dev/null
	fi
	wait "$rh_pid" 2>/dev/null
	rh_pid=""
}

stop_coverage() {
	[ -n "$cov_pid" ] || return 0
	kill "$cov_pid" 2>/dev/null
	wait "$cov_pid" 2>/dev/null
	cov_pid=""
	[ "$rh_kind" = image ] && podman rm -f -t 2 "rush-hour-$cur_id-cov" >/dev/null 2>&1
	local n
	n=$(grep -c 'rush-hour coverage failed' "$cur_dir/coverage.err" 2>/dev/null)
	if [ "${n:-0}" -gt 0 ]; then
		warn "rush-hour coverage failed $n time(s) during the run (see $cur_dir/coverage.err)"
		echo "rush-hour coverage failed $n time(s), see coverage.err" >>"$cur_dir/measure.log"
	fi
	return 0
}

stop_probe() {
	[ -n "$probe_pid" ] || return 0
	if kill -0 "$probe_pid" 2>/dev/null; then
		kill -TERM "$probe_pid" 2>/dev/null
		local i
		for i in $(seq 1 100); do
			kill -0 "$probe_pid" 2>/dev/null || break
			sleep 0.1
		done
		kill -KILL "$probe_pid" 2>/dev/null
	fi
	wait "$probe_pid" 2>/dev/null
	probe_pid=""
	[ "$rh_kind" = image ] && podman rm -f -t 5 "rush-hour-$cur_id-probe" >/dev/null 2>&1
	return 0
}

stop_drain_cmd() {
	[ -n "$drain_pid" ] || return 0
	kill -TERM "$drain_pid" 2>/dev/null
	wait "$drain_pid" 2>/dev/null
	drain_pid=""
}

# distribution OUT: writes {"workers": {node: n}, "balanced", "crowded",
# "unschedulable"} for the current koupa Pods (not terminating) against the
# workers (no control-plane role); balanced = every worker has exactly one
# and their number is spec.replicas.
distribution() {
	local out=$1 tmp
	tmp=$(mktemp -d)
	"$KUBECTL" get nodes -o json >"$tmp/nodes.json" 2>/dev/null
	"$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name=koupa -o json >"$tmp/pods.json" 2>/dev/null
	"$KUBECTL" -n "$NS" get deploy koupa -o json >"$tmp/deploy.json" 2>/dev/null
	python3 -I - "$tmp" "$out" <<'PY'
import json, os, sys
tmp, out = sys.argv[1:]
def load(n):
    try:
        return json.load(open(os.path.join(tmp, n)))
    except (OSError, ValueError):
        return None
nodes, pods, dep = load("nodes.json"), load("pods.json"), load("deploy.json")
res = {"workers": {}, "balanced": False, "crowded": None, "unschedulable": [], "pending": 0, "error": None}
if not (nodes and pods and dep):
    res["error"] = "cannot read nodes, Pods or the Deployment"
else:
    ws = sorted(n["metadata"]["name"] for n in nodes["items"]
                if "node-role.kubernetes.io/control-plane" not in n["metadata"].get("labels", {}))
    res["unschedulable"] = sorted(n["metadata"]["name"] for n in nodes["items"] if n.get("spec", {}).get("unschedulable"))
    c = {w: [] for w in ws}
    for p in pods["items"]:
        if p["metadata"].get("deletionTimestamp"):
            continue
        n = p.get("spec", {}).get("nodeName")
        if not n:
            res["pending"] += 1
            continue
        c.setdefault(n, []).append(p["metadata"]["name"])
    res["workers"] = {w: len(v) for w, v in sorted(c.items())}
    want = dep["spec"].get("replicas", 1)
    res["balanced"] = (len(ws) == want and all(len(c.get(w, [])) == 1 for w in ws)
                       and sum(len(v) for v in c.values()) == want and not res["pending"])
    crowded = [sorted(v)[-1] for w, v in sorted(c.items()) if len(v) > 1]
    res["crowded"] = crowded[0] if crowded else None
json.dump(res, open(out, "w"), indent=2)
sys.exit(0 if res["balanced"] else 1)
PY
	local rc=$?
	rm -rf "$tmp"
	return "$rc"
}

# wait_available N: the Deployment is done with N available Pods and no
# koupa Pod is terminating, within RESTORE_TIMEOUT_S.
wait_available() {
	local n=$1 deadline=$(($(now_ns) + RESTORE_TIMEOUT_S * 1000000000))
	while :; do
		if [ "$(deploy_status)" = done ] &&
			"$KUBECTL" -n "$NS" get deploy koupa -o json 2>/dev/null | python3 -I -c '
import json, sys
d = json.load(sys.stdin)
sys.exit(0 if d["spec"].get("replicas") == int(sys.argv[1]) and d.get("status", {}).get("availableReplicas") == int(sys.argv[1]) else 1)' "$n" &&
			old_pods_gone; then
			return 0
		fi
		[ "$(now_ns)" -lt "$deadline" ] || return 1
		sleep 1
	done
}

# rebalance OUT: delete a Pod on a crowded worker until the Pods are spread
# one per worker (A15: never assume one delete is enough); at most
# REBALANCE_ATTEMPTS deletions. OUT gets the last distribution. 0 balanced.
rebalance_attempts=0
rebalance() {
	local out=$1 i pod
	rebalance_attempts=0
	for i in $(seq 0 "$REBALANCE_ATTEMPTS"); do
		distribution "$out" && return 0
		[ "$i" -lt "$REBALANCE_ATTEMPTS" ] || break
		pod=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["crowded"] or "")' "$out")
		if [ -z "$pod" ]; then
			# Not crowded but not balanced (a Pod pending, a worker cordoned):
			# wait for the Deployment instead of deleting.
			wait_available 3 || true
			continue
		fi
		rebalance_attempts=$((rebalance_attempts + 1))
		log "rebalance: deleting $pod (on a worker with more than one koupa Pod)"
		bg_wait "$KUBECTL" -n "$NS" delete pod "$pod" >>"$cur_dir/measure.log" 2>&1
		wait_available 3 || true
	done
	return 1
}

# restore_cluster: undo the change of a drain or scale-down run and confirm
# it (A15): uncordon + node schedulable + 3 available (+ rebalance when
# REBALANCE=1), or scale back to 3 + 3 available. Writes restore.json;
# returns 1 when the cluster is not back.
# node_unschedulable NODE: 0 when NODE is cordoned (or cannot be read).
node_unschedulable() {
	"$KUBECTL" get nodes -o json 2>/dev/null | python3 -I -c '
import json, sys
try:
    items = json.load(sys.stdin)["items"]
except (ValueError, KeyError):
    sys.exit(0)
n = [x for x in items if x["metadata"]["name"] == sys.argv[1]]
sys.exit(0 if not n or n[0].get("spec", {}).get("unschedulable") else 1)' "$1"
}
deploy_replicas() {
	"$KUBECTL" -n "$NS" get deploy koupa -o json 2>/dev/null | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["spec"].get("replicas"))' 2>/dev/null
}

# retry_restore WHAT CHECK CMD...: run CMD until it succeeds and CHECK
# (a command) confirms, RESTORE_RETRIES times at most. 0 restored.
RESTORE_RETRIES=${MEASURE_RESTORE_RETRIES:-3}
RESTORE_RETRY_SLEEP_S=${MEASURE_RESTORE_RETRY_SLEEP_S:-3}
retry_restore() {
	local what=$1 check=$2 i
	shift 2
	for i in $(seq 1 "$RESTORE_RETRIES"); do
		if "$@" >>"$cur_dir/measure.log" 2>&1 && eval "$check"; then
			return 0
		fi
		warn "$what failed (attempt $i/$RESTORE_RETRIES)"
		[ "$i" -lt "$RESTORE_RETRIES" ] && sleep "$RESTORE_RETRY_SLEEP_S"
	done
	return 1
}

restore_cluster() {
	local rebal=${1:-0} actions=() pending=() ok=1 why="" node=$drained
	[ -n "$drained" ] || [ "$scaled" = 1 ] || return 0
	if [ -n "$drained" ]; then
		stop_drain_cmd
		log "uncordon $drained"
		# drained stays set until the node is confirmed schedulable, so the
		# EXIT trap tries again.
		if retry_restore "kubectl uncordon $drained" '! node_unschedulable "$drained"' "$KUBECTL" uncordon "$drained"; then
			actions+=("uncordon $drained")
			drained=""
		else
			ok=0 why="kubectl uncordon $drained 失敗（試了 $RESTORE_RETRIES 次），$drained 仍然不可排程"
			pending+=("uncordon $drained")
		fi
	fi
	if [ "$scaled" = 1 ]; then
		log "scale deploy/koupa back to 3 replicas"
		if retry_restore "kubectl scale --replicas=3" '[ "$(deploy_replicas)" = 3 ]' "$KUBECTL" -n "$NS" scale deploy/koupa --replicas=3; then
			actions+=("scale deploy/koupa --replicas=3")
			scaled=0
		else
			ok=0 why="${why:+$why；}kubectl scale deploy/koupa --replicas=3 失敗（試了 $RESTORE_RETRIES 次）"
			pending+=("scale deploy/koupa --replicas=3")
		fi
	fi
	if [ "$ok" = 1 ] && ! wait_available 3; then
		ok=0 why="Deployment 在 ${RESTORE_TIMEOUT_S} 秒內沒有回到 3 個可用 Pod（$(deploy_status)）"
	fi
	distribution "$cur_dir/.dist.json"
	if [ "$ok" = 1 ] && [ -n "$node" ] && python3 -I -c 'import json,sys; sys.exit(0 if sys.argv[2] in json.load(open(sys.argv[1]))["unschedulable"] else 1)' "$cur_dir/.dist.json" "$node"; then
		ok=0 why="$node 仍然不可排程"
	fi
	if [ "$ok" = 1 ] && [ "$rebal" = 1 ] && ! rebalance "$cur_dir/.dist.json"; then
		ok=0 why="重新平衡 $rebalance_attempts 次後，koupa Pod 仍然沒有每個 worker 剛好一個（$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["workers"])' "$cur_dir/.dist.json")）"
	fi
	python3 -I - "$cur_dir/restore.json" "$cur_dir/.dist.json" "$ok" "$why" "$rebalance_attempts" "$(printf '%s\n' "${actions[@]}")" "$(printf '%s\n' "${pending[@]}")" <<'PY'
import datetime, json, sys
out, dist, ok, why, att, actions, pending = sys.argv[1:]
d = json.load(open(dist))
json.dump({"at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "ok": ok == "1",
           "reason": why or None, "actions": [a for a in actions.split("\n") if a],
           "pending": [a for a in pending.split("\n") if a],
           "replicas": sum(d["workers"].values()), "distribution": d["workers"],
           "unschedulable": d["unschedulable"], "rebalance_deletions": int(att)},
          open(out, "w"), indent=2, ensure_ascii=False)
PY
	rm -f "$cur_dir/.dist.json"
	if [ "$ok" = 1 ]; then
		log "cluster restored: ${actions[*]}; 3 Pods available"
		return 0
	fi
	warn "cluster not restored: $why"
	restore_failed=$why
	return 1
}

stop_collect() {
	[ "$collecting" = 1 ] || return 0
	collecting=0
	if ! "$COLLECT" stop "$cur_dir" >>"$cur_dir/measure.log" 2>&1; then
		warn "collect-timeline.sh stop failed (see $cur_dir/measure.log)"
		return 1
	fi
}

# record_left SIGNAL: write interrupted.json, the cluster state this run
# leaves behind, and print it. After an interruption measure.sh only undoes
# a drain or scale-down (restore_cluster, recorded as "restored"); a
# rollout in progress is finished by the Deployment controller, and the
# next run starts from A8 step 1 (apply, image as the mode wants).
record_left() {
	"$KUBECTL" -n "$NS" get deploy koupa -o json >"$cur_dir/.deploy-left.json" 2>/dev/null || : >"$cur_dir/.deploy-left.json"
	"$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name=koupa -o json >"$cur_dir/.pods-left.json" 2>/dev/null || : >"$cur_dir/.pods-left.json"
	python3 -I - "$cur_dir" "$1" "$(deploy_status)" "$mode" <<'PY'
import json, os, sys
d, sig, status, mode = sys.argv[1:]
def load(name):
    try:
        return json.load(open(os.path.join(d, name)))
    except (OSError, ValueError):
        return None
dep, pods = load(".deploy-left.json"), load(".pods-left.json")
restored = load("restore.json")
c = {"image": None, "generation": None, "observed_generation": None, "rollout": status, "pods": []}
if dep:
    cs = dep["spec"]["template"]["spec"].get("containers", [])
    c["image"] = next((x.get("image") for x in cs if x.get("name") == "koupa"), None)
    c["generation"] = dep["metadata"].get("generation")
    c["observed_generation"] = dep.get("status", {}).get("observedGeneration")
for p in (pods or {}).get("items", []):
    st = next((x for x in p.get("status", {}).get("containerStatuses", []) or [] if x.get("name") == "koupa"), {})
    c["pods"].append({"name": p["metadata"].get("name"), "phase": p.get("status", {}).get("phase"),
                      "terminating": bool(p["metadata"].get("deletionTimestamp")),
                      "image": st.get("image"), "ready": st.get("ready")})
out = {"signal": sig, "mode": mode, "cluster": c, "restored": restored,
       "note": "after the interruption measure.sh only undid a drain or scale-down (restored: "
               "null when there was none); a rollout in progress is finished by the Deployment "
               "controller; the next measure.sh run applies the overlay again (A8 step 1)."}
json.dump(out, open(os.path.join(d, "interrupted.json"), "w"), indent=2, ensure_ascii=False)
print("cluster left: Deployment koupa image %s, rollout %s, %d Pod(s)" % (c["image"], status, len(c["pods"])))
PY
	rm -f "$cur_dir/.deploy-left.json" "$cur_dir/.pods-left.json"
}

# Interruption (A8: a run that does not finish is invalid). INT/TERM only
# record the signal and exit; the EXIT trap does the cleanup, so an
# unexpected exit inside a run is handled the same way.
in_run=0 interrupted_sig=""
on_signal() {
	trap '' INT TERM
	interrupted_sig=$1
	warn "interrupted (SIG$1)"
	exit "$2"
}
on_exit() {
	local rc=$?
	trap '' INT TERM
	[ "$own_tooling" = 1 ] && rm -f "$tooling"
	if [ -n "$all_child" ]; then
		kill -TERM "$all_child" 2>/dev/null
		wait "$all_child" 2>/dev/null
	fi
	if [ "$in_run" != 1 ]; then
		# A restoration that failed after a finished run: one more try.
		if { [ -n "$drained" ] || [ "$scaled" = 1 ]; } && [ -d "$cur_dir" ]; then
			warn "the cluster is still not restored: trying once more"
			restore_cluster 0 || warn "叢集仍未恢復：$restore_failed（見 $cur_dir/restore.json），請手動處理"
			[ "$rc" -ne 0 ] || rc=1
		fi
		exit "$rc"
	fi
	in_run=0
	local sig=${interrupted_sig:-EXIT}
	warn "the run $cur_id did not finish: stopping rush-hour and the collectors"
	[ -n "$fg_pid" ] && kill "$fg_pid" 2>/dev/null
	stop_rush_hour
	stop_probe
	stop_coverage
	if [ -n "$drained" ] || [ "$scaled" = 1 ]; then
		warn "undoing the change of this $mode run (A15)"
		restore_cluster 0
	fi
	stop_collect
	if [ -n "$cur_dir" ] && [ -d "$cur_dir" ]; then
		local why="量測被中斷（measure.sh 收到 SIG$sig）"
		[ "$sig" = EXIT ] && why="量測被中斷（measure.sh 意外結束，狀態 $rc）"
		record_left "$sig" | tee -a "$cur_dir/measure.log" >&2
		[ -f "$cur_dir/rollout.json" ] || echo '{"completed": null, "note": "interrupted before the change"}' >"$cur_dir/rollout.json"
		if [ -f "$cur_dir/scorecard.json" ]; then
			scorecard_py "$cur_dir" invalid "$why"
		else
			scorecard_py "$cur_dir" invalid-new "$why"
		fi
		"$stage/scripts/evidence.sh" "$cur_dir" >>"$cur_dir/measure.log" 2>&1
		warn "本次量測無效：$why；evidence: $cur_dir"
	fi
	[ "$rc" -ne 0 ] || rc=1
	exit "$rc"
}
trap 'on_signal INT 130' INT
trap 'on_signal TERM 143' TERM
trap on_exit EXIT

# abort_run REASON: a step failed or timed out: stop, mark the run invalid,
# keep its evidence, end the invocation with 1 (plan A8 last paragraph).
abort_run() {
	local why=$1
	warn "本次量測無效：$why"
	stop_rush_hour
	stop_probe
	stop_coverage
	restore_cluster 0 || why="$why；$restore_failed"
	stop_collect || why="$why；時間線收集停止失敗"
	set -- "$why"
	[ -f "$cur_dir/rollout.json" ] || echo '{"completed": null, "note": "the run stopped before the change"}' >"$cur_dir/rollout.json"
	if [ -f "$cur_dir/scorecard.json" ]; then
		scorecard_py "$cur_dir" invalid "$1"
	else
		scorecard_py "$cur_dir" invalid-new "$1"
	fi
	"$stage/scripts/evidence.sh" "$cur_dir" >>"$cur_dir/measure.log" 2>&1
	in_run=0
	# The invocation stops here: the comparison it belongs to is broken, so
	# with --diagnostic every scorecard of it gets the banner (A13).
	[ "$diagnostic" = 1 ] && apply_banner
	print_cards
	exit 1
}

# apply_banner: the diagnostic banner on line 1 of every scorecard so far.
apply_banner() {
	local d
	for d in "${run_dirs[@]}"; do scorecard_py "$d" banner "$BANNER"; done
	warn "$BANNER（每張成績單第一行都已標示）"
}

# print_cards: every scorecard of this invocation, as saved (banner and
# invalid marks already applied).
print_cards() {
	local d
	for d in "${run_dirs[@]}"; do
		echo
		cat "$d/scorecard.txt"
		echo "evidence: $d"
	done
	echo
}

# measure_one N: one complete run (A8 steps 1-7). Sets run_dirs.
measure_one() {
	local n=$1 rand
	rand=$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')
	cur_id="$mode-$(date +%Y%m%d-%H%M%S)-r$n-$rand"
	cur_dir=$EVIDENCE/$cur_id
	base_url=""
	mkdir -p "$cur_dir/manifest"
	cp "$tooling" "$cur_dir/tooling.json"
	drain_target=""
	in_run=1
	run_dirs+=("$cur_dir")
	log "run $n/$runs: $cur_id"

	# Resolve once per run what the images are now: the koupa tags in the
	# registry (preflight compares every Pod against v1; the evidence pack
	# keeps both) and the local rush-hour image, which then runs by its ID.
	local v1_digest v2_digest err=$cur_dir/.resolve.err
	v1_digest=$(skopeo inspect --format '{{.Digest}}' "docker://$IMG_V1" 2>"$err") || v1_digest=""
	v2_digest=$(skopeo inspect --format '{{.Digest}}' "docker://$IMG_V2" 2>>"$err") || v2_digest=""
	rh_image_id=""
	if [ "$rh_kind" = image ]; then
		rh_image_id=$(podman image inspect --format '{{.Id}}' "$RUSH_HOUR_IMAGE" 2>>"$err") || rh_image_id=""
	fi
	python3 -I - "$cur_dir/image-digests.json" "$IMG_V1" "$v1_digest" "$IMG_V2" "$v2_digest" \
		"$rh_kind" "$rh_ref" "$rh_image_id" <<'PY'
import datetime, json, sys
out, i1, d1, i2, d2, kind, ref, iid = sys.argv[1:]
now = datetime.datetime.now(datetime.timezone.utc).isoformat()
json.dump({"resolved_at": now, "how": "skopeo inspect --format {{.Digest}} docker://<tag>",
           "koupa": {i1: {"digest": d1 or None}, i2: {"digest": d2 or None}},
           "rush_hour": {"kind": kind, "ref": ref, "image_id": iid or None}},
          open(out, "w"), indent=2)
PY
	local before_digest=$v1_digest after_digest=$v2_digest
	[ "$img_before" = "$IMG_V2" ] && before_digest=$v2_digest after_digest=$v1_digest
	write_params "$n"
	[ -n "$v1_digest" ] && [ -n "$v2_digest" ] ||
		abort_run "無法從 registry 解析 koupa 映像檔 digest（$(head -c 200 "$err" | tr '\n' ' ')）"
	[ "$rh_kind" = bin ] || [ -n "$rh_image_id" ] ||
		abort_run "找不到本機 rush-hour 映像檔 $RUSH_HOUR_IMAGE（先執行 podman pull $RUSH_HOUR_IMAGE）"
	rm -f "$err"

	# 1. render, image as the mode starts (v1; rollback: v2), apply.
	if ! "$KUBECTL" kustomize "$overlay_dir" >"$cur_dir/manifest/rendered.yaml" 2>>"$cur_dir/measure.log"; then
		abort_run "kubectl kustomize $overlay_dir 失敗"
	fi
	sed "s#ghcr.io/braveantony/koupa:v[0-9][0-9]*#$img_before#" "$cur_dir/manifest/rendered.yaml" >"$cur_dir/manifest/applied.yaml"
	rm -f "$cur_dir/manifest/rendered.yaml"
	if [ "$mode" = calibrate ]; then
		calib_prestop=$(python3 -I -c '
import sys, yaml
d = [x for x in yaml.safe_load_all(open(sys.argv[1])) if x and x.get("kind") == "Deployment"][0]
c = [c for c in d["spec"]["template"]["spec"]["containers"] if c["name"] == "koupa"][0]
print(c["lifecycle"]["preStop"]["sleep"]["seconds"])' "$cur_dir/manifest/applied.yaml" 2>/dev/null)
		[[ $calib_prestop =~ ^[0-9]+$ ]] || abort_run "k8s/calibrate 沒有 preStop sleep 秒數"
	fi
	if ! "$KUBECTL" apply -f - <"$cur_dir/manifest/applied.yaml" >>"$cur_dir/measure.log" 2>&1; then
		abort_run "kubectl apply 失敗（見 measure.log）"
	fi

	# 2. rollout, old Pods gone, preflight.
	log "waiting for the rollout to ${img_before##*:} ($overlay settings)"
	if ! bg_wait "$KUBECTL" -n "$NS" rollout status deploy/koupa --timeout="${APPLY_ROLLOUT_TIMEOUT_S}s" >>"$cur_dir/measure.log" 2>&1; then
		abort_run "套用 $mode 後 rollout 在 ${APPLY_ROLLOUT_TIMEOUT_S} 秒內沒有完成"
	fi
	local deadline=$(($(now_ns) + OLD_PODS_TIMEOUT_S * 1000000000))
	until old_pods_gone; do
		[ "$(now_ns)" -lt "$deadline" ] || abort_run "舊 Pod 在 ${OLD_PODS_TIMEOUT_S} 秒內沒有全部結束"
		sleep 1
	done
	deadline=$(($(now_ns) + APP_START_TIMEOUT_S * 1000000000))
	until app_started; do
		if [ "$(now_ns)" -ge "$deadline" ]; then
			warn "not every Pod logged tx_timeout_check within ${APP_START_TIMEOUT_S} s; preflight decides"
			break
		fi
		sleep 1
	done
	if ! "$stage/scripts/preflight.sh" "$overlay" --image "$img_before" --digest "$before_digest" >"$cur_dir/preflight.txt" 2>&1; then
		sed 's/^/  /' "$cur_dir/preflight.txt" >&2
		abort_run "量測前核對不符：$(grep '^FAIL' "$cur_dir/preflight.txt" | sed 's/^FAIL \([a-z-]*\):.*/\1/' | paste -sd, -)"
	fi
	log "preflight: all 7 checks passed"
	# The Deployment as measured: the scorecard's preStop label reads its
	# koupa.dev/prestop-calibrated annotation (no file = unknown).
	"$KUBECTL" -n "$NS" get deploy koupa -o json >"$cur_dir/deployment-at-start.json" 2>>"$cur_dir/measure.log" ||
		rm -f "$cur_dir/deployment-at-start.json"

	# drain (A15): one koupa Pod per worker before the run (rebalance if
	# not), then a target worker that passes the checks, or stop.
	if [ "$change" = drain ]; then
		if ! rebalance "$cur_dir/distribution-before.json"; then
			abort_run "drain 前 koupa Pod 不是每個 worker 剛好一個，重新平衡 $rebalance_attempts 次後仍不符合（$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["workers"])' "$cur_dir/distribution-before.json" 2>/dev/null)）"
		fi
		[ "$rebalance_attempts" = 0 ] || wait_available 3 || true
		if ! drain_target=$("$stage/scripts/drain-target.sh" "$cur_dir/drain-target.json" 2>"$cur_dir/drain-target.err"); then
			drain_target=""
			sed 's/^/  /' "$cur_dir/drain-target.err" >&2
			abort_run "drain：沒有符合 A15 條件的 worker（$(grep -v 'no worker satisfies' "$cur_dir/drain-target.err" | sed 's/^drain-target: //' | paste -sd'；' -)），停止"
		fi
		log "drain target: $drain_target ($(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["koupa_pods_affected"])' "$cur_dir/drain-target.json") koupa Pod(s) affected)"
	fi

	# 3. collectors, Gateway IP.
	if ! "$COLLECT" start "$cur_dir" >>"$cur_dir/measure.log" 2>&1; then
		collecting=1
		abort_run "時間線收集啟動失敗（見 measure.log）"
	fi
	collecting=1
	local ip
	ip=$("$KUBECTL" -n "$NS" get gateway koupa -o jsonpath='{.status.addresses[0].value}' 2>>"$cur_dir/measure.log")
	[ -n "$ip" ] || abort_run "Gateway koupa 的 status 沒有 IP"
	base_url=http://$ip
	write_params "$n"

	# calibrate: probes (A11 step 2) from before sending until the old Pods
	# are gone; stopped explicitly (the duration is only a bound).
	if [ "$mode" = calibrate ]; then
		log "starting rush-hour probe (50/s, new connection per request)"
		rush_hour --exec --name "rush-hour-$cur_id-probe" "$cur_dir" probe --base "$base_url" --run "$cur_id" \
			--duration "$((duration + 600))s" --out "$cur_dir" >"$cur_dir/probe.stdout" 2>"$cur_dir/probe.stderr" &
		probe_pid=$!
		sleep 1
	fi

	# 4. rush-hour; wait for SENDING_STARTED.
	local args=(run --base "$base_url" --run "$cur_id" --duration "${duration}s" --rate "$rate" --boards 3
		--out "$cur_dir" --ingress-log "$cur_dir/ingress.log" --app-logs "$cur_dir/app")
	[ "$verbose" = 1 ] && args+=(--verbose)
	log "starting rush-hour ($rh_kind $rh_ref) against $base_url"
	rush_hour --exec "$cur_dir" "${args[@]}" >"$cur_dir/rush-hour.stdout" 2>"$cur_dir/rush-hour.stderr" &
	rh_pid=$!
	local t0="" wait_until=$(($(now_ns) + SENDING_TIMEOUT_S * 1000000000))
	while :; do
		t0=$(sed -n 's/^SENDING_STARTED \([0-9][0-9]*\)$/\1/p' "$cur_dir/rush-hour.stdout" | head -1)
		[ -n "$t0" ] && break
		kill -0 "$rh_pid" 2>/dev/null || break
		[ "$(now_ns)" -lt "$wait_until" ] || abort_run "rush-hour 在 ${SENDING_TIMEOUT_S} 秒內沒有開始送單"
		sleep 0.1
	done
	local rollout_invalid=""
	if [ -n "$t0" ]; then
		log "sending started at $(rfc "$t0")"
		# App log meta for Pods whose log is complete (terminated Pods), so the
		# wind-down can settle their attempts; collect-timeline.sh stop
		# rewrites every meta at the end.
		# Same artifact as the run (image mode: the recorded image ID).
		(while sleep 2; do
			rush_hour --name "rush-hour-$cur_id-cov" "$cur_dir" coverage "$cur_dir" >/dev/null 2>>"$cur_dir/coverage.err" ||
				echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) rush-hour coverage failed (exit $?)" >>"$cur_dir/coverage.err"
		done) &
		cov_pid=$!

		# 5. the change.
		local change_ns=$((t0 + CHANGE_AT_S * 1000000000))
		local deadline_ns=$((t0 + (duration - ROLLOUT_MARGIN_S) * 1000000000))
		if [ "$change" = none ]; then
			local gen
			gen=$("$KUBECTL" -n "$NS" get deploy koupa -o jsonpath='{.metadata.generation}' 2>/dev/null)
			printf '{"mode": "control", "change": null, "completed": null, "generation_at_start": "%s", "sending_started_at": "%s"}\n' \
				"$gen" "$(rfc "$t0")" >"$cur_dir/rollout.json"
		else
			sleep_until "$change_ns"
			local at status="" done_at="" desc seen drc=""
			case $change in
			image) desc="set image $img_after" ;;
			scale) desc="scale deploy/koupa --replicas=2" ;;
			drain) desc="drain $drain_target --ignore-daemonsets" ;;
			esac
			at=$(now_ns)
			log "second $CHANGE_AT_S: $desc"
			case $change in
			image | scale)
				local cmd=(set image deploy/koupa "koupa=$img_after")
				if [ "$change" = scale ]; then
					cmd=(scale deploy/koupa --replicas=2)
					scaled=1 # before the call: an interruption right after it still restores
				fi
				if ! "$KUBECTL" -n "$NS" "${cmd[@]}" >>"$cur_dir/measure.log" 2>&1; then
					rollout_invalid="第 $CHANGE_AT_S 秒 kubectl ${cmd[0]} 失敗"
				else
					while :; do
						status=$(deploy_status)
						# The time the read came back: "done" counts only when
						# it was observed before D - 10 s.
						seen=$(now_ns)
						[ "$seen" -lt "$deadline_ns" ] || break
						if [ "$status" = done ]; then
							done_at=$seen
							break
						fi
						sleep 0.5
					done
					if [ -z "$done_at" ]; then
						rollout_invalid="rollout 在第 $((duration - ROLLOUT_MARGIN_S)) 秒（D − $ROLLOUT_MARGIN_S）前沒有完成（$status）"
					elif [ "$change" = image ] && ! pods_digest_check "$after_digest" "$cur_dir/.v2check.json"; then
						rollout_invalid="rollout 後的 Pod 映像檔 digest 不是 $img_after 的 $after_digest（$(python3 -I -c 'import json,sys; print("; ".join(json.load(open(sys.argv[1]))["bad"]))' "$cur_dir/.v2check.json" 2>/dev/null)）"
					fi
				fi
				;;
			drain)
				# Complete when kubectl drain returns (every Pod evicted and
				# gone) before D - 10 s. A replacement Pod may stay Pending
				# while the worker is cordoned (topology spread).
				drained=$drain_target # before the call: the trap uncordons
				local left=$(((deadline_ns - at) / 1000000000))
				[ "$left" -ge 1 ] || left=1
				"$KUBECTL" drain "$drain_target" --ignore-daemonsets --timeout="${left}s" >"$cur_dir/drain.log" 2>&1 &
				drain_pid=$!
				while kill -0 "$drain_pid" 2>/dev/null && [ "$(now_ns)" -lt "$deadline_ns" ]; do sleep 0.2; done
				if kill -0 "$drain_pid" 2>/dev/null; then
					stop_drain_cmd
					status="kubectl drain still running"
					rollout_invalid="drain 在第 $((duration - ROLLOUT_MARGIN_S)) 秒（D − $ROLLOUT_MARGIN_S）前沒有完成"
				else
					wait "$drain_pid"
					drc=$?
					drain_pid=""
					seen=$(now_ns)
					status=$(deploy_status)
					cat "$cur_dir/drain.log" >>"$cur_dir/measure.log"
					if [ "$drc" -ne 0 ]; then
						# A15: a Pod that needs --delete-emptydir-data (or any
						# other refusal) stops the measurement.
						abort_run "kubectl drain $drain_target 失敗（結束狀態 $drc）：$(grep -v '^node/' "$cur_dir/drain.log" | head -3 | tr '\n' ' ')"
					elif [ "$seen" -lt "$deadline_ns" ]; then
						done_at=$seen
						distribution "$cur_dir/distribution-at-drain.json" || true
					else
						rollout_invalid="drain 在第 $((duration - ROLLOUT_MARGIN_S)) 秒（D − $ROLLOUT_MARGIN_S）前沒有完成"
					fi
				fi
				;;
			esac
			python3 -I - "$cur_dir/rollout.json" "$cur_dir/.v2check.json" <<PY
import json, sys
json.dump({"mode": "$mode", "change": "$desc",
    "sending_started_at": "$(rfc "$t0")", "change_at": "$(rfc "$at")",
    "change_offset_s": round(($at - $t0) / 1e9, 3), "deadline_at": "$(rfc "$deadline_ns")",
    "completed": bool("$done_at"),
    "completed_at": "$( [ -n "$done_at" ] && rfc "$done_at")" or None,
    "completed_offset_s": round((${done_at:-0} - $t0) / 1e9, 3) if "$done_at" else None,
    "status_at_deadline": None if "$done_at" else "${status:-}",
    "deployment_at_completion": "${status:-}" if "$done_at" else None,
    "drain_exit": int("$drc") if "$drc" else None,
    "completion_rule": ("kubectl drain returned (every Pod evicted and gone) before D - 10 s; the "
                        "replacement Pod may stay Pending until uncordon (topology spread "
                        "DoNotSchedule with the worker cordoned), so 3 available is not required"
                        ) if "$change" == "drain" else "Deployment rollout status done before D - 10 s",
    "invalid_reason": """$rollout_invalid""" or None,
    "${img_after##*:}_digest_check" if "$img_after" else "digest_check":
        json.load(open(sys.argv[2])) if __import__("os").path.exists(sys.argv[2]) else None},
    open(sys.argv[1], "w"), indent=2, ensure_ascii=False)
PY
			rm -f "$cur_dir/.v2check.json"
			if [ -n "$rollout_invalid" ]; then
				warn "$rollout_invalid"
			else
				log "$desc complete at second $(awk -v d="$((done_at - t0))" 'BEGIN{printf "%.1f", d/1e9}')"
			fi
		fi
	fi

	# 6. rush-hour sends until D, winds down and writes its scorecard.
	local rc=0
	wait "$rh_pid" || rc=$?
	rh_pid=""
	stop_coverage
	log "rush-hour exited ($rc)"
	# A8: a step that failed stops the invocation, whatever rush-hour wrote.
	[ -n "$t0" ] || abort_run "rush-hour 沒有印出 SENDING_STARTED（結束狀態 $rc，見 rush-hour.stderr）"
	[ "$rc" -eq 0 ] || abort_run "rush-hour 結束狀態 $rc（見 rush-hour.stderr）"
	if [ "$mode" = calibrate ]; then
		# Probe until every terminated Pod is gone (its preStop has ended,
		# so its U, if any, is behind us), at most OLD_PODS_TIMEOUT_S.
		local until_ns=$(($(now_ns) + OLD_PODS_TIMEOUT_S * 1000000000))
		until old_pods_gone || [ "$(now_ns)" -ge "$until_ns" ]; do sleep 1; done
		stop_probe
		[ -s "$cur_dir/probes.jsonl" ] || abort_run "rush-hour probe 沒有寫出 probes.jsonl（見 probe.stderr）"
	fi

	# 7. collectors, cluster restored (drain, scale-down), evidence.
	stop_collect || abort_run "時間線收集停止失敗（見 measure.log）"
	local restore_invalid=""
	if ! restore_cluster 1; then
		echo "cluster not restored after the run: $restore_failed" >>"$cur_dir/measure.log"
		# A15: restoring and confirming is part of the run.
		restore_invalid="量測後叢集沒有恢復：$restore_failed"
	fi
	if [ ! -f "$cur_dir/scorecard.json" ]; then
		abort_run "rush-hour 沒有寫出成績單（結束狀態 $rc，見 rush-hour.stderr）"
	elif [ -n "$rollout_invalid" ]; then
		scorecard_py "$cur_dir" invalid "$rollout_invalid"
	fi
	[ -z "$restore_invalid" ] || scorecard_py "$cur_dir" invalid "$restore_invalid"
	if ! "$stage/scripts/evidence.sh" "$cur_dir" >>"$cur_dir/measure.log" 2>&1; then
		scorecard_py "$cur_dir" is-invalid ||
			scorecard_py "$cur_dir" invalid "證據包不完整（見 evidence-index.txt）"
	fi
	if [ "$change" = drain ] && ! scorecard_py "$cur_dir" is-invalid; then
		printf '\n註：drain 的「完成」是 kubectl drain 在第 D − 10 秒前結束（Pod 都已驅逐並結束）；worker cordon 期間，替代 Pod 受 topology spread（DoNotSchedule）限制，可能到 uncordon 才排程，所以這段時間可能只有 2 個 Pod（見 rollout.json、distribution-at-drain.json）。\n' >>"$cur_dir/scorecard.txt"
	fi
	scorecard_py "$cur_dir" prestop || warn "preStop label not written for $cur_id"
	if [ "$mode" = calibrate ]; then
		if ! "$HOST_RH" calibrate --prestop "${calib_prestop}s" --json "$cur_dir/calibrate.json" "$cur_dir" \
			>"$cur_dir/calibrate.txt" 2>>"$cur_dir/measure.log"; then
			warn "rush-hour calibrate failed for $cur_id (see measure.log)"
		fi
	fi
	in_run=0
	if scorecard_py "$cur_dir" is-invalid; then
		log "run $n/$runs finished: invalid (evidence: $cur_dir)"
	else
		log "run $n/$runs finished (evidence: $cur_dir)"
	fi
}

# ---------------------------------------------------------------- all
# run_all: rounds of control, step-0 .. step-5, one run each, by running
# this script per mode. A step that fails (exit 1) or a control that does
# not hold (exit 3, without --diagnostic) stops the sequence; the step-0
# gate is judged here over all its runs.
run_all() {
	local list=$EVIDENCE/.all-$started.runs r m rc any=0 stop_rc=""
	: >"$list"
	export MEASURE_IN_ALL=1 MEASURE_RUN_LIST=$list MEASURE_TOOLING_FILE=$tooling
	log "all: $runs round(s) of ${ALL_SEQUENCE[*]}"
	for r in $(seq 1 "$runs"); do
		for m in "${ALL_SEQUENCE[@]}"; do
			log "all: round $r: measure.sh $m --runs 1 ${all_opts[*]}"
			"$0" "$m" --runs 1 "${all_opts[@]}" &
			all_child=$!
			rc=0
			wait "$all_child" || rc=$?
			all_child=""
			case $rc in
			0) ;;
			2) any=1 ;;
			*) stop_rc=$rc; warn "all: round $r $m ended with $rc: stopping the sequence"; break 2 ;;
			esac
		done
	done
	local d step0=() seen=""
	mapfile -t run_dirs <"$list"
	for d in "${run_dirs[@]}"; do
		case $(basename "$d") in step-0-*) step0+=("$d") ;; esac
	done
	if [ -z "$stop_rc" ] && [ "${#step0[@]}" -gt 0 ]; then
		for d in "${step0[@]}"; do
			scorecard_py "$d" problem >/dev/null && seen=1
		done
		if [ -z "$seen" ]; then
			warn "對照組不成立：步驟 0 的 ${#step0[@]} 次都沒觀察到預定的問題（送出沒有成功回應、漏號、重複訂單）"
			[ "$diagnostic" = 1 ] || stop_rc=3
		fi
	fi
	local summary=$EVIDENCE/summary-all-$started.txt
	{
		for m in "${ALL_SEQUENCE[@]}"; do
			local md=()
			for d in "${run_dirs[@]}"; do case $(basename "$d") in "$m"-2*) md+=("$d") ;; esac; done
			[ "${#md[@]}" -gt 0 ] || continue
			echo "==== $m（${#md[@]} 次）"
			cur_id=summary-all-$started
			rush_hour "$EVIDENCE" summarize "${md[@]}" 2>&1
		done
	} >"$summary"
	rm -f "$list"
	echo "================ all 彙整（$summary）"
	cat "$summary"
	[ -z "$stop_rc" ] || exit "$stop_rc"
	[ "$any" = 1 ] && exit 2
	exit 0
}

# ---------------------------------------------------------------- calibrate
# calib_aggregate DIR...: the samples of the valid calibrate runs so far, in
# evidence/calibrate-<started>/ (samples.txt, candidate.json with the
# step-2 preStop value prestop_s = ceil(candidate) + PRESTOP_BUFFER_S).
# Prints the valid count.
calib_aggregate() {
	local out=$EVIDENCE/calibrate-$started
	mkdir -p "$out"
	"$HOST_RH" calibrate --prestop "${calib_prestop}s" --json "$out/.report.json" "$@" >"$out/samples.txt" 2>"$out/calibrate.err" || {
		warn "rush-hour calibrate failed (see $out/calibrate.err)"
		echo 0
		return
	}
	[ -s "$out/calibrate.err" ] || rm -f "$out/calibrate.err"
	python3 -I - "$out/.report.json" "$out/candidate.json" "$PRESTOP_BUFFER_S" "$CALIB_MIN_VALID" "$@" <<'PY'
import json, math, os, sys
src, dst, buf, need, dirs = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), sys.argv[5:]
r = json.load(open(src))
r["runs"] = [os.path.basename(d) for d in dirs]
r["run_dirs"] = dirs
r["min_valid_samples"] = need
r["buffer_s"] = buf
r["buffer_note"] = "provisional: %d s until measured more often on this environment (A11 step 6)" % buf
c = r.get("candidate_s")
r["prestop_s"] = (math.ceil(c) + buf) if c is not None else None
r["calibrated"] = c is not None
json.dump(r, open(dst, "w"), indent=2, ensure_ascii=False)
os.remove(src)
print(r.get("valid_samples", 0))
PY
}

# ---------------------------------------------------------------- main
run_dirs=()
gate_failed="" problem_seen=""
any_invalid=0
started=$(date +%Y%m%d-%H%M%S)
calib_prestop="" calib_dirs=() calib_valid=0
[ "$mode" = all ] && run_all
log "$mode: $runs run(s), D=${duration}s, rate $rate/s$([ "$diagnostic" = 1 ] && echo ', --diagnostic')"

for n in $(seq 1 "$runs"); do
	measure_one "$n"
	[ -z "${MEASURE_RUN_LIST:-}" ] || echo "$cur_dir" >>"$MEASURE_RUN_LIST"
	scorecard_py "$cur_dir" is-invalid && any_invalid=1
	if [ -n "$restore_failed" ]; then
		print_cards
		warn "停止：叢集沒有恢復到下一次量測的前置條件：$restore_failed（見 $cur_dir/restore.json）"
		exit 1
	fi
	case $mode in
	calibrate)
		if scorecard_py "$cur_dir" is-invalid; then
			warn "calibrate run $n is invalid: its samples are not used"
		else
			calib_dirs+=("$cur_dir")
		fi
		if [ "${#calib_dirs[@]}" -gt 0 ]; then
			calib_valid=$(calib_aggregate "${calib_dirs[@]}")
			log "calibrate: $calib_valid valid sample(s) after $n run(s) (need $CALIB_MIN_VALID)"
			[ "$calib_valid" -ge "$CALIB_MIN_VALID" ] && break
		fi
		;;
	control)
		if why=$(scorecard_py "$cur_dir" control-ok); then
			log "control run $n: $why"
		else
			gate_failed="control 第 $n 次（$cur_id）：$why"
			warn "對照組不成立：$gate_failed"
			[ "$diagnostic" = 1 ] || break
		fi
		;;
	step-0)
		if seen=$(scorecard_py "$cur_dir" problem); then
			problem_seen="${problem_seen:+$problem_seen；}第 $n 次：$seen"
		fi
		;;
	esac
done

if [ "$mode" = step-0 ] && [ -n "${MEASURE_IN_ALL:-}" ]; then
	: # judged by the `all` parent over every round
elif [ "$mode" = step-0 ] && [ -z "$problem_seen" ]; then
	gate_failed="步驟 0 的 ${#run_dirs[@]} 次都沒觀察到預定的問題（送出沒有成功回應、漏號、重複訂單）"
	warn "對照組不成立：$gate_failed"
elif [ "$mode" = step-0 ]; then
	log "step 0 observed: $problem_seen"
fi

banner=0
if [ -n "$gate_failed" ] && [ "$diagnostic" = 1 ]; then
	banner=1
elif [ "$diagnostic" = 1 ] && [[ $mode == step-[1-5] ]]; then
	banner=1
fi
# The gate and the banner are decided; only now are the scorecards shown.
[ "$banner" = 1 ] && apply_banner
print_cards

summary=$EVIDENCE/summary-$mode-$started.txt
{
	[ "$banner" = 1 ] && echo "$BANNER"
	cur_id=summary-$started
	rush_hour "$EVIDENCE" summarize "${run_dirs[@]}" 2>&1
	[ -n "$gate_failed" ] && echo && echo "對照組不成立：$gate_failed"
} >"$summary"
echo "================ $mode 彙整（$summary）"
cat "$summary"
for d in "${run_dirs[@]}"; do echo "  $d"; done

calib_short=0
if [ "$mode" = calibrate ]; then
	cdir=$EVIDENCE/calibrate-$started
	if [ -f "$cdir/candidate.json" ]; then
		echo "================ calibrate 樣本（$cdir）"
		cat "$cdir/samples.txt"
	fi
	prestop=$(python3 -I -c 'import json,sys; v=json.load(open(sys.argv[1])).get("prestop_s"); print("" if v is None else v)' "$cdir/candidate.json" 2>/dev/null)
	if [ -n "$prestop" ]; then
		log "calibrate: 有效樣本 $calib_valid 個；步驟 2 的 preStop 候選值 $prestop 秒（ceil(候選值) + 緩衝 $PRESTOP_BUFFER_S 秒，緩衝暫定；上界為模型推估）；$cdir/candidate.json"
	else
		warn "calibrate: 有效樣本 $calib_valid 個，不足 $CALIB_MIN_VALID 個：不產生候選值；步驟 2 改用保守值，並把 koupa.dev/prestop-calibrated 改成 \"false\"，成績單才會標示「preStop 未經校正」（TEACHER.md 第 5 節）"
		calib_short=1
	fi
fi

if [ -n "$gate_failed" ] && [ "$diagnostic" != 1 ]; then
	warn "停止正式比較：先調查原因（可能在量測工具、環境或 App）；講師可以加上 --diagnostic 繼續收集資料"
	exit 3
fi
[ "$any_invalid" = 1 ] && exit 2
[ "$calib_short" = 1 ] && exit 2
exit 0
