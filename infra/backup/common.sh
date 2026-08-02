#!/bin/sh
# Shared helpers + storage_destinations config for Cyclopus backup/DR (§9.1).
#
# WAL-G talks to an S3-compatible storage_destinations target. Credentials come
# from the environment (never hardcoded). The unit gate only syntax-checks these
# scripts; live use requires the env below.
set -eu

log() { printf '\n==> backup: %s\n' "$1"; }
warn() { printf 'backup: WARN: %s\n' "$*" >&2; }
fail() { printf 'backup: FAILED: %s\n' "$*" >&2; exit 1; }

# Pinned versions come from versions.env (generated from versions.yaml) — the
# single source of truth. Resolution: caller-provided vars (standalone use /
# operator override), explicit override file, else the repo root relative to
# this file's location.
if [ -n "${WAL_G_VERSION:-}" ] && [ -n "${PG_MAJOR:-}" ]; then
  :
elif [ -n "${CYCLOPUS_VERSIONS_ENV:-}" ]; then
  # shellcheck disable=SC1090
  . "$CYCLOPUS_VERSIONS_ENV"
else
  _versions_env=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)/versions.env
  # Fallback: a sibling versions.env staged next to these scripts (the one-line
  # installer stages them at /opt/cyclopus/backup/ with versions.env alongside).
  [ -f "$_versions_env" ] || _versions_env=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/versions.env
  [ -f "$_versions_env" ] || fail "versions.env not found; set CYCLOPUS_VERSIONS_ENV"
  # shellcheck disable=SC1090
  . "$_versions_env"
fi

retry() {
  _attempts=$1
  shift
  _count=1
  until "$@"; do
    if [ "$_count" -ge "$_attempts" ]; then
      return 1
    fi
    _count=$((_count + 1))
    sleep 3
  done
}

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null 2>&1 || fail "must run as root or with sudo available"
    sudo -n true 2>/dev/null || fail "passwordless sudo (or root) is required"
    SUDO=sudo
  else
    SUDO=
  fi
}

arch_go() {
  case "$(uname -m)" in
    aarch64 | arm64) printf 'aarch64' ;;
    x86_64 | amd64) printf 'amd64' ;;
    *) fail "unsupported architecture $(uname -m)" ;;
  esac
}

# Run a command as the Postgres superuser. Cannot be written as
# `$SUDO -u "$PG_USER" ...`: when running AS root, require_root sets SUDO=''
# and the bare `-u` becomes argv[0] ("-u: not found"). runuser (util-linux,
# present on the supported Ubuntu 24.04 / Debian 12 images) is the root path.
as_pg() {
  _pg_u=${PG_USER:-postgres}
  if [ -n "${SUDO:-}" ]; then
    $SUDO -u "$_pg_u" "$@"
  else
    runuser -u "$_pg_u" -- "$@"
  fi
}

# Export WAL-G's S3 configuration from storage_destinations env. Callers set:
#   WALG_S3_ENDPOINT           e.g. https://s3.<region>.<provider>.example
#   WALG_S3_BUCKET             storage_destinations bucket
#   WALG_S3_REGION             storage region
#   AWS_ACCESS_KEY_ID          S3 access key
#   AWS_SECRET_ACCESS_KEY      S3 secret key
#   WALG_S3_PREFIX_SUBPATH     optional prefix (defaults to postgres_backup)
walg_env() {
  : "${WALG_S3_ENDPOINT:?WALG_S3_ENDPOINT (storage_destinations S3 endpoint) is required}"
  : "${WALG_S3_BUCKET:?WALG_S3_BUCKET (storage_destinations bucket) is required}"
  : "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID is required}"
  : "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY is required}"
  _sub=${WALG_S3_PREFIX_SUBPATH:-postgres_backup}
  export WALG_S3_PREFIX="s3://${WALG_S3_BUCKET}/${_sub}"
  # WAL-G v3 reads WALG_S3_PREFIX; keep WALE_S3_PREFIX as an accepted alias for
  # older invocations. Both point at the SAME single source of truth.
  export WALE_S3_PREFIX="$WALG_S3_PREFIX"
  export AWS_ENDPOINT="$WALG_S3_ENDPOINT"
  export AWS_REGION="${WALG_S3_REGION:-us-east-1}"
  export AWS_S3_FORCE_PATH_STYLE=true
  export WALG_COMPRESSION_METHOD=lz4
}

