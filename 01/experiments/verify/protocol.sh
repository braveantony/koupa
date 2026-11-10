#!/usr/bin/env bash
# SSE protocol cross-check (plan A14 協定, design 5.6): a real browser
# EventSource, Rush Hour's board client and the raw stream (curl -N) on the
# same call numbers.
#
#   experiments/verify/protocol.sh            run both phases, then check
#   experiments/verify/protocol.sh check DIR  only the expectations on DIR
#
# Phase gateway: Chromium (Playwright, pinned container) opens
#   http://<gateway>/board, curl -N reads /events, `rush-hour run` (60 s,
#   20/s, 3 boards) through the Gateway; at second 10 the image is set to v2
#   (step 5 settings: graceful shutdown, resume), so every Pod terminates.
#   Checks: for every pair of connections (browser vs each Rush Hour board,
#   browser vs curl), over the interval both were connected — from the
#   later hello start to the earlier last received ready_seq — the same
#   ready_seq set; no client got a number twice; every connection, by the Pod it was on (X-Koupa-Pod-UID),
#   got exactly one shutdown when that Pod terminated, none while it ran.
# Phase same-pod: a port-forward to one Pod P; the browser page, curl -N and
#   a one-board `rush-hour run --no-ingress` (2/s) all connect to P; then P
#   is deleted (SIGTERM after preStop). Checks: all three are on P, ready_seq
#   equal over the common range, each got exactly one shutdown after P's
#   sigterm log line (browser: dispatched with empty data), the three within
#   2 s of each other, P logged board_closed reason=shutdown for them, and
#   curl's raw stream passes curl-stream.sh check --expect-shutdown.
#
# Evidence: $VERIFY_EVIDENCE/protocol/{gateway,same-pod}/. At exit the
# Deployment is restored to step 5 (v1).
set -uo pipefail
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"
KUBECTL=kubectl
# shellcheck source=../../scripts/lib/kube.sh
. "$stage/scripts/lib/kube.sh"

PW_IMAGE=mcr.microsoft.com/playwright:v1.49.1-noble
V2_IMAGE=ghcr.io/braveantony/koupa:v2
CURL=$verify_dir/curl-stream.sh
PW_NAMES=()
RH_PIDS=()

verify_cleanup() {
	local rc=$? n p
	for n in "${PW_NAMES[@]}"; do podman rm -f -t 2 "$n" >/dev/null 2>&1; done
	for p in "${RH_PIDS[@]}"; do kill -TERM "$p" 2>/dev/null; done
	(exit "$rc")
	crit_cleanup
}
trap verify_cleanup EXIT

# browser_start NAME URL DIR: the recording test in the background; waits
# until the page got hello. Output DIR/NAME.json (+ .partial, .started).
browser_start() {
	local name=$1 url=$2 dir=$3 cname=verify-pw-$1-$$ i
	rm -f "$dir/$name.stop"
	podman run -d --name "$cname" --network host --ipc host --init \
		-v "$verify_dir:/work/verify:ro,z" -v "$stage/experiments/browser/node_modules:/work/node_modules:ro,z" \
		-v "$dir:/out:z" -w /work/verify \
		-e BOARD_URL="$url" -e OUT_FILE="/out/$name.json" -e STOP_FILE="/out/$name.stop" -e MAX_SECONDS=900 \
		"$PW_IMAGE" /work/node_modules/.bin/playwright test -c playwright.config.ts >/dev/null || return 1
	PW_NAMES+=("$cname")
	for i in $(seq 1 120); do
		[ -f "$dir/$name.json.started" ] && { log "browser $name: hello received ($url)"; return 0; }
		[ "$(podman inspect -f '{{.State.Running}}' "$cname" 2>/dev/null)" = true ] || break
		sleep 0.5
	done
	podman logs "$cname" >"$dir/$name.playwright.log" 2>&1
	log "browser $name did not get hello"
	return 1
}

browser_stop() {
	local name=$1 dir=$2 cname=verify-pw-$1-$$
	touch "$dir/$name.stop"
	timeout 60 podman wait "$cname" >"$dir/$name.playwright.exit" 2>&1
	podman logs "$cname" >"$dir/$name.playwright.log" 2>&1
	podman rm -f -t 2 "$cname" >/dev/null 2>&1
}

