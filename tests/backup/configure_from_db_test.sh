#!/bin/sh
# Offline unit tests for infra/backup/configure_from_db.sh. Host dependencies
# (sudo, psql, k3s kubectl, systemctl, runuser, install_walg.sh) are stubbed in
# a temp PATH / via the script's env-override seams; no live Postgres or cluster.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
SCRIPT="$ROOT/infra/backup/configure_from_db.sh"
assertions=0
tmp=$(mktemp -d "${TMPDIR:-/tmp}/cyclopus-configure-from-db-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

fail() { printf '%s\n' "not ok - $1" >&2; exit 1; }
pass() { assertions=$((assertions + 1)); printf '%s\n' "ok - $1"; }

perms() { stat -f %Lp "$1" 2>/dev/null || stat -c %a "$1"; }

# ---------------------------------------------------------------- stubs ------
BIN="$tmp/bin"
mkdir -p "$BIN"

# sudo: strip -n / -u USER flags, exec the rest (as require_root/as_pg call it).
cat > "$BIN/sudo" <<'EOF'
#!/bin/sh
while [ $# -gt 0 ]; do
  case $1 in
    -n) shift ;;
    -u) shift 2 ;;
    *) break ;;
  esac
done
exec "$@"
EOF

# runuser (root path of as_pg): strip "-u USER --", exec the rest.
cat > "$BIN/runuser" <<'EOF'
#!/bin/sh
while [ $# -gt 0 ]; do
  case $1 in
    -u) shift 2 ;;
    --) shift; break ;;
    *) break ;;
  esac
done
exec "$@"
EOF

# psql: answer by SQL text. PSQL_STUB_COUNT feeds SELECT count(*) from
# storage_destinations WITHOUT org filter (default 1), PSQL_STUB_ORG_COUNT the
# ORG_ID-scoped one (default 1); PSQL_STUB_ROW feeds the storage_destinations
# SELECT (empty = no rows); PSQL_STUB_ARCHIVE_MODE feeds SHOW archive_mode.
cat > "$BIN/psql" <<'EOF'
#!/bin/sh
for a in "$@"; do
  case $a in
    *count*)
      case $a in
        *org_id*) printf '%s\n' "${PSQL_STUB_ORG_COUNT:-1}" ;;
        *) printf '%s\n' "${PSQL_STUB_COUNT:-1}" ;;
      esac ;;
    *"SHOW archive_mode"*) printf '%s\n' "${PSQL_STUB_ARCHIVE_MODE:-on}" ;;
    *storage_destinations*) [ -n "${PSQL_STUB_ROW:-}" ] && printf '%s\n' "$PSQL_STUB_ROW" ;;
  esac
done
exit 0
EOF

# k3s kubectl: emit base64 for the requested data key of the fake Secret.
# K3S_STUB_MISSING=<key> simulates the key being absent from the Secret.
cat > "$BIN/k3s" <<'EOF'
#!/bin/sh
key=
for a in "$@"; do
  case $a in
    jsonpath=\{.data.*\})
      key=${a#jsonpath={.data.}
      key=${key%\}} ;;
  esac
done
case $key in
  access_key_id) [ "${K3S_STUB_MISSING:-}" = access_key_id ] || printf '%s' "${K3S_STUB_ACCESS_KEY_ID:-ak-id-123}" | base64 ;;
  secret_access_key) [ "${K3S_STUB_MISSING:-}" = secret_access_key ] || printf '%s' "${K3S_STUB_SECRET_ACCESS_KEY:-sk-secret-456}" | base64 ;;
esac
exit 0
EOF

# systemctl: log invocations; list-timers/is-enabled print plausible output.
cat > "$BIN/systemctl" <<'EOF'
#!/bin/sh
printf 'systemctl %s\n' "$*" >> "${SYSTEMCTL_LOG:?SYSTEMCTL_LOG must be set}"
case " $* " in
  *" is-enabled "*) printf 'enabled\n' ;;
  *" list-timers "*) printf 'NEXT                        LEFT LAST PASSED UNIT ACTIVATES\n' ;;
esac
exit 0
EOF

chmod 0755 "$BIN/sudo" "$BIN/runuser" "$BIN/psql" "$BIN/k3s" "$BIN/systemctl"

# install_walg.sh stub: record the PG_* dirs it was invoked with.
INSTALL_WALG_STUB="$tmp/install_walg.sh"
cat > "$INSTALL_WALG_STUB" <<'EOF'
#!/bin/sh
printf 'PG_CONFDIR=%s PG_DATADIR=%s\n' "${PG_CONFDIR:-}" "${PG_DATADIR:-}" >> "${INSTALL_WALG_LOG:?INSTALL_WALG_LOG must be set}"
EOF
chmod 0755 "$INSTALL_WALG_STUB"

