#!/usr/bin/env bash
# setup.sh stops before changing anything when a koupa image tag of
# images.env is not on the registry (「映像檔 vX 還沒發布」) or the registry
# cannot be asked; with both published it goes on.
. "$(dirname "$0")/lib.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/setup-test.XXXXXX")
trap 'rm -rf "$T"' EXIT
FAKE=$T/fake
mkdir -p "$FAKE/bin"
export FAKE REAL_KUBECTL PATH=$FAKE/bin:$PATH
fake_registry "$FAKE/bin"
cat >"$FAKE/bin/kubectl" <<'K'
#!/usr/bin/env bash
echo "kubectl $*" >>"$FAKE/calls.log"
case " $* " in
*" kustomize "*) exec "$REAL_KUBECTL" "$@" ;;
*" config current-context "*) echo fake ;;
*" get nodes "*"-l !node-role"*) printf 'tkha-worker-1\ntkha-worker-2\ntkha-worker-3\n' ;;
*" apply "*) echo "fake: apply refused" >&2; exit 1 ;;
esac
exit 0
K
chmod +x "$FAKE/bin/kubectl"
unchanged() { ! grep -qE '^kubectl (label|apply|delete|create)' "$FAKE/calls.log"; }
run_setup() { : >"$FAKE/calls.log"; "$STAGE/setup.sh" >"$T/out" 2>&1; rc=$?; }

echo "$IMG_V1" >"$FAKE/unpublished"
run_setup
check "v1 tag unpublished: fails (rc=$rc)" [ "$rc" -ne 0 ]
check "says 映像檔 $KOUPA_V1_TAG 還沒發布" grep -q "映像檔 $KOUPA_V1_TAG 還沒發布" "$T/out"
check "nothing changed in the cluster" unchanged

echo "$IMG_V2" >"$FAKE/unpublished"
run_setup
check "v2 tag unpublished: fails (rc=$rc)" [ "$rc" -ne 0 ]
check "says 映像檔 $KOUPA_V2_TAG 還沒發布" grep -q "映像檔 $KOUPA_V2_TAG 還沒發布" "$T/out"
check "nothing changed in the cluster" unchanged

rm -f "$FAKE/unpublished"
touch "$FAKE/registry-down"
run_setup
check "registry unreachable: fails (rc=$rc)" [ "$rc" -ne 0 ]
check "says it cannot tell" grep -q "無法確認映像檔 $IMG_V1 是否已發布" "$T/out"
check "nothing changed in the cluster" unchanged

rm -f "$FAKE/registry-down"
run_setup
check "both published: goes on to the infra worker label" grep -q '^kubectl label node tkha-worker-1' "$FAKE/calls.log"
grep -q "還沒發布" "$T/out" && fail "both published: no unpublished message"
finish
