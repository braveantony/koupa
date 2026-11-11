#!/usr/bin/env bash
# setup.sh step 2: when reading the migration Job's status fails, setup.sh
# prints the kubectl error and still dumps the Job's logs and describe
# before it stops (not a bare `set -e` exit).
. "$(dirname "$0")/lib.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/setup-test.XXXXXX")
trap 'rm -rf "$T"' EXIT
FAKE=$T/fake
mkdir -p "$FAKE/bin"
export FAKE REAL_KUBECTL PATH=$FAKE/bin:$PATH
fake_registry "$FAKE/bin"
cat >"$FAKE/bin/kubectl" <<'K'
#!/usr/bin/env bash
# Fake kubectl for setup.sh: steps 0 and 1 succeed; reading the migration
# Job fails ($FAKE/job-status overrides with a status to print).
echo "kubectl $*" >>"$FAKE/calls.log"
case " $* " in
*" kustomize "*) exec "$REAL_KUBECTL" "$@" ;;
*" config current-context "*) echo fake ;;
*" get nodes "*"-l !node-role"*) printf 'tkha-worker-1\ntkha-worker-2\ntkha-worker-3\n' ;;
*" get nodes "*"koupa.dev/infra=true"*) echo tkha-worker-1 ;;
*" get pvc "*) exit 1 ;;
*" get cluster koupa-db "*) printf 'Cluster in healthy state|True' ;;
*" get job "*)
	if [ -f "$FAKE/job-status" ]; then cat "$FAKE/job-status"; exit 0; fi
	echo 'error: the server was unable to return a response in the time allotted' >&2
	exit 1 ;;
*" logs "*"job-name="*) echo "fake migration log line" ;;
*" describe job "*) echo "fake describe: Pods Statuses: 0 Active / 0 Succeeded / 1 Failed" ;;
*" apply "*" -f - "*) cat >/dev/null ;;
esac
exit 0
K
printf '#!/bin/sh\nexit 0\n' >"$FAKE/bin/sleep"
chmod +x "$FAKE/bin/kubectl" "$FAKE/bin/sleep"

run_setup() { : >"$FAKE/calls.log"; timeout 120 "$STAGE/setup.sh" </dev/null >"$T/out" 2>&1; rc=$?; }

run_setup
check "status query fails: setup fails (rc=$rc)" [ "$rc" -eq 1 ]
check "status query fails: the kubectl error is printed" grep -q 'could not read the status of migration Job koupa-migrate-.*unable to return a response' "$T/out"
check "status query fails: Job logs dumped" grep -q 'fake migration log line' "$T/out"
check "status query fails: Job described" grep -q 'fake describe' "$T/out"
check "status query fails: says migration failed" grep -q 'setup.sh: migration failed' "$T/out"
check "status query fails: step 3 not applied" bash -c '! grep -q "apply -k k8s/base" "$1"' _ "$FAKE/calls.log"

printf 'Failed ' >"$FAKE/job-status"
run_setup
check "Job Failed: setup fails (rc=$rc)" [ "$rc" -eq 1 ]
check "Job Failed: conditions and logs printed" bash -c 'grep -q "conditions: Failed" "$1" && grep -q "fake migration log line" "$1"' _ "$T/out"

printf 'Complete ' >"$FAKE/job-status"
run_setup
check "Job Complete: goes on to step 3" grep -q 'apply -k k8s/base' "$FAKE/calls.log"
finish
