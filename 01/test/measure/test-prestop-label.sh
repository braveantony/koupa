#!/usr/bin/env bash
# The scorecard says whether preStop was calibrated (design 4.x, plan A11):
# the koupa.dev/prestop-calibrated annotation of the live Deployment read
# after preflight decides; "false" -> 「preStop 未經校正」; missing or any other
# value -> unknown (never silently calibrated); runs without preStop
# (step 0, step 1, control) get no label.
. "$(dirname "$0")/lib.sh"
setup_fakes
echo 15 >"$FAKE/rh-backdate"
KEY=koupa.dev/prestop-calibrated

# status RUN: prestop_calibration.status in scorecard.json ("" if absent).
status() {
	python3 -I -c 'import json,sys; print((json.load(open(sys.argv[1])).get("prestop_calibration") or {}).get("status", ""))' "$1/scorecard.json"
}
# marker EDIT: make_live edit that sets (or with "del", removes) the annotation.
marker() {
	if [ "$1" = del ]; then
		echo 'dep["spec"]["template"].setdefault("metadata", {}).setdefault("annotations", {}).pop("'$KEY'", None)'
	else
		echo 'dep["spec"]["template"].setdefault("metadata", {}).setdefault("annotations", {})["'$KEY'"] = "'"$1"'"'
	fi
}
# measure MODE EDIT: one run of MODE with the live Deployment edited.
measure() {
	rm -rf "${MEASURE_EVIDENCE_DIR:?}"/*
	make_live "$2" "$3"
	"$STAGE/measure.sh" "$1" --runs 1 --duration 30 >"$T/out" 2>"$T/err"
	echo $? >"$T/rc"
	run=$(latest_run)
}

# The rendered step-2..5 overlays carry the marker (inherited from step 2).
for o in step-2 step-3 step-4 step-5; do
	check "$o renders $KEY: \"true\"" bash -c '"$1" kustomize "$2" | grep -q "$3: \"true\""' _ "$REAL_KUBECTL" "$STAGE/k8s/steps/$o" "$KEY"
done

measure step-2 step-2 ""
check "step-2 calibrated: exit 0" [ "$(cat "$T/rc")" = 0 ]
check "step-2 calibrated: deployment-at-start.json in the evidence pack" [ -s "$run/deployment-at-start.json" ]
check "step-2 calibrated: JSON status calibrated" [ "$(status "$run")" = calibrated ]
check "step-2 calibrated: text says 8 s calibrated, right after the title" \
	[ "$(sed -n 2p "$run/scorecard.txt")" = "preStop：8 秒（已依本環境校正）" ]
check "step-2 calibrated: stdout shows the label" grep -q "^preStop：8 秒（已依本環境校正）" "$T/out"
check "step-2 calibrated: no 未經校正" bash -c '! grep -q "未經校正" "$1"' _ "$run/scorecard.txt"

# (The seconds stay 8: preflight requires the live spec to match the overlay.)
measure step-3 step-3 "$(marker false)"
check "step-3 conservative: JSON status uncalibrated" [ "$(status "$run")" = uncalibrated ]
check "step-3 conservative: text says preStop 未經校正" \
	grep -q "^preStop 未經校正：8 秒是講師手冊的保守值" "$run/scorecard.txt"
check "step-3 conservative: JSON marker false, 8 s" python3 -I -c '
import json, sys
p = json.load(open(sys.argv[1]))["prestop_calibration"]
sys.exit(0 if p["marker"] == "false" and p["prestop_s"] == 8 else 1)' "$run/scorecard.json"

measure step-2 step-2 "$(marker del)"
check "step-2 marker missing: JSON status unknown" [ "$(status "$run")" = unknown ]
check "step-2 marker missing: text says unknown, names the marker" \
	grep -q "^preStop 是否經過校正：未知（8 秒；Deployment 沒有 $KEY 標記）" "$run/scorecard.txt"
check "step-2 marker missing: not labelled calibrated" bash -c '! grep -q "已依本環境校正" "$1"' _ "$run/scorecard.txt"

measure step-4 step-4 "$(marker yes)"
check "step-4 marker 'yes': JSON status unknown" [ "$(status "$run")" = unknown ]
check "step-4 marker 'yes': text says unknown" grep -q "^preStop 是否經過校正：未知" "$run/scorecard.txt"

# rollback runs with the step-5 settings: labelled too.
measure rollback step-5 "$(marker false)"
check "rollback (step-5 settings) conservative: uncalibrated" [ "$(status "$run")" = uncalibrated ]

for m in step-0 step-1 control; do
	o=$m
	[ "$m" = control ] && o=step-0
	measure "$m" "$o" ""
	# (a clean step 0 exits 3: no predefined problem observed, A13)
	[ "$m" = step-0 ] || check "$m: exit 0" [ "$(cat "$T/rc")" = 0 ]
	check "$m: JSON status not_applicable" [ "$(status "$run")" = not_applicable ]
	check "$m: no preStop line in the scorecard" bash -c '! grep -q "^preStop" "$1"' _ "$run/scorecard.txt"
done

# scorecard.py directly: step 2 without the Deployment file is unknown,
# and running the action twice keeps one line.
measure step-2 step-2 ""
rm -f "$run/deployment-at-start.json"
python3 -I "$STAGE/scripts/lib/scorecard.py" "$run" prestop
python3 -I "$STAGE/scripts/lib/scorecard.py" "$run" prestop
check "no deployment-at-start.json: unknown" [ "$(status "$run")" = unknown ]
check "no deployment-at-start.json: text names the missing file" \
	grep -q "^preStop 是否經過校正：未知（秒數不明；沒有讀到量測開始時的 Deployment" "$run/scorecard.txt"
check "action twice: one preStop line" [ "$(grep -c '^preStop' "$run/scorecard.txt")" = 1 ]
finish
