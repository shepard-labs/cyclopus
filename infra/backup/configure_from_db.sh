#!/bin/sh
# Wire WAL-G backups from the Cyclopus storage_destinations record (§5, §9.1).
#
# Runs ON THE POSTGRES HOST (the same host the one-line installer targets —
# `sudo -u postgres psql` and `k3s kubectl` are available here). Reads the
# latest storage_destinations row with purpose='postgres_backup', resolves its
# credential_secret_ref against the Kubernetes API, materializes
# /etc/cyclopus/walg.env, runs install_walg.sh, converges Postgres archiving
# (restart on first activation, reload after), and installs + enables the
# daily base-backup systemd timer. Idempotent: re-running converges.
#
# credential_secret_ref format (internal/secretref/secretref.go; migration
# CHECK at internal/store/migrations/000001_foundation.up.sql):
#   k8s-secret://<namespace>/<name>#<key>
# An S3 credential is a PAIR, so the referenced Secret MUST carry both
# `access_key_id` and `secret_access_key` data keys; both are read from the
# named Secret (the #key selector is validated as part of the ref's syntax).
#
# Multi-org invariant: v1 wires a SINGLE postgres_backup destination per host.
# If more than one purpose='postgres_backup' row exists across orgs, silently
# picking the newest would retarget WAL archiving onto another org's bucket and
# split the WAL chain (restore via LATEST breaks) — so this script FAILS loudly
# unless ORG_ID=<org uuid> is set, which scopes the selection to that org
# (validated as UUID charset; exactly one row must exist for it, guaranteed at
# most one by the UNIQUE(org_id, purpose) constraint).
#
# Exit 64 (EX_USAGE) when no postgres_backup destination exists yet.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/backup/common.sh
. "$SCRIPT_DIR/common.sh"

require_root

# Env-overridable seams (unit tests stub binaries/paths through these + PATH).
PSQL=${PSQL:-psql}
KUBECTL=${KUBECTL:-"k3s kubectl"}
SYSTEMCTL=${SYSTEMCTL:-systemctl}
INSTALL_WALG_SH=${INSTALL_WALG_SH:-"$SCRIPT_DIR/install_walg.sh"}
PG_DB=${PG_DB:-cyclopus}
PG_USER=${PG_SUPERUSER:-postgres}
PG_CONFDIR=${PG_CONFDIR:-/etc/postgresql/${PG_MAJOR}/main}
PG_DATADIR=${PG_DATADIR:-/var/lib/postgresql/${PG_MAJOR}/main}
WALG_ENV_FILE=${WALG_ENV_FILE:-/etc/cyclopus/walg.env}
INSTALL_DIR=${INSTALL_DIR:-/opt/cyclopus/backup}
SYSTEMD_DIR=${SYSTEMD_DIR:-/etc/systemd/system}
export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}

# --- 1. read the storage_destinations row -------------------------------------
# Count BEFORE selecting: >1 postgres_backup rows means multiple orgs each own a
# destination, and v1 wires only ONE per host (see header) — fail loudly rather
# than silently retarget the WAL archive, unless ORG_ID scopes the selection.
log "reading storage_destinations (purpose=postgres_backup) from database '$PG_DB'"
_where="purpose='postgres_backup'"
if ! _count=$(as_pg "$PSQL" -d "$PG_DB" -XAt -c \
  "SELECT count(*) FROM storage_destinations WHERE purpose='postgres_backup'"); then
  fail "could not query storage_destinations in '$PG_DB' as $PG_USER (is Postgres up and migrated?)"
fi
case $_count in
  0)
    cat >&2 <<MSG

backup: no storage_destinations row with purpose='postgres_backup'.
  Create the destination first via the Cyclopus API/UI (Settings -> storage
  destinations, purpose postgres_backup), with credential_secret_ref
  k8s-secret://<namespace>/<name>#<key> pointing at a Secret holding
  access_key_id + secret_access_key keys. Then re-run this script.