ROW='https://s3.eu-test-1.example.com|cyclopus-backups|k8s-secret://cyclopus/walg-backup#s3'

# run_case NAME — fresh per-case dirs; env from caller's exported stub vars.
setup_case() {
  _d="$tmp/$1"
  mkdir -p "$_d"
  : > "$_d/systemctl.log"
  : > "$_d/install_walg.log"
  printf '%s\n' "$_d"
}

# ------------------------------------------------- (a) happy path + rerun ----
d=$(setup_case a)
env_out=$(
  PATH="$BIN:/usr/bin:/bin" \
  PSQL_STUB_ROW="$ROW" PSQL_STUB_ARCHIVE_MODE=on \
  SYSTEMCTL_LOG="$d/systemctl.log" INSTALL_WALG_LOG="$d/install_walg.log" \
  INSTALL_WALG_SH="$INSTALL_WALG_STUB" \
  WALG_ENV_FILE="$d/etc/walg.env" SYSTEMD_DIR="$d/systemd" INSTALL_DIR="$d/backup" \
  sh "$SCRIPT"
)
[ -f "$d/etc/walg.env" ] || fail "walg.env not written"
[ "$(perms "$d/etc/walg.env")" = 600 ] || fail "walg.env mode is $(perms "$d/etc/walg.env"), want 600"
pass "happy path writes walg.env mode 0600"
[ "$(perms "$d/etc")" = 700 ] || fail "walg.env dir mode is $(perms "$d/etc"), want 700"
pass "walg.env directory mode 0700"
for want in \
  'WALG_S3_ENDPOINT=https://s3.eu-test-1.example.com' \
  'WALG_S3_BUCKET=cyclopus-backups' \
  'WALG_S3_REGION=us-east-1' \
  'WALG_S3_PREFIX_SUBPATH=postgres_backup' \
  'WALG_S3_PREFIX=s3://cyclopus-backups/postgres_backup' \
  'AWS_ENDPOINT=https://s3.eu-test-1.example.com' \
  'AWS_REGION=us-east-1' \
  'AWS_ACCESS_KEY_ID=ak-id-123' \
  'AWS_SECRET_ACCESS_KEY=sk-secret-456' \
  'AWS_S3_FORCE_PATH_STYLE=true' \
  'WALG_COMPRESSION_METHOD=lz4'; do
  grep -qF "$want" "$d/etc/walg.env" || fail "walg.env missing: $want"
done
pass "walg.env carries storage_destinations endpoint/bucket + resolved S3 credential pair"
grep -qF "ExecStart=$d/backup/backup.sh" "$d/systemd/cyclopus-walg-backup.service" || fail "service ExecStart not templated to INSTALL_DIR"
! grep -q '@INSTALL_DIR@' "$d/systemd/cyclopus-walg-backup.service" || fail "service retains @INSTALL_DIR@ placeholder"
pass "service unit installed with INSTALL_DIR templated"
[ -f "$d/systemd/cyclopus-walg-backup.timer" ] && grep -q 'OnCalendar=daily' "$d/systemd/cyclopus-walg-backup.timer" || fail "timer missing/daily schedule absent"
pass "timer unit installed with OnCalendar=daily"
grep -q 'daemon-reload' "$d/systemctl.log" || fail "systemctl daemon-reload not invoked"
grep -q 'enable --now cyclopus-walg-backup.timer' "$d/systemctl.log" || fail "timer enable --now not invoked"
pass "systemctl daemon-reload + enable --now cyclopus-walg-backup.timer invoked"
grep -q 'reload postgresql' "$d/systemctl.log" || fail "expected reload (archive_mode already on), got: $(cat "$d/systemctl.log")"
pass "archive_mode already on -> reload (no full restart)"
grep -Eq 'PG_CONFDIR=/etc/postgresql/[0-9]+/main PG_DATADIR=/var/lib/postgresql/[0-9]+/main' "$d/install_walg.log" || fail "install_walg.sh ran without PG_CONFDIR/PG_DATADIR from PG_MAJOR"
pass "install_walg.sh invoked with PG_CONFDIR/PG_DATADIR derived from PG_MAJOR"
[ -x "$d/backup/backup.sh" ] && [ -f "$d/backup/common.sh" ] || fail "backup.sh/common.sh not staged into INSTALL_DIR"
pass "backup.sh + common.sh staged into INSTALL_DIR (unit ExecStart target exists)"
[ -f "$d/backup/versions.env" ] || fail "versions.env not staged into INSTALL_DIR"
pass "versions.env staged into INSTALL_DIR (staged common.sh resolves its pins)"
# The staged layout must be self-sufficient on a host with no repo checkout:
# executing the STAGED backup.sh must get past common.sh's versions.env
# resolution + walg_env's credential checks and fail only at the wal-g binary.
backup_rc=0
backup_out=$(PATH="$BIN:/usr/bin:/bin" WALG_ENV_FILE="$d/etc/walg.env" sh "$d/backup/backup.sh" 2>&1) || backup_rc=$?
[ "$backup_rc" != 0 ] || fail "staged backup.sh unexpectedly succeeded"
printf '%s\n' "$backup_out" | grep -q 'WAL-G is not installed' || fail "staged backup.sh did not reach the wal-g check: $backup_out"
! printf '%s\n' "$backup_out" | grep -q 'versions.env not found' || fail "staged backup.sh cannot resolve versions.env: $backup_out"
pass "staged backup.sh runs: sources walg.env, fails with 'WAL-G is not installed' (not 'versions.env not found')"
printf '%s\n' "$env_out" | grep -q 'daily base backups enabled' || fail "missing final summary in output: $env_out"
pass "summary output reports destination + timer"

