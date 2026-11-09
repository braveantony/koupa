#!/usr/bin/env bash
# Ingress retries and late arrival (plan A10, the first four must-tests),
# reproduced locally against the same Envoy as Cilium's Gateway:
#
#   Rush Hour (cmd/ingress) -> Envoy 1.39.5 (podman) -> testproxy.HTTP -> App v2 -> test DB
#
# Five checks: late-arrival, retry-success, retry-ended-no-response,
# retry-ended-error (case 3's two sub-cases) and retry-uf. In every case
# the server-side result must be 未知 (unsettled) and the A10 state as
# listed in check_case. Evidence: evidence/ingress-<ts>/ (summary.txt,
# results.jsonl, envoy/access.log, app/<uid>.log, cases/<case>/scorecard.*).
# Exits non-zero on any FAIL. The EXIT trap stops Envoy, the harness (it
# owns the proxy) and the App, and drops the case database; the test DB
# container itself is left running.
#
# Envoy here has retries on (num_retries 1, as Cilium makes from an
# HTTPRoute `retry: {attempts: 1}`); the koupa HTTPRoute has no retry, so
# formal measurements run without ingress retries. This harness only checks
# the scorecard's classification if someone turns them on.
#
#   experiments/ingress/run.sh
#
# Test seam: INGRESS_RESULTS_CMD, when set, is run instead of the whole
# setup and harness; its stdout is taken as results.jsonl (one JSON object
# per case, see cmd/ingress). Ports: INGRESS_APP_PORT (18080),
# INGRESS_ENVOY_PORT (18000), INGRESS_ADMIN_PORT (18001).
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
stage=$(cd "$here/../.." && pwd)
EVID=${INGRESS_EVIDENCE:-$stage/evidence/ingress-$(date -u +%Y%m%dT%H%M%SZ)}
SUMMARY=$EVID/summary.txt
RESULTS=$EVID/results.jsonl
ENVOY_IMAGE=docker.io/envoyproxy/envoy:v1.39.5@sha256:15ecaee735013685c53d3e4e2189ccee6fbd20450ab1dffe98ef85f1e1b36d78
ENVOY_NAME=koupa-ingress-envoy
DB_CONTAINER=koupa-test-db
APP_PORT=${INGRESS_APP_PORT:-18080}
ENVOY_PORT=${INGRESS_ENVOY_PORT:-18000}
ADMIN_PORT=${INGRESS_ADMIN_PORT:-18001}
POD_UID=00000000-0000-4000-8000-000000000025
POD_NAME=koupa-ingress-local
CASES="late-arrival retry-success retry-ended-no-response retry-ended-error retry-uf"
mkdir -p "$EVID"
: >"$SUMMARY"
: >"$RESULTS"
app_pid="" harness_pid="" db_name="" envoy_started=""

log() { echo "ingress: $*" >&2; }

# expect CASE DESCRIPTION JQ-EXPR: the case's result object must satisfy
# the jq expression (evaluated with -e on that one object).
expect() {
	local c=$1 what=$2 expr=$3 obj
	obj=$(jq -c --arg c "$c" 'select(.case == $c)' "$RESULTS" 2>/dev/null | head -1)
	if [ -z "$obj" ]; then
		echo "FAIL $c - no result" >>"$SUMMARY"
		return 1
	fi
	if jq -e "$expr" <<<"$obj" >/dev/null 2>&1; then
		echo "ok   $c - $what" >>"$SUMMARY"
		return 0
	fi
	echo "FAIL $c - $what (got: $(jq -c '{error, upstream_attempts: .ingress.upstream_attempts, flags: .ingress.response_flags, settled, settle_reason, state, full_success, client_status, client_err, orders_at_eval, orders_after_release, duplicate_refs}' <<<"$obj"))" >>"$SUMMARY"
	return 1
}

