#!/bin/sh
# Take a WAL-G base backup of Cyclopus's Postgres to storage_destinations (§9.1).
#
# Base backups run daily via the daily cyclopus-walg-backup.timer; WAL segments
# stream continuously via archive_command (see install_walg.sh), giving the
# 60-second RPO. This script forces a base backup on demand — also used by the
# DR scenario before simulating loss.
#
# Required env (from storage_destinations): WALG_S3_ENDPOINT, WALG_S3_BUCKET,
# AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY. PGDATA optional. Sourced from
# $WALG_ENV_FILE (default /etc/cyclopus/walg.env) when present — deliberately
# NOT a systemd EnvironmentFile so `systemctl show` cannot leak the S3 secret.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/backup/common.sh
. "$SCRIPT_DIR/common.sh"

require_root
WALG_ENV_FILE=${WALG_ENV_FILE:-/etc/cyclopus/walg.env}
# shellcheck disable=SC1090
[ -f "$WALG_ENV_FILE" ] && { set -a; . "$WALG_ENV_FILE"; set +a; }
walg_env

BIN=/usr/local/bin/wal-g
[ -x "$BIN" ] || fail "WAL-G is not installed; run install_walg.sh first"
PGDATA="${PGDATA:-/var/lib/postgresql/${PG_MAJOR}/main}"
PG_USER="${PG_SUPERUSER:-postgres}"

log "taking WAL-G base backup from $PGDATA"
# Run as the postgres superuser so WAL-G can read PGDATA and call pg_backup_*.
as_pg env \
  WALE_S3_PREFIX="$WALE_S3_PREFIX" \
  AWS_ENDPOINT="$AWS_ENDPOINT" \
  AWS_REGION="$AWS_REGION" \
  AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
  AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
  AWS_S3_FORCE_PATH_STYLE="$AWS_S3_FORCE_PATH_STYLE" \
  WALG_COMPRESSION_METHOD="$WALG_COMPRESSION_METHOD" \
  "$BIN" backup-push "$PGDATA" || fail "WAL-G base backup failed"

log "verifying the backup is listed in storage_destinations"
as_pg env \
  WALE_S3_PREFIX="$WALE_S3_PREFIX" AWS_ENDPOINT="$AWS_ENDPOINT" AWS_REGION="$AWS_REGION" \
  AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
  AWS_S3_FORCE_PATH_STYLE="$AWS_S3_FORCE_PATH_STYLE" \
  "$BIN" backup-list || fail "WAL-G backup-list failed after push"

log "WAL-G base backup complete"
