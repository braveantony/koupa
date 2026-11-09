#!/usr/bin/env bash
# Koupa measurement (plan A8, A13; design 5.1, 5.4).
#
#   ./measure.sh <mode> [--runs 5] [--duration 60] [--rate 20] [--verbose] [--diagnostic]
#
# mode: step-0 .. step-4, control. (calibrate, rollback, drain, scale-down
# and all come with Task 23.)
#
# One run, every time from step 1 (plan A8):
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
#
# A13 gate: control must be 0 failures, 0 missed calls, 0 duplicates with no
# unknown and not invalid; step-0 must observe at least one predefined
# problem over its runs. Otherwise exit 3, unless --diagnostic: then the
# runs go on and every scorecard of this invocation starts with
# 「診斷模式：對照組不成立，不可用於比較」. With --diagnostic on step-1..4 (a
# comparison continued after a failed control) every scorecard gets it too.
#
# Exit: 0 ok; 1 a step failed or timed out (run marked invalid, stopped);
# 2 at least one run invalid; 3 the A13 gate failed; 64 usage.
#
# Environment: RUSH_HOUR_IMAGE (default ghcr.io/braveantony/rush-hour:0.1.0);
# RUSH_HOUR_BIN=<path> runs a local rush-hour binary instead of podman (tests,
# development); MEASURE_EVIDENCE_DIR (default ./evidence); COLLECT_TIMELINE
# (default scripts/collect-timeline.sh); KUBECTL; KOUPA_NS.
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

usage() {
	sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//' >&2
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
runs=5 duration=60 rate=20 verbose=0 diagnostic=0
while [ $# -gt 0 ]; do
	case $1 in
	--runs) [ $# -ge 2 ] || usage; runs=$2; shift 2 ;;
	--duration) [ $# -ge 2 ] || usage; duration=${2%s}; shift 2 ;;
	--rate) [ $# -ge 2 ] || usage; rate=$2; shift 2 ;;
	--verbose) verbose=1; shift ;;
	--diagnostic) diagnostic=1; shift ;;
	-h | --help) usage ;;
	*) warn "unknown argument: $1"; usage ;;
	esac
done
case $mode in
step-0 | control) overlay_dir=$stage/k8s/base ;;
step-[1-4]) overlay_dir=$stage/k8s/steps/$mode ;;
calibrate | rollback | drain | scale-down | all)
	warn "mode $mode is not implemented yet (Task 23)"
	exit 64 ;;
*) usage ;;
esac
[[ $runs =~ ^[1-9][0-9]*$ ]] || { warn "--runs must be a positive integer"; exit 64; }
[[ $duration =~ ^[0-9]+$ ]] && [ "$duration" -ge 30 ] || { warn "--duration must be whole seconds, at least 30 (plan A8)"; exit 64; }
[[ $rate =~ ^[0-9]+([.][0-9]+)?$ ]] || { warn "--rate must be a positive number"; exit 64; }

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