# Idempotency: a second run converges without error and keeps perms.
PATH="$BIN:/usr/bin:/bin" \
  PSQL_STUB_ROW="$ROW" PSQL_STUB_ARCHIVE_MODE=on \
  SYSTEMCTL_LOG="$d/systemctl.log" INSTALL_WALG_LOG="$d/install_walg.log" \
  INSTALL_WALG_SH="$INSTALL_WALG_STUB" \
  WALG_ENV_FILE="$d/etc/walg.env" SYSTEMD_DIR="$d/systemd" INSTALL_DIR="$d/backup" \
  sh "$SCRIPT" >/dev/null
[ "$(perms "$d/etc/walg.env")" = 600 ] || fail "rerun changed walg.env mode"
[ "$(perms "$d/systemd/cyclopus-walg-backup.service")" = 644 ] || fail "rerun changed unit mode"
pass "re-run converges (idempotent)"

# ------------------------------------------------------------ (b) no row ----
d=$(setup_case b)
: > "$d/out"; : > "$d/err"
rc=0
PATH="$BIN:/usr/bin:/bin" \
  PSQL_STUB_ROW='' PSQL_STUB_COUNT=0 \
  SYSTEMCTL_LOG="$d/systemctl.log" INSTALL_WALG_LOG="$d/install_walg.log" \
  INSTALL_WALG_SH="$INSTALL_WALG_STUB" \
  WALG_ENV_FILE="$d/etc/walg.env" SYSTEMD_DIR="$d/systemd" INSTALL_DIR="$d/backup" \
  sh "$SCRIPT" >"$d/out" 2>"$d/err" || rc=$?
[ "$rc" = 64 ] || fail "no-row exit code is $rc, want 64 (stderr: $(cat "$d/err"))"
pass "no storage_destinations row -> exit 64"
grep -q "purpose='postgres_backup'" "$d/err" || fail "exit-64 message lacks guidance: $(cat "$d/err")"
pass "exit-64 message tells the operator to create the destination first"
[ ! -e "$d/etc/walg.env" ] || fail "walg.env written despite missing destination row"
[ ! -e "$d/systemd/cyclopus-walg-backup.timer" ] || fail "timer installed despite missing destination row"
pass "nothing materialized when the destination row is missing"

# ---------------------------------------------------- (c) missing secret key ----
d=$(setup_case c)
rc=0
PATH="$BIN:/usr/bin:/bin" \
  PSQL_STUB_ROW="$ROW" K3S_STUB_MISSING=secret_access_key \
  SYSTEMCTL_LOG="$d/systemctl.log" INSTALL_WALG_LOG="$d/install_walg.log" \
  INSTALL_WALG_SH="$INSTALL_WALG_STUB" \
  WALG_ENV_FILE="$d/etc/walg.env" SYSTEMD_DIR="$d/systemd" INSTALL_DIR="$d/backup" \
  sh "$SCRIPT" >"$d/out" 2>"$d/err" || rc=$?
