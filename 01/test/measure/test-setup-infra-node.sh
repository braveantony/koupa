#!/usr/bin/env bash
# setup.sh infra worker (plan A15): INFRA_NODE must be a worker and the
# database's local-path volume must already be on it; otherwise setup.sh
# stops BEFORE any node label changes or anything is applied.
. "$(dirname "$0")/lib.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/setup-test.XXXXXX")
trap 'rm -rf "$T"' EXIT
FAKE=$T/fake
mkdir -p "$FAKE/bin"
export FAKE REAL_KUBECTL PATH=$FAKE/bin:$PATH
cat >"$FAKE/bin/kubectl" <<'K'
#!/usr/bin/env bash
# Fake kubectl for setup.sh: 3 workers, the koupa-db volume on worker-1
# ($FAKE/pv-node overrides); apply fails so setup stops after step 0.
echo "kubectl $*" >>"$FAKE/calls.log"
case " $* " in
*" kustomize "*) exec "$REAL_KUBECTL" "$@" ;;
*" config current-context "*) echo fake ;;
*" get nodes "*"-l !node-role"*) printf 'tkha-worker-1\ntkha-worker-2\ntkha-worker-3\n' ;;
*" get nodes "*"koupa.dev/infra=true"*) echo "${INFRA_NOW:-tkha-worker-1}" ;;
*" get pvc koupa-db-1 "*) echo pvc-123 ;;
*" get pv pvc-123 "*) cat "$FAKE/pv-node" 2>/dev/null || echo tkha-worker-1 ;;
*" label "*) ;;
*" apply "*) echo "fake: apply refused" >&2; exit 1 ;;
esac
exit 0
K
chmod +x "$FAKE/bin/kubectl"

run_setup() { : >"$FAKE/calls.log"; env "$@" "$STAGE/setup.sh" >"$T/out" 2>&1; rc=$?; }

run_setup INFRA_NODE=tkha-worker-2
check "PV on worker-1, INFRA_NODE=worker-2: fails (rc=$rc)" [ "$rc" -ne 0 ]
check "PV elsewhere: says where the volume is" grep -q 'tkha-worker-1' "$T/out"
check "PV elsewhere: no label changed" bash -c '! grep -q "^kubectl label" "$1"' _ "$FAKE/calls.log"
check "PV elsewhere: nothing applied" bash -c '! grep -q "^kubectl apply" "$1"' _ "$FAKE/calls.log"

run_setup INFRA_NODE=tkha-worker-9
check "INFRA_NODE not a worker: fails (rc=$rc)" [ "$rc" -ne 0 ]
check "INFRA_NODE not a worker: no label changed" bash -c '! grep -q "^kubectl label" "$1"' _ "$FAKE/calls.log"

run_setup
check "default: labels worker-1 (then stops at the fake apply)" grep -q '^kubectl label node tkha-worker-1 koupa.dev/infra=true --overwrite' "$FAKE/calls.log"
check "default: PV checked before labelling" bash -c 'awk "/get pv pvc-123/{p=NR} /^kubectl label/{if(!l)l=NR} END{exit !(p && l && p < l)}" "$1"' _ "$FAKE/calls.log"
finish
