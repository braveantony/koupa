#!/usr/bin/env bash
# Collect the timeline sources of one measurement run (plan A11, design 5.4).
#
#   collect-timeline.sh start DIR   start every collector in the background
#   collect-timeline.sh stop DIR    stop them, write app/<uid>.meta.json,
#                                   timeline.jsonl and timeline.txt (safe twice)
#
# Files in DIR. Every collected line starts with the collector's receive time
# (Unix seconds, nine decimals) and a space, written by `rush-hour stamp`;
# JSON watch streams are compacted to one object per line.
#   pods.jsonl            kubectl get pods -w -o json --output-watch-events
#   endpointslices.jsonl  same for EndpointSlices of Service koupa
#   events.jsonl          same for events
#   hubble.jsonl          hubble observe --namespace koupa -o json, in every cilium agent
#   ingress.log           access-log lines of every cilium-envoy Pod (filtered on "attempt")
#   envoy-stats-start.txt / envoy-stats-end.txt
#                         cilium-dbg envoy admin metrics, koupa clusters, every agent;
#                         "# collected_at=<stamp> node=<node> pod=<pod>" header per agent
#   host.jsonl            {"at","cpu_busy_pct","mem_used_pct"} once per second
#   app/<uid>.log         `kubectl logs -f --timestamps`, one per koupa Pod, via
#                         `rush-hour follow-log` (refills breaks with --since-time)
#   app/<uid>.collect.json  follower state; app/<uid>.meta.json written at stop
#   audit.log             API server audit lines deleting koupa Pods (at stop)
#   pods-at-stop.json     kubectl get pods -o json at stop
#   missing.jsonl         {"source","reason","at"} for sources not collected or interrupted
#   collectors.pid        process-group ids of the collectors
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
stage=$(dirname "$here")
NS=${KOUPA_NS:-koupa}
KUBECTL=${KUBECTL:-kubectl}
RH=${RUSH_HOUR:-$stage/bin/rush-hour}
AUDIT_LOG=${AUDIT_LOG:-/opt/taroko/logs/apiserver/audit.log}
APP_SELECTOR=app.kubernetes.io/name=koupa

usage() {
	echo "usage: $0 start|stop DIR" >&2
	exit 2
}

now_rfc() { date -u +%Y-%m-%dT%H:%M:%S.%NZ; }
stamp() { date +%s.%N; }

