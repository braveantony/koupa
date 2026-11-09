#!/usr/bin/env bash
# Pre-measurement checks against the LIVE cluster (plan A8 step 2).
#
#   scripts/preflight.sh <step-0..step-4|control> [--image IMAGE]
#
# Run after the step's manifest is applied and the rollout has finished.
# IMAGE (default ghcr.io/braveantony/koupa:v1) is the image the Pods must run.
# The six checks, each printed as "ok <name>: ..." or "FAIL <name>: ...":
#   generation     Deployment observedGeneration == generation
#   replicaset     every koupa Pod belongs to the current ReplicaSet, runs, is
#                  Ready, and there are spec.replicas of them
#   spec           image, env, preStop and terminationGracePeriodSeconds equal
#                  the rendered overlay (so previous-step-only settings are gone)
#   timing         GRACEFUL_SHUTDOWN on: preStop + SHUTDOWN_TIMEOUT + 5 < grace
#                  (A5); otherwise an enabled preStop < grace
#   test-switches  TEST_HOOKS and TEST_MUTATION are not set (A12, A14)
#   tx-timeout     every Pod's log shows transaction_timeout = 60s ("1min")
#                  and its startup line has no test switch on (A6)
# Exit 0 all passed, 1 a check failed, 2 usage or render error.
set -euo pipefail
stage=$(cd "$(dirname "$0")/.." && pwd)
NS=${KOUPA_NS:-koupa}
KUBECTL=${KUBECTL:-kubectl}

usage() {
	echo "usage: $0 <step-0..step-4|control> [--image IMAGE]" >&2
	exit 2
}

step=${1:-}
[ $# -ge 1 ] && shift
image=ghcr.io/braveantony/koupa:v1
while [ $# -gt 0 ]; do
	case $1 in
	--image) [ $# -ge 2 ] || usage; image=$2; shift 2 ;;
	*) usage ;;
	esac
done
case $step in
step-0 | control) dir=$stage/k8s/base ;;
step-[1-4]) dir=$stage/k8s/steps/$step ;;
*) usage ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

"$KUBECTL" kustomize "$dir" >"$tmp/rendered.yaml" 2>"$tmp/err" || {
	echo "preflight: kubectl kustomize $dir failed: $(cat "$tmp/err")" >&2
	exit 2
}
"$KUBECTL" -n "$NS" get deploy koupa -o json >"$tmp/deploy.json" 2>"$tmp/err" || {
	echo "FAIL generation: cannot read Deployment koupa: $(cat "$tmp/err")"
	exit 1
}
"$KUBECTL" -n "$NS" get rs -l app.kubernetes.io/name=koupa -o json >"$tmp/rs.json"
"$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name=koupa -o json >"$tmp/pods.json"
mkdir -p "$tmp/logs"
for pod in $(python3 -I -c 'import json,sys; print(" ".join(p["metadata"]["name"] for p in json.load(open(sys.argv[1]))["items"]))' "$tmp/pods.json"); do
	"$KUBECTL" -n "$NS" logs "$pod" -c koupa >"$tmp/logs/$pod.log" 2>"$tmp/logs/$pod.err" || true
done

python3 -I - "$step" "$image" "$tmp" <<'PY'
import json, os, re, sys, yaml

step, image, tmp = sys.argv[1:]
failures = []

def report(name, ok, msg):
    print(("ok   " if ok else "FAIL ") + name + ": " + msg)
    if not ok:
        failures.append(name)

def seconds(v):
    m = re.fullmatch(r"(\d+)(ms|s|m)", v or "")
    if not m:
        return None
    n, unit = int(m.group(1)), m.group(2)
    return {"ms": n / 1000, "s": n, "m": n * 60}[unit]

docs = [d for d in yaml.safe_load_all(open(os.path.join(tmp, "rendered.yaml"))) if d]
want_dep = [d for d in docs if d.get("kind") == "Deployment" and d["metadata"]["name"] == "koupa"][0]
dep = json.load(open(os.path.join(tmp, "deploy.json")))
rss = json.load(open(os.path.join(tmp, "rs.json")))["items"]
pods = json.load(open(os.path.join(tmp, "pods.json")))["items"]

