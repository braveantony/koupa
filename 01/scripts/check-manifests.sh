#!/usr/bin/env bash
# Check the rendered manifest of one overlay against the expectation table.
#
#   scripts/check-manifests.sh <base|step-1|step-2|step-3|step-4|calibrate|test-hooks|idle-lab>
#
# Pure check: renders with `kubectl kustomize` and inspects the output; it
# never talks to the cluster. Exit 0 = all checks passed, 1 = a check failed,
# 2 = usage or render error.
set -euo pipefail
cd "$(dirname "$0")/.."

step=${1:-}
case "$step" in
  base) dir=k8s/base ;;
  step-[1-4]) dir=k8s/steps/$step ;;
  calibrate|test-hooks|idle-lab) dir=k8s/$step ;;
  *) echo "usage: $0 <base|step-1..step-4|calibrate|test-hooks|idle-lab>" >&2; exit 2 ;;
esac

if [ ! -f "$dir/kustomization.yaml" ]; then
  echo "FAIL $step: $dir/kustomization.yaml does not exist" >&2
  exit 1
fi

rendered=$(kubectl kustomize "$dir") || { echo "FAIL $step: kubectl kustomize $dir failed" >&2; exit 2; }

printf '%s\n' "$rendered" | python3 -I -c '
import re, sys, yaml

step = sys.argv[1]
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]

# ABSENT = the field must not be set; any other value must match exactly.
ABSENT = object()

# Expectation table: overlay -> field -> expected value.
# preStop and grace are integers (seconds); env values are strings.
BASE = {
    "preStop": ABSENT,
    "terminationGracePeriodSeconds": ABSENT,
    "GRACEFUL_SHUTDOWN": ABSENT,
    "SHUTDOWN_TIMEOUT": ABSENT,
    "RESUME_FROM_LAST_ID": ABSENT,
    "IDEMPOTENCY_KEY": ABSENT,
    "SSE_HEARTBEAT": ABSENT,
    "TEST_HOOKS": ABSENT,
    "TEST_MUTATION": ABSENT,
    "hooksPort": ABSENT,
    "readinessProbe": ABSENT,
    "imageTag": "v1",
}
STEP1 = {**BASE, "preStop": 15}
STEP2 = {**STEP1, "GRACEFUL_SHUTDOWN": "1", "SHUTDOWN_TIMEOUT": "15s",
         "terminationGracePeriodSeconds": 40}
STEP3 = {**STEP2, "RESUME_FROM_LAST_ID": "1"}
STEP4 = {**STEP3, "IDEMPOTENCY_KEY": "1"}
TABLE = {
    "base": BASE,
    "step-1": STEP1,
    "step-2": STEP2,
    "step-3": STEP3,
    "step-4": STEP4,
    "calibrate": {**BASE, "preStop": 20, "terminationGracePeriodSeconds": 45},
    "test-hooks": {**STEP4, "TEST_HOOKS": "1", "hooksPort": 9090},
    "idle-lab": {**BASE, "SSE_HEARTBEAT": "15s"},
}

ACCESS_LOG_KEYS = {
    "start_time": "%START_TIME%",
    "path": "%REQ(:PATH)%",
    "method": "%REQ(:METHOD)%",
    "status": "%RESPONSE_CODE%",
    "upstream_host": "%UPSTREAM_HOST%",
    "response_flags": "%RESPONSE_FLAGS%",
    "response_code_details": "%RESPONSE_CODE_DETAILS%",
    "upstream_attempts": "%UPSTREAM_REQUEST_ATTEMPT_COUNT%",
    "duration_ms": "%DURATION%",
    "attempt": "%REQ(X-KOUPA-ATTEMPT)%",
}

failures = []
def check(ok, msg):
    print(("ok   " if ok else "FAIL ") + step + ": " + msg)
    if not ok:
        failures.append(msg)

def find(kind, name=None):
    return [d for d in docs if d.get("kind") == kind
            and (name is None or d["metadata"]["name"] == name)]

def seconds(v):
    m = re.fullmatch(r"(\d+)(ms|s|m)", v or "")
    if not m:
        return None
    n, unit = int(m.group(1)), m.group(2)
    return {"ms": n / 1000, "s": n, "m": n * 60}[unit]

exp = TABLE[step]

deps = find("Deployment", "koupa")
check(len(deps) == 1, "exactly one Deployment koupa")
if not deps:
    sys.exit(1)
dep = deps[0]
check(dep["metadata"].get("namespace") == "koupa", "Deployment is in namespace koupa")
spec = dep["spec"]
pod = spec["template"]["spec"]
ctrs = [c for c in pod["containers"] if c["name"] == "koupa"]
check(len(ctrs) == 1, "container koupa exists")
c = ctrs[0]
env = {e["name"]: e for e in c.get("env", [])}

# Fixed shape shared by every overlay.
check(spec.get("replicas") == 3, "replicas is 3")
check(spec.get("strategy") in (None, {"type": "RollingUpdate"}),
      "default rolling update (no strategy tuning)")
tsc = pod.get("topologySpreadConstraints") or []
check(any(t.get("topologyKey") == "kubernetes.io/hostname" and t.get("maxSkew") == 1
          and t.get("whenUnsatisfiable") == "DoNotSchedule"
          and t.get("matchLabelKeys") == ["pod-template-hash"] for t in tsc),
      "topologySpread hostname maxSkew 1 DoNotSchedule, per ReplicaSet")