json_str() {
	local s=$1
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	s=${s//$'\t'/ }
	s=${s//$'\n'/ }
	printf '"%s"' "$s"
}

# missing SOURCE REASON
missing() {
	printf '{"source":%s,"reason":%s,"at":"%s"}\n' "$(json_str "$1")" "$(json_str "$2")" "$(now_rfc)" >>"$dir/missing.jsonl"
}

stopping() { [ -e "$dir/.stopping" ]; }

# marker: names this run's processes inside the cilium agents.
marker() { echo "koupa-collect-$(printf %s "$dir" | cksum | cut -d' ' -f1)"; }

# reap_remote: end any hubble observe of this run still inside an agent.
reap_remote() {
	local pod m
	m=$(marker)
	for pod in $("$KUBECTL" -n kube-system get pods -l k8s-app=cilium -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
		"$KUBECTL" -n kube-system exec "$pod" -c cilium-agent -- pkill -f -- "--pod $m " >/dev/null 2>&1 || true
	done
}

# with_lock CMD...: run CMD holding DIR/.lock (registration vs stop).
with_lock() {
	(
		flock 9
		"$@"
	) 9>>"$dir/.lock"
}

# set_stopping: after this no collector can register any more.
set_stopping() { with_lock touch "$dir/.stopping"; }

# spawn NAME CMD DIR ARGS...: run "$0 _child DIR NAME CMD ARGS..." in its own session /
# process group. The child registers itself (pgid = its pid) under the lock
# and exits at once if stop has begun, so a stop never misses a collector.
spawn() {
	local name=$1 cmd=$2
	shift 3
	setsid "$0" _child "$dir" "$name" "$cmd" "$@" </dev/null >>"$dir/collectors.err" 2>&1 &
}

_register() {
	stopping && return 1
	echo "$1 $2" >>"$dir/collectors.pid"
}

registered() { grep -c . "$dir/collectors.pid" 2>/dev/null || true; }

# wait_registered N: until N groups registered (at most 10 s).
wait_registered() {
	local i
	for i in $(seq 100); do
		[ "$(registered)" -ge "$1" ] && return 0
		sleep 0.1
	done
	echo "collect-timeline: only $(registered) of $1 collectors registered" >&2
}

# abort WHAT: a failed or interrupted start/stop must not leave collectors
# running. The state stays for a later stop to finalize.
abort() {
	trap - ERR INT TERM
	set +e
	set_stopping
	kill_groups TERM
	local i
	for i in $(seq 50); do
		alive || break
		sleep 0.1
	done
	alive && kill_groups KILL
	reap_remote
	echo "collect-timeline: $1 failed or was interrupted; collectors stopped. Run '$0 stop $dir' to finalize." >&2
	exit 1
}

# ---------------------------------------------------------------- collectors
# Each runs in its own process group (spawned via "$0 _name DIR ...").

# _watch DIR SOURCE FILE ARGS...: a kubectl watch, restarted when it ends.
_watch() {
	local src=$1 file=$2
	shift 2
	while :; do
		set +e
		"$KUBECTL" "$@" -w -o json --output-watch-events 2>>"$dir/$src.err" | "$RH" stamp --json >>"$dir/$file"
		local rc=${PIPESTATUS[0]}
		set -e
		stopping && exit 0
		missing "$src" "watch 中斷（kubectl rc=$rc），1 秒後重新連線；期間的變化只能從重新列出的狀態看到"
		sleep 1
	done
}

# _hubble DIR AGENT_POD
_hubble() {
	local pod=$1
	while :; do
		set +e
		# kubectl exec does not end the remote command when the client dies:
		# hubble runs beside a `cat` on the exec's stdin, which sees EOF once
		# the stream closes and stops it. --not --pod <marker> filters nothing
		# but names this run's processes for the cleanup in stop.
		local sp
		exec 3< <(exec sleep infinity)
		sp=$!
		"$KUBECTL" -n kube-system exec -i "$pod" -c cilium-agent -- sh -c \
			"hubble observe -f --not --pod $(marker) --namespace $NS -o json & p=\$!; cat >/dev/null; kill \$p" \
			<&3 2>>"$dir/hubble.err" | "$RH" stamp --json >>"$dir/hubble.jsonl"
		local rc=${PIPESTATUS[0]}
		exec 3<&-
		kill "$sp" 2>/dev/null || true
		set -e
		stopping && exit 0
		missing "hubble" "agent $pod 的 hubble observe 中斷（rc=$rc），重新連線"
		sleep 1
	done
}

# _ingress DIR ENVOY_POD SINCE
_ingress() {
	local pod=$1 since=$2
	while :; do
		set +e
		"$KUBECTL" -n kube-system logs -f --since-time="$since" "$pod" 2>>"$dir/ingress.err" |
			grep --line-buffered -F '"attempt"' | "$RH" stamp >>"$dir/ingress.log"
		local rc=${PIPESTATUS[0]}
		set -e
		stopping && exit 0
		since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
		missing "ingress" "$pod 的 access log 串流中斷（rc=$rc），從 $since 重新讀取；同一秒內可能重複或缺行"
		sleep 1
	done
}

# _host DIR: CPU busy and memory used, once per second, from /proc.
_host() {
	local cpu u n s idle io irq sirq st rest total idle_all ptotal=0 pidle=0
	while ! stopping; do
		read -r cpu u n s idle io irq sirq st rest </proc/stat
		total=$((u + n + s + idle + io + irq + sirq + st))
		idle_all=$((idle + io))
		local mt ma
		mt=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)
		ma=$(awk '/^MemAvailable:/{print $2}' /proc/meminfo)
		if [ "$ptotal" -gt 0 ] && [ "$total" -gt "$ptotal" ]; then
			local dt=$((total - ptotal)) di=$((idle_all - pidle))
			local busy=$(((dt - di) * 1000 / dt)) mem=$(((mt - ma) * 1000 / mt))
			printf '{"at":"%s","cpu_busy_pct":%d.%d,"mem_used_pct":%d.%d}\n' "$(now_rfc)" \
				$((busy / 10)) $((busy % 10)) $((mem / 10)) $((mem % 10)) >>"$dir/host.jsonl"
		fi
		ptotal=$total pidle=$idle_all
		sleep 1
	done
}

# _applogs DIR: start a follower for every koupa Pod, now and later.
_applogs() {
	local line uid name
	while ! stopping; do
		while read -r uid name; do
			[ -n "$uid" ] || continue
			[ -e "$dir/app/$uid.collect.json" ] || [ -e "$dir/app/.started.$uid" ] && continue
			: >"$dir/app/.started.$uid"
			spawn "app:$name" _follow "$dir" "$uid" "$name"
		done < <("$KUBECTL" -n "$NS" get pods -l "$APP_SELECTOR" \
			-o jsonpath='{range .items[*]}{.metadata.uid}{" "}{.metadata.name}{"\n"}{end}' 2>>"$dir/applogs.err" || true)
		sleep 1
	done
}

_follow() {
	local uid=$1 name=$2
	exec "$RH" follow-log --namespace "$NS" --kubectl "$KUBECTL" --pod "$name" --uid "$uid" --dir "$dir/app" \
		2>>"$dir/app/$uid.follow.err"
}

# stats FILE: koupa cluster counters of every cilium agent's Envoy.
stats() {
	local file=$1 pod node out ok=0
	: >"$dir/$file"
	while read -r pod node; do
		[ -n "$pod" ] || continue
		if out=$("$KUBECTL" -n kube-system exec "$pod" -c cilium-agent -- cilium-dbg envoy admin metrics 2>&1); then
			{
				echo "# collected_at=$(stamp) node=$node pod=$pod"
				grep -E '^envoy_cluster_[a-z0-9_]+\{.*envoy_cluster_name="koupa/' <<<"$out" || true
			} >>"$dir/$file"
			ok=$((ok + 1))
		else
			missing "envoy-stats" "$file：$pod（$node）的 cilium-dbg envoy admin metrics 失敗：${out:0:200}"
		fi
	done < <("$KUBECTL" -n kube-system get pods -l k8s-app=cilium \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}')
	[ "$ok" -gt 0 ] || missing "envoy-stats" "$file：沒有任何 agent 取得統計"
}

# audit SINCE: koupa Pod deletes from every control plane's API audit log.
audit() {
	local since=$1 node got=0
	: >"$dir/audit.log"
	for node in $("$KUBECTL" get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[*].metadata.name}'); do
		if sudo -n podman exec "$node" cat "$AUDIT_LOG" 2>/dev/null |
			awk -v s="$since" -v ns="$NS" '$1 >= s && /method="delete"/ && index($0, "/api/v1/namespaces/" ns "/pods/")' >>"$dir/audit.log"; then
			got=$((got + 1))
		else
			missing "audit" "$node：讀不到 $AUDIT_LOG（需要 sudo podman exec）"
		fi
	done
	[ "$got" -gt 0 ] || missing "audit" "沒有任何 control plane 的 audit log"
}

# ---------------------------------------------------------------- start / stop

build() {
	if (cd "$stage" && CGO_ENABLED=0 scripts/go build -o bin/rush-hour ./rush-hour) >/dev/null 2>"$dir/build.err"; then
		return 0
	fi
	if [ -x "$RH" ]; then
		echo "collect-timeline: build failed, using the existing $RH (see $dir/build.err)" >&2
		return 0
	fi
	echo "collect-timeline: cannot build rush-hour (see $dir/build.err)" >&2
	exit 1
}

start() {
	mkdir -p "$dir/app"
	if [ -s "$dir/collectors.pid" ]; then
		echo "collect-timeline: $dir already has collectors.pid; stop it first" >&2
		exit 1
	fi
	rm -f "$dir/.stopping"
	[ -n "${RUSH_HOUR:-}" ] || build
	local since
	since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	printf '{"started_at":"%s","namespace":"%s","context":%s}\n' "$(now_rfc)" "$NS" \
		"$(json_str "$("$KUBECTL" config current-context 2>/dev/null || echo unknown)")" >"$dir/collect.json"
	: >"$dir/collectors.pid"
	set -E
	trap 'abort start' ERR INT TERM
	local want=0

	spawn pods _watch "$dir" pods pods.jsonl -n "$NS" get pods
	spawn endpointslices _watch "$dir" endpointslices endpointslices.jsonl -n "$NS" get endpointslice -l kubernetes.io/service-name=koupa
	spawn events _watch "$dir" events events.jsonl -n "$NS" get events
	spawn applogs _applogs "$dir"
	spawn host _host "$dir"
	want=5

	local pod n=0
	for pod in $("$KUBECTL" -n kube-system get pods -l k8s-app=cilium -o jsonpath='{.items[*].metadata.name}'); do
		spawn "hubble:$pod" _hubble "$dir" "$pod"
		n=$((n + 1)) want=$((want + 1))
	done
	[ "$n" -gt 0 ] || missing hubble "找不到 cilium agent Pod"
	n=0
	for pod in $("$KUBECTL" -n kube-system get pods -l k8s-app=cilium-envoy -o jsonpath='{.items[*].metadata.name}'); do
		spawn "ingress:$pod" _ingress "$dir" "$pod" "$since"
		n=$((n + 1)) want=$((want + 1))
	done
	[ "$n" -gt 0 ] || missing ingress "找不到 cilium-envoy Pod"
	stats envoy-stats-start.txt
	wait_registered "$want"
	trap - ERR INT TERM
	echo "collect-timeline: started $(registered) collectors in $dir (App log followers are added as Pods appear)"
}

# kill_groups SIGNAL: signal every recorded process group.
kill_groups() {
	local pg name
	[ -f "$dir/collectors.pid" ] || return 0
	while read -r pg name; do
		[ -n "$pg" ] && kill "-$1" -- "-$pg" 2>/dev/null || true
	done <"$dir/collectors.pid"
}

alive() {
	local pg name
	[ -f "$dir/collectors.pid" ] || return 1
	while read -r pg name; do
		[ -n "$pg" ] && kill -0 -- "-$pg" 2>/dev/null && return 0
	done <"$dir/collectors.pid"
	return 1
}

stop() {
	[ -d "$dir" ] || {
		echo "collect-timeline: no such directory $dir" >&2
		exit 1
	}
	if [ -s "$dir/collectors.pid" ]; then
		set -E
		trap 'abort stop' ERR INT TERM
		# No collector can register from here on; discovery must be gone
		# before the state at stop is taken and the followers are ended.
		set_stopping
		local pg name i
		while read -r pg name; do
			if [ "$name" = applogs ]; then
				kill -TERM -- "-$pg" 2>/dev/null || true
				for i in $(seq 50); do
					kill -0 -- "-$pg" 2>/dev/null || break
					sleep 0.1
				done
				kill -KILL -- "-$pg" 2>/dev/null || true
			fi
		done <"$dir/collectors.pid"
		"$KUBECTL" -n "$NS" get pods -o json >"$dir/pods-at-stop.json" 2>>"$dir/collectors.err" ||
			{ rm -f "$dir/pods-at-stop.json"; missing pods "停止時取不到 Pod 狀態（pods-at-stop.json）"; }
		stats envoy-stats-end.txt
		audit "$(sed -n 's/.*"started_at":"\([^"]*\)".*/\1/p' "$dir/collect.json" 2>/dev/null)"
		kill_groups TERM
		for i in $(seq 50); do
			alive || break
			sleep 0.1
		done
		alive && kill_groups KILL
		reap_remote
		printf '%s\n' "$(sed 's/}$//' "$dir/collect.json"),\"stopped_at\":\"$(now_rfc)\"}" >"$dir/collect.json.tmp" &&
			mv "$dir/collect.json.tmp" "$dir/collect.json"
		mv "$dir/collectors.pid" "$dir/collectors.pid.stopped"
		rm -f "$dir"/app/.started.*
		trap - ERR INT TERM
	else
		echo "collect-timeline: $dir is not collecting (already stopped?); recomputing meta and timeline"
	fi
	"$RH" coverage "$dir"
	"$RH" timeline "$dir"
	echo "collect-timeline: stopped; $dir/timeline.txt"
}

[ $# -ge 2 ] || usage
cmd=$1
[ "$cmd" = start ] && mkdir -p "$2/app"
dir=$(realpath -m -- "$2")
shift 2
case $cmd in
start) start ;;
stop) stop ;;
_child)
	# _child NAME CMD ARGS...: register this process group, then run CMD.
	name=$1 sub=$2
	shift 2
	with_lock _register "$$" "$name" || exit 0
	case $sub in
	_watch | _hubble | _ingress | _host | _applogs | _follow) "$sub" "$@" ;;
	*) exit 2 ;;
	esac
	;;
*) usage ;;
esac
