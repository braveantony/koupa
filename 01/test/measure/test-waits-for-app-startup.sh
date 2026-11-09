#!/usr/bin/env bash
# No readinessProbe (design 4.9): a Pod is Ready before the app has opened
# its database pool, so its tx_timeout_check line can come seconds after
# the rollout. measure.sh waits for that line in every Pod's log before
# preflight instead of failing the tx-timeout check on a race; a Pod that
# never logs it still fails preflight after the wait.
. "$(dirname "$0")/lib.sh"
setup_fakes
echo 25 >"$FAKE/rh-backdate"

make_live step-0
cp "$FAKE/logs/koupa-abc-1.log" "$T/full.log"
sed -i '/tx_timeout_check/d' "$FAKE/logs/koupa-abc-1.log"
(sleep 3; cp "$T/full.log" "$FAKE/logs/koupa-abc-1.log") &
MEASURE_APP_START_TIMEOUT_S=20 "$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
wait
check "late tx_timeout_check: control runs, exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -15 "$T/out" | sed 's/^/    /'
check "preflight passed" grep -q 'preflight: all 7 checks passed' "$T/out"
check "did not wait out the timeout" bash -c '! grep -q "not every Pod logged" "$1"' _ "$T/out"

rm -rf "$MEASURE_EVIDENCE_DIR"/*
make_live step-0
sed -i '/tx_timeout_check/d' "$FAKE/logs/koupa-abc-1.log"
MEASURE_APP_START_TIMEOUT_S=2 "$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
check "never logged: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "never logged: preflight names tx-timeout" grep -q '量測前核對不符：tx-timeout' "$T/out"

# A long log (live Pods log every request): grep -q stops reading early, so
# kubectl logs dies of SIGPIPE; under pipefail that must not count as "no
# tx_timeout_check yet".
rm -rf "$MEASURE_EVIDENCE_DIR"/*
make_live step-0
for f in "$FAKE"/logs/*.log; do
	python3 -I -c 'import sys; sys.stdout.write("{\"event\":\"request_end\",\"pad\":\"%s\"}\n" % ("x" * 200) * 20000)' >>"$f"
done
MEASURE_APP_START_TIMEOUT_S=5 "$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
check "long log: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
check "long log: did not wait out the timeout" bash -c '! grep -q "not every Pod logged" "$1"' _ "$T/out"
finish
