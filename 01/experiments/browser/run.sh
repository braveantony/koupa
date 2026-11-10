#!/usr/bin/env bash
# Run the Playwright page tests against a locally built koupa (v2).
# Needs the test database (scripts/test-db.sh) to be up. Creates a fresh
# database cloned from koupa_template, starts the app on a free local port,
# runs the tests and cleans up. The shutdown test starts its own instance.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
DB=koupa_pw_$$
APP=
psql_su() { podman exec koupa-test-db psql -q -U postgres "$@"; }
cleanup() {
  [ -n "$APP" ] && kill "$APP" 2>/dev/null && wait "$APP" 2>/dev/null
  psql_su -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >&2 || true
}
trap cleanup EXIT
psql_su -c "CREATE DATABASE $DB TEMPLATE koupa_template OWNER koupa" >&2
(cd "$ROOT" && scripts/go build -ldflags "-X koupa/app/config.Version=v2" -o bin/koupa ./app)
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
UID_MAIN="pw-main-$$-$PORT"
export DATABASE_URL="postgres://koupa:koupa@127.0.0.1:55432/$DB?sslmode=disable" # also used by the shutdown test's own instance
POD_UID=$UID_MAIN POD_NAME=$UID_MAIN GRACEFUL_SHUTDOWN=1 LISTEN_ADDR=127.0.0.1:$PORT \
  "$ROOT/bin/koupa" >/dev/null 2>&1 &
APP=$!
# Ready only when our own child answers with our pod uid.
ready=
for _ in $(seq 1 40); do
  kill -0 "$APP" 2>/dev/null || { echo "run.sh: app exited before it was ready" >&2; exit 1; }
  if curl -sf "http://127.0.0.1:$PORT/api/whoami" | grep -q "$UID_MAIN"; then ready=1; break; fi
  sleep 0.5
done
[ -n "$ready" ] || { echo "run.sh: app not ready" >&2; exit 1; }
BASE_URL="http://127.0.0.1:$PORT" KOUPA_BIN="$ROOT/bin/koupa" node_modules/.bin/playwright test "$@"