# check_case CASE: the A10 expectations. Every case: the harness ran it
# (no error), the ingress logged the attempt, and the server-side result is
# 未知 (Settle: not settled; the scorecard has unknowns).
check_case() {
	local c=$1 ok=0
	expect "$c" "harness ran the case" '.error == ""' || return 1
	expect "$c" "scorecard is valid" '.invalid == false' || ok=1
	expect "$c" "伺服器端結果未知" '.settled == false and .has_unknown == true' || ok=1
	case $c in
	late-arrival)
		expect "$c" "入口轉給 Pod 一次（upstream_attempts 1）" '.ingress.upstream_attempts == 1' || ok=1
		expect "$c" "客人沒有收到完整回應，狀態「未知」" '.full_success == false and .client_status == null and .state == "unknown"' || ok=1
		expect "$c" "收尾時還沒有訂單，不印成確定的 0" '.orders_at_eval == 0 and .duplicate_refs.unknown >= 1' || ok=1
		expect "$c" "放行後下一次對帳查得到訂單" '.orders_after_release == 1' || ok=1
		;;
	retry-success)
		expect "$c" "入口重試（upstream_attempts 2）且回應成功" '.ingress.upstream_attempts == 2 and .ingress.status == 201 and .full_success == true and .client_status == 201' || ok=1
		expect "$c" "成功的嘗試不列入失敗狀態" '.state == ""' || ok=1
		expect "$c" "重複訂單不印成確定的 0" '.orders_at_eval == 1 and .duplicate_refs.confirmed == 0 and .duplicate_refs.unknown >= 1 and .duplicate_extra.unknown >= 1' || ok=1
		expect "$c" "第一次在收尾後才提交，對帳查得到兩張訂單" '.orders_after_release == 2' || ok=1
		;;
	retry-ended-no-response)
		expect "$c" "入口重試（upstream_attempts 2）" '.ingress.upstream_attempts == 2' || ok=1
		expect "$c" "第二次在 App 正常結束" '.app_ends_at_eval == 1 and .orders_at_eval == 1' || ok=1
		expect "$c" "客人沒有收到完整回應，狀態「未知」" '.full_success == false and .client_status == null and .state == "unknown"' || ok=1
		expect "$c" "放行後對帳查得到兩張訂單" '.orders_after_release == 2' || ok=1
		;;
	retry-ended-error)
		expect "$c" "入口重試（upstream_attempts 2）" '.ingress.upstream_attempts == 2' || ok=1
		expect "$c" "第二次在 App 正常結束" '.app_ends_at_eval == 1 and .orders_at_eval == 1' || ok=1
		expect "$c" "客人收到完整的 5xx，狀態「客人收到錯誤回應」" '.client_status >= 500 and .state == "client_got_error"' || ok=1
		expect "$c" "放行後對帳查得到兩張訂單" '.orders_after_release == 2' || ok=1
		;;
	retry-uf)
		expect "$c" "入口重試且最終旗標是 UF（upstream_attempts 2）" '.ingress.upstream_attempts == 2 and (.ingress.response_flags | split(",") | index("UF") != null)' || ok=1
		expect "$c" "本機回應 5xx：狀態「客人收到錯誤回應」，不是「沒送到 App」" '.client_status >= 500 and .state == "client_got_error"' || ok=1
		expect "$c" "放行後對帳查得到第一次建立的訂單" '.orders_at_eval == 0 and .orders_after_release == 1' || ok=1
		;;
	*)
		echo "FAIL $c - unknown case" >>"$SUMMARY"
		ok=1
		;;
	esac
	return $ok
}

# access_log_keys FILE: "key: %OPERATOR%" lines of an access-log JSON map.
access_log_keys() {
	sed -nE 's/^ *([a-z_]+): "(%[^"]*%)" *$/\1: \2/p' "$1" | sort
}

cleanup() {
	local rc=$?
	trap - EXIT
	trap "" INT TERM
	if [ -n "$envoy_started" ]; then
		podman logs "$ENVOY_NAME" >"$EVID/envoy/envoy.log" 2>&1
		podman rm -f "$ENVOY_NAME" >/dev/null 2>&1
	fi
	[ -n "$harness_pid" ] && kill "$harness_pid" 2>/dev/null && wait "$harness_pid" 2>/dev/null
	[ -n "$app_pid" ] && kill "$app_pid" 2>/dev/null && wait "$app_pid" 2>/dev/null
	if [ -n "$db_name" ]; then
		podman exec "$DB_CONTAINER" psql -q -U postgres -d postgres \
			-c "DROP DATABASE IF EXISTS $db_name WITH (FORCE)" >/dev/null 2>&1
	fi
	echo "--- summary ($EVID)"
	cat "$SUMMARY"
	grep -q '^FAIL' "$SUMMARY" && rc=1
	exit $rc
}
trap cleanup EXIT
trap 'exit 130' INT TERM

fail_setup() {
	echo "FAIL setup - $*" >>"$SUMMARY"
	exit 1
}

wait_http() { # URL SECONDS
	local i
	for i in $(seq 1 $(($2 * 5))); do
		curl -fsS -o /dev/null --max-time 1 "$1" 2>/dev/null && return 0
		sleep 0.2
	done
	return 1
}

