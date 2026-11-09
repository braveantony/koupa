#!/usr/bin/env bash
# preflight.sh step-1 must fail when the live Deployment still carries
# GRACEFUL_SHUTDOWN from step 2 (a previous-step setting not removed).
. "$(dirname "$0")/lib.sh"
setup_fakes

make_live step-1
out=$("$STAGE/scripts/preflight.sh" step-1 2>&1)
rc=$?
check "clean step-1 passes (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || echo "$out" | sed 's/^/    /'

make_live step-1 'env["GRACEFUL_SHUTDOWN"] = "1"'
out=$("$STAGE/scripts/preflight.sh" step-1 2>&1)
rc=$?
check "leftover GRACEFUL_SHUTDOWN fails with exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "names the spec check and the variable" grep -q '^FAIL spec:.*GRACEFUL_SHUTDOWN' <<<"$out"
echo "$out" | grep '^FAIL' | sed 's/^/    /'

# The same leftover on a correct step-2 overlay is fine (it belongs there).
make_live step-2
out=$("$STAGE/scripts/preflight.sh" step-2 2>&1)
check "clean step-2 passes" [ $? -eq 0 ]

# A Pod of an older ReplicaSet still running.
make_live step-1 'pass'
python3 -I - "$FAKE/pods.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["items"][0]["metadata"]["ownerReferences"][0]["name"] = "koupa-old"
json.dump(p, open(sys.argv[1], "w"))
PY
out=$("$STAGE/scripts/preflight.sh" step-1 2>&1)
rc=$?
check "Pod of an old ReplicaSet fails (rc=$rc)" [ "$rc" -eq 1 ]
check "names the replicaset check" grep -q '^FAIL replicaset:' <<<"$out"

# Startup log without the transaction_timeout check line.
make_live step-1
sed -i '/tx_timeout_check/d' "$FAKE/logs/koupa-abc-1.log"
out=$("$STAGE/scripts/preflight.sh" step-1 2>&1)
check "missing transaction_timeout line fails" grep -q '^FAIL tx-timeout:.*koupa-abc-1' <<<"$out"

# observedGeneration behind generation.
make_live step-1 'pass'
python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); d["metadata"]["generation"]=3; json.dump(d,open(sys.argv[1],"w"))' "$FAKE/deploy.json"
out=$("$STAGE/scripts/preflight.sh" step-1 2>&1)
check "observedGeneration behind fails" grep -q '^FAIL generation:' <<<"$out"
finish
