#!/usr/bin/env bash
# rollback, scale-down and calibrate run end to end (plan A8 other modes,
# A11 calibrate, A15 scale-down).
. "$(dirname "$0")/lib.sh"
setup_fakes
echo 15 >"$FAKE/rh-backdate"
after() { awk -v a="$1" -v b="$2" '$0 ~ a {s=1} s && $0 ~ b {f=1} END {exit !f}' "$FAKE/calls.log"; }

# rollback: step-5 settings, applied with v2, the change sets v1.
make_live step-5 'c["image"] = "ghcr.io/braveantony/koupa:v2"'
"$STAGE/measure.sh" rollback --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "rollback: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -20 "$T/out" | sed 's/^/    /'
check "rollback: applied image v2" grep -q 'ghcr.io/braveantony/koupa:v2' "$run/manifest/applied.yaml"
check "rollback: applied has the step-5 env" grep -q 'IDEMPOTENCY_KEY' "$run/manifest/applied.yaml"
check "rollback: change sets v1" grep -q '^kubectl .*set image deploy/koupa koupa=ghcr.io/braveantony/koupa:v1' "$FAKE/calls.log"
check "rollback: params before v2, after v1, overlay step-5" python3 -I -c '
import json, sys
p = json.load(open(sys.argv[1]))
sys.exit(0 if p["image_before"].endswith(":v2") and p["image_after"].endswith(":v1") and p["overlay"] == "step-5" and p["mode"] == "rollback" else 1)' "$run/params.json"

# scale-down: replicas 2 at the change, back to 3 afterwards, checked.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
make_live step-5
"$STAGE/measure.sh" scale-down --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
run=$(latest_run)
check "scale-down: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -20 "$T/out" | sed 's/^/    /'
check "scale-down: scaled to 2, then back to 3" after 'scale deploy/koupa --replicas=2' 'scale deploy/koupa --replicas=3'
check "scale-down: no set image" bash -c '! grep -q "set image" "$1"' _ "$FAKE/calls.log"
check "scale-down: restore.json ok with 3 available" python3 -I -c '
import json, sys
r = json.load(open(sys.argv[1]))
sys.exit(0 if r["ok"] and r["replicas"] == 3 else 1)' "$run/restore.json"
check "scale-down: rollout.json completed" python3 -I -c 'import json,sys; r=json.load(open(sys.argv[1])); sys.exit(0 if r["completed"] and r["change"] == "scale deploy/koupa --replicas=2" else 1)' "$run/rollout.json"

# calibrate: probe alongside the load, calibrate analysis per run with the
# freshly built host binary; stops once 5 valid samples are in (2 per run
# -> 3 runs, not --runs 8); candidate and samples in evidence/calibrate-*/.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
make_live calibrate
"$STAGE/measure.sh" calibrate --runs 8 --duration 30 >"$T/out" 2>&1
rc=$?
check "calibrate: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -30 "$T/out" | sed 's/^/    /'
check "calibrate: 3 runs (stops at 5 valid samples)" [ "$(grep -c '^rush-hour run' "$FAKE/calls.log")" -eq 3 ]
check "calibrate: a probe per run" [ "$(grep -c '^rush-hour probe' "$FAKE/calls.log")" -eq 3 ]
check "calibrate: probe started before the load" after '^rush-hour probe' '^rush-hour run'
check "calibrate: applied preStop 20, grace 45" python3 -I -c '
import sys, yaml
d = [x for x in yaml.safe_load_all(open(sys.argv[1])) if x and x["kind"] == "Deployment"][0]
s = d["spec"]["template"]["spec"]
sys.exit(0 if s["terminationGracePeriodSeconds"] == 45 and s["containers"][0]["lifecycle"]["preStop"]["sleep"]["seconds"] == 20 else 1)' "$(ls -1dt "$MEASURE_EVIDENCE_DIR"/calibrate-*-r1-*/ | head -1)manifest/applied.yaml"
cdir=$(ls -1d "$MEASURE_EVIDENCE_DIR"/calibrate-2*/ 2>/dev/null | grep -v -- '-r[0-9]' | head -1)
check "calibrate: evidence/calibrate-<time>/ written" [ -n "$cdir" ]
check "calibrate: candidate.json with 6 valid samples, 3.2 s" python3 -I -c '
import json, sys
c = json.load(open(sys.argv[1] + "/candidate.json"))
sys.exit(0 if c["valid_samples"] == 6 and c["candidate_s"] == 3.2 and len(c["runs"]) == 3 else 1)' "$cdir"
check "calibrate: samples.txt" grep -q 'candidate' "$cdir/samples.txt"
check "calibrate: the analysis ran the host build with --prestop 20s" grep -q "^rush-hour calibrate --prestop 20s --json" "$FAKE/calls.log"
check "calibrate: candidate printed" grep -q 'candidate' "$T/out"

# calibrate with too few valid samples: no candidate, says 未經校正.
rm -rf "$MEASURE_EVIDENCE_DIR"/*
: >"$FAKE/calls.log"
echo 0 >"$FAKE/calib-valid-per-run"
"$STAGE/measure.sh" calibrate --runs 2 --duration 30 >"$T/out" 2>&1
rc=$?
check "calibrate too few: exit 2 (rc=$rc)" [ "$rc" -eq 2 ]
check "calibrate too few: 2 runs" [ "$(grep -c '^rush-hour run' "$FAKE/calls.log")" -eq 2 ]
check "calibrate too few: says 未經校正" grep -q '未經校正' "$T/out"
rm -f "$FAKE/calib-valid-per-run"
finish
