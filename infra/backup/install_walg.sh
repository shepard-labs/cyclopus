#!/bin/sh
# Checksum-verified WAL-G v3.0.8 install + Postgres archive/restore wiring (§9.1).
#
# Wires Postgres archive_command to WAL-G streaming into the storage_destinations
# S3 target, with archive_timeout=60s for the 60-second RPO target. Idempotent.
#
# Optional env:
#   WALG_LOCAL_DIR   directory containing the pre-downloaded wal-g tarball
#                    (offline bundle). The checksum is still verified against
#                    the pinned constant below.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/backup/common.sh
. "$SCRIPT_DIR/common.sh"

require_root

WALG_ARCH=$(arch_go)
BIN=/usr/local/bin/wal-g
# WAL-G v3 publishes wal-g-pg-<ubuntu>-<arch>.tar.gz; the 20.04 build is
# statically linked and runs on both v1 profiles (Ubuntu 24.04, Debian 12).
TARBALL="wal-g-pg-20.04-${WALG_ARCH}.tar.gz"
BASE="https://github.com/wal-g/wal-g/releases/download/${WAL_G_VERSION}"

# Pinned upstream sha256 constants (versions.yaml components.wal_g.checksums).
# Verified against a reviewed value, NOT a same-origin sidecar downloaded now.
case "$WALG_ARCH" in
  amd64) WALG_SHA256='8a0ca72ff3aa10d5a288cdb3e5b2a2e7bc6f8b4ffcf7b3046d718cfeee99a7e7' ;;
  aarch64) WALG_SHA256='dd07f0c171b8ccdf72d2dbdd0f153efe729e24f398546fd55db22273f289c1f3' ;;
  *) fail "no pinned WAL-G checksum for arch $WALG_ARCH" ;;
esac

log "installing WAL-G $WAL_G_VERSION ($WALG_ARCH)"
if [ -x "$BIN" ] && "$BIN" --version 2>/dev/null | grep -q "${WAL_G_VERSION}"; then
  log "WAL-G already at $WAL_G_VERSION"
else
  tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

  if [ -n "${WALG_LOCAL_DIR:-}" ]; then
    [ -f "$WALG_LOCAL_DIR/$TARBALL" ] || fail "WALG_LOCAL_DIR missing $TARBALL: $WALG_LOCAL_DIR/$TARBALL"
    cp "$WALG_LOCAL_DIR/$TARBALL" "$tmp_dir/$TARBALL"
  else
    retry 3 curl -fsSLo "$tmp_dir/$TARBALL" "$BASE/$TARBALL" || fail "WAL-G download failed"
  fi
  actual=$(sha256sum "$tmp_dir/$TARBALL" | cut -d ' ' -f 1)
  [ "$actual" = "$WALG_SHA256" ] || fail "WAL-G checksum mismatch: expected $WALG_SHA256, observed $actual"

  tar -C "$tmp_dir" -xzf "$tmp_dir/$TARBALL"
  extracted=$(find "$tmp_dir" -maxdepth 2 -type f -name 'wal-g*' ! -name '*.tar.gz' | head -n 1)
  [ -n "$extracted" ] || fail "WAL-G binary not found in tarball"
  $SUDO install -m 0755 "$extracted" "$BIN"
fi
"$BIN" --version 2>/dev/null | grep -q "$WAL_G_VERSION" || fail "WAL-G $WAL_G_VERSION not installed"

log "wiring Postgres archive_command to WAL-G on the running cluster"
# CONFDIR is the Debian/PGDG configuration directory (postgresql.conf +
# conf.d/), distinct from the data directory. PG_DATADIR is the real cluster
# data directory that restore.sh repopulates via backup-fetch.
CONFDIR="${PG_CONFDIR:-${PGDATA:-/etc/postgresql/${PG_MAJOR}/main}}"
PG_DATADIR="${PG_DATADIR:-/var/lib/postgresql/${PG_MAJOR}/main}"
ENVFILE=/etc/cyclopus/walg.env
$SUDO install -d -m 0700 /etc/cyclopus

# The env file is referenced by archive/restore commands; it is created by the
# operator/DR runbook from storage_destinations outputs, never committed.
if [ ! -f "$ENVFILE" ]; then
  warn "$ENVFILE not present; create it from storage_destinations before enabling archiving"
fi

# Archiving must be enabled on the config the RUNNING cluster reads. The PGDG
# default postgresql.conf carries `include_dir = 'conf.d'`; use it if present so
# our drop-in is actually loaded, otherwise fall back to ALTER SYSTEM (writes
# the running cluster's $PGDATA/postgresql.auto.conf) which PG always reads.
# NOTE: only the archive side is wired here. restore_command is NOT written
# here — a base backup fetch wipes $PGDATA, so restore.sh writes restore_command
# into the freshly-fetched $PGDATA/postgresql.auto.conf itself (§9.1 / C1).
ARCHIVE_CMD="set -a; . $ENVFILE; $BIN wal-push %p"
wired=false
if [ -f "$CONFDIR/postgresql.conf" ] && grep -Eq "^[[:space:]]*include_dir[[:space:]]*=[[:space:]]*'?conf\.d'?" "$CONFDIR/postgresql.conf"; then
  CONF_D="$CONFDIR/conf.d"
  if $SUDO install -d -m 0755 "$CONF_D" 2>/dev/null; then
    $SUDO tee "$CONF_D/cyclopus-walg.conf" >/dev/null <<CONF
# Cyclopus WAL-G streaming backup (§9.1). RPO target 60s.
wal_level = replica
archive_mode = on
archive_timeout = 60
archive_command = '$ARCHIVE_CMD'
CONF
    wired=true
    log "archive_command wired via $CONF_D/cyclopus-walg.conf (included by postgresql.conf)"
  fi
fi

if [ "$wired" = false ]; then
  command -v psql >/dev/null 2>&1 || fail "cannot wire archiving: no conf.d include and psql unavailable for ALTER SYSTEM"
  log "no conf.d include found; wiring archive_command via ALTER SYSTEM"
  PG_USER="${PG_SUPERUSER:-postgres}"
  _q=$(printf "%s" "$ARCHIVE_CMD" | sed "s/'/''/g")
  $SUDO -u "$PG_USER" psql -X -v ON_ERROR_STOP=1 <<SQL || fail "ALTER SYSTEM archiving wiring failed"
ALTER SYSTEM SET wal_level = 'replica';
ALTER SYSTEM SET archive_mode = 'on';
ALTER SYSTEM SET archive_timeout = '60';
ALTER SYSTEM SET archive_command = '$_q';
SQL
  warn "archive_mode change requires a full Postgres restart (not just reload) to take effect"
fi

log "WAL-G installed and Postgres archiving wired; restart Postgres to activate archive_mode"
log "pgBackRest is the documented heavier alternative if PITR needs outgrow WAL-G (§9.1)"
