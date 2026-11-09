#!/usr/bin/env bash
# preflight.sh must refuse a measurement while TEST_HOOKS or TEST_MUTATION
# is set (A12, A14), on any step.
. "$(dirname "$0")/lib.sh"
setup_fakes

make_live step-0 'env["TEST_HOOKS"] = "1"'
out=$("$STAGE/scripts/preflight.sh" step-0 2>&1)
rc=$?
check "TEST_HOOKS fails with exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "names the test-switches check" grep -q '^FAIL test-switches:.*TEST_HOOKS' <<<"$out"

make_live step-4 'env["TEST_MUTATION"] = "drop-ready=3"'
out=$("$STAGE/scripts/preflight.sh" step-4 2>&1)
check "TEST_MUTATION fails" grep -q '^FAIL test-switches:.*TEST_MUTATION' <<<"$out"

# The rendered test-hooks overlay itself cannot be measured.
out=$("$STAGE/scripts/preflight.sh" test-hooks 2>&1)
check "test-hooks overlay is not a measurement step (rc 2)" [ $? -eq 2 ]
finish