MSG
    exit 64 ;;
  1) : ;;
  *)
    if [ -z "${ORG_ID:-}" ]; then
      fail "found $_count storage_destinations rows with purpose='postgres_backup' across orgs; v1 supports a SINGLE postgres backup destination per host (a second org's destination would silently retarget WAL archiving and split the WAL chain). Dedupe to one destination, or re-run with ORG_ID=<org uuid> to select that org's destination explicitly."
    fi
    # UUID charset only: anything else could break out of the SQL literal below.
    case $ORG_ID in
      *[!0-9a-fA-F-]*) fail "ORG_ID must be UUID-charset ([0-9a-fA-F-] only), got: '$ORG_ID'" ;;
    esac
    _where="purpose='postgres_backup' AND org_id='$ORG_ID'"
    if ! _count=$(as_pg "$PSQL" -d "$PG_DB" -XAt -c \
      "SELECT count(*) FROM storage_destinations WHERE $_where"); then
      fail "could not query storage_destinations in '$PG_DB' as $PG_USER (is Postgres up and migrated?)"
    fi
    [ "$_count" = 1 ] || fail "expected exactly 1 storage_destinations row for $_where, found $_count (0: that org has no postgres_backup destination; >1: dedupe the org's destinations)"
    ;;
esac
if ! _row=$(as_pg "$PSQL" -d "$PG_DB" -XAt -F'|' -c \
  "SELECT endpoint, bucket, credential_secret_ref FROM storage_destinations WHERE $_where ORDER BY created_at DESC LIMIT 1"); then
  fail "could not query storage_destinations in '$PG_DB' as $PG_USER (is Postgres up and migrated?)"
fi
[ -n "$_row" ] || fail "storage_destinations row vanished between count and read; re-run"
_endpoint=${_row%%|*}
_rest=${_row#*|}
_bucket=${_rest%%|*}
_ref=${_rest#*|}
[ -n "$_endpoint" ] && [ -n "$_bucket" ] && [ -n "$_ref" ] || fail "malformed storage_destinations row: '$_row'"
# The endpoint CHECK constraint allows pipes/newlines, which would shift the
# -F'|' split above; only accept a well-formed S3 endpoint URL scheme.
case $_endpoint in
  http://* | https://*) : ;;
  *) fail "postgres_backup endpoint must start with http:// or https://, got: '$_endpoint'" ;;
esac

# --- 2. parse credential_secret_ref (k8s-secret://<ns>/<name>#<key>) ----------
case $_ref in
  k8s-secret://*/*#?*) : ;;
  *) fail "credential_secret_ref must be k8s-secret://<namespace>/<name>#<key>, got: $_ref" ;;
