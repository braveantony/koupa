#!/usr/bin/env bash
# Fake-data test of compare-tools.sh compare (plan A14): a run counts only
# when every required result field is present and numeric, and each tool
# and step needs five such runs. Also cleanup-k6-orders against a fake
# kubectl: only the run's 'preparing' orders, on the database primary.
#
#   experiments/verify/compare-tools_test.sh     exit 0 when all cases pass
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

# make DIR [K6_STEP0_RESULT_OVERRIDE_JSON]: 5 Rush Hour + 5 k6 runs per step.
make() {
	local d=$1 bad=${2:-}
	python3 -I - "$d" "$bad" <<'PY'
import json, os, sys
d, bad = sys.argv[1], sys.argv[2]
for step, first in (("step-0", 40), ("step-3", 10)):
    for n in range(1, 6):
        rd = os.path.join(d, "rush-hour", "%s-20261109-0000%02d-r1-ab%02d" % (step, n, n))
        os.makedirs(rd)
        json.dump({"invalid": False, "items": {"orders": {"confirmed": 1200, "unknown": 0},
                   "first_try_failed": {"confirmed": first + n, "unknown": 0},
                   "failed_after_retry": {"confirmed": n, "unknown": 0}}}, open(os.path.join(rd, "scorecard.json"), "w"))
        kd = os.path.join(d, "k6", "k6-%s-r%d" % (step, n))
        os.makedirs(kd)
        res = {"orders": 1200, "first_try_failed": first + n, "failed_after_retry": n, "dropped_iterations": 0}
        if bad and not bad.startswith("rh:") and step == "step-0" and n == 3:
            res = json.loads(bad)
        if bad.startswith("rh:") and step == "step-3" and n == 2:
            sc = json.load(open(os.path.join(rd, "scorecard.json")))
            del sc["items"][bad[3:]]
            json.dump(sc, open(os.path.join(rd, "scorecard.json"), "w"))
        json.dump({"run": "k6-%s-r%d" % (step, n), "step": step, "valid": True, "k6_exit": 0, "result": res},
                  open(os.path.join(kd, "k6-run.json"), "w"))
PY
}

case_() {
	local name=$1 want_rc=$2 want_text=$3 dir=$tmp/$1
	shift 3
	make "$dir" "$@"
	out=$(CRIT_NO_RESTORE=1 VERIFY_EVIDENCE=$tmp/ev "$here/compare-tools.sh" compare "$dir" 2>/dev/null)
	rc=$?
	if [ "$rc" = "$want_rc" ] && grep -qF -- "$want_text" <<<"$out"; then
		echo "ok   $name"
	else
		echo "FAIL $name (exit $rc, want $want_rc; want text: $want_text)"
		grep -E '比較不完整|每個工具|k6 .*step-0' <<<"$out" | sed 's/^/     /'
		fail=1
	fi
}

case_ complete 0 "每個工具、每個步驟都有 5 次有效量測"
case_ missing-field 1 "k6 step-0：有效 4 次，不足 5 次" '{"orders": 1200, "failed_after_retry": 3, "dropped_iterations": 0}'
case_ non-numeric 1 "k6 step-0：有效 4 次，不足 5 次" '{"orders": 1200, "first_try_failed": "x", "failed_after_retry": 3, "dropped_iterations": 0}'
case_ null-field 1 "k6 step-0：有效 4 次，不足 5 次" '{"orders": 1200, "first_try_failed": 43, "failed_after_retry": null, "dropped_iterations": 0}'
case_ rush-hour-missing 1 "rush-hour step-3：有效 4 次，不足 5 次" "rh:failed_after_retry"
# cleanup-k6-orders with a fake kubectl that records the exec.
fake=$tmp/fakebin
mkdir -p "$fake"
cat >"$fake/kubectl" <<'SH'
#!/usr/bin/env bash
case " $* " in
*" get pod "*) echo koupa-db-1 ;;
*" exec "*)
	printf '%s\n' "$@" >"$FAKE_LOG.args"
	cat >"$FAKE_LOG.sql"
	[ "${FAKE_EXEC_FAIL:-0}" = 1 ] && { echo "ERROR: boom" >&2; exit 3; }
	echo deleted_orders=3 ;;
esac
SH
chmod +x "$fake/kubectl"
cleanup_case() {
	local name=$1 want_rc=$2 runid=$3 check=$4 log=$tmp/cl-$1
	rm -f "$log".*
	FAKE_LOG=$log PATH=$fake:$PATH CRIT_NO_RESTORE=1 VERIFY_EVIDENCE=$tmp/ev \
		"$here/compare-tools.sh" cleanup-k6-orders "$runid" "$tmp/cl-$name-dir" >/dev/null 2>&1
	rc=$?
	if [ "$rc" = "$want_rc" ] && eval "$check"; then
		echo "ok   cleanup $name"
	else
		echo "FAIL cleanup $name (exit $rc, want $want_rc; check: $check)"
		fail=1
	fi
}
cleanup_case scoped 0 k6-step-0-20261110-140000-r1 \
	'grep -qx koupa-db-1 $log.args && grep -qx postgres $log.args && grep -qx "ref=k6-step-0-20261110-140000-r1-c%" $log.args &&
	 grep -qF "client_ref LIKE :'"'"'ref'"'"' AND status = '"'"'preparing'"'"'" $log.sql && grep -q "^COMMIT;" $log.sql &&
	 grep -qx deleted_orders=3 $tmp/cl-scoped-dir/cleanup.txt'
cleanup_case not-k6-run 1 "step-0-20261110-140000-r1-ab12" '[ ! -e $log.args ]'
cleanup_case quoting 1 "k6-x';DROP TABLE orders;--" '[ ! -e $log.args ]'
FAKE_EXEC_FAIL=1 cleanup_case exec-fails 1 k6-step-3-20261110-140000-r2 'grep -q boom $tmp/cl-exec-fails-dir/cleanup.txt'
exit $fail
