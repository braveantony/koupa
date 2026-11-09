#!/usr/bin/env bash
# The gate and the diagnostic banner are decided before any scorecard is
# printed: on stdout too, every scorecard of a --diagnostic invocation
# whose control failed starts with the banner line.
. "$(dirname "$0")/lib.sh"
setup_fakes
echo 15 >"$FAKE/rh-backdate"
banner='診斷模式：對照組不成立，不可用於比較'

# every_card_bannered FILE N: N scorecard headers on stdout, each right
# after the banner line; and the saved files start with it.
every_card_bannered() {
	python3 -I - "$1" "$2" "$banner" <<'PY'
import sys
lines = open(sys.argv[1], encoding="utf-8").read().splitlines()
n, banner = int(sys.argv[2]), sys.argv[3]
heads = [i for i, l in enumerate(lines) if l.startswith("Rush Hour 成績單")]
ok = len(heads) == n and all(i > 0 and lines[i - 1] == banner for i in heads)
if not ok:
    print("    headers at", heads, [lines[i - 1] if i else None for i in heads])
sys.exit(0 if ok else 1)
PY
}

make_live step-0
scorecard 's["has_unknown"] = True; s["items"]["missing"]["board-1"]["unknown"] = 2'
"$STAGE/measure.sh" control --runs 2 --duration 30 --diagnostic >"$T/out" 2>/dev/null
check "control --diagnostic: exit 0" [ $? -eq 0 ]
check "stdout: both scorecards start with the banner" every_card_bannered "$T/out" 2
for f in "$MEASURE_EVIDENCE_DIR"/control-*/scorecard.txt; do
	check "file: banner on line 1 ($(basename "$(dirname "$f")"))" [ "$(head -1 "$f")" = "$banner" ]
done

rm -rf "$MEASURE_EVIDENCE_DIR"/*
scorecard_clean
make_live step-0
"$STAGE/measure.sh" step-0 --runs 2 --duration 30 --diagnostic >"$T/out" 2>/dev/null
check "step-0 nothing observed --diagnostic: stdout bannered" every_card_bannered "$T/out" 2

rm -rf "$MEASURE_EVIDENCE_DIR"/*
make_live step-0
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>/dev/null
check "clean control: scorecard printed without banner" bash -c '! grep -q "$2" "$1" && grep -q "^Rush Hour 成績單" "$1"' _ "$T/out" "$banner"
finish