setup_and_run() {
	# 0. The local Envoy must log exactly the cluster's access-log keys.
	if ! diff <(access_log_keys "$stage/k8s/base/gateway.yaml") <(access_log_keys "$here/envoy.yaml") >"$EVID/access-log-keys.diff"; then
		fail_setup "envoy.yaml access-log keys differ from k8s/base/gateway.yaml (see access-log-keys.diff)"
	fi
	[ -s "$EVID/access-log-keys.diff" ] || echo "ok   setup - envoy.yaml access-log keys match k8s/base/gateway.yaml" >>"$SUMMARY"

	# 1. Test database: reuse a running scripts/test-db.sh container, else start one.
	if [ "$(podman exec "$DB_CONTAINER" psql -tA -U postgres -d postgres \
		-c "SELECT 1 FROM pg_database WHERE datname = 'koupa_template'" 2>/dev/null)" != 1 ]; then
		log "starting the test database"
		(cd "$stage" && scripts/test-db.sh >/dev/null) || fail_setup "scripts/test-db.sh failed"
	fi
	db_name=koupa_ingress_$(date -u +%Y%m%d%H%M%S)
	local i
	for i in 1 2 3 4 5; do
		podman exec "$DB_CONTAINER" psql -q -U postgres -d postgres \
			-c "CREATE DATABASE $db_name TEMPLATE koupa_template OWNER koupa" >/dev/null 2>&1 && break
		[ "$i" = 5 ] && fail_setup "cannot create database $db_name"
		sleep 1
	done

	# 2. Build the App (v2) and the harness.
	local bin=$stage/bin/ingress
	mkdir -p "$bin"
	log "building"
	(cd "$stage" && CGO_ENABLED=0 scripts/go build -ldflags "-X koupa/app/config.Version=v2" -o "$bin/koupa" ./app &&
		CGO_ENABLED=0 scripts/go build -o "$bin/ingress" ./experiments/ingress/cmd/ingress) >"$EVID/build.log" 2>&1 ||
		fail_setup "build failed (see build.log)"

	# 3. App: stdout is the Pod log Rush Hour reads (--app-logs layout).
	mkdir -p "$EVID/app" "$EVID/envoy" "$EVID/cases"
	local collect_start
	collect_start=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
	DATABASE_URL="postgres://koupa:koupa@127.0.0.1:55432/$db_name?sslmode=disable" \
		POD_UID=$POD_UID POD_NAME=$POD_NAME LISTEN_ADDR=127.0.0.1:$APP_PORT \
		IDEMPOTENCY_KEY=0 GRACEFUL_SHUTDOWN=1 TEST_HOOKS= TEST_MUTATION= \
		"$bin/koupa" >"$EVID/app/$POD_UID.log" 2>"$EVID/app-stderr.log" &
	app_pid=$!
	wait_http "http://127.0.0.1:$APP_PORT/healthz" 30 || fail_setup "App did not become healthy (see app-stderr.log)"

	# 4. Harness (owns testproxy.HTTP); it writes the proxy address, then
	# waits for Envoy.
	"$bin/ingress" --app "http://127.0.0.1:$APP_PORT" --envoy "http://127.0.0.1:$ENVOY_PORT" \
		--access-log "$EVID/envoy/access.log" --app-logs "$EVID/app" \
		--pod-uid "$POD_UID" --pod-name "$POD_NAME" --collect-start "$collect_start" \
		--out "$EVID/cases" --proxy-addr-file "$EVID/proxy.addr" \
		>"$RESULTS" 2>"$EVID/harness.log" &
	harness_pid=$!
	for i in $(seq 1 50); do [ -s "$EVID/proxy.addr" ] && break; sleep 0.2; done
	[ -s "$EVID/proxy.addr" ] || fail_setup "harness wrote no proxy address (see harness.log)"
	local proxy_port
	proxy_port=$(sed 's/.*://' "$EVID/proxy.addr")

	# 5. Envoy.
	sed -e "s/__ADMIN_PORT__/$ADMIN_PORT/" -e "s/__LISTEN_PORT__/$ENVOY_PORT/" \
		-e "s/__PROXY_PORT__/$proxy_port/" "$here/envoy.yaml" >"$EVID/envoy/envoy.yaml"
	: >"$EVID/envoy/access.log"
	podman rm -f "$ENVOY_NAME" >/dev/null 2>&1
	podman run -d --name "$ENVOY_NAME" --network host -e ENVOY_UID=0 \
		-v "$EVID/envoy:/logs:Z" "$ENVOY_IMAGE" \
		-c /logs/envoy.yaml --file-flush-interval-msec 100 --log-level warn >/dev/null ||
		fail_setup "podman run envoy failed"
	envoy_started=1

	wait "$harness_pid"
	local code=$?
	harness_pid=""
	[ "$code" = 0 ] || echo "FAIL harness - exited $code (see harness.log)" >>"$SUMMARY"
}

if [ -n "${INGRESS_RESULTS_CMD:-}" ]; then
	$INGRESS_RESULTS_CMD >"$RESULTS" || echo "FAIL harness - results command exited $?" >>"$SUMMARY"
else
	setup_and_run
fi
for c in $CASES; do
	check_case "$c"
done
