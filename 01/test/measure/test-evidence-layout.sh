#!/usr/bin/env bash
# evidence.sh RUN_DIR stores the design 5.4 evidence pack: manifests
# (kustomize output and live objects), versions, image digests, parameters,
# raw records, /api/audit results, failures, missing sources, scorecard.
. "$(dirname "$0")/lib.sh"
setup_fakes
make_live step-3

run=$MEASURE_EVIDENCE_DIR/step-3-20260101-000000-r1-abcd
mkdir -p "$run/app"
cat >"$run/params.json" <<J
{"run_id":"step-3-20260101-000000-r1-abcd","mode":"step-3","overlay":"step-3","image_before":"ghcr.io/braveantony/koupa:v1","image_after":"ghcr.io/braveantony/koupa:v2","duration_s":60,"rate":20,"boards":3,"base_url":"http://172.22.0.201","rush_hour":{"kind":"image","ref":"ghcr.io/braveantony/rush-hour:0.1.0"}}
J
scorecard 's["diagnostics"]["failed_attempts"] = [{"attempt": "a1", "kind": "order"}]'
sed 's/@RUN@/step-3-20260101-000000-r1-abcd/g' "$FAKE/scorecard.json" >"$run/scorecard.json"
echo "Rush Hour 成績單" >"$run/scorecard.txt"
mkdir -p "$run/manifest"; echo "kind: Deployment" >"$run/manifest/applied.yaml"; echo "{\"completed\": true}" >"$run/rollout.json"
: >"$run/attempts.jsonl"
cat >"$run/image-digests.json" <<J
{"koupa":{"ghcr.io/braveantony/koupa:v1":{"digest":"sha256:$(printf '%064d' 1)"}},"rush_hour":{"kind":"image","image_id":null}}
J
: >"$run/boards.jsonl"
"$COLLECT_TIMELINE" start "$run"
"$COLLECT_TIMELINE" stop "$run"
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
rc=$?
check "evidence.sh exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || sed 's/^/    /' "$T/out"
for f in image-digests.json manifest/kustomize.yaml manifest/live.yaml versions.txt images.json params.json \
	audit/used-by-scorecard.json audit/cursor.json audit/orders.json audit/ready.json audit/queries.txt \
	failures.json missing.jsonl timeline.txt scorecard.txt scorecard.json attempts.jsonl boards.jsonl \
	ingress.log app evidence-index.txt; do
	check "has $f" [ -e "$run/$f" ]
done
check "kustomize output is step-3 with image v1" bash -c 'grep -q "GRACEFUL_SHUTDOWN" "$1" && grep -q "koupa:v1" "$1"' _ "$run/manifest/kustomize.yaml"
check "images.json has the Pod digests" grep -q 'koupa@sha256:' "$run/images.json"
check "images.json: Pod digest matches the registry" grep -q '"digest_matches_registry": true' "$run/images.json"
check "images.json has the rush-hour digest" grep -q 'sha256:fakedigest' "$run/images.json"
check "versions has the git commit" grep -q '^git_commit=' "$run/versions.txt"
check "audit queried with the run id" grep -q 'api/audit?run=step-3-20260101-000000-r1-abcd&mode=orders' "$FAKE/calls.log"
check "failures.json has the failed attempt" grep -q '"a1"' "$run/failures.json"
check "index lists no missing required file" bash -c '! grep -q "^MISSING" "$1"' _ "$run/evidence-index.txt"

# A missing scorecard is reported and fails.
rm "$run/scorecard.json"
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
check "missing scorecard.json: exit 1" [ $? -eq 1 ]
check "index says MISSING scorecard.json" grep -q '^MISSING scorecard.json' "$run/evidence-index.txt"
finish
