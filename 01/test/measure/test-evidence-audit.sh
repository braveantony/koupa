#!/usr/bin/env bash
# evidence.sh checks the /api/audit evidence: the re-queries must succeed
# (HTTP 200, usable JSON) or be marked FAILED in the index and
# audit/status.json; the audit the scorecard used must be usable, else the
# pack is incomplete (exit 1).
. "$(dirname "$0")/lib.sh"
setup_fakes
make_live step-0
mk_run() {
	run=$MEASURE_EVIDENCE_DIR/control-20260101-000000-r1-abcd
	rm -rf "$run"
	mkdir -p "$run/app" "$run/manifest"
	echo '{"run_id":"control-20260101-000000-r1-abcd","overlay":"control","image_before":"ghcr.io/braveantony/koupa:v1","base_url":"http://172.22.0.201","rush_hour":{"kind":"bin","ref":"/bin/true"}}' >"$run/params.json"
	echo '{"koupa":{}}' >"$run/image-digests.json"
	sed 's/@RUN@/control-20260101-000000-r1-abcd/g' "$FAKE/scorecard.json" >"$run/scorecard.json"
	echo x >"$run/scorecard.txt"; echo x >"$run/manifest/applied.yaml"; echo '{}' >"$run/rollout.json"
	: >"$run/attempts.jsonl"; : >"$run/boards.jsonl"
	"$COLLECT_TIMELINE" start "$run"; "$COLLECT_TIMELINE" stop "$run"
}

mk_run
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
check "all good: exit 0" [ $? -eq 0 ]
check "status.json: cursor ok" python3 -I -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if all(s[m]["ok"] for m in ("cursor","orders","ready")) else 1)' "$run/audit/status.json"

mk_run
touch "$FAKE/curl-fail"
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
check "unreachable: index FAILED audit/cursor.json" grep -q '^FAILED  audit/cursor.json' "$run/evidence-index.txt"
check "unreachable: status.json not ok" python3 -I -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if not s["cursor"]["ok"] and s["cursor"]["error"] else 1)' "$run/audit/status.json"
check "unreachable: no bare audit/cursor.json left" [ ! -e "$run/audit/cursor.json" ]
rm -f "$FAKE/curl-fail"

mk_run
touch "$FAKE/curl-500"
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
check "HTTP 500: FAILED audit/ready.json" grep -q '^FAILED  audit/ready.json' "$run/evidence-index.txt"
check "HTTP 500: status records 500" grep -q '"http": "500"' "$run/audit/status.json"
rm -f "$FAKE/curl-500"

# The scorecard's own audit missing or unusable: the pack is incomplete.
scorecard 's["facts"]["audit"] = None'
mk_run
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
check "scorecard audit null: exit 1" [ $? -eq 1 ]
check "scorecard audit null: FAILED used-by-scorecard" grep -q '^FAILED  audit/used-by-scorecard.json' "$run/evidence-index.txt"
scorecard 's["facts"]["audit"]["cursor"] = "x"'
mk_run
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
check "scorecard audit cursor not an int: exit 1" [ $? -eq 1 ]

# The orders answer, strictly: an object keyed by "<run-id>-..." whose
# values are non-empty lists of integer order ids; {} only with 0 orders.
scorecard_clean
while IFS='|' read -r what body; do
	mk_run
	printf '%s\n' "$body" >"$FAKE/audit-orders"
	"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
	rc=$?
	check "re-query orders $what: FAILED, pack incomplete (rc=$rc)" bash -c 'grep -q "^FAILED  audit/orders.json" "$1" && [ "$2" -eq 1 ]' _ "$run/evidence-index.txt" "$rc"
done <<'CASES'
{}|{}
[]|[]
error object|{"error":"database unavailable"}
wrong-run keys|{"control-20250101-000000-r1-ffff-c1":[1]}
non-integer ids|{"@RUN@-c1":["1"]}
empty id list|{"@RUN@-c1":[]}
bool id|{"@RUN@-c1":[true]}
CASES
rm -f "$FAKE/audit-orders"

while IFS='|' read -r what edit; do
	scorecard "$edit"
	mk_run
	"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
	rc=$?
	check "scorecard audit orders $what: FAILED used-by-scorecard (rc=$rc)" bash -c 'grep -q "^FAILED  audit/used-by-scorecard.json" "$1" && [ "$2" -eq 1 ]' _ "$run/evidence-index.txt" "$rc"
done <<'CASES'
{}|s["facts"]["audit"]["orders"] = {}
[]|s["facts"]["audit"]["orders"] = []
error object|s["facts"]["audit"]["orders"] = {"error": "x"}
wrong-run keys|s["facts"]["audit"]["orders"] = {"other-run-c1": [1]}
non-integer ids|s["facts"]["audit"]["orders"] = {"@RUN@-c1": [1.5]}
CASES

# Zero orders: {} is the right answer, in both places.
scorecard 's["items"]["orders"]["confirmed"] = 0; s["facts"]["audit"]["orders"] = {}'
echo '{}' >"$FAKE/audit-orders"
mk_run
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
check "0 orders with {}: exit 0" [ $? -eq 0 ]
rm -f "$FAKE/audit-orders"

# The order count must be the integer 0, not false (False == 0 in Python).
scorecard 's["items"]["orders"]["confirmed"] = False; s["facts"]["audit"]["orders"] = {}'
echo '{}' >"$FAKE/audit-orders"
mk_run
"$STAGE/scripts/evidence.sh" "$run" >"$T/out" 2>&1
rc=$?
check "order count false with {}: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "order count false: FAILED orders and used-by-scorecard" bash -c 'grep -q "^FAILED  audit/orders.json" "$1" && grep -q "^FAILED  audit/used-by-scorecard.json" "$1"' _ "$run/evidence-index.txt"
rm -f "$FAKE/audit-orders"
finish