# 1. generation
gen = dep["metadata"].get("generation")
obs = dep.get("status", {}).get("observedGeneration")
report("generation", gen is not None and obs == gen,
       "observedGeneration %s, generation %s" % (obs, gen))

# 2. replicaset: the current RS carries the Deployment's revision.
rev = dep["metadata"].get("annotations", {}).get("deployment.kubernetes.io/revision")
def owned(o, kind, name):
    return any(r.get("kind") == kind and r.get("name") == name and r.get("controller")
               for r in o["metadata"].get("ownerReferences", []))
cur = [r for r in rss if owned(r, "Deployment", "koupa")
       and r["metadata"].get("annotations", {}).get("deployment.kubernetes.io/revision") == rev]
want_n = dep["spec"].get("replicas", 1)
if len(cur) != 1:
    report("replicaset", False, "no single ReplicaSet with revision %s (found %d)" % (rev, len(cur)))
    cur_name = None
else:
    cur_name = cur[0]["metadata"]["name"]
    bad = []
    for p in pods:
        name = p["metadata"]["name"]
        if not owned(p, "ReplicaSet", cur_name):
            owner = ",".join(r.get("name", "?") for r in p["metadata"].get("ownerReferences", []))
            bad.append("%s belongs to %s" % (name, owner or "nothing"))
        elif p["metadata"].get("deletionTimestamp"):
            bad.append("%s is terminating" % name)
        elif p.get("status", {}).get("phase") != "Running":
            bad.append("%s is %s" % (name, p.get("status", {}).get("phase")))
        elif not any(c.get("type") == "Ready" and c.get("status") == "True"
                     for c in p["status"].get("conditions", [])):
            bad.append("%s is not Ready" % name)
    if len(pods) != want_n:
        bad.append("%d Pods, want %d" % (len(pods), want_n))
    report("replicaset", not bad,
           "; ".join(bad) if bad else "%d Pods, all of ReplicaSet %s (revision %s)" % (len(pods), cur_name, rev))

# 3. spec: live Deployment template (and every Pod) equals the rendered one.
def container(podspec):
    cs = [c for c in podspec.get("containers", []) if c["name"] == "koupa"]
    return cs[0] if cs else {}

def env_map(c):
    out = {}
    for e in c.get("env", []) or []:
        if "value" in e:
            out[e["name"]] = ("value", e["value"])
        else:
            vf = e.get("valueFrom", {})
            if "fieldRef" in vf:
                out[e["name"]] = ("fieldRef", vf["fieldRef"].get("fieldPath"))
            elif "secretKeyRef" in vf:
                out[e["name"]] = ("secretKeyRef", vf["secretKeyRef"].get("name"), vf["secretKeyRef"].get("key"))
            else:
                out[e["name"]] = ("valueFrom", json.dumps(vf, sort_keys=True))
    return out

def prestop(c):
    s = ((c.get("lifecycle") or {}).get("preStop") or {}).get("sleep", {}).get("seconds")
    return s

def settings(podspec):
    c = container(podspec)
    return {"image": c.get("image"), "env": env_map(c), "preStop": prestop(c),
            "lifecycle": c.get("lifecycle") or None,
            "grace": podspec.get("terminationGracePeriodSeconds", 30)}

want = settings(want_dep["spec"]["template"]["spec"])
want["image"] = image
def diff(have, label):
    out = []
    if have["image"] != want["image"]:
        out.append("%s image %s, want %s" % (label, have["image"], want["image"]))
    for k in sorted(set(have["env"]) | set(want["env"])):
        h, w = have["env"].get(k), want["env"].get(k)
        if h == w:
            continue
        if w is None:
            out.append("%s env %s=%s is set but not in %s (left over from another step?)" % (label, k, h[-1], step))
        elif h is None:
            out.append("%s env %s missing (want %s)" % (label, k, w[-1]))
        else:
            out.append("%s env %s=%s, want %s" % (label, k, h[-1], w[-1]))
    if have["preStop"] != want["preStop"] or (want["lifecycle"] is None) != (have["lifecycle"] is None):
        out.append("%s preStop %s, want %s" % (label, have["preStop"] or have["lifecycle"], want["preStop"]))
    if have["grace"] != want["grace"]:
        out.append("%s terminationGracePeriodSeconds %s, want %s" % (label, have["grace"], want["grace"]))
    return out