check(env.get("POD_UID", {}).get("valueFrom", {}).get("fieldRef", {}).get("fieldPath") == "metadata.uid",
      "POD_UID from downward API metadata.uid")
check(env.get("POD_NAME", {}).get("valueFrom", {}).get("fieldRef", {}).get("fieldPath") == "metadata.name",
      "POD_NAME from downward API metadata.name")
ref = env.get("DATABASE_URL", {}).get("valueFrom", {}).get("secretKeyRef", {})
check(ref.get("name") == "koupa-db-app" and ref.get("key") == "uri",
      "DATABASE_URL from secret koupa-db-app key uri")
ports = {p["containerPort"]: p for p in c.get("ports", [])}
check(8080 in ports, "containerPort 8080")

# Image.
image = c.get("image", "")
check(image.startswith("ghcr.io/braveantony/koupa:"), "image is ghcr.io/braveantony/koupa (" + image + ")")
check(image.rsplit(":", 1)[-1] == exp["imageTag"], "image tag is " + exp["imageTag"])

# Table-driven fields.
def actual(field):
    if field == "preStop":
        s = (c.get("lifecycle") or {}).get("preStop", {}).get("sleep", {}).get("seconds")
        return ABSENT if s is None else s
    if field == "terminationGracePeriodSeconds":
        return pod.get(field, ABSENT)
    if field == "hooksPort":
        return 9090 if 9090 in ports else ABSENT
    if field == "readinessProbe":
        return c.get("readinessProbe", ABSENT)
    if field == "imageTag":
        return None  # checked above
    e = env.get(field)
    if e is None:
        return ABSENT
    return e.get("value")

for field, want in exp.items():
    if field == "imageTag":
        continue
    got = actual(field)
    if want is ABSENT:
        check(got is ABSENT, field + " is absent" + ("" if got is ABSENT else " (got " + repr(got) + ")"))
    else:
        check(got == want, field + " = " + repr(want) + ("" if got == want else " (got " + repr(got) + ")"))

# Timing inequalities (plan A5, A8).
pre = actual("preStop")
pre = 0 if pre is ABSENT else pre
grace = actual("terminationGracePeriodSeconds")
grace = 30 if grace is ABSENT else grace
if exp["GRACEFUL_SHUTDOWN"] == "1":
    st = seconds(actual("SHUTDOWN_TIMEOUT"))
    check(st is not None and pre + st + 5 < grace,
          "A5: preStop %s + SHUTDOWN_TIMEOUT %s + 5 < grace %s" % (pre, st, grace))
elif pre:
    check(pre < grace, "preStop %s < grace %s" % (pre, grace))

# Service: port 80 -> 8080; the hooks port is never exposed.
svcs = find("Service", "koupa")
check(len(svcs) == 1, "Service koupa exists")
if svcs:
    sp = svcs[0]["spec"]["ports"]
    check(any(p.get("port") == 80 and p.get("targetPort") in (8080, "http") for p in sp),
          "Service port 80 -> 8080")
    check(all(p.get("port") != 9090 and p.get("targetPort") not in (9090, "hooks") for p in sp),
          "Service does not expose 9090")

# Gateway, GatewayClass and the access log contract.
gws = find("Gateway", "koupa")
check(len(gws) == 1 and gws[0]["spec"].get("gatewayClassName") == "koupa",
      "Gateway koupa uses GatewayClass koupa")
gcs = find("GatewayClass", "koupa")
check(len(gcs) == 1, "GatewayClass koupa exists")
if gcs:
    gc = gcs[0]["spec"]
    pr = gc.get("parametersRef") or {}
    check(gc.get("controllerName") == "io.cilium/gateway-controller", "GatewayClass controller is cilium")
    check(pr.get("kind") == "CiliumGatewayClassConfig" and pr.get("name") == "koupa-access-log"
          and pr.get("namespace") == "koupa", "GatewayClass parametersRef -> koupa/koupa-access-log")
cfgs = find("CiliumGatewayClassConfig", "koupa-access-log")
check(len(cfgs) == 1, "CiliumGatewayClassConfig koupa-access-log exists")
if cfgs:
    # Local (the Helm default) leaves no node eligible for L2 announcement.
    check(cfgs[0]["spec"].get("service", {}).get("externalTrafficPolicy") == "Cluster",
          "gateway Service externalTrafficPolicy Cluster (host can reach the LB IP)")
    logs = cfgs[0]["spec"].get("telemetry", {}).get("accessLogs", [])
    check(len(logs) == 1 and logs[0].get("format") == "JSON", "one JSON access log")
    if logs:
        check(logs[0].get("json") == ACCESS_LOG_KEYS, "access log keys match the rush-hour contract")

# HTTPRoute: no retry, no timeouts.
routes = find("HTTPRoute", "koupa")
check(len(routes) == 1, "HTTPRoute koupa exists")
if routes:
    rules = routes[0]["spec"].get("rules", [])
    check(all("retry" not in r for r in rules), "HTTPRoute has no retry")
    check(all("timeouts" not in r for r in rules), "HTTPRoute has no timeouts")
    check(any(b.get("name") == "koupa" and b.get("port") == 80
              for r in rules for b in r.get("backendRefs", [])), "HTTPRoute backend is koupa:80")

if failures:
    print("FAIL %s: %d check(s) failed" % (step, len(failures)))
    sys.exit(1)
print("PASS " + step)
' "$step"