# rh_start DIR ARGS...: rush-hour run in the background (sets RH_PID) and
# wait for SENDING_STARTED (sets T0, Unix ns).
rh_start() {
	local dir=$1 i
	shift
	"$RH_BIN" run "$@" --out "$dir/rh" >"$dir/rh.stdout" 2>"$dir/rh.stderr" &
	RH_PID=$!
	RH_PIDS+=("$RH_PID")
	T0=""
	for i in $(seq 1 900); do
		T0=$(sed -n 's/^SENDING_STARTED \([0-9]*\)$/\1/p' "$dir/rh.stdout" 2>/dev/null | head -1)
		[ -n "$T0" ] && return 0
		kill -0 "$RH_PID" 2>/dev/null || break
		sleep 0.1
	done
	return 1
}

pods_json() { kubectl -n "$NS" get pods -l "$APP_SELECTOR" -o json >"$1"; }

phase_gateway() {
	local dir=$VERIFY_EVIDENCE/protocol/gateway base cpid rc
	mkdir -p "$dir"
	base=$(gateway_base) || { orch_fail "$dir" "no Gateway address"; return 1; }
	echo "$base" >"$dir/base.txt"
	pods_json "$dir/pods-before.json"
	browser_start gateway "$base/board" "$dir" || { orch_fail "$dir" "browser did not start"; return 1; }
	"$CURL" capture "$base/events" "$dir/curl-gateway" 400 2>"$dir/curl-gateway.err" &
	cpid=$!
	BG_PIDS+=("$cpid")
	if ! rh_start "$dir" --base "$base" --run "verify-proto-gw-$(date -u +%H%M%S)" --duration 60s --rate 20 --boards 3; then
		orch_fail "$dir" "rush-hour did not start sending"
		return 1
	fi
	local at=$((T0 + 10000000000)) d
	d=$(((at - $(date +%s%N)) / 1000000))
	[ "$d" -gt 0 ] && sleep "$(awk -v d="$d" 'BEGIN{printf "%.3f", d/1000}')"
	log "second 10: set image $V2_IMAGE (step 5 settings)"
	date -u +%Y-%m-%dT%H:%M:%S.%NZ >"$dir/change-at.txt"
	kubectl -n "$NS" set image deploy/koupa "koupa=$V2_IMAGE" >&2 || orch_fail "$dir" "set image failed"
	local deadline=$(($(date +%s) + 120))
	until [ "$(deploy_status)" = done ] && old_pods_gone; do
		[ "$(date +%s)" -lt "$deadline" ] || { orch_fail "$dir" "rollout to v2 not complete in 120 s"; break; }
		sleep 1
	done
	log "rollout to v2 complete; waiting for rush-hour"
	timeout 600 tail --pid="$RH_PID" -f /dev/null
	wait "$RH_PID"
	rc=$?
	log "rush-hour exited $rc"
	sleep 2
	browser_stop gateway "$dir"
	kill "$cpid" 2>/dev/null
	wait "$cpid" 2>/dev/null
	pods_json "$dir/pods-after.json"
}

phase_same_pod() {
	local dir=$VERIFY_EVIDENCE/protocol/same-pod p uid lp cpid i
	mkdir -p "$dir"
	wait_settled 240 || { orch_fail "$dir" "Pods not settled"; return 1; }
	p=$(koupa_pods | head -1)
	uid=$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.metadata.uid}')
	echo "$p $uid" >"$dir/pod.txt"
	pf "$p" 8080 || { orch_fail "$dir" "port-forward to $p failed"; return 1; }
	lp=$PF_PORT
	kubectl -n "$NS" logs -f --timestamps "$p" >"$dir/app-$uid.log" 2>"$dir/app.err" &
	BG_PIDS+=($!)
	browser_start same-pod "http://127.0.0.1:$lp/board" "$dir" || { orch_fail "$dir" "browser did not start"; return 1; }
	"$CURL" capture "http://127.0.0.1:$lp/events" "$dir/curl-pod" 300 2>"$dir/curl-pod.err" &
	cpid=$!
	BG_PIDS+=("$cpid")
	if ! rh_start "$dir" --base "http://127.0.0.1:$lp" --run "verify-proto-pod-$(date -u +%H%M%S)" --duration 60s --rate 2 --boards 1 --no-ingress; then
		orch_fail "$dir" "rush-hour did not start sending"
		return 1
	fi
	sleep 10
	log "deleting $p (graceful: preStop, then SIGTERM)"
	date -u +%Y-%m-%dT%H:%M:%S.%NZ >"$dir/delete-at.txt"
	kubectl -n "$NS" delete pod "$p" --wait=false >&2 || orch_fail "$dir" "delete pod failed"
	for i in $(seq 1 120); do
		grep -q '"k":"shutdown"' "$dir/same-pod.json.partial" 2>/dev/null &&
			grep -q '"kind":"shutdown"' "$dir/rh/boards.jsonl" 2>/dev/null &&
			grep -q '^event: shutdown$' "$dir/curl-pod.raw" 2>/dev/null && break
		sleep 0.5
	done
	[ "$i" -lt 120 ] || log "not every client saw shutdown within 60 s"
	sleep 2
	browser_stop same-pod "$dir"
	kill -TERM "$RH_PID" 2>/dev/null
	timeout 30 tail --pid="$RH_PID" -f /dev/null || kill -KILL "$RH_PID" 2>/dev/null
	wait "$RH_PID" 2>/dev/null
	kill "$cpid" 2>/dev/null
	wait "$cpid" 2>/dev/null
}