live = settings(dep["spec"]["template"]["spec"])
problems = diff(live, "Deployment")
for p in pods:
    s = settings(p.get("spec", {}))
    problems += diff(s, "Pod " + p["metadata"]["name"])
    st = [c for c in p.get("status", {}).get("containerStatuses", []) if c.get("name") == "koupa"]
    if st and st[0].get("image") not in (image, "docker.io/" + image):
        problems.append("Pod %s runs image %s, want %s" % (p["metadata"]["name"], st[0].get("image"), image))
report("spec", not problems, "; ".join(problems) if problems else
       "image %s, env %s, preStop %s, grace %s match %s" % (image, ",".join(sorted(want["env"])), want["preStop"], want["grace"], step))

# 4. timing (live values)
pre = live["preStop"] or 0
grace = live["grace"]
gs = live["env"].get("GRACEFUL_SHUTDOWN")
if gs == ("value", "1"):
    st_raw = live["env"].get("SHUTDOWN_TIMEOUT", ("value", "15s"))[-1]
    st = seconds(st_raw)
    report("timing", st is not None and pre + st + 5 < grace,
           "A5: preStop %s + SHUTDOWN_TIMEOUT %s + 5 < grace %s" % (pre, st_raw, grace))
elif pre:
    report("timing", pre < grace, "preStop %s < grace %s" % (pre, grace))
else:
    report("timing", True, "no preStop, GRACEFUL_SHUTDOWN off: nothing to check")

# 5. test switches
on = sorted(k for k in ("TEST_HOOKS", "TEST_MUTATION") if k in live["env"])
for p in pods:
    on += sorted("%s (Pod %s)" % (k, p["metadata"]["name"]) for k in ("TEST_HOOKS", "TEST_MUTATION")
                 if k in env_map(container(p.get("spec", {}))))
report("test-switches", not on, ("set: " + ", ".join(on)) if on else "TEST_HOOKS and TEST_MUTATION not set")

# 6. transaction_timeout from every Pod's log
bad = []
for p in pods:
    name = p["metadata"]["name"]
    path = os.path.join(tmp, "logs", name + ".log")
    lines = []
    try:
        for raw in open(path, encoding="utf-8", errors="replace"):
            i = raw.find("{")
            if i < 0:
                continue
            try:
                lines.append(json.loads(raw[i:]))
            except ValueError:
                pass
    except OSError:
        pass
    if not lines:
        err = ""
        try:
            err = open(os.path.join(tmp, "logs", name + ".err")).read().strip()
        except OSError:
            pass
        bad.append("%s: no log (%s)" % (name, err or "empty"))
        continue
    checks = [l for l in lines if l.get("event") == "tx_timeout_check"]
    if not any(l.get("ok") is True and l.get("got") == "1min" for l in checks):
        bad.append("%s: no tx_timeout_check ok with got=1min" % name)
    if any(l.get("ok") is not True for l in checks):
        bad.append("%s: a tx_timeout_check failed" % name)
    starts = [l for l in lines if l.get("event") == "startup"]
    if not starts:
        bad.append("%s: no startup line (log rotated?)" % name)
    for l in starts:
        if l.get("test_hooks") or l.get("test_mutation_drop_ready") or l.get("test_mutation_duplicate_order"):
            bad.append("%s: startup line has a test switch on" % name)
report("tx-timeout", not bad, "; ".join(bad) if bad else
       "%d Pods logged transaction_timeout 1min (60s)" % len(pods))

if failures:
    print("preflight %s: FAILED (%s)" % (step, ", ".join(failures)))
    sys.exit(1)
print("preflight %s: all 6 checks passed" % step)
PY