# Materialize /etc/cyclopus/walg.env from the current WAL-G S3 environment so the
# archive_command / restore_command (which source this file) do not depend on
# ambient env. Callers must have run walg_env first. Single source of truth: the
# WALG_S3_* + AWS_* scheme above. Written 0600, root-owned dir 0700.
#
# Plain `VAR=value` lines, NOT `export VAR=value`: sh-sourceable via `set -a; .
# walg.env` (and parseable as a systemd EnvironmentFile should one be needed).
# The daily-backup unit deliberately does NOT use EnvironmentFile= — systemd
# unit introspection (`systemctl show`) would expose the S3 secret; backup.sh
# sources this file itself instead. The WALG_S3_* input vars are included so
# walg_env's required-var checks pass when the file is the ONLY env source
# (e.g. the daily-backup service).
write_walg_env() {
  _envfile=${1:-/etc/cyclopus/walg.env}
  : "${WALG_S3_REGION:=us-east-1}" "${WALG_S3_PREFIX_SUBPATH:=postgres_backup}"
  # Values are emitted UNQUOTED, so anything a shell would re-lex on sourcing
  # (space, #, $, quotes, backticks, newline ...) would corrupt the file — a
  # space alone splits into a command. Reject unsafe characters up front.
  for _var in WALG_S3_ENDPOINT WALG_S3_BUCKET WALG_S3_REGION WALG_S3_PREFIX_SUBPATH \
    WALG_S3_PREFIX WALE_S3_PREFIX AWS_ENDPOINT AWS_REGION \
    AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY \
    AWS_S3_FORCE_PATH_STYLE WALG_COMPRESSION_METHOD; do
    _value=$(eval "printf '%s' \"\${$_var:-}\"")
    case $_value in
      *[!A-Za-z0-9/+=:._-]*) fail "unsafe character in $_var value (walg.env must be sh-sourceable)" ;;
    esac
  done
  $SUDO install -d -m 0700 "$(dirname -- "$_envfile")"
  {
    printf 'WALG_S3_ENDPOINT=%s\n' "$WALG_S3_ENDPOINT"
    printf 'WALG_S3_BUCKET=%s\n' "$WALG_S3_BUCKET"
    printf 'WALG_S3_REGION=%s\n' "${WALG_S3_REGION:-us-east-1}"
    printf 'WALG_S3_PREFIX_SUBPATH=%s\n' "${WALG_S3_PREFIX_SUBPATH:-postgres_backup}"
    printf 'WALG_S3_PREFIX=%s\n' "$WALG_S3_PREFIX"
    printf 'WALE_S3_PREFIX=%s\n' "$WALE_S3_PREFIX"
    printf 'AWS_ENDPOINT=%s\n' "$AWS_ENDPOINT"
    printf 'AWS_REGION=%s\n' "$AWS_REGION"
    printf 'AWS_ACCESS_KEY_ID=%s\n' "$AWS_ACCESS_KEY_ID"
    printf 'AWS_SECRET_ACCESS_KEY=%s\n' "$AWS_SECRET_ACCESS_KEY"
    printf 'AWS_S3_FORCE_PATH_STYLE=%s\n' "$AWS_S3_FORCE_PATH_STYLE"
    printf 'WALG_COMPRESSION_METHOD=%s\n' "$WALG_COMPRESSION_METHOD"
  } | $SUDO tee "$_envfile" >/dev/null
  $SUDO chmod 0600 "$_envfile"
}
