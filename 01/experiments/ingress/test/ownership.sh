#!/usr/bin/env bash
# Check that experiments/ingress/run.sh touches only what it created:
#   1. a database container that exists but is not usable fails the setup
#      and is left in place (scripts/test-db.sh is not run);
#   2. two runs at the same time (different ports) both PASS, and neither
#      removes the other's resources nor bystanders: a container under the
#      old fixed Envoy name, a koupa_ingress_* database it did not create,
#      and the shared test DB container (same container id afterwards).
# Needs the koupa-test-db container running (scripts/test-db.sh).
# Evidence: evidence/ingress-ownership-<ts>/. Exits non-zero on any FAIL.
set -uo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
stage=$(cd "$here/../.." && pwd)
out=$stage/evidence/ingress-ownership-$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$out"
tag=$(od -An -N4 -tx4 /dev/urandom | tr -d ' ')
fake=koupa-ingress-fakedb-$tag
by_ctr=koupa-ingress-envoy # the name run.sh used to force-remove
by_db=koupa_ingress_bystander_$tag
db=koupa-test-db
rc=0
made_ctr=""
pass() { echo "ok   $*"; }
fail() { echo "FAIL $*"; rc=1; }
psql_db() { podman exec "$db" psql -tA -U postgres -d postgres -c "$1"; }

cleanup() {
	podman rm -f "$fake" >/dev/null 2>&1
	[ -n "$made_ctr" ] && podman rm -f "$by_ctr" >/dev/null 2>&1
	psql_db "DROP DATABASE IF EXISTS $by_db" >/dev/null 2>&1
}
trap cleanup EXIT

db_id=$(podman inspect -f '{{.Id}}' "$db" 2>/dev/null) || { echo "FAIL $db is not running"; exit 1; }

# 1. Unusable database container.
podman create --name "$fake" docker.io/library/postgres:17 >/dev/null
INGRESS_DB_CONTAINER=$fake INGRESS_EVIDENCE=$out/unusable-db "$here/run.sh" >"$out/unusable-db.txt" 2>&1
code=$?
[ "$code" != 0 ] && pass "unusable db container: run.sh exited $code" || fail "unusable db container: run.sh exited 0"
grep -q 'exists but is not a usable test DB' "$out/unusable-db/summary.txt" &&
	pass "unusable db container: setup failed with the expected reason" ||
	fail "unusable db container: reason missing ($(head -3 "$out/unusable-db/summary.txt"))"
podman container exists "$fake" && pass "unusable db container: left in place" || fail "unusable db container: removed"
[ "$(podman inspect -f '{{.Id}}' "$db")" = "$db_id" ] && pass "$db untouched" || fail "$db was replaced"

# 2. Bystanders, then two concurrent runs.
if podman container exists "$by_ctr"; then
	echo "note: $by_ctr already exists; using it as the bystander"
else
	podman create --name "$by_ctr" docker.io/library/postgres:17 >/dev/null && made_ctr=1
fi
by_id=$(podman inspect -f '{{.Id}}' "$by_ctr")
psql_db "CREATE DATABASE $by_db" >/dev/null

INGRESS_EVIDENCE=$out/run-a "$here/run.sh" >"$out/run-a.txt" 2>&1 &
pa=$!
INGRESS_EVIDENCE=$out/run-b INGRESS_APP_PORT=18180 INGRESS_ENVOY_PORT=18100 INGRESS_ADMIN_PORT=18101 \
	"$here/run.sh" >"$out/run-b.txt" 2>&1 &
pb=$!
wait "$pa"; ca=$?
wait "$pb"; cb=$?
for r in a b; do
	c=$([ $r = a ] && echo $ca || echo $cb)
	n_ok=$(grep -c '^ok' "$out/run-$r/summary.txt")
	n_fail=$(grep -c '^FAIL' "$out/run-$r/summary.txt")
	[ "$c" = 0 ] && [ "$n_fail" = 0 ] && pass "concurrent run $r: exit 0, $n_ok ok, 0 FAIL" ||
		fail "concurrent run $r: exit $c, $n_ok ok, $n_fail FAIL"
done
[ "$(podman inspect -f '{{.Id}}' "$by_ctr" 2>/dev/null)" = "$by_id" ] && pass "bystander container $by_ctr untouched" || fail "bystander container $by_ctr removed"
[ "$(psql_db "SELECT 1 FROM pg_database WHERE datname = '$by_db'")" = 1 ] && pass "bystander database $by_db untouched" || fail "bystander database $by_db dropped"
[ "$(podman inspect -f '{{.Id}}' "$db")" = "$db_id" ] && pass "$db untouched" || fail "$db was replaced"
left_db=$(psql_db "SELECT datname FROM pg_database WHERE datname LIKE 'koupa\_ingress\_%' AND datname NOT LIKE 'koupa\_ingress\_bystander\_%'")
[ -z "$left_db" ] && pass "no case database left behind" || fail "case databases left: $left_db"
left_ctr=$(podman ps -a --format '{{.Names}}' | grep '^koupa-ingress-envoy-' || true)
[ -z "$left_ctr" ] && pass "no Envoy container left behind" || fail "Envoy containers left: $left_ctr"
ls -d "$stage"/bin/ingress-* >/dev/null 2>&1 && fail "build directories left: $(ls -d "$stage"/bin/ingress-*)" || pass "no build directory left behind"
echo "--- evidence: $out"
exit $rc
