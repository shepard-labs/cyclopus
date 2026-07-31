#!/bin/sh
# Restore Cyclopus's Postgres from WAL-G (§9.1).
#
# WAL-G restore IS the recovery path for Cyclopus state — there is no replica
# (Postgres sits on VPS1 local-path storage, §2.2). Restores the latest base
# backup then replays WAL up to the latest segment (or a target time).
#
# Required env (from storage_destinations): WALG_S3_ENDPOINT, WALG_S3_BUCKET,
# AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY.
# Optional: PITR_TARGET_TIME (ISO 8601) for point-in-time recovery.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/backup/common.sh
. "$SCRIPT_DIR/common.sh"

require_root
walg_env

BIN=/usr/local/bin/wal-g
[ -x "$BIN" ] || fail "WAL-G is not installed; run install_walg.sh first"
# PGDATA is the real data directory (Debian/PGDG default), NOT the /etc config
# dir. backup-fetch wipes and repopulates this directory, so recovery params
# must be (re)written here after the fetch — see below.
PGDATA="${PGDATA:-/var/lib/postgresql/${PG_MAJOR}/main}"
PG_USER="${PG_SUPERUSER:-postgres}"
ENVFILE=/etc/cyclopus/walg.env

walg_run() {
  $SUDO -u "$PG_USER" env \
    WALE_S3_PREFIX="$WALE_S3_PREFIX" AWS_ENDPOINT="$AWS_ENDPOINT" AWS_REGION="$AWS_REGION" \
    AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
    AWS_S3_FORCE_PATH_STYLE="$AWS_S3_FORCE_PATH_STYLE" \
    WALG_COMPRESSION_METHOD="$WALG_COMPRESSION_METHOD" \
    "$BIN" "$@"
}

log "stopping Postgres before restore"
$SUDO systemctl stop postgresql 2>/dev/null || warn "postgresql service not running"

log "confirming a base backup exists in storage_destinations"
walg_run backup-list >/dev/null || fail "no WAL-G backups found in storage_destinations"

log "clearing target PGDATA and fetching the latest base backup"
$SUDO rm -rf "${PGDATA:?}/"* 2>/dev/null || true
walg_run backup-fetch "$PGDATA" LATEST || fail "WAL-G base backup fetch failed"
$SUDO chown -R "$PG_USER:$PG_USER" "$PGDATA"

# PostgreSQL 12+ ignores recovery.conf entirely. Recovery parameters MUST live
# in the config the freshly-fetched cluster actually reads on startup. Because
# backup-fetch overwrites $PGDATA, any conf.d/include from the archiving-time
# config is gone; we cannot rely on it surviving. We therefore write the
# restore_command and recovery target directly into $PGDATA/postgresql.auto.conf
# (always read last, overriding earlier settings) and create $PGDATA/recovery.signal
# to put the cluster into archive-recovery mode (§9.1).
log "configuring WAL replay recovery in postgresql.auto.conf"
AUTOCONF="$PGDATA/postgresql.auto.conf"
# Preserve any auto.conf that came with the base backup, minus stale recovery
# lines we are about to (re)write, then append our recovery parameters.
$SUDO -u "$PG_USER" sh -c "
  touch '$AUTOCONF'
  grep -v -E '^[[:space:]]*(restore_command|recovery_target_time|recovery_target_action|recovery_target_timeline)[[:space:]]*=' '$AUTOCONF' > '$AUTOCONF.cyclopus.tmp' || true
  mv '$AUTOCONF.cyclopus.tmp' '$AUTOCONF'
"
{
  printf "restore_command = 'set -a; . %s; %s wal-fetch %%f %%p'\n" "$ENVFILE" "$BIN"
  printf "recovery_target_timeline = 'latest'\n"
  printf "recovery_target_action = 'promote'\n"
  if [ -n "${PITR_TARGET_TIME:-}" ]; then
    printf "recovery_target_time = '%s'\n" "$PITR_TARGET_TIME"
  fi
} | $SUDO -u "$PG_USER" tee -a "$AUTOCONF" >/dev/null
[ -n "${PITR_TARGET_TIME:-}" ] && log "point-in-time recovery target: $PITR_TARGET_TIME"

# recovery.signal (not standby.signal): archive recovery that promotes to a
# normal read/write primary once the target is reached (§9.1: no replica).
$SUDO -u "$PG_USER" touch "$PGDATA/recovery.signal"

log "starting Postgres to replay WAL"
$SUDO systemctl start postgresql || fail "Postgres failed to start for recovery"

# Wait for recovery to finish: recovery.signal is removed and the cluster is
# out of recovery (pg_is_in_recovery() = false) once replay reaches the target
# and promotes. pg_isready alone is insufficient — it returns true while still
# replaying WAL, masking a non-functional restore.
log "waiting for WAL replay + promotion to complete"
retry 60 $SUDO -u "$PG_USER" pg_isready -q || fail "Postgres did not accept connections after restore"
_recovered=false
_i=0
while [ "$_i" -lt 120 ]; do
  _inrec=$($SUDO -u "$PG_USER" psql -XAtqc 'SELECT pg_is_in_recovery();' 2>/dev/null || printf 'error')
  if [ "$_inrec" = "f" ]; then
    _recovered=true
    break
  fi
  _i=$((_i + 1))
  sleep 2
done
[ "$_recovered" = true ] || fail "WAL replay did not complete: cluster still in recovery after restore"

log "WAL-G restore complete; run reconcile to rebuild cluster state (dr_runbook.sh)"
