#!/bin/sh
set -eu

: "${POSTGRES_TEST_URL:?POSTGRES_TEST_URL must point to PostgreSQL 18}"

schema="cyclopus_schema_test_$(date +%s)_$$"
root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)

cleanup() {
    psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 -c "DROP SCHEMA IF EXISTS $schema CASCADE" >/dev/null
}
trap cleanup EXIT INT TERM

psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 -c "CREATE SCHEMA $schema" >/dev/null
PGOPTIONS="-c search_path=$schema" psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -f "$root/internal/store/migrations/000001_foundation.up.sql" >/dev/null
PGOPTIONS="-c search_path=$schema" psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -f "$root/tests/schema/production_schema_test.sql" >/dev/null

printf '%s\n' 'production schema integration tests passed'
