#!/usr/bin/env bash
# Start a throwaway PostgreSQL 17, build koupa_template with all migrations
# applied, and print ONLY the export line on stdout (logs go to stderr):
#   eval "$(scripts/test-db.sh)"
set -euo pipefail
cd "$(dirname "$0")/.."

NAME=koupa-test-db
PORT=55432
IMAGE=docker.io/library/postgres:17@sha256:996e9dc583ba9f0265a2c75547a9c95fb22c251e394749cab205a0a00e1856a1
MIGRATE_IMAGE=docker.io/migrate/migrate:v4.18.1
SUPER_PW=postgres
KOUPA_PW=koupa

log() { echo "test-db: $*" >&2; }

podman rm -f "$NAME" >/dev/null 2>&1 || true
log "starting $NAME on 127.0.0.1:$PORT"
podman run -d --name "$NAME" -p "127.0.0.1:$PORT:5432" \
  -e POSTGRES_PASSWORD="$SUPER_PW" "$IMAGE" \
  -c track_commit_timestamp=on >/dev/null

psql_su() { podman exec -i "$NAME" psql -v ON_ERROR_STOP=1 -q -U postgres "$@" >&2; }

for i in $(seq 1 60); do
  # Official image restarts once after init; require two consecutive successes.
  if podman exec "$NAME" pg_isready -q -h 127.0.0.1 -U postgres 2>/dev/null; then
    sleep 1
    podman exec "$NAME" pg_isready -q -h 127.0.0.1 -U postgres 2>/dev/null && break
  fi
  sleep 1
  [ "$i" = 60 ] && { log "postgres did not become ready"; exit 1; }
done

psql_su -d postgres <<SQL
CREATE ROLE koupa LOGIN PASSWORD '$KOUPA_PW';
CREATE DATABASE koupa_template OWNER koupa;
SQL

if ls migrations/*.sql >/dev/null 2>&1; then
  log "applying migrations"
  podman run --rm --network host -v "$PWD/migrations:/migrations:Z" "$MIGRATE_IMAGE" \
    -path=/migrations \
    -database "postgres://koupa:$KOUPA_PW@127.0.0.1:$PORT/koupa_template?sslmode=disable" up >&2
else
  log "no migrations yet; template stays empty"
fi

# Superuser DSN: testdb.New runs CREATE DATABASE ... TEMPLATE koupa_template, then connects as koupa.
echo "export KOUPA_TEST_DSN='postgres://postgres:$SUPER_PW@127.0.0.1:$PORT/postgres?sslmode=disable'"
