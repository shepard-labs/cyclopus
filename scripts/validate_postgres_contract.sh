#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture="$ROOT/tests/workflow/fixtures/pending_sha_pg18.sql"

test -s "$fixture" || { printf '%s\n' "missing PostgreSQL contract fixture: $fixture" >&2; exit 1; }

if [ -z "${POSTGRES_CONTRACT_URL:-}" ]; then
  printf '%s\n' "BLOCKED: set POSTGRES_CONTRACT_URL to a disposable PostgreSQL 18 database to execute the contract." >&2
  exit 2
fi
command -v psql >/dev/null 2>&1 || { printf '%s\n' "BLOCKED: psql is required" >&2; exit 2; }

version=$(psql "$POSTGRES_CONTRACT_URL" -XAtqc "SHOW server_version_num")
test "$version" -ge 180000 || { printf '%s\n' "PostgreSQL 18+ required, observed server_version_num=$version" >&2; exit 1; }
psql "$POSTGRES_CONTRACT_URL" -X -v ON_ERROR_STOP=1 -f "$fixture"