[ "$rc" = 1 ] || fail "missing-key exit code is $rc, want 1 (stderr: $(cat "$d/err"))"
grep -q 'secret_access_key' "$d/err" || fail "failure message does not name the missing key: $(cat "$d/err")"
pass "secret ref missing secret_access_key -> fails loudly naming the key"
[ ! -e "$d/etc/walg.env" ] || fail "walg.env written despite unresolved secret key"
! grep -q 'enable --now' "$d/systemctl.log" 2>/dev/null || fail "timer enabled despite unresolved secret key"
pass "aborts before walg.env / timer when the credential is incomplete"

# ------------------------------------------ (d) multi-org: refuse loudly ------
d=$(setup_case d)
rc=0
PATH="$BIN:/usr/bin:/bin" \
  PSQL_STUB_COUNT=2 PSQL_STUB_ROW="$ROW" \
  SYSTEMCTL_LOG="$d/systemctl.log" INSTALL_WALG_LOG="$d/install_walg.log" \
  INSTALL_WALG_SH="$INSTALL_WALG_STUB" \
  WALG_ENV_FILE="$d/etc/walg.env" SYSTEMD_DIR="$d/systemd" INSTALL_DIR="$d/backup" \
  sh "$SCRIPT" >"$d/out" 2>"$d/err" || rc=$?
[ "$rc" = 1 ] || fail "multi-org exit code is $rc, want 1 (stderr: $(cat "$d/err"))"
grep -q 'SINGLE postgres backup destination' "$d/err" || fail "refusal does not explain the single-destination invariant: $(cat "$d/err")"
grep -q 'ORG_ID=' "$d/err" || fail "refusal does not name the ORG_ID escape hatch: $(cat "$d/err")"
pass "multiple postgres_backup rows -> fails loudly naming the invariant + ORG_ID hatch"
[ ! -e "$d/etc/walg.env" ] || fail "walg.env written despite multi-org ambiguity"
! grep -q 'enable --now' "$d/systemctl.log" 2>/dev/null || fail "timer enabled despite multi-org ambiguity"
pass "aborts before walg.env / timer when multiple postgres_backup rows exist"

# Non-UUID ORG_ID (here: a SQL-injection-shaped string) is rejected on charset
# grounds before it can ever reach a psql invocation.
rc=0
PATH="$BIN:/usr/bin:/bin" \
  PSQL_STUB_COUNT=2 PSQL_STUB_ROW="$ROW" ORG_ID="x'; DROP TABLE orgs;--" \
  SYSTEMCTL_LOG="$d/systemctl.log" INSTALL_WALG_LOG="$d/install_walg.log" \
  INSTALL_WALG_SH="$INSTALL_WALG_STUB" \
  WALG_ENV_FILE="$d/etc/walg.env" SYSTEMD_DIR="$d/systemd" INSTALL_DIR="$d/backup" \
  sh "$SCRIPT" >"$d/out" 2>"$d/err" || rc=$?
[ "$rc" = 1 ] && grep -q 'ORG_ID must be UUID-charset' "$d/err" || fail "non-UUID ORG_ID not rejected: rc=$rc stderr=$(cat "$d/err")"
! grep -q 'enable --now' "$d/systemctl.log" 2>/dev/null || fail "timer enabled despite invalid ORG_ID"
pass "non-UUID ORG_ID rejected before forming the scoped query"

# ------------------------------------------ (e) ORG_ID scopes the selection ---
d=$(setup_case e)
PATH="$BIN:/usr/bin:/bin" \
  PSQL_STUB_COUNT=3 PSQL_STUB_ORG_COUNT=1 PSQL_STUB_ROW="$ROW" PSQL_STUB_ARCHIVE_MODE=on \
  ORG_ID="123e4567-e89b-42d3-a456-426614174000" \
  SYSTEMCTL_LOG="$d/systemctl.log" INSTALL_WALG_LOG="$d/install_walg.log" \
  INSTALL_WALG_SH="$INSTALL_WALG_STUB" \
  WALG_ENV_FILE="$d/etc/walg.env" SYSTEMD_DIR="$d/systemd" INSTALL_DIR="$d/backup" \
  sh "$SCRIPT" >"$d/out" 2>"$d/err"
[ -f "$d/etc/walg.env" ] || fail "ORG_ID-scoped run did not write walg.env (stderr: $(cat "$d/err"))"
grep -qF 'WALG_S3_ENDPOINT=https://s3.eu-test-1.example.com' "$d/etc/walg.env" || fail "scoped run wired the wrong destination"
pass "ORG_ID=<uuid> scopes the selection to that org's single destination and wires it"

printf '%s\n' "configure_from_db tests passed: $assertions assertions"