check_protocol() {
	local root=$1 rc=0
	for f in gateway/curl-gateway same-pod/curl-pod; do
		[ -f "$root/$f.raw" ] || continue
		local exp=""
		# The gateway connection's Pod terminated in the rollout; the
		# same-pod one was deleted.
		exp=--expect-shutdown
		"$CURL" check "$root/$f.raw" $exp --json "$root/$f.check.json" >"$root/$f.check.txt" 2>&1
		sed 's/^/    curl /' "$root/$f.check.txt" >&2
	done
	vcheck "$root" <<'PY' || rc=1
import collections, datetime, re
def T(s):
    m = re.match(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$", (s or "").strip())
    if not m:
        return None
    frac = (m.group(2) or "0")[:6].ljust(6, "0")
    return datetime.datetime.fromisoformat(m.group(1) + "." + frac + ("+00:00" if m.group(3) == "Z" else m.group(3))).timestamp()
def browser(path):
    """{conn: {pod, start, seqs, shutdown: [t], shutdown_data}} from the Playwright recording."""
    b = load(path)
    if not b:
        return None
    resp = [r for r in b["responses"] if r["status"] == 200]
    conns = {}
    for e in b["events"]:
        c = conns.setdefault(e["conn"], {"pod": None, "start": None, "seqs": [], "shutdown": [], "shutdown_data": []})
        if e["k"] == "open" and e["conn"] - 1 < len(resp):
            c["pod"] = resp[e["conn"] - 1]["pod_uid"]
        elif e["k"] == "hello":
            c["start"] = int(e["id"])
        elif e["k"] == "ready":
            c["seqs"].append(int(e["id"]))
        elif e["k"] == "shutdown":
            c["shutdown"].append(e["t"] / 1000.0)
            c["shutdown_data"].append(e["data"])
    return {k: v for k, v in conns.items() if k > 0}
def rh(path):
    """{(board, conn): {...}} from Rush Hour's boards.jsonl."""
    conns = {}
    for e in jsonl(path):
        c = conns.setdefault((e["board"], e["conn"]), {"pod": None, "start": None, "seqs": [], "shutdown": []})
        if e.get("pod_uid"):
            c["pod"] = e["pod_uid"]
        if e["kind"] == "hello":
            c["start"] = e["start"]
        elif e["kind"] == "ready":
            c["seqs"].append(e["ready_seq"])
        elif e["kind"] == "shutdown":
            c["shutdown"].append(T(e["at"]))
    return conns
def compare(name_a, conns_a, name_b, conns_b):
    """Compare per pair of connections over the interval both were
    connected. A connection's recorded boundaries are its hello start and
    the last ready_seq it received: it covered (start, last]. For each pair
    whose coverage overlaps, (max start, min last] is a range both received
    through, so any number in it that only one got is a real difference;
    ranges where one client was between connections are not compared."""
    ca = [c for c in conns_a.values() if c["start"] is not None]
    cb = [c for c in conns_b.values() if c["start"] is not None]
    if not ca or not cb or not any(c["seqs"] for c in ca) or not any(c["seqs"] for c in cb):
        ok(False, "%s vs %s: both sides have a connection with a start and ready events" % (name_a, name_b))
        return None
    for nm, cs in ((name_a, ca), (name_b, cb)):
        allseq = [x for c in cs for x in c["seqs"]]
        dup = sorted(x for x, n in collections.Counter(allseq).items() if n > 1)
        ok(not dup, "%s: no ready_seq received twice (over all its connections): %s" % (nm, dup[:5]))
    out = []
    for a in ca:
        for b in cb:
            if not a["seqs"] or not b["seqs"]:
                continue
            lo, hi = max(a["start"], b["start"]), min(max(a["seqs"]), max(b["seqs"]))
            if hi <= lo:
                continue
            sa = {x for x in a["seqs"] if lo < x <= hi}
            sb = {x for x in b["seqs"] if lo < x <= hi}
            only_a, only_b = sorted(sa - sb), sorted(sb - sa)
            ok(not only_a and not only_b and sa,
               "%s conn@%s vs %s conn@%s: same ready_seq over the interval both were connected (%d, %d]: %d numbers; only %s %s, only %s %s"
               % (name_a, (a["pod"] or "?")[:8], name_b, (b["pod"] or "?")[:8], lo, hi, len(sa | sb), name_a, only_a[:5], name_b, only_b[:5]))
            out.append({"pair": [name_a, name_b], "pods": [a["pod"], b["pod"]], "range": [lo, hi],
                        "common": len(sa & sb), "only_a": only_a, "only_b": only_b})
    ok(bool(out), "%s vs %s: at least one interval where both were connected" % (name_a, name_b))
    return out
def curl(prefix):
    c = load(prefix + ".check.json") or {}
    pod = None
    try:
        for l in open(os.path.join(D, prefix + ".headers")):
            if l.lower().startswith("x-koupa-pod-uid:"):
                pod = l.split(":", 1)[1].strip()
    except OSError:
        pass
    return c, pod
result = {}
# ------------------------------------------------ gateway
before = load("gateway/pods-before.json") or {"items": []}
after = load("gateway/pods-after.json") or {"items": []}
old = {p["metadata"]["uid"] for p in before["items"]}
alive = {p["metadata"]["uid"] for p in after["items"] if not p["metadata"].get("deletionTimestamp")}
term = old - alive
ok(old and term == old, "gateway: every Pod present at the start terminated in the rollout (%d of %d)" % (len(term), len(old)))
B = browser("gateway/gateway.json")
ok(B is not None, "gateway: the browser recording exists")
R = rh("gateway/rh/boards.jsonl")
ok(len({b for b, _ in R}) == 3, "gateway: Rush Hour recorded 3 boards")
cc, cpod = curl("gateway/curl-gateway")
ok(not cc.get("failed") and cc, "gateway: curl raw stream grammar (with shutdown): %s" % (cc.get("failed") or ("ok" if cc else "no check result")))
pairs = []
if B:
    for board in sorted({b for b, _ in R}):
        pairs.append(compare("browser", B, board, {k: v for k, v in R.items() if k[0] == board}))
    if cc.get("seqs"):
        pairs.append(compare("browser", B, "curl", {1: {"pod": cpod, "start": cc["start"], "seqs": cc["seqs"]}}))
    for n, c in sorted(B.items()):
        want = 1 if c["pod"] in term else 0
        ok(len(c["shutdown"]) == want, "gateway: browser connection %d on Pod %s (%s) got %d shutdown, want %d"
           % (n, (c["pod"] or "?")[:8], "terminated" if c["pod"] in term else "still running", len(c["shutdown"]), want))
        ok(all(d == "" for d in c["shutdown_data"]), "gateway: browser connection %d: shutdown dispatched with empty data" % n)
for (board, n), c in sorted(R.items()):
    want = 1 if c["pod"] in term else 0
    ok(len(c["shutdown"]) == want, "gateway: %s connection %d on Pod %s (%s) got %d shutdown, want %d"
       % (board, n, (c["pod"] or "?")[:8], "terminated" if c["pod"] in term else "still running", len(c["shutdown"]), want))
ok(cpod in term, "gateway: curl's Pod %s terminated (so its stream must end with shutdown)" % (cpod or "?")[:8])
result["gateway"] = {"terminated_pods": sorted(term), "pairs": pairs,
                     "browser_connections": {n: {"pod": c["pod"], "start": c["start"], "ready": len(c["seqs"]), "shutdown": len(c["shutdown"])} for n, c in (B or {}).items()},
                     "rush_hour_connections": {"%s#%d" % k: {"pod": c["pod"], "start": c["start"], "ready": len(c["seqs"]), "shutdown": len(c["shutdown"])} for k, c in R.items()},
                     "curl": {"pod": cpod, "ready": cc.get("ready"), "shutdown": cc.get("shutdown"), "retry": cc.get("retry")}}
# ------------------------------------------------ same pod
try:
    pname, puid = open(os.path.join(D, "same-pod/pod.txt")).read().split()
except (OSError, ValueError):
    pname = puid = None
ok(puid is not None, "same-pod: the Pod under test is recorded")
B2 = browser("same-pod/same-pod.json") or {}
R2 = rh("same-pod/rh/boards.jsonl")
c2, c2pod = curl("same-pod/curl-pod")
b1 = B2.get(1, {"pod": None, "start": None, "seqs": [], "shutdown": [], "shutdown_data": []})
r1 = R2.get(("board-1", 1), {"pod": None, "start": None, "seqs": [], "shutdown": []})
ok(puid is not None and b1["pod"] == puid and r1["pod"] == puid and c2pod == puid,
   "same-pod: browser, Rush Hour and curl are all on %s (%s, %s, %s)" % ((puid or "?")[:8], (b1["pod"] or "?")[:8], (r1["pod"] or "?")[:8], (c2pod or "?")[:8]))
p2 = [compare("browser", {1: b1}, "board-1", {k: v for k, v in R2.items() if k[0] == "board-1"})]
if c2.get("seqs"):
    p2.append(compare("browser", {1: b1}, "curl", {1: {"pod": c2pod, "start": c2["start"], "seqs": c2["seqs"]}}))
ok(len(b1["shutdown"]) == 1 and b1["shutdown_data"] == [""], "same-pod: the browser got exactly one shutdown, dispatched with empty data: %r" % b1["shutdown_data"])
ok(len(r1["shutdown"]) == 1, "same-pod: Rush Hour board-1 got exactly one shutdown: %d" % len(r1["shutdown"]))
ok(not c2.get("failed") and c2, "same-pod: curl raw stream grammar (with shutdown): %s" % (c2.get("failed") or ("ok" if c2 else "no check result")))
app = []
try:
    for l in open(os.path.join(D, "same-pod/app-%s.log" % puid), errors="replace"):
        i = l.find("{")
        if i >= 0:
            try:
                app.append(json.loads(l[i:]))
            except ValueError:
                pass
except (OSError, TypeError):
    pass
sig = [T(m["ts"]) for m in app if m.get("event") == "sigterm"]
closed = [m for m in app if m.get("event") == "board_closed" and m.get("reason") == "shutdown"]
ok(len(sig) == 1, "same-pod: the Pod logged sigterm once: %d" % len(sig))
ok(len(closed) >= 3, "same-pod: the Pod logged board_closed reason=shutdown for the three clients: %d" % len(closed))
times = {"browser": b1["shutdown"][:1], "rush-hour": r1["shutdown"][:1]}
if sig:
    for k, v in times.items():
        ok(v and sig[0] - 0.2 <= v[0] <= sig[0] + 20, "same-pod: %s got shutdown %.3f s after sigterm" % (k, (v[0] - sig[0]) if v else float("nan")))
    if all(times.values()):
        spread = max(v[0] for v in times.values()) - min(v[0] for v in times.values())
        ok(spread <= 2, "same-pod: browser and Rush Hour got shutdown within 2 s of each other: %.3f s" % spread)
result["same_pod"] = {"pod": puid, "pairs": p2, "sigterm": sig[:1], "board_closed_shutdown": len(closed),
                      "browser": {"start": b1["start"], "ready": len(b1["seqs"]), "shutdown_at": b1["shutdown"]},
                      "rush_hour": {"start": r1["start"], "ready": len(r1["seqs"]), "shutdown_at": r1["shutdown"]},
                      "curl": {"ready": c2.get("ready"), "shutdown": c2.get("shutdown"), "retry": c2.get("retry")}}
json.dump(result, open(os.path.join(D, "protocol-result.json"), "w"), indent=2)
PY
	return $rc
}

if [ "${1:-}" = check ]; then
	[ -n "${2:-}" ] || { echo "usage: $0 check DIR" >&2; exit 64; }
	CRIT_NO_RESTORE=1
	check_protocol "$2"
	exit $?
fi

build_rush_hour || { log "cannot build rush-hour"; exit 1; }
root=$VERIFY_EVIDENCE/protocol
mkdir -p "$root"
podman image inspect --format '{{.Id}} {{index .RepoDigests 0}}' "$PW_IMAGE" >"$root/playwright-image.txt" 2>&1
# Start from step 5 (v1) without test settings.
restore_step5 || { log "cannot restore step 5 before the run"; exit 1; }
phase_gateway
phase_same_pod
stop_bg
rc=0
check_protocol "$root" || rc=1
verdict protocol all "$root" "$rc"
