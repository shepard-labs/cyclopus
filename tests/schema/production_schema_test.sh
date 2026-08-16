#!/bin/sh
set -eu

: "${POSTGRES_TEST_URL:?POSTGRES_TEST_URL must point to PostgreSQL 18}"

schema="cyclopus_schema_test_$(date +%s)_$$"
root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)

run_with_timeout() {
    seconds=$1
    shift
    perl -e 'alarm shift; exec @ARGV' "$seconds" "$@"
}

cleanup() {
    run_with_timeout 20 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
        -c "DROP SCHEMA IF EXISTS $schema CASCADE" >/dev/null || true
}
trap cleanup EXIT INT TERM

run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -c "CREATE SCHEMA $schema" >/dev/null
for migration in "$root"/internal/store/migrations/*.up.sql; do
    PGOPTIONS="-c search_path=$schema" run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
        -f "$migration" >/dev/null
done
PGOPTIONS="-c search_path=$schema" run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -f "$root/tests/schema/production_schema_test.sql" >/dev/null

ids=$(PGOPTIONS="-c search_path=$schema" run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -At -v ON_ERROR_STOP=1 \
    -c "SELECT org.id || '|' || app.id || '|' || environment.id FROM orgs org JOIN projects project ON project.org_id = org.id JOIN apps app ON app.project_id = project.id JOIN environments environment ON environment.app_id = app.id WHERE org.slug = 'schema-org' AND app.slug = 'schema-app' AND environment.kind = 'production'")
IFS='|' read -r org_id app_id environment_id <<EOF
$ids
EOF

enqueue() {
    suffix=$1
    key=$2
    PGOPTIONS="-c search_path=$schema" run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
        -c "SELECT * FROM enqueue_orchestration_push('$org_id', '$app_id', '$environment_id', 701, '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main', repeat('$suffix', 40), '$key')" >/dev/null
}
enqueue a concurrent-identical & first=$!
enqueue a concurrent-identical & second=$!
wait "$first"
wait "$second"

PGOPTIONS="-c search_path=$schema" run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -c "DO \$\$ BEGIN IF NOT EXISTS (SELECT 1 FROM apps app JOIN environments environment ON environment.app_id = app.id WHERE app.id = '$app_id' AND environment.id = '$environment_id' AND app.desired_generation = 3 AND environment.desired_generation = 3) THEN RAISE EXCEPTION 'concurrent identical replay advanced generation more than once'; END IF; IF (SELECT count(*) FROM orchestration_jobs WHERE app_id = '$app_id' AND environment_id = '$environment_id' AND status = 'queued') <> 1 THEN RAISE EXCEPTION 'concurrent identical replay created duplicate jobs'; END IF; END \$\$" >/dev/null

enqueue b concurrent-b & first=$!
enqueue c concurrent-c & second=$!
wait "$first"
wait "$second"

PGOPTIONS="-c search_path=$schema" run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -c "DO \$\$ BEGIN IF NOT EXISTS (SELECT 1 FROM apps app JOIN environments environment ON environment.app_id = app.id WHERE app.id = '$app_id' AND environment.id = '$environment_id' AND app.desired_generation = 5 AND environment.desired_generation = 5) THEN RAISE EXCEPTION 'concurrent distinct pushes lost a generation increment'; END IF; IF NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE app_id = '$app_id' AND environment_id = '$environment_id' AND status = 'queued' AND desired_generation = 3 AND pending_desired_generation = 5 AND desired_checkout_sha = repeat('a', 40) AND pending_checkout_sha IN (repeat('b', 40), repeat('c', 40))) THEN RAISE EXCEPTION 'queued attempt was not preserved with the latest pending result'; END IF; END \$\$" >/dev/null

# Synchronize through server-observed session state rather than assuming sleeps
# established ordering. The enqueue must be waiting on the advisory lock before
# the holder is terminated; only then may it complete.
holder_name="cyclopus-lock-holder-$$"
enqueue_name="cyclopus-lock-waiter-$$"
PGAPPNAME="$holder_name" PGOPTIONS="-c search_path=$schema" run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -c "BEGIN; SELECT pg_advisory_xact_lock(hashtextextended('$org_id', 1380270412)); SELECT pg_sleep(30); COMMIT" >/dev/null 2>&1 & tombstone_lock=$!

attempt=0
while ! run_with_timeout 5 psql "$POSTGRES_TEST_URL" -X -At -v ON_ERROR_STOP=1 \
    -c "SELECT EXISTS (SELECT 1 FROM pg_stat_activity activity JOIN pg_locks held ON held.pid=activity.pid WHERE activity.application_name='$holder_name' AND held.locktype='advisory' AND held.granted)" | tr -d '[:space:]' | grep -q '^t$'; do
    attempt=$((attempt + 1))
    [ "$attempt" -lt 20 ] || { printf '%s\n' 'advisory lock holder was not observed' >&2; exit 1; }
    sleep 1
done

PGAPPNAME="$enqueue_name" PGOPTIONS="-c search_path=$schema" run_with_timeout 120 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -c "SELECT * FROM enqueue_orchestration_push('$org_id', '$app_id', '$environment_id', 701, '.github/workflows/deploy.yml', repeat('4', 40), 'refs/heads/main', repeat('d', 40), 'tombstone-order')" >/dev/null & ordered_enqueue=$!

attempt=0
while ! run_with_timeout 5 psql "$POSTGRES_TEST_URL" -X -At -v ON_ERROR_STOP=1 \
    -c "SELECT EXISTS (SELECT 1 FROM pg_stat_activity activity JOIN pg_locks waiting ON waiting.pid=activity.pid WHERE activity.application_name='$enqueue_name' AND waiting.locktype='advisory' AND NOT waiting.granted AND activity.wait_event_type='Lock')" | tr -d '[:space:]' | grep -q '^t$'; do
    attempt=$((attempt + 1))
    [ "$attempt" -lt 20 ] || { printf '%s\n' 'enqueue was not observed blocked on advisory lock' >&2; exit 1; }
    kill -0 "$ordered_enqueue"
    sleep 1
done

run_with_timeout 5 psql "$POSTGRES_TEST_URL" -X -v ON_ERROR_STOP=1 \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='$holder_name'" >/dev/null
wait "$tombstone_lock" || true
wait "$ordered_enqueue"

printf '%s\n' 'production schema integration tests passed'