write_params() {
	python3 -I - "$cur_dir/params.json" <<PY
import json, sys
json.dump({
    "run_id": "$cur_id", "mode": "$mode", "overlay": "$mode", "run_number": $1, "runs": $runs,
    "image_before": "$IMG_V1", "image_after": $( [ "$mode" = control ] && echo None || echo "\"$IMG_V2\"" ),
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

stop_collect() {
	[ "$collecting" = 1 ] || return 0
	collecting=0
	if ! "$COLLECT" stop "$cur_dir" >>"$cur_dir/measure.log" 2>&1; then
		warn "collect-timeline.sh stop failed (see $cur_dir/measure.log)"
		return 1
	fi
}

# record_left SIGNAL: write interrupted.json, the cluster state this run
# leaves behind, and print it. measure.sh does not touch the cluster after
# an interruption: a rollout in progress is finished by the Deployment
# controller, and the next run starts from A8 step 1 (apply, image v1).
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
out = {"signal": sig, "mode": mode, "cluster": c,
       "note": "measure.sh changed nothing after the interruption; a rollout in progress is "
               "finished by the Deployment controller; the next measure.sh run applies the "
               "overlay with image v1 again (A8 step 1)."}
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
	[ "$in_run" = 1 ] || exit "$rc"
	in_run=0
	local sig=${interrupted_sig:-EXIT}
	warn "the run $cur_id did not finish: stopping rush-hour and the collectors"
	[ -n "$fg_pid" ] && kill "$fg_pid" 2>/dev/null
	stop_rush_hour
	stop_coverage
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
	stop_coverage
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
	write_params "$n"
	[ -n "$v1_digest" ] && [ -n "$v2_digest" ] ||
		abort_run "無法從 registry 解析 koupa 映像檔 digest（$(head -c 200 "$err" | tr '\n' ' ')）"
	[ "$rh_kind" = bin ] || [ -n "$rh_image_id" ] ||
		abort_run "找不到本機 rush-hour 映像檔 $RUSH_HOUR_IMAGE"
	rm -f "$err"

	# 1. render, image v1, apply.
	if ! "$KUBECTL" kustomize "$overlay_dir" >"$cur_dir/manifest/rendered.yaml" 2>>"$cur_dir/measure.log"; then
		abort_run "kubectl kustomize $overlay_dir 失敗"
	fi
	sed "s#ghcr.io/braveantony/koupa:v[0-9][0-9]*#$IMG_V1#" "$cur_dir/manifest/rendered.yaml" >"$cur_dir/manifest/applied.yaml"
	rm -f "$cur_dir/manifest/rendered.yaml"
	if ! "$KUBECTL" apply -f - <"$cur_dir/manifest/applied.yaml" >>"$cur_dir/measure.log" 2>&1; then
		abort_run "kubectl apply 失敗（見 measure.log）"
	fi

	# 2. rollout, old Pods gone, preflight.
	log "waiting for the rollout to v1 ($mode settings)"
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
	if ! "$stage/scripts/preflight.sh" "$mode" --image "$IMG_V1" --digest "$v1_digest" >"$cur_dir/preflight.txt" 2>&1; then
		sed 's/^/  /' "$cur_dir/preflight.txt" >&2
		abort_run "量測前核對不符：$(grep '^FAIL' "$cur_dir/preflight.txt" | sed 's/^FAIL \([a-z-]*\):.*/\1/' | paste -sd, -)"
	fi
	log "preflight: all 7 checks passed"

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
		if [ "$mode" = control ]; then
			local gen
			gen=$("$KUBECTL" -n "$NS" get deploy koupa -o jsonpath='{.metadata.generation}' 2>/dev/null)
			printf '{"mode": "control", "change": null, "completed": null, "generation_at_start": "%s", "sending_started_at": "%s"}\n' \
				"$gen" "$(rfc "$t0")" >"$cur_dir/rollout.json"
		else
			sleep_until "$change_ns"
			local at status done_at=""
			at=$(now_ns)
			log "second $CHANGE_AT_S: set image $IMG_V2"
			if ! "$KUBECTL" -n "$NS" set image deploy/koupa "koupa=$IMG_V2" >>"$cur_dir/measure.log" 2>&1; then
				rollout_invalid="第 $CHANGE_AT_S 秒 kubectl set image 失敗"
			else
				local seen
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
				elif ! pods_digest_check "$v2_digest" "$cur_dir/.v2check.json"; then
					rollout_invalid="rollout 後的 Pod 映像檔 digest 不是 $IMG_V2 的 $v2_digest（$(python3 -I -c 'import json,sys; print("; ".join(json.load(open(sys.argv[1]))["bad"]))' "$cur_dir/.v2check.json" 2>/dev/null)）"
				fi
			fi
			python3 -I - "$cur_dir/rollout.json" "$cur_dir/.v2check.json" <<PY
import json, sys
json.dump({"mode": "$mode", "change": "set image $IMG_V2",
    "sending_started_at": "$(rfc "$t0")", "change_at": "$(rfc "$at")",
    "change_offset_s": round(($at - $t0) / 1e9, 3), "deadline_at": "$(rfc "$deadline_ns")",
    "completed": bool("$done_at"),
    "completed_at": "$( [ -n "$done_at" ] && rfc "$done_at")" or None,
    "completed_offset_s": round((${done_at:-0} - $t0) / 1e9, 3) if "$done_at" else None,
    "status_at_deadline": None if "$done_at" else "${status:-}",
    "invalid_reason": """$rollout_invalid""" or None,
    "v2_digest_check": json.load(open(sys.argv[2])) if __import__("os").path.exists(sys.argv[2]) else None},
    open(sys.argv[1], "w"), indent=2, ensure_ascii=False)
PY
			rm -f "$cur_dir/.v2check.json"
			if [ -n "$rollout_invalid" ]; then
				warn "$rollout_invalid"
			else
				log "rollout complete at second $(awk -v d="$((done_at - t0))" 'BEGIN{printf "%.1f", d/1e9}')"
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

	# 7. collectors, evidence.
	stop_collect || abort_run "時間線收集停止失敗（見 measure.log）"
	if [ ! -f "$cur_dir/scorecard.json" ]; then
		abort_run "rush-hour 沒有寫出成績單（結束狀態 $rc，見 rush-hour.stderr）"
	elif [ -n "$rollout_invalid" ]; then
		scorecard_py "$cur_dir" invalid "$rollout_invalid"
	fi
	if ! "$stage/scripts/evidence.sh" "$cur_dir" >>"$cur_dir/measure.log" 2>&1; then
		scorecard_py "$cur_dir" is-invalid ||
			scorecard_py "$cur_dir" invalid "證據包不完整（見 evidence-index.txt）"
	fi
	in_run=0
	if scorecard_py "$cur_dir" is-invalid; then
		log "run $n/$runs finished: invalid (evidence: $cur_dir)"
	else
		log "run $n/$runs finished (evidence: $cur_dir)"
	fi
}

# ---------------------------------------------------------------- main
run_dirs=()
gate_failed="" problem_seen=""
any_invalid=0
started=$(date +%Y%m%d-%H%M%S)
log "$mode: $runs run(s), D=${duration}s, rate $rate/s$([ "$diagnostic" = 1 ] && echo ', --diagnostic')"

for n in $(seq 1 "$runs"); do
	measure_one "$n"
	scorecard_py "$cur_dir" is-invalid && any_invalid=1
	case $mode in
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

if [ "$mode" = step-0 ] && [ -z "$problem_seen" ]; then
	gate_failed="步驟 0 的 ${#run_dirs[@]} 次都沒觀察到預定的問題（送出沒有成功回應、漏號、重複訂單）"
	warn "對照組不成立：$gate_failed"
elif [ "$mode" = step-0 ]; then
	log "step 0 observed: $problem_seen"
fi

banner=0
if [ -n "$gate_failed" ] && [ "$diagnostic" = 1 ]; then
	banner=1
elif [ "$diagnostic" = 1 ] && [[ $mode == step-[1-4] ]]; then
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

if [ -n "$gate_failed" ] && [ "$diagnostic" != 1 ]; then
	warn "停止正式比較：先調查原因（可能在量測工具、環境或 App）；講師可以加上 --diagnostic 繼續收集資料"
	exit 3
fi
[ "$any_invalid" = 1 ] && exit 2
exit 0
