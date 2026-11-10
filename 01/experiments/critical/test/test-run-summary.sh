#!/usr/bin/env bash
# run.sh's summary must FAIL an experiment that exits non-zero (or times
# out) after writing PASS for some setting, or that leaves an expected
# setting (its "# settings:" line) without a verdict. Uses stub experiments
# and a no-op restore; no cluster needed.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
run=$here/../run.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0

# stub NAME SETTINGS EXIT VERDICTS...: an experiment declaring SETTINGS
# that appends VERDICTS ("PASS on", ...) and exits EXIT.
stub() {
	local name=$1 settings=$2 code=$3
	shift 3
	{
		echo '#!/usr/bin/env bash'
		echo "# settings: $settings"
		for v in "$@"; do
			echo "echo \"${v% *} $name ${v#* } /dev/null\" >>\"\$CRIT_SUMMARY\""
		done
		echo "exit $code"
	} >"$tmp/exp/$name.sh"
	chmod +x "$tmp/exp/$name.sh"
}

# case NAME WANT_RC NN...: run run.sh for experiments NN over the stubs.
case_() {
	local name=$1 want=$2 rc
	shift 2
	CRIT_EXP_DIR=$tmp/exp CRIT_RESTORE=true CRIT_EVIDENCE=$tmp/ev-$name "$run" "$@" >"$tmp/$name.out" 2>&1
	rc=$?
	if { [ "$want" = 0 ] && [ $rc = 0 ]; } || { [ "$want" != 0 ] && [ $rc != 0 ]; }; then
		echo "ok   $name (rc $rc)"
	else
		echo "FAIL $name: rc $rc, want $([ "$want" = 0 ] && echo 0 || echo non-zero)"
		sed 's/^/     | /' "$tmp/$name.out"
		fails=$((fails + 1))
	fi
}

reset() { rm -rf "$tmp/exp"; mkdir -p "$tmp/exp"; }

reset
stub 01-a "off on" 0 "PASS off" "PASS on"
stub 02-b "on" 0 "PASS on"
case_ all-pass 0 01 02

reset
stub 01-a "off on" 3 "PASS off"
case_ pass-then-exit-nonzero 1 01

reset
stub 01-a "off on" 124 "PASS off" "PASS on"
case_ all-pass-but-timed-out 1 01

reset
stub 01-a "off on" 0 "PASS off"
case_ missing-setting 1 01

reset
stub 01-a "off on" 0 "PASS off" "PASS on"
stub 02-b "on" 0
case_ second-without-verdict 1 01 02

[ $fails = 0 ] && echo "PASS test-run-summary" || echo "FAIL test-run-summary ($fails)"
[ $fails = 0 ]