esac
_ref_path=${_ref#k8s-secret://}
_ns_name=${_ref_path%%#*}
# First-'#' split (NOT last), matching internal/secretref/secretref.go's
# strings.Cut(path, "#") so the shell and API parse the same ref identically.
_ref_key=${_ref_path#*#}
_ns=${_ns_name%%/*}
_name=${_ns_name#*/}
[ -n "$_ns" ] && [ -n "$_name" ] && [ -n "$_ref_key" ] || fail "malformed credential_secret_ref: $_ref"

# --- 3. resolve the S3 credential pair from the referenced Secret -------------
_s3_secret_value() {
  # kubectl prints the base64 value with no trailing newline; tolerate both.
  $KUBECTL -n "$_ns" get secret "$_name" -o "jsonpath={.data.$1}" 2>/dev/null | base64 -d 2>/dev/null | tr -d '\r\n'
}
log "resolving credential $_ref"
AWS_ACCESS_KEY_ID=$(_s3_secret_value access_key_id)
[ -n "$AWS_ACCESS_KEY_ID" ] || fail "secret $_ns/$_name has no readable data.access_key_id (secret missing, or key missing)"
AWS_SECRET_ACCESS_KEY=$(_s3_secret_value secret_access_key)
[ -n "$AWS_SECRET_ACCESS_KEY" ] || fail "secret $_ns/$_name has no readable data.secret_access_key (secret missing, or key missing)"

# --- 4. materialize walg.env (0600, dir 0700) ---------------------------------
WALG_S3_ENDPOINT=$_endpoint
WALG_S3_BUCKET=$_bucket
WALG_S3_REGION=${WALG_S3_REGION:-us-east-1}
WALG_S3_PREFIX_SUBPATH=${WALG_S3_PREFIX_SUBPATH:-postgres_backup}
export WALG_S3_ENDPOINT WALG_S3_BUCKET WALG_S3_REGION WALG_S3_PREFIX_SUBPATH
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
walg_env
write_walg_env "$WALG_ENV_FILE"
log "wrote $WALG_ENV_FILE (mode 0600)"

# --- 5. install WAL-G + wire the archive_command ------------------------------
[ -f "$INSTALL_WALG_SH" ] || fail "install_walg.sh not found at $INSTALL_WALG_SH"
export PG_CONFDIR PG_DATADIR
sh "$INSTALL_WALG_SH" || fail "install_walg.sh failed"
# install_walg.sh wires config but deliberately does NOT restart Postgres.

# --- 6. converge Postgres on the archive settings -----------------------------
# archive_mode / wal_level are postmaster-start settings: a FULL restart is
# required the first time archiving turns on. Once active, a reload suffices
# to pick up archive_command / env changes without downtime.
_archive_mode=$(as_pg "$PSQL" -d "$PG_DB" -XAtqc 'SHOW archive_mode;' 2>/dev/null || printf 'unknown')
if [ "$_archive_mode" = on ]; then
  log "archive_mode already on; reloading Postgres to pick up any archive_command change"
  $SUDO "$SYSTEMCTL" reload postgresql || warn "postgresql reload failed (non-fatal)"
else
  log "activating archive_mode with a full Postgres restart (one-time)"
  $SUDO "$SYSTEMCTL" restart postgresql || fail "postgres restart failed (archive_mode not yet active)"
fi

# --- 7. install the daily base-backup timer ------------------------------------
# The unit runs backup.sh from INSTALL_DIR (where the one-line installer stages
# this toolkit). When run from a different checkout, stage the runtime pair so
# the ExecStart path actually exists — plus versions.env, which the staged
# common.sh resolves as its sibling fallback (its ../../ path only exists in a
# checkout layout); scripts/install.sh stages the same trio.
if [ "$SCRIPT_DIR" != "$INSTALL_DIR" ]; then
  $SUDO install -d -m 0755 "$INSTALL_DIR"
  $SUDO install -m 0755 "$SCRIPT_DIR/backup.sh" "$SCRIPT_DIR/common.sh" "$INSTALL_DIR/"
  [ -f "$SCRIPT_DIR/../../versions.env" ] || fail "versions.env missing at $SCRIPT_DIR/../../versions.env; run from a repo checkout"
  $SUDO install -m 0644 "$SCRIPT_DIR/../../versions.env" "$INSTALL_DIR/"
fi
$SUDO install -d -m 0755 "$SYSTEMD_DIR"
for _unit in cyclopus-walg-backup.service cyclopus-walg-backup.timer; do
  [ -f "$SCRIPT_DIR/$_unit" ] || fail "unit file $SCRIPT_DIR/$_unit missing"
  sed "s|@INSTALL_DIR@|$INSTALL_DIR|g" "$SCRIPT_DIR/$_unit" | $SUDO tee "$SYSTEMD_DIR/$_unit" >/dev/null
  $SUDO chmod 0644 "$SYSTEMD_DIR/$_unit"
done
$SUDO "$SYSTEMCTL" daemon-reload
$SUDO "$SYSTEMCTL" enable --now cyclopus-walg-backup.timer || fail "could not enable cyclopus-walg-backup.timer"
$SUDO "$SYSTEMCTL" list-timers cyclopus-walg-backup.timer || true

log "WAL-G configured from storage_destinations: s3://$_bucket/$WALG_S3_PREFIX_SUBPATH on $_endpoint"
log "daily base backups enabled via cyclopus-walg-backup.timer (manual run: $INSTALL_DIR/backup.sh)"
