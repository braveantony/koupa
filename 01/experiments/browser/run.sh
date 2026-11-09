#!/usr/bin/env bash
# Run the Playwright page tests against a locally built koupa (v2).
# Needs the test database (scripts/test-db.sh) to be up. Creates a fresh
# database cloned from koupa_template, starts the app on 127.0.0.1:18080,
# runs the tests and cleans up. The shutdown test starts its own instance.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
DB=koupa_pw_$$
podman exec koupa-test-db psql -q -U postgres -c "CREATE DATABASE $DB TEMPLATE koupa_template OWNER koupa" >&2
(cd "$ROOT" && scripts/go build -ldflags "-X koupa/app/config.Version=v2" -o bin/koupa ./app)
export DATABASE_URL="postgres://koupa:koupa@127.0.0.1:55432/$DB?sslmode=disable"
export POD_UID=test POD_NAME=test GRACEFUL_SHUTDOWN=1 LISTEN_ADDR=127.0.0.1:18080
"$ROOT/bin/koupa" >/dev/null 2>&1 &
APP=$!
cleanup() {
  kill "$APP" 2>/dev/null || true
  podman exec koupa-test-db psql -q -U postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >&2 || true
}
trap cleanup EXIT
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:18080/healthz >/dev/null && break; sleep 0.5; done
KOUPA_BIN="$ROOT/bin/koupa" node_modules/.bin/playwright test "$@"
