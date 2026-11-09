#!/usr/bin/env bash
# Design 5.5 row 5 / plan A12 timing 5: one board stops reading, a write to
# it is really blocked, and the program gets SIGTERM.
#   slow-board (a test Pod) connects straight to one koupa Pod's IP, no
#   ingress: two boards read normally, one stops after hello. In test mode
#   the App sends a 64 KiB comment line every 10 ms and logs write_blocked
#   when a write has not returned after 1s. On that line:
#   kubectl delete pod <target> --wait=false -> wait for its SIGTERM line,
#   timing starts there -> both healthy boards got shutdown, the program
#   ended within SHUTDOWN_TIMEOUT plus the recorded margin, delete->exit
#   within the grace period -> wait for the Pod to end.
# Expected (mechanism on; the design has no "off" expectation for this row).
. "$(dirname "$0")/lib.sh"
exp=05-blocked-write
SHUTDOWN_TIMEOUT_S=15 GRACE_S=40
# Margin on "ended within SHUTDOWN_TIMEOUT": both times are the App's own
# log timestamps (same clock); 1s covers the exit path after Shutdown
# returns and log flushing.
MARGIN_MS=1000
build_tools || exit 1
setting=on
d=$(setting_dir $exp $setting)
run() {
	deploy blocked-write "$d" || return 1
	follow_logs "$d/logs" || return 1
	local target ip sb
	target=$(koupa_pods | head -1)
	ip=$(kubectl -n "$NS" get pod "$target" -o jsonpath='{.status.podIP}')
	[ -n "$ip" ] || return 1
	sb=crit-slow-board-$(date +%H%M%S)
	TEST_PODS+=("$sb")
	cat >"$d/slow-board-pod.yaml" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $sb
  namespace: $NS
  labels: {app.kubernetes.io/name: crit-slow-board}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - {key: node-role.kubernetes.io/control-plane, operator: DoesNotExist}
  containers:
    - name: slow-board
      image: $CRIT_SLOW_BOARD_IMAGE
      args: ["--target", "$ip:8080", "--healthy", "2", "--duration", "5m"]
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
YAML
	# Count earlier lines: only a write_blocked after slow-board started counts.
	local n0
	n0=$(grep -c '"event":"write_blocked"' "$d/logs/$target.log" 2>/dev/null)
	printf '{"target_pod":"%s","target_ip":"%s","slow_board":"%s"' "$target" "$ip" "$sb" >"$d/result.json"
	timeout 30 kubectl apply -f "$d/slow-board-pod.yaml" >&2 || return 1
	timeout 90 kubectl -n "$NS" wait --for=condition=Ready "pod/$sb" --timeout=80s >&2 || return 1
	kubectl -n "$NS" logs -f "$sb" >"$d/slow-board.jsonl" 2>/dev/null &
	BG_PIDS+=($!)
	local f=$d/slow-board.jsonl l=$d/logs/$target.log
	wait_grep "$f" '"board":"healthy-1","conn":[0-9]+,"event":"hello"' 30 && wait_grep "$f" '"board":"healthy-2","conn":[0-9]+,"event":"hello"' 30 &&
		wait_grep "$f" '"event":"stuck_open"' 30 || return 1
	log "boards connected to $target ($ip); waiting for write_blocked"
	local deadline0=$(($(date +%s) + 60))
	until [ "$(grep -c '"event":"write_blocked"' "$l")" -gt "${n0:-0}" ]; do
		[ "$(date +%s)" -lt "$deadline0" ] || { log "no new write_blocked within 60s"; return 1; }
		sleep 0.1
	done
	local del
	del=$(now_rfc)
	log "write_blocked seen; kubectl delete pod $target --wait=false"
	timeout 30 kubectl -n "$NS" delete pod "$target" --wait=false >&2 || return 1
	printf ',"deleted_at":"%s"' "$del" >>"$d/result.json"
	wait_grep "$l" '"event":"sigterm"' 60 || return 1
	log "SIGTERM logged; timing starts"
	wait_grep "$l" '"event":"exit"' $((SHUTDOWN_TIMEOUT_S + 20)) || return 1
	local deadline=$(($(date +%s) + 60)) fin=""
	while [ "$(date +%s)" -lt "$deadline" ]; do
		fin=$(kubectl -n "$NS" get pod "$target" -o jsonpath='{.status.containerStatuses[0].state.terminated.finishedAt}' 2>/dev/null)
		[ -n "$fin" ] && break
		kubectl -n "$NS" get pod "$target" >/dev/null 2>&1 || break
		sleep 0.5
	done
	printf ',"finished_at":"%s"' "$fin" >>"$d/result.json"
	timeout 90 kubectl -n "$NS" wait --for=delete "pod/$target" --timeout=80s >&2 && printf ',"pod_gone":true' >>"$d/result.json"
	sleep 1
}
run || log "$exp: a step failed or timed out (evidence kept)"
[ -f "$d/result.json" ] && echo '}' >>"$d/result.json"
export SHUTDOWN_TIMEOUT_S GRACE_S MARGIN_MS
check "$d" <<'PY'
SHUTDOWN_MS, GRACE_MS, MARGIN_MS = int(os.environ["SHUTDOWN_TIMEOUT_S"]) * 1000, int(os.environ["GRACE_S"]) * 1000, int(os.environ["MARGIN_MS"])
t = T
def ms(a, b): return (a - b).total_seconds() * 1000
pod = R.get("target_pod")
L = lines(pod) if pod else []
sig = next((l for l in L if l.get("event") == "sigterm"), None)
ext = next((l for l in L if l.get("event") == "exit"), None)
blocked = [l for l in L if l.get("event") == "write_blocked"]
ok(bool(blocked), "the App logged write_blocked on the target Pod (%d lines)" % len(blocked))
ok(sig is not None, "the target Pod logged SIGTERM")
if sig:
    ts = t(sig["ts"])
    ends = {(l.get("conn"), l.get("write")): t(l["ts"]) for l in L if l.get("event") == "write_end"}
    stuck = [b for b in blocked if t(b["started_at"]) < ts and (ends.get((b.get("conn"), b.get("write"))) is None or ends[(b.get("conn"), b.get("write"))] > ts)]
    ok(bool(stuck), "a write was blocked when SIGTERM arrived (%s)" % [(b.get("conn"), b.get("write"), b.get("started_at")) for b in stuck[:3]])
    sb = []
    try:
        sb = [json.loads(x) for x in open(os.path.join(D, "slow-board.jsonl")) if x.startswith("{")]
    except OSError:
        pass
    for name in ("healthy-1", "healthy-2"):
        got = [e for e in sb if e["board"] == name and e["event"] == "shutdown" and t(e["t"]) >= ts]
        ok(bool(got), "%s got shutdown after SIGTERM (%s)" % (name, got[0]["t"] if got else None))
    if ext:
        d = ms(t(ext["ts"]), ts)
        ok(d <= SHUTDOWN_MS + MARGIN_MS, "program ended %.0f ms after SIGTERM (limit SHUTDOWN_TIMEOUT %d ms + margin %d ms)" % (d, SHUTDOWN_MS, MARGIN_MS))
    else:
        ok(False, "the target Pod logged exit")
    end = t(ext["ts"]) if ext else None
    fin = t(R.get("finished_at")) if R.get("finished_at") else None
    last = max([x for x in (end, fin) if x] or [None]) if (end or fin) else None
    if last and R.get("deleted_at"):
        d = ms(last, t(R["deleted_at"]))
        # finishedAt has 1s precision: add 1s to it before comparing.
        if fin and last == fin:
            d += 1000
        ok(d <= GRACE_MS, "delete -> program end %.0f ms <= grace %d ms (exit %s, finishedAt %s)" % (d, GRACE_MS, ext and ext["ts"], R.get("finished_at")))
    else:
        ok(False, "delete and program end times recorded")
    ok(R.get("pod_gone") is True, "the Pod ended and was removed")
PY
verdict $exp $setting "$d" $?
