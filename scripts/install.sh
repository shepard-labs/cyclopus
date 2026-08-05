#!/bin/bash
# Cyclopus VPS Installer
# One-line install (like Coolify):
#   curl -fsSL https://raw.githubusercontent.com/shepard-labs/cyclopus/main/scripts/install.sh | bash
#
# Zero-config install for Ubuntu 24.04 / Debian 12 on a fresh VPS.
# Requires a public IPv4 address (IPv6-only hosts unsupported; platform is single-stack IPv4).
# Detects public IP, configures sslip.io hostnames, generates secrets,
# installs PostgreSQL + k3s + gVisor + Kyverno + Traefik + cert-manager + KEDA + Cyclopus.
#
# Speed: by default the installer downloads ONE offline bundle (built per
# release by scripts/build_bundle.sh, attached to the GitHub release) — all
# helm charts, all container images (k3s airgap set + platform workloads), the
# k3s/helm binaries, checksum-verified gVisor/WAL-G artifacts, and the
# observability/backup files — and installs everything from local artifacts.
# Independent chart installs (Traefik/Kyverno, then cert-manager/KEDA) run in
# parallel. If the bundle is unavailable (arm64, pre-bundle releases, network),
# the installer falls back to the online path; CYCLOPUS_ONLINE=1 forces it.
#
# TLS: cert-manager issues Let's Encrypt PRODUCTION certificates automatically for
# every app/preview/custom-domain Ingress and the control-plane console. Traefik is
# the ingress controller; TLS terminates at Traefik (the pod stays plaintext inside
# the cluster). No hand-rolled ACME.
#
# Optional env vars:
#   CYCLOPUS_VERSION     image/chart version (default: latest — resolved to the
#                        newest GitHub release tag at install time)
#   CYCLOPUS_ONLINE      "1" forces the legacy online install path (per-artifact
#                        downloads + registry pulls) instead of the offline
#                        bundle fast path
#   CYCLOPUS_RELEASE     helm release name (default: cyclopus)
#   CYCLOPUS_NAMESPACE   namespace (default: cyclopus)
#   NODE_IP              override auto-detected node IP (pod-reachable interface)
#   LETSENCRYPT_EMAIL    ACME registration/expiry-notice email (default: warns)
#   CYCLOPUS_CONTROL_PLANE_TAINT  "true" taints the node cyclopus.app/role=
#                        control-plane:NoSchedule (§2.2). Do NOT enable on a
#                        single-node install unless worker nodes will join —
#                        app pods will not schedule on the tainted node.
#   WALG_S3_ENDPOINT     S3 endpoint for Postgres WAL-G backups (optional)
#   WALG_S3_BUCKET       S3 bucket for WAL-G backups (optional)
#   WALG_S3_REGION       S3 region (optional; default us-east-1)
#   WALG_S3_PREFIX_SUBPATH  S3 prefix subpath (optional; default postgres_backup)
#   AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
#                        S3 credential pair (optional). If WALG_S3_ENDPOINT +
#                        WALG_S3_BUCKET + both AWS_* keys are all set, WAL-G
#                        archiving + daily base backups are configured during
#                        install (best-effort: failures warn, never fail).

set -euo pipefail

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log() { printf "${GREEN}[cyclopus]${NC} %s\n" "$*"; }
warn() { printf "${YELLOW}[cyclopus]${NC} %s\n" "$*" >&2; }
fail() {
  printf "${RED}[cyclopus]${NC} %s\n" "$*" >&2
  [ -z "${LOG_FILE:-}" ] || printf "${RED}[cyclopus]${NC} %s\n" "Full log: $LOG_FILE" >&2
  exit 1
}

# Check prerequisites (before the log redirect: a non-root run must not start tee
# against /var/log; fail() guards LOG_FILE being unset at this point).
log "Checking prerequisites..."
[ "$(id -u)" -eq 0 ] || fail "This script must be run as root"

# Mirror all output (stdout + stderr) to a timestamped install log. The process
# substitution wires the pipe to tee's stdin, so tee cannot race bash for the
# script's stdin under curl|bash (do NOT add </dev/null — it would override the
# pipe and silently discard all output).
LOG_FILE="/var/log/cyclopus-install-$(date +%Y%m%d-%H%M%S).log"
exec > >(tee -a "$LOG_FILE") 2>&1
log "Install log: $LOG_FILE"

# Hardened curl: bounded time + retries (#8).
dl() { curl -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 2 "$@"; }
# Long-window download for the multi-GB offline bundle (the 120s cap above
# would abort a slow link partway through a 2+GB transfer).
dl_big() { curl -fsSL --connect-timeout 10 --max-time 3600 --retry 3 --retry-delay 5 "$@"; }

# helm_deployed REL NS: true only when the release is in 'deployed' state. A
# 'failed'/'pending' release is uninstalled so a clean install can retry (helm
# status returns success even for a failed release, which would wrongly skip it).
helm_deployed() {
  _st=$(helm status "$1" -n "$2" -o json 2>/dev/null | grep -o '"status":"[^"]*"' | head -n1 | cut -d'"' -f4 || true)
  if [ "$_st" = "deployed" ]; then return 0; fi
  if [ -n "$_st" ]; then
    warn "$1 release is $_st (not deployed) — uninstalling for clean retry"
    helm uninstall "$1" -n "$2" >/dev/null 2>&1 || true
  fi
  return 1
}

# Install configuration
CYCLOPUS_VERSION=${CYCLOPUS_VERSION:-latest}
CYCLOPUS_ONLINE=${CYCLOPUS_ONLINE:-}
CYCLOPUS_RELEASE=${CYCLOPUS_RELEASE:-cyclopus}
CYCLOPUS_NAMESPACE=${CYCLOPUS_NAMESPACE:-cyclopus}
LETSENCRYPT_EMAIL=${LETSENCRYPT_EMAIL:-}
CYCLOPUS_CONTROL_PLANE_TAINT=${CYCLOPUS_CONTROL_PLANE_TAINT:-false}
# Optional WAL-G backup wiring (see header docs; all four must be set to wire).
WALG_S3_ENDPOINT=${WALG_S3_ENDPOINT:-}
WALG_S3_BUCKET=${WALG_S3_BUCKET:-}
WALG_S3_REGION=${WALG_S3_REGION:-}
AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-}
AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-}

# Resolve "latest" to the newest GitHub release tag. Everything below (bundle
# URL, OCI chart version, raw bootstrap files) then pins to that one tag, so a
# "latest" install deploys a single coherent release instead of mixing main-
# branch scripts with the newest image.
if [ "$CYCLOPUS_VERSION" = "latest" ]; then
  log "Resolving latest release tag..."
  CYCLOPUS_VERSION=$(dl https://api.github.com/repos/shepard-labs/cyclopus/releases/latest \
    | grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4 || true)
  [ -n "$CYCLOPUS_VERSION" ] || fail "could not resolve latest release tag; set CYCLOPUS_VERSION=vX.Y.Z explicitly"
fi
log "Cyclopus version: $CYCLOPUS_VERSION"
CHART_VER=${CYCLOPUS_VERSION#v}
CYCLOPUS_IMAGE=${CYCLOPUS_IMAGE:-ghcr.io/shepard-labs/cyclopus:${CYCLOPUS_VERSION}}

# Canonical raw base for bootstrap scripts (single source of truth for gVisor, #7).
RAW_BASE="https://raw.githubusercontent.com/shepard-labs/cyclopus/${CYCLOPUS_VERSION}"

KUBECTL="k3s kubectl"

# ---- global cleanup / failure handling (#16) -------------------------------
SECRETS_FILE="/root/.cyclopus-install-secrets"
STATE_FILE="/root/.cyclopus-install.env"
INSTALL_OK=0
TMP_FILES=""
cleanup() {
  # Remove any temp values files regardless of outcome.
  for f in $TMP_FILES; do rm -f "$f" 2>/dev/null || true; done
  # Stop an orphaned background bundle download (e.g. install failed before the
  # wait point) and drop its partial artifacts. ${BUNDLE_PID:-} guards: the
  # trap can fire before the download variables are assigned.
  [ -z "${BUNDLE_PID:-}" ] || kill "$BUNDLE_PID" 2>/dev/null || true
  [ -z "${BUNDLE_PID:-}" ] || rm -f "${BUNDLE_TARBALL:-}" "${BUNDLE_TARBALL:-}.sha256" 2>/dev/null || true
  # Best-effort delete of the Kyverno readiness probe pod (#12).
  $KUBECTL delete pod cyclopus-webhook-probe -n default --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
  if [ "$INSTALL_OK" -ne 1 ] && [ -f "$STATE_FILE" ]; then
    warn "install did not complete; secrets state kept at $STATE_FILE for idempotent rerun (delete to start over — regeneration changes the OpenObserve admin password and invalidates enrolled Beszel agents)"
  fi
}
trap cleanup EXIT HUP INT TERM

# Component versions: single source of truth is versions.env (generated from
# versions.yaml by scripts/generate_versions_env.sh). Resolution order:
# explicit override, repo checkout (cwd), download from the canonical raw base.
if [ -n "${CYCLOPUS_VERSIONS_ENV:-}" ]; then
  [ -f "$CYCLOPUS_VERSIONS_ENV" ] || fail "CYCLOPUS_VERSIONS_ENV is set but not a file: $CYCLOPUS_VERSIONS_ENV"
  # shellcheck disable=SC1090
  . "$CYCLOPUS_VERSIONS_ENV"
  RESOLVED_VERSIONS_ENV=$CYCLOPUS_VERSIONS_ENV
elif [ -f ./versions.env ]; then
  # shellcheck disable=SC1091
  . ./versions.env
  RESOLVED_VERSIONS_ENV=./versions.env
else
  VERSIONS_ENV_TMP=$(mktemp)
  TMP_FILES="$TMP_FILES $VERSIONS_ENV_TMP"
  dl "$RAW_BASE/versions.env" -o "$VERSIONS_ENV_TMP" || fail "versions.env not found locally and download failed; set CYCLOPUS_VERSIONS_ENV"
  # shellcheck disable=SC1090
  . "$VERSIONS_ENV_TMP"
  RESOLVED_VERSIONS_ENV=$VERSIONS_ENV_TMP
fi

# Detect OS
if [ -f /etc/os-release ]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  OS=$ID
else
  fail "Cannot detect OS. /etc/os-release not found."
fi

case "$OS" in
  ubuntu) [ "$VERSION_ID" = "24.04" ] || warn "Tested on Ubuntu 24.04, you have $VERSION_ID" ;;
  debian) [ "$VERSION_ID" = "12" ] || warn "Tested on Debian 12, you have $VERSION_ID" ;;
  *) fail "Unsupported OS: $OS $VERSION_ID. Use Ubuntu 24.04 or Debian 12." ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) GO_ARCH=amd64 ;;
  aarch64|arm64) GO_ARCH=arm64 ;;
  *) fail "Unsupported architecture: $ARCH" ;;
esac
log "OS: $OS $VERSION_ID, Arch: $ARCH"

# Start the offline-bundle download in the BACKGROUND so it overlaps secrets
# generation, apt, PostgreSQL, and firewall setup (several minutes on a fresh
# image). The bundle is amd64-only today (the cyclopus image is built
# linux/amd64-only); arm64 and CYCLOPUS_ONLINE=1 use the online path.
BUNDLE_DIR=/opt/cyclopus-bundle
BUNDLE_MODE=0
BUNDLE_PID=""
BUNDLE_TARBALL=/tmp/cyclopus-bundle.tar.zst
if [ "$CYCLOPUS_ONLINE" != "1" ] && [ "$GO_ARCH" = "amd64" ]; then
  BUNDLE_URL="https://github.com/shepard-labs/cyclopus/releases/download/${CYCLOPUS_VERSION}/cyclopus-bundle-${CYCLOPUS_VERSION}-linux-${GO_ARCH}.tar.zst"
  log "Starting background bundle download: $BUNDLE_URL"
  (
    dl_big "$BUNDLE_URL" -o "$BUNDLE_TARBALL" &&
    dl "$BUNDLE_URL.sha256" -o "$BUNDLE_TARBALL.sha256" &&
    # The sidecar names the release filename; verify against our local path.
    echo "$(awk '{print $1}' "$BUNDLE_TARBALL.sha256")  $BUNDLE_TARBALL" | sha256sum -c -
  ) > /tmp/cyclopus-bundle-dl.log 2>&1 &
  BUNDLE_PID=$!
fi

# Disk space pre-check: fail under 10G free; warn under 20G free or 30G total.
# -P (POSIX output) keeps long device names (e.g. LVM /dev/mapper/...) on one line.
DISK_LINE=$(df -PBG / 2>/dev/null | awk 'NR==2' || true)
DISK_TOTAL=$(echo "$DISK_LINE" | awk '{print $2}' | tr -d 'G')
DISK_AVAIL=$(echo "$DISK_LINE" | awk '{print $4}' | tr -d 'G')
if ! [[ "$DISK_TOTAL" =~ ^[0-9]+$ ]] || ! [[ "$DISK_AVAIL" =~ ^[0-9]+$ ]]; then
  warn "Could not parse disk space from 'df -PBG /' — continuing without disk pre-check"
elif [ "$DISK_AVAIL" -lt 10 ]; then
  fail "Insufficient disk space: ${DISK_AVAIL}G available on / (need at least 10G free)"
elif [ "$DISK_AVAIL" -lt 20 ] || [ "$DISK_TOTAL" -lt 30 ]; then
  warn "Low disk space: ${DISK_AVAIL}G available / ${DISK_TOTAL}G total on / (recommend 20G+ free, 30G+ total)"
fi
# The offline bundle adds ~7G peak usage (tarball + extraction + image import)
# beyond the base install.
if [ -n "$BUNDLE_PID" ] && [[ "$DISK_AVAIL" =~ ^[0-9]+$ ]] && [ "$DISK_AVAIL" -lt 15 ]; then
  fail "Insufficient disk space for the offline bundle: ${DISK_AVAIL}G available (need 15G+, or run with CYCLOPUS_ONLINE=1)"
fi

# Detect public IP (bounded, #8)
log "Detecting public IP..."
PUBLIC_IP=$(dl https://ifconfig.io 2>/dev/null || dl https://api.ipify.org 2>/dev/null || true)
[ -n "$PUBLIC_IP" ] || fail "Cannot detect public IP (set NODE_IP and rerun, or check network)"
log "Public IP: $PUBLIC_IP"

# Platform is single-stack IPv4: the detected public IP must be dotted-quad IPv4
# with each octet <= 255.
NOT_IPV4_MSG="Detected public IP '$PUBLIC_IP' is not IPv4. IPv6-only hosts are not supported (platform is single-stack IPv4); use an IPv4 host or set NODE_IP on a dual-stack host."
if ! [[ "$PUBLIC_IP" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || [ "$(echo "$PUBLIC_IP" | awk -F. '{for(i=1;i<=NF;i++) if ($i>255) {print "bad"; exit}}')" = "bad" ]; then
  fail "$NOT_IPV4_MSG"
fi

# Generate sslip.io hostname suffix + control-plane console host
HOSTNAME_SUFFIX=$(echo "$PUBLIC_IP" | tr '.' '-').sslip.io
CONSOLE_HOST="cyclopus.${HOSTNAME_SUFFIX}"
log "Hostname suffix: $HOSTNAME_SUFFIX"
log "Console host:    $CONSOLE_HOST"

# ACME email (#15)
if [ -z "$LETSENCRYPT_EMAIL" ]; then
  LETSENCRYPT_EMAIL="admin@${HOSTNAME_SUFFIX}"
  warn "LETSENCRYPT_EMAIL not set; using $LETSENCRYPT_EMAIL (you will NOT receive cert expiry notices)"
fi

# Load-or-generate secrets. All six values persist in STATE_FILE so a rerun
# reuses the secrets already baked into the Postgres role, the OpenObserve
# admin account, and the cluster secrets (regenerating them would desync).
# BESZEL_* keys are base64-encoded in STATE_FILE (the private key is multiline).
if [ -f "$STATE_FILE" ]; then
  # Sourcing hardening: every line must be a comment or a KEY="value" pair whose
  # value is drawn from a safe charset (base64 for the BESZEL_* keys, plus '!'
  # which OO_PASSWORD's class sentinel "Cyclopus!0…" uses — see the generator
  # below). None of these are shell-active inside the double quotes at source
  # time, so $( )/` ` injection still aborts before sourcing. Also turns a set -u
  # abort on garbage content into a clear message.
  # NOTE: '!' MUST stay in this class or every idempotent rerun after secrets are
  # generated would falsely trip "possible tampering" on its own OO_PASSWORD.
  if grep -vqE '^(#.*|(AUTH_BOOTSTRAP_SECRET|AUTH_TOKEN_PEPPER|DB_PASSWORD|OO_PASSWORD|BESZEL_ADMIN_PASSWORD|BESZEL_PUBLIC_KEY|BESZEL_PRIVATE_KEY)="[A-Za-z0-9+/=!]*")$' "$STATE_FILE"; then
    fail "STATE_FILE $STATE_FILE contains unexpected content (possible tampering); not sourcing"
  fi
  # shellcheck disable=SC1090
  . "$STATE_FILE"
  _missing=""
  for _v in AUTH_BOOTSTRAP_SECRET AUTH_TOKEN_PEPPER DB_PASSWORD OO_PASSWORD BESZEL_ADMIN_PASSWORD BESZEL_PUBLIC_KEY BESZEL_PRIVATE_KEY; do
    [ -n "${!_v:-}" ] || _missing="$_missing $_v"
  done
  [ -z "$_missing" ] || fail "STATE_FILE $STATE_FILE is incomplete (missing${_missing}); fix it, or delete it to regenerate (WARNING: regenerating changes the OpenObserve admin password and invalidates enrolled Beszel agents)"
  # Decode the base64-serialized Beszel keys after sourcing, and validate the
  # decodes (a corrupt-but-valid-base64 value must not silently propagate).
  BESZEL_PUBLIC_KEY=$(printf '%s' "$BESZEL_PUBLIC_KEY" | base64 -d) || fail "STATE_FILE $STATE_FILE: BESZEL_PUBLIC_KEY is not valid base64"
  BESZEL_PRIVATE_KEY=$(printf '%s' "$BESZEL_PRIVATE_KEY" | base64 -d) || fail "STATE_FILE $STATE_FILE: BESZEL_PRIVATE_KEY is not valid base64"
  case "$BESZEL_PRIVATE_KEY" in
    "-----BEGIN OPENSSH PRIVATE KEY-----"*) ;;
    *) fail "STATE_FILE $STATE_FILE: BESZEL_PRIVATE_KEY does not decode to an OpenSSH private key" ;;
  esac
  case "$BESZEL_PUBLIC_KEY" in
    "ssh-ed25519 "*) ;;
    *) fail "STATE_FILE $STATE_FILE: BESZEL_PUBLIC_KEY does not decode to an ssh-ed25519 public key; fix it or delete it to regenerate" ;;
  esac
  log "Loaded existing secrets from $STATE_FILE"
else
  log "Generating secrets..."
  AUTH_BOOTSTRAP_SECRET=$(openssl rand -base64 32 | tr -d '\n')
  AUTH_TOKEN_PEPPER=$(openssl rand -base64 32 | tr -d '\n')
  # Hex (not base64): this value is interpolated into the postgres:// URL at
  # line ~1068, and base64's '+', '/', '=' are not URL-safe — the API's URL
  # parser fails on them. Hex keeps the same entropy (32 bytes) with no
  # URL-encoding needed anywhere it's consumed.
  DB_PASSWORD=$(openssl rand -hex 32 | tr -d '\n')
  # OpenObserve v0.91+ rejects weak root passwords: must be 8-128 chars with at
  # least one lowercase, one uppercase, one digit, and one special character. A
  # raw base64 blob satisfies none of those classes reliably (and tr '/+' 'xy'
  # strips the only special chars base64 can produce), so build one that meets
  # every class unconditionally: fixed class sentinel + random base64 body.
  OO_PASSWORD="Cyclopus!0$(openssl rand -base64 24 | tr -d '\n' | tr '/+' 'xy')"
  # Beszel PocketBase superuser password (§8). Plain base64 is fine here: unlike
  # OpenObserve, PocketBase has no character-class password policy, just a
  # minimum length that a 32-byte base64 value clears comfortably. This is a
  # password the client re-authenticates with, not a pre-minted session token —
  # see internal/adapters/observability/beszel.go for why that distinction
  # matters (a static token would eventually expire; a credential doesn't).
  BESZEL_ADMIN_PASSWORD=$(openssl rand -base64 32 | tr -d '\n')
  # Beszel hub<->agent SSH keypair. Only the PUBLIC key enters the cluster
  # (agents); the private key stays on the operator side in $SECRETS_FILE.
  # ssh-keygen comes from openssh-client (present on the supported base
  # images; ensure it here since this block runs before the PostgreSQL
  # prerequisite install).
  command -v ssh-keygen >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq openssh-client; }
  BESZEL_KEY_TMP=$(mktemp)
  rm -f "$BESZEL_KEY_TMP"
  TMP_FILES="$TMP_FILES $BESZEL_KEY_TMP $BESZEL_KEY_TMP.pub"
  ssh-keygen -t ed25519 -f "$BESZEL_KEY_TMP" -N '' -C cyclopus-beszel >/dev/null
  BESZEL_PUBLIC_KEY=$(cat "$BESZEL_KEY_TMP.pub")
  BESZEL_PRIVATE_KEY=$(cat "$BESZEL_KEY_TMP")
  ( umask 0077; cat > "$STATE_FILE" <<EOF
# Cyclopus install state (idempotent reruns). BESZEL_* keys are base64-encoded.
AUTH_BOOTSTRAP_SECRET="${AUTH_BOOTSTRAP_SECRET}"
AUTH_TOKEN_PEPPER="${AUTH_TOKEN_PEPPER}"
DB_PASSWORD="${DB_PASSWORD}"
OO_PASSWORD="${OO_PASSWORD}"
BESZEL_ADMIN_PASSWORD="${BESZEL_ADMIN_PASSWORD}"
BESZEL_PUBLIC_KEY="$(printf '%s' "$BESZEL_PUBLIC_KEY" | base64 | tr -d '\n')"
BESZEL_PRIVATE_KEY="$(printf '%s' "$BESZEL_PRIVATE_KEY" | base64 | tr -d '\n')"
EOF
  )
  log "Generated new secrets (saved to $STATE_FILE)"
fi
# Fixed, non-secret identity for the Beszel superuser Cyclopus provisions and
# authenticates as (§8). Not persisted in STATE_FILE: it isn't a secret and
# doesn't change between reruns.
BESZEL_ADMIN_EMAIL="beszel-admin@cyclopus.local"

# Install PostgreSQL. One apt transaction for ALL prerequisites (zstd extracts
# the offline bundle; ufw avoids a second update+install in the firewall step).
log "Installing PostgreSQL $PG_MAJOR..."
if ! command -v psql >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y curl ca-certificates gnupg lsb-release openssh-client zstd ufw
  # Write the key to a file first, then dearmor from the file — piping directly
  # to gpg can wedge if the pipe doesn't close cleanly (gpg blocks in pselect
  # waiting for stdin EOF). Also bound gpg with timeout so a hung download
  # can't wedge the installer forever.
  _pgdg_key_tmp=$(mktemp)
  TMP_FILES="$TMP_FILES $_pgdg_key_tmp"
  dl https://www.postgresql.org/media/keys/ACCC4CF8.asc -o "$_pgdg_key_tmp" || fail "PGDG key download failed"
  timeout 30 gpg --dearmor -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg "$_pgdg_key_tmp" || fail "PGDG key dearmor failed"
  rm -f "$_pgdg_key_tmp"
  echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list
  apt-get update -qq
  apt-get install -y postgresql-"$PG_MAJOR"
else
  log "PostgreSQL already installed, skipping"
fi

# Ensure the cluster is running and accepting connections before we use it. The
# postgresql apt postinstall usually auto-starts it, but don't assume: on some
# systemd environments (containers, slow first boot) the socket isn't up yet
# when the role/database SQL below runs, which aborts the install with a "could
# not connect to server" error. Only act if it isn't already up, and use
# --no-block so a slow/queued systemd start job can never wedge the installer —
# readiness is confirmed by polling pg_isready, not by systemctl returning.
if ! sudo -u postgres pg_isready -q 2>/dev/null; then
  systemctl enable postgresql >/dev/null 2>&1 || true
  systemctl start --no-block postgresql >/dev/null 2>&1 || true
fi
PG_WAIT=0
until sudo -u postgres pg_isready -q 2>/dev/null; do
  PG_WAIT=$((PG_WAIT + 1))
  [ "$PG_WAIT" -ge 30 ] && fail "PostgreSQL did not become ready (service failed to start)"
  sleep 2
done

# Create database and user.
# #1/#4: pass password via PGPASSWORD to the SINGLE psql process (not a shell-wide
# export), and use a here-doc without psql :vars so nothing lands in ~/.psql_history.
# The password reconciliation below is INTENTIONAL: pre-Phase-B installs have a
# cyclopus role whose password exists nowhere on disk, so a fresh STATE_FILE
# would desync helm values from the role. The unconditional ALTER ROLE converges
# the role password to STATE_FILE's DB_PASSWORD on every run (this also covers
# the partial-first-run case: CREATE is IF NOT EXISTS, ALTER heals the rest).
log "Creating Cyclopus database and user..."
CREATE_ROLE_SQL=$(cat <<SQL
DO \$do\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'cyclopus') THEN
    EXECUTE format('CREATE ROLE cyclopus LOGIN PASSWORD %L', '${DB_PASSWORD}');
  ELSE
    EXECUTE format('ALTER ROLE cyclopus PASSWORD %L', '${DB_PASSWORD}');
  END IF;
END
\$do\$;
SQL
)
sudo -u postgres env PGPASSWORD="$DB_PASSWORD" psql -v ON_ERROR_STOP=1 -q -c "$CREATE_ROLE_SQL"
sudo -u postgres env PGPASSWORD="$DB_PASSWORD" psql -v ON_ERROR_STOP=1 -tqc \
  "SELECT 1 FROM pg_database WHERE datname='cyclopus'" | grep -q 1 || \
  sudo -u postgres createdb -O cyclopus cyclopus

# Configure PostgreSQL for pod access
NODE_IP=${NODE_IP:-$(ip -o -4 addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -n1 || true)}
[ -n "$NODE_IP" ] || fail "Could not detect node IP. Set NODE_IP env var manually."
log "Node IP: $NODE_IP"

# #5: enable self-signed TLS for Postgres (Debian ships snakeoil certs) and require it.
PG_CONF="/etc/postgresql/$PG_MAJOR/main/postgresql.conf"
PG_HBA="/etc/postgresql/$PG_MAJOR/main/pg_hba.conf"
sed -i "s/^#\?listen_addresses.*/listen_addresses = 'localhost,$NODE_IP'/" "$PG_CONF"
if ! grep -q "^ssl = on" "$PG_CONF"; then
  {
    echo "ssl = on"
    echo "ssl_cert_file = '/etc/ssl/certs/ssl-cert-snakeoil.pem'"
    echo "ssl_key_file = '/etc/ssl/private/ssl-cert-snakeoil.key'"
  } >> "$PG_CONF"
fi
# Ensure the postgres user can read the snakeoil key.
usermod -aG ssl-cert postgres 2>/dev/null || true
# Pods connect FROM the pod network, not the node IP. Allow the k3s cluster pod
# CIDR (default 10.42.0.0/16) over SSL (hostssl) with scram-sha-256. listen_addresses
# is already restricted to localhost + node IP, so only in-cluster clients reach 5432.
POD_CIDR_HBA="10.42.0.0/16"
if ! grep -q "hostssl cyclopus cyclopus $POD_CIDR_HBA scram-sha-256" "$PG_HBA"; then
  echo "hostssl cyclopus cyclopus $POD_CIDR_HBA scram-sha-256" >> "$PG_HBA"
fi
# --no-block + pg_isready poll: a forking postgresql unit can leave systemd's job
# tracking in "activating" on some hosts, wedging a synchronous restart even
# though the server comes back up. Don't block the installer on that; confirm
# readiness by polling the socket instead.
systemctl restart --no-block postgresql >/dev/null 2>&1 || fail "could not restart PostgreSQL"
PG_WAIT=0
until sudo -u postgres pg_isready -q 2>/dev/null; do
  PG_WAIT=$((PG_WAIT + 1))
  [ "$PG_WAIT" -ge 30 ] && fail "PostgreSQL did not become ready after config restart"
  sleep 2
done
log "PostgreSQL configured (SSL on; listen node-IP+localhost; pod-CIDR auth)"

# Configure firewall before installing k3s (#3/#9)
log "Configuring firewall (ufw)..."
if ! command -v ufw >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq ufw
fi
ufw allow 22/tcp                                   # SSH (prevent lockout)
ufw allow 80/tcp                                    # HTTP  (Traefik hostPort; ACME HTTP-01 needs 80)
ufw allow 443/tcp                                   # HTTPS (Traefik hostPort)
ufw allow from 127.0.0.1 to any port 6443 proto tcp # k3s API localhost-only
ufw --force enable
systemctl enable ufw                               # #9: persist across reboot
log "Firewall configured (SSH, 80, 443; k3s API localhost-only; port 8080 NOT exposed)"

# Collect the background bundle download. On any failure (missing artifact,
# network, checksum mismatch) fall back to the online path — every online
# artifact is independently checksum-verified, so the fallback is not a
# security downgrade.
if [ -n "$BUNDLE_PID" ]; then
  log "Waiting for bundle download..."
  if wait "$BUNDLE_PID"; then
    command -v zstd >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq zstd; }
    rm -rf "$BUNDLE_DIR"
    install -d -m 0755 "$BUNDLE_DIR"
    if tar --zstd -C "$BUNDLE_DIR" --strip-components=1 -xf "$BUNDLE_TARBALL"; then
      BUNDLE_MODE=1
      rm -f "$BUNDLE_TARBALL" "$BUNDLE_TARBALL.sha256"
      log "Offline bundle ready at $BUNDLE_DIR (single-download fast path)"
    else
      warn "bundle extraction failed — falling back to online install path"
    fi
  else
    warn "bundle download/verify failed (see /tmp/cyclopus-bundle-dl.log) — falling back to online install path"
  fi
fi
[ "$BUNDLE_MODE" = 1 ] || log "Install mode: online (per-artifact downloads + registry pulls)"

# Install k3s with hardening. #10: download installer, then execute (no curl|sh).
# Bundle mode: use the bundled k3s binary + installer and pre-stage ALL images
# (k3s airgap set + platform/workload images) in the k3s auto-import directory
# so containerd never hits a registry.
log "Installing k3s $K3S_VERSION with hardening..."
if [ "$BUNDLE_MODE" = 1 ]; then
  install -m 0755 "$BUNDLE_DIR/bin/k3s" /usr/local/bin/k3s
  install -d -m 0755 /var/lib/rancher/k3s/agent/images
  cp "$BUNDLE_DIR"/images/*.tar* /var/lib/rancher/k3s/agent/images/
  K3S_INSTALLER="$BUNDLE_DIR/bin/k3s-install.sh"
  INSTALL_K3S_SKIP_DOWNLOAD=true
  export INSTALL_K3S_SKIP_DOWNLOAD
else
  K3S_INSTALLER=/tmp/k3s-install.sh
  dl https://get.k3s.io -o "$K3S_INSTALLER"
fi
chmod +x "$K3S_INSTALLER"
INSTALL_K3S_VERSION=$K3S_VERSION \
INSTALL_K3S_EXEC="server \
  --disable traefik \
  --secrets-encryption \
  --kube-apiserver-arg=audit-log-path=/var/log/k3s-audit.log \
  --kube-apiserver-arg=audit-log-maxage=30 \
  --kube-apiserver-arg=audit-log-maxbackup=10 \
  --kube-apiserver-arg=audit-log-maxsize=100 \
  --node-label=cyclopus.app/role=control-plane" \
sh "$K3S_INSTALLER"
[ "$BUNDLE_MODE" = 1 ] || rm -f "$K3S_INSTALLER"
systemctl enable --now k3s

log "Waiting for k3s API..."
# Bundle mode: k3s imports the staged image tarballs (several GB) during
# startup, which delays the API — allow up to 5 minutes there, 2 otherwise.
K3S_WAIT=60; [ "$BUNDLE_MODE" = 1 ] && K3S_WAIT=150
i=0; while [ $i -lt "$K3S_WAIT" ]; do
  $KUBECTL get --raw=/readyz >/dev/null 2>&1 && break
  i=$((i+1)); [ $i -lt "$K3S_WAIT" ] || fail "k3s API did not become ready"; sleep 2
done
# Image import completes before the API serves; the staged tarballs are now
# duplicated inside the containerd content store, so reclaim the disk (both
# the k3s staging dir and the bundle copies).
[ "$BUNDLE_MODE" = 1 ] && rm -f /var/lib/rancher/k3s/agent/images/*.tar* "$BUNDLE_DIR"/images/*.tar*
log "k3s ready"

# §2.2 opt-in: repel ordinary app pods from the control-plane node. Off by
# default because a single-node install must keep the node schedulable.
if [ "$CYCLOPUS_CONTROL_PLANE_TAINT" = "true" ]; then
  NODE_COUNT=$($KUBECTL get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ' || true)
  [ -n "$NODE_COUNT" ] || fail "could not query nodes"
  if [ "$NODE_COUNT" -eq 1 ]; then
    fail "CYCLOPUS_CONTROL_PLANE_TAINT=true refused: single-node install — tainting the only node leaves app pods unschedulable. Join worker nodes first (then re-run), or unset CYCLOPUS_CONTROL_PLANE_TAINT."
  fi
  # k3s labels its server node(s) node-role.kubernetes.io/control-plane.
  CP_NODES=$($KUBECTL get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[*].metadata.name}')
  [ -n "$CP_NODES" ] || fail "no nodes carry the node-role.kubernetes.io/control-plane label"
  for NODE_NAME in $CP_NODES; do
    $KUBECTL taint node "$NODE_NAME" cyclopus.app/role=control-plane:NoSchedule --overwrite
    log "Tainted node $NODE_NAME cyclopus.app/role=control-plane:NoSchedule"
  done
fi

# #14: validate NODE_IP is the pod-reachable interface once the API is up.
POD_CIDR=$($KUBECTL get node -o jsonpath='{.items[0].spec.podCIDR}' 2>/dev/null || echo "")
log "Pod CIDR: ${POD_CIDR:-unknown}"

# Install gVisor via the CANONICAL bootstrap script (#7: no duplicated checksums).
log "Installing gVisor (canonical bootstrap script)..."
if [ "$BUNDLE_MODE" = 1 ]; then
  # Bundled scripts + binaries; checksum verification happens inside
  # install_gvisor.sh regardless of the local source (GVISOR_LOCAL_DIR).
  cp "$RESOLVED_VERSIONS_ENV" "$BUNDLE_DIR/bootstrap/versions.env"
  ( cd "$BUNDLE_DIR/bootstrap" && CYCLOPUS_VERSIONS_ENV="$BUNDLE_DIR/bootstrap/versions.env" \
    GVISOR_LOCAL_DIR="$BUNDLE_DIR/gvisor" sh ./install_gvisor.sh ) || fail "gVisor bootstrap failed"
  rm -f "$BUNDLE_DIR/bootstrap/versions.env"
else
  GVISOR_SH=/tmp/install_gvisor.sh
  dl "$RAW_BASE/infra/bootstrap/install_gvisor.sh" -o "$GVISOR_SH"
  dl "$RAW_BASE/infra/bootstrap/common.sh" -o /tmp/common.sh
  # Reuse the versions.env resolved above so local-checkout pins are honored.
  cp "$RESOLVED_VERSIONS_ENV" /tmp/versions.env
  TMP_FILES="$TMP_FILES /tmp/versions.env"
  ( cd /tmp && CYCLOPUS_VERSIONS_ENV=/tmp/versions.env sh "$GVISOR_SH" ) || fail "gVisor bootstrap failed"
  rm -f "$GVISOR_SH" /tmp/common.sh /tmp/versions.env
fi
systemctl restart k3s
i=0; while [ $i -lt 60 ]; do
  $KUBECTL get --raw=/readyz >/dev/null 2>&1 && break
  i=$((i+1)); [ $i -lt 60 ] || fail "k3s API not ready after gVisor"; sleep 2
done

# Register both RuntimeClasses; keep in sync with the canonical
# infra/bootstrap/register_runtimeclass.sh used by other install paths.
log "Registering gVisor and runc RuntimeClasses..."
$KUBECTL apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: runc
handler: runc
EOF
$KUBECTL get runtimeclass gvisor >/dev/null 2>&1 || fail "gvisor RuntimeClass missing"
$KUBECTL get runtimeclass runc >/dev/null 2>&1 || fail "runc RuntimeClass missing"

# Install Helm (#8 bounded downloads)
log "Installing Helm $HELM_VERSION..."
if ! command -v helm >/dev/null 2>&1; then
  if [ "$BUNDLE_MODE" = 1 ]; then
    install -m 0755 "$BUNDLE_DIR/bin/helm" /usr/local/bin/helm
  else
    HELM_TAR="helm-${HELM_VERSION}-linux-${GO_ARCH}.tar.gz"
    dl "https://get.helm.sh/$HELM_TAR" -o /tmp/helm.tar.gz
    dl "https://get.helm.sh/${HELM_TAR}.sha256sum" -o /tmp/helm.sha256
    echo "$(awk '{print $1}' /tmp/helm.sha256)  /tmp/helm.tar.gz" | sha256sum -c - || fail "Helm checksum mismatch"
    tar -C /tmp -xzf /tmp/helm.tar.gz
    install -m 0755 /tmp/linux-${GO_ARCH}/helm /usr/local/bin/helm
    rm -rf /tmp/helm.tar.gz /tmp/helm.sha256 /tmp/linux-${GO_ARCH}
  fi
else
  log "Helm already installed"
fi

# Helm needs a kubeconfig; k3s writes one at a fixed path.
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# installed_chart_version REL NS: print the currently installed chart version for
# release REL in namespace NS. The grep is anchored on the release name (helm
# list json: {"name":"REL",...,"chart":"REL-VER",...}) so REL=keda in NS=keda
# cannot collide with keda-add-ons-http, which shares the "keda-" chart prefix.
# Returns 1 when the version cannot be read.
installed_chart_version() {
  _rel=$1; _ns=$2
  _v=$(helm list -n "$_ns" -o json | grep -o "\"name\":\"${_rel}\"[^}]*\"chart\":\"${_rel}-[^\"]*\"" | head -n1 | sed "s/.*${_rel}-//; s/\"//" || true)
  [ -n "$_v" ] || return 1
  printf '%s\n' "$_v"
}

# #11: assert an installed release's chart version matches what we requested.
assert_chart_version() {
  _rel=$1; _ns=$2; _want=$3
  _got=$(installed_chart_version "$_rel" "$_ns") || fail "$_rel: could not read installed chart version"
  [ "$_got" = "$_want" ] || fail "$_rel chart version mismatch: installed $_got, expected $_want"
}

# converge_chart REL NS PIN: three-way decision for install-as-upgrade.
#   0 = deployed and already at PIN (logged; caller does nothing)
#   1 = not deployed (caller installs)
#   2 = deployed at a different version (caller upgrades to PIN)
# Fails hard when the release is deployed but its version is unreadable.
converge_chart() {
  _rel=$1; _ns=$2; _pin=$3
  if ! helm_deployed "$_rel" "$_ns"; then return 1; fi
  _cur=$(installed_chart_version "$_rel" "$_ns") || fail "$_rel: could not read installed chart version"
  if [ "$_cur" = "$_pin" ]; then
    log "$_rel already at pinned chart version $_pin"
    return 0
  fi
  # Downgrade visibility: sort -V puts the lower version first; if the pin is
  # the minimum, the installed version is NEWER than the pin.
  _min=$(printf '%s\n%s\n' "$_pin" "$_cur" | sort -V | head -n1 || true)
  if [ "$_min" = "$_pin" ]; then
    warn "$_rel: installed chart version $_cur is NEWER than pinned $_pin — downgrading to pin"
  fi
  return 2
}

# Allow unprivileged processes to bind low ports so the non-root Traefik container
# can listen on :80/:443 over hostNetwork (avoids fragile CAP_NET_BIND_SERVICE +
# allowPrivilegeEscalation wrangling with drop:[ALL]). Persisted for reboots.
log "Enabling unprivileged low-port binding (net.ipv4.ip_unprivileged_port_start=0)..."
echo 'net.ipv4.ip_unprivileged_port_start=0' > /etc/sysctl.d/99-cyclopus-lowports.conf
sysctl -q -w net.ipv4.ip_unprivileged_port_start=0

# chart_ref NAME VERSION REPO: the chart source to install from — the bundled
# tgz in bundle mode, or <repo>/<name> from the remote repos in online mode.
chart_ref() {
  if [ "$BUNDLE_MODE" = 1 ]; then
    [ -f "$BUNDLE_DIR/charts/$1-$2.tgz" ] || fail "bundle is missing charts/$1-$2.tgz"
    printf '%s\n' "$BUNDLE_DIR/charts/$1-$2.tgz"
  else
    printf '%s\n' "$3/$1"
  fi
}

# Online mode only: add + update all repos ONCE up front. helm's repo config
# is a shared file — doing this serially before the parallel installs below
# avoids write races between background helm processes. Bundle mode needs no
# repos at all (local tgzs).
if [ "$BUNDLE_MODE" != 1 ]; then
  log "Adding helm repos..."
  helm repo add traefik https://traefik.github.io/charts 2>/dev/null || true
  helm repo add kyverno https://kyverno.github.io/kyverno/ 2>/dev/null || true
  helm repo add jetstack https://charts.jetstack.io 2>/dev/null || true
  helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
  helm repo update
fi

# Parallel chart-install driver. Each install function runs in the background
# with its own log; wait_charts collects exit codes and dumps failing logs.
# (fail() inside a backgrounded function only kills that subshell — the
# nonzero exit is what wait_charts reports.)
declare -A CHART_PID_NAME CHART_PID_LOG
CHART_PIDS=""
start_chart() {
  _log=/tmp/cyclopus-chart-$1.log
  TMP_FILES="$TMP_FILES $_log"
  "$2" >"$_log" 2>&1 &
  CHART_PIDS="$CHART_PIDS $!"
  CHART_PID_NAME[$!]=$1
  CHART_PID_LOG[$!]=$_log
}
wait_charts() {
  _failed=0
  for _p in $CHART_PIDS; do
    if ! wait "$_p"; then
      _failed=1
      warn "chart install failed: ${CHART_PID_NAME[$_p]} — full log follows"
      cat "${CHART_PID_LOG[$_p]}" >&2
    fi
  done
  CHART_PIDS=""
  [ "$_failed" = 0 ] || fail "parallel chart install failed (see logs above)"
}

# Install Traefik as a host-network DaemonSet binding ports 80/443 directly on the
# node. This is REQUIRED for Let's Encrypt HTTP-01: the ACME server connects to
# http://<host>:80/.well-known/acme-challenge/... — a NodePort (30080) is not
# reachable by Let's Encrypt, so certs would never issue. hostNetwork puts Traefik
# on the node's :80/:443 so both ACME and normal traffic work.
install_traefik() {
  # Traefik's config lives entirely in --set flags (no values file), so the same
  # array is applied on install AND upgrade (deliberately NOT --reuse-values: the
  # flags are the config, and an upgrade must converge them).
  TRAEFIK_SET_ARGS=(
    --set deployment.kind=DaemonSet
    --set hostNetwork=true
    --set service.enabled=false
    --set updateStrategy.rollingUpdate.maxUnavailable=1
    --set updateStrategy.rollingUpdate.maxSurge=0
    --set ports.web.port=80
    --set ports.web.hostPort=80
    --set ports.websecure.port=443
    --set ports.websecure.hostPort=443
    --set resources.requests.cpu=50m
    --set resources.requests.memory=64Mi
    --set resources.limits.memory=128Mi
  )
  _chart=$(chart_ref traefik "$TRAEFIK_CHART_VERSION" traefik)
  _ver_args=(); [ "$BUNDLE_MODE" = 1 ] || _ver_args=(--version "$TRAEFIK_CHART_VERSION")
  _rc=0
  converge_chart traefik kube-system "$TRAEFIK_CHART_VERSION" || _rc=$?
  case "$_rc" in
    1)
      helm install traefik "$_chart" \
        "${_ver_args[@]}" \
        --namespace kube-system \
        "${TRAEFIK_SET_ARGS[@]}" \
        --timeout 10m
      ;;
    2)
      log "Traefik chart not at pin — upgrading to $TRAEFIK_CHART_VERSION"
      helm upgrade traefik "$_chart" \
        "${_ver_args[@]}" \
        --namespace kube-system \
        "${TRAEFIK_SET_ARGS[@]}" \
        --cleanup-on-fail --timeout 10m
      ;;
  esac
  assert_chart_version traefik kube-system "$TRAEFIK_CHART_VERSION"
}

install_kyverno() {
  # Scope the fail-closed webhooks (§7.1) to platform-managed namespaces only.
  # Kyverno's generated webhook configurations otherwise match every namespace
  # except kube-system, so a webhook outage fails closed cluster-wide — including
  # the kyverno and cyclopus namespaces, which deadlocks Kyverno's own recovery.
  # With this selector only app namespaces (labeled managed-by=cyclopus) are
  # webhook-matched; a Kyverno restart then pauses app-namespace admissions
  # (§9.3's accepted behavior) without touching the control plane. Same flags on
  # install AND upgrade (no --reuse-values): the flags are the config.
  # The label/value MUST stay in sync with kyverno.policies.managedByLabel /
  # managedByValue in charts/cyclopus/values.yaml (asserted by
  # scripts/test_infrastructure.sh).
  KYVERNO_SET_ARGS=(
    --set-json 'config.webhooks={"namespaceSelector":{"matchLabels":{"app.kubernetes.io/managed-by":"cyclopus"}}}'
  )
  _chart=$(chart_ref kyverno "$KYVERNO_CHART_VERSION" kyverno)
  _ver_args=(); [ "$BUNDLE_MODE" = 1 ] || _ver_args=(--version "$KYVERNO_CHART_VERSION")
  _rc=0
  converge_chart kyverno kyverno "$KYVERNO_CHART_VERSION" || _rc=$?
  case "$_rc" in
    1)
      helm install kyverno "$_chart" \
        "${_ver_args[@]}" \
        --namespace kyverno --create-namespace \
        "${KYVERNO_SET_ARGS[@]}" \
        --wait --timeout 5m
      ;;
    2)
      log "Kyverno chart not at pin — upgrading to $KYVERNO_CHART_VERSION"
      # Upgrade passes identical value flags as install by design (no --reuse-values).
      # Note: helm never upgrades the chart's crds/ directory — a pin bump with CRD
      # schema changes needs a manual CRD apply (out of scope).
      helm upgrade kyverno "$_chart" \
        "${_ver_args[@]}" \
        --namespace kyverno \
        "${KYVERNO_SET_ARGS[@]}" \
        --cleanup-on-fail --wait --timeout 5m
      ;;
  esac
  assert_chart_version kyverno kyverno "$KYVERNO_CHART_VERSION"
}

install_cert_manager() {
  _chart=$(chart_ref cert-manager "$CERT_MANAGER_VERSION" jetstack)
  _ver_args=(); [ "$BUNDLE_MODE" = 1 ] || _ver_args=(--version "$CERT_MANAGER_VERSION")
  _rc=0
  converge_chart cert-manager cert-manager "$CERT_MANAGER_VERSION" || _rc=$?
  case "$_rc" in
    1)
      helm install cert-manager "$_chart" \
        "${_ver_args[@]}" \
        --namespace cert-manager --create-namespace \
        --set crds.enabled=true \
        --wait --timeout 5m
      ;;
    2)
      log "cert-manager chart not at pin — upgrading to $CERT_MANAGER_VERSION"
      # Upgrade passes identical value flags as install by design (no --reuse-values).
      helm upgrade cert-manager "$_chart" \
        "${_ver_args[@]}" \
        --namespace cert-manager \
        --set crds.enabled=true \
        --cleanup-on-fail --wait --timeout 5m
      ;;
  esac
  assert_chart_version cert-manager cert-manager "$CERT_MANAGER_VERSION"
}

install_keda() {
  _chart=$(chart_ref keda "$KEDA_VERSION" kedacore)
  _ver_args=(); [ "$BUNDLE_MODE" = 1 ] || _ver_args=(--version "$KEDA_VERSION")
  _rc=0
  converge_chart keda keda "$KEDA_VERSION" || _rc=$?
  case "$_rc" in
    1)
      helm install keda "$_chart" \
        "${_ver_args[@]}" \
        --namespace keda --create-namespace \
        --wait --timeout 5m
      ;;
    2)
      log "KEDA chart not at pin — upgrading to $KEDA_VERSION"
      # Upgrade passes identical value flags as install by design (no --reuse-values).
      helm upgrade keda "$_chart" \
        "${_ver_args[@]}" \
        --namespace keda \
        --cleanup-on-fail --wait --timeout 5m
      ;;
  esac
  assert_chart_version keda keda "$KEDA_VERSION"
}

# Stage 1: Traefik ∥ Kyverno. Traefik runs in kube-system, which Kyverno's
# fail-closed admission webhooks exclude by default — so it is safe for these
# two to race. Namespaced workloads (cert-manager, keda, cyclopus) must wait
# for the Kyverno webhook to be verified ready (stage 2 below).
log "Installing Traefik v$TRAEFIK_CHART_VERSION + Kyverno $KYVERNO_VERSION in parallel..."
start_chart traefik install_traefik
start_chart kyverno install_kyverno
wait_charts

# #13: verify Traefik is actually reachable on host :80 (ACME HTTP-01 depends on it).
i=0; while [ $i -lt 60 ]; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:80/ 2>/dev/null || echo 000)
  # Any HTTP response (404 from Traefik with no route is fine) means it's listening.
  [ "$code" != "000" ] && break
  i=$((i+1)); [ $i -lt 60 ] || fail "Traefik is not listening on host :80 (ACME HTTP-01 would fail)"; sleep 2
done
log "Traefik listening on host :80/:443 (ACME HTTP-01 reachable)"

# Wait for Kyverno webhook. #12: probe pod is cleaned up here AND in the EXIT trap.
log "Waiting for Kyverno webhook..."
i=0; while [ $i -lt 120 ]; do
  ready=$($KUBECTL -n kyverno get deployment kyverno-admission-controller -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
  if [ "${ready:-0}" -gt 0 ] 2>/dev/null; then
    if $KUBECTL run cyclopus-webhook-probe -n default --image=alpine --restart=Never --dry-run=server >/dev/null 2>&1; then
      $KUBECTL delete pod cyclopus-webhook-probe -n default --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
      break
    fi
  fi
  i=$((i+1)); [ $i -lt 120 ] || fail "Kyverno webhook not ready after 4 minutes"; sleep 2
done
log "Kyverno webhook ready"

# Stage 2: cert-manager ∥ KEDA — safe now that the Kyverno webhook is ready.
log "Installing cert-manager $CERT_MANAGER_VERSION + KEDA v$KEDA_VERSION in parallel..."
start_chart cert-manager install_cert_manager
start_chart keda install_keda
wait_charts

log "Waiting for cert-manager webhook..."
i=0; while [ $i -lt 60 ]; do
  $KUBECTL -n cert-manager get deployment cert-manager-webhook -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null | grep -q '^True$' && break
  i=$((i+1)); [ $i -lt 60 ] || fail "cert-manager webhook not ready"; sleep 2
done

# Create Let's Encrypt ClusterIssuers (production default; staging available). #15 email.
log "Creating Let's Encrypt ClusterIssuers (production default)..."
$KUBECTL apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-production
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: ${LETSENCRYPT_EMAIL}
    privateKeySecretRef:
      name: letsencrypt-production-key
    solvers:
    - http01:
        ingress:
          class: traefik
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-staging
spec:
  acme:
    server: https://acme-staging-v02.api.letsencrypt.org/directory
    email: ${LETSENCRYPT_EMAIL}
    privateKeySecretRef:
      name: letsencrypt-staging-key
    solvers:
    - http01:
        ingress:
          class: traefik
EOF

# KEDA core was installed in stage 2 above; the HTTP add-on must follow it
# (it extends KEDA's CRDs).
log "Installing KEDA HTTP Add-on v$KEDA_HTTP_ADDON_VERSION..."
# Like Traefik, the resources --sets ARE the config here, so they are re-applied
# on upgrade (deliberately NOT --reuse-values) via a shared array.
KEDA_HTTP_SET_ARGS=(
  --set operator.resources.requests.cpu=50m
  --set operator.resources.requests.memory=64Mi
  --set operator.resources.limits.memory=128Mi
  --set interceptor.resources.requests.cpu=50m
  --set interceptor.resources.requests.memory=64Mi
  --set interceptor.resources.limits.memory=128Mi
  --set scaler.resources.requests.cpu=50m
  --set scaler.resources.requests.memory=64Mi
  --set scaler.resources.limits.memory=128Mi
)
_chart=$(chart_ref keda-add-ons-http "$KEDA_HTTP_ADDON_VERSION" kedacore)
_ver_args=(); [ "$BUNDLE_MODE" = 1 ] || _ver_args=(--version "$KEDA_HTTP_ADDON_VERSION")
_rc=0
converge_chart keda-add-ons-http keda "$KEDA_HTTP_ADDON_VERSION" || _rc=$?
case "$_rc" in
  1)
    helm install keda-add-ons-http "$_chart" \
      "${_ver_args[@]}" \
      --namespace keda \
      "${KEDA_HTTP_SET_ARGS[@]}" \
      --wait --timeout 10m
    ;;
  2)
    log "KEDA HTTP Add-on chart not at pin — upgrading to $KEDA_HTTP_ADDON_VERSION"
    helm upgrade keda-add-ons-http "$_chart" \
      "${_ver_args[@]}" \
      --namespace keda \
      "${KEDA_HTTP_SET_ARGS[@]}" \
      --cleanup-on-fail --wait --timeout 10m
    ;;
esac
assert_chart_version keda-add-ons-http keda "$KEDA_HTTP_ADDON_VERSION"

# Observability secrets must exist BEFORE the Cyclopus API boots: the API
# resolves its OpenObserve credential through a k8s-secret reference at startup
# (cmd/cyclopus/main.go buildTelemetryProvider -> internal/secretref), so a
# missing secret would crash-loop the pod and fail the --atomic helm install.
log "Creating observability secrets..."
$KUBECTL create namespace "$CYCLOPUS_NAMESPACE" --dry-run=client -o yaml | $KUBECTL apply -f -
# OO_PASSWORD and the Beszel keypair are loaded/generated in the secrets block
# above (STATE_FILE persistence); only the derived values are computed here.
# credential: raw "user:password" the API base64-encodes itself
# (internal/adapters/observability/openobserve.go OpenObserveConfig.Credential).
# basic-auth: pre-encoded "Basic <base64>" header value for the OTel Collector
# exporter (OpenObserve requires auth on its ingestion endpoints too).
OO_CREDENTIAL="admin@cyclopus.local:${OO_PASSWORD}"
OO_BASIC_AUTH=$(printf '%s' "$OO_CREDENTIAL" | base64 | tr -d '\n')
# Write the Secret as a 0600 manifest instead of --from-literal so the password
# never appears in process argv (same pattern as VALUES_FILE below).
OO_SECRET_FILE=$( (umask 0077; mktemp) )
TMP_FILES="$TMP_FILES $OO_SECRET_FILE"
cat > "$OO_SECRET_FILE" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: openobserve-auth
  namespace: ${CYCLOPUS_NAMESPACE}
type: Opaque
stringData:
  password: "${OO_PASSWORD}"
  credential: "${OO_CREDENTIAL}"
  basic-auth: "${OO_BASIC_AUTH}"
EOF
$KUBECTL apply -f "$OO_SECRET_FILE"
rm -f "$OO_SECRET_FILE"
$KUBECTL -n "$CYCLOPUS_NAMESPACE" create secret generic beszel-agent-key \
  --from-literal=public="$BESZEL_PUBLIC_KEY" \
  --dry-run=client -o yaml | $KUBECTL apply -f -

# Install the observability stack (OpenObserve + Beszel + OTel Collector +
# Beszel agents) into the Cyclopus namespace (§8). Applied BEFORE the Cyclopus
# helm install below so its pods schedule and start while the control plane
# rolls out — readiness is only verified (non-fatally) at the end. Raw
# manifests so the one-line install works without a repo checkout.
#
# Neither backend gets an Ingress: §8 requires OpenObserve to be "queried
# server-side through the Cyclopus API, never exposed directly" and Beszel to be
# "proxied the same way", so both are reached only in-cluster over their
# ClusterIP Services (see the observability block in the values file below), and
# network-policy.yaml restricts even that to this namespace. Operators read
# logs, traces, and metrics through the Cyclopus console.
#
# The manifests hardcode `namespace: cyclopus`; rewrite when CYCLOPUS_NAMESPACE
# differs — including the collector exporter DNS name and the filelog
# self-exclusion prefix, otherwise the collector tails its own logs
# (self-ingest loop).
log "Installing observability stack (OpenObserve + Beszel + OTel Collector + agents)..."
for OBS_NAME in openobserve beszel otel-collector beszel-agent network-policy; do
  if [ "$BUNDLE_MODE" = 1 ]; then
    OBS_MANIFEST="$BUNDLE_DIR/observability/$OBS_NAME.yaml"
    [ -f "$OBS_MANIFEST" ] || fail "bundle is missing observability/$OBS_NAME.yaml"
  else
    OBS_MANIFEST=$(mktemp)
    TMP_FILES="$TMP_FILES $OBS_MANIFEST"
    dl "$RAW_BASE/infra/observability/$OBS_NAME.yaml" -o "$OBS_MANIFEST"
  fi
  if [ "$CYCLOPUS_NAMESPACE" != "cyclopus" ]; then
    [ "$BUNDLE_MODE" = 1 ] && { cp "$OBS_MANIFEST" /tmp/obs-edit.yaml; OBS_MANIFEST=/tmp/obs-edit.yaml; TMP_FILES="$TMP_FILES $OBS_MANIFEST"; }
    sed -i "s/namespace: cyclopus/namespace: ${CYCLOPUS_NAMESPACE}/g; s/openobserve\.cyclopus\.svc/openobserve.${CYCLOPUS_NAMESPACE}.svc/g; s|/var/log/pods/cyclopus_|/var/log/pods/${CYCLOPUS_NAMESPACE}_|g" "$OBS_MANIFEST"
  fi
  $KUBECTL apply -f "$OBS_MANIFEST"
done

# --- Beszel admin + operator-metrics credential (§8) -------------------------
# Fully automates what used to require opening the Beszel UI: create the
# PocketBase superuser non-interactively, store its credential (not a
# pre-minted session token — see internal/adapters/observability/beszel.go for
# why a password credential is used instead) as a k8s Secret, and register this
# node as a Beszel "system" so the hub starts polling it immediately. Beszel is
# an observability add-on: any failure below only leaves operator metrics
# disabled (internal/config Observability.MetricsEnabled() requires both a base
# URL and this credential) rather than failing the install.
BESZEL_CREDENTIAL_SECRETREF=""
# The whole chain runs inside one subshell so ANY step failing (script staging,
# a slow/never-ready pod, the superuser CLI, secret creation) is caught by the
# single `if (...)` below instead of tripping this script's `set -euo
# pipefail` and aborting the entire platform install over an add-on.
if (
  mkdir -p /opt/cyclopus/beszel &&
  if [ "$BUNDLE_MODE" = 1 ]; then
    REGISTER_NODE_SRC="$BUNDLE_DIR/observability/register_beszel_node.sh"
    [ -f "$REGISTER_NODE_SRC" ] || exit 1
    cp "$REGISTER_NODE_SRC" /opt/cyclopus/beszel/register_node.sh
  else
    dl "$RAW_BASE/infra/observability/register_beszel_node.sh" -o /opt/cyclopus/beszel/register_node.sh
  fi &&
  chmod 0755 /opt/cyclopus/beszel/register_node.sh &&
  $KUBECTL -n "$CYCLOPUS_NAMESPACE" rollout status deploy/beszel --timeout=3m >/dev/null 2>&1 &&
  BESZEL_POD=$($KUBECTL -n "$CYCLOPUS_NAMESPACE" get pods -l app=beszel -o jsonpath='{.items[0].metadata.name}') &&
  [ -n "$BESZEL_POD" ] &&
  {
    # A few retries: the pod can report Ready slightly before PocketBase has
    # finished its local database migration, which the superuser CLI needs.
    _bz_attempt=0
    _bz_ok=0
    while [ $_bz_attempt -lt 10 ]; do
      if $KUBECTL -n "$CYCLOPUS_NAMESPACE" exec "$BESZEL_POD" -- /beszel superuser upsert "$BESZEL_ADMIN_EMAIL" "$BESZEL_ADMIN_PASSWORD" >/dev/null 2>&1; then
        _bz_ok=1
        break
      fi
      _bz_attempt=$((_bz_attempt + 1)); sleep 3
    done
    [ "$_bz_ok" = 1 ]
  } &&
  $KUBECTL -n "$CYCLOPUS_NAMESPACE" create secret generic beszel-auth \
    --from-literal=credential="${BESZEL_ADMIN_EMAIL}:${BESZEL_ADMIN_PASSWORD}" \
    --dry-run=client -o yaml | $KUBECTL apply -f -
); then
  BESZEL_CREDENTIAL_SECRETREF="k8s-secret://${CYCLOPUS_NAMESPACE}/beszel-auth#credential"
  /opt/cyclopus/beszel/register_node.sh "$(hostname)" "$NODE_IP" "$CYCLOPUS_NAMESPACE" ||
    warn "Beszel node registration failed (non-fatal); rerun /opt/cyclopus/beszel/register_node.sh $(hostname) $NODE_IP once Beszel is reachable"
else
  warn "Beszel superuser/credential provisioning failed (non-fatal); operator metrics stay disabled — rerun the installer, or provision manually and wire secrets.beszelCredential via helm upgrade"
fi

# Install Cyclopus. #2: secrets go in a 0600 values file (umask), never in argv.
log "Installing Cyclopus..."
DB_URL="postgres://cyclopus:${DB_PASSWORD}@${NODE_IP}:5432/cyclopus?sslmode=require"

# Chart source: bundled tgz, else the OCI registry the release workflow
# publishes to (oci://ghcr.io/shepard-labs/charts/cyclopus; the chart version
# always equals the release tag minus the leading v).
if [ "$BUNDLE_MODE" = 1 ]; then
  CYCLOPUS_CHART="$BUNDLE_DIR/charts/cyclopus-${CHART_VER}.tgz"
  [ -f "$CYCLOPUS_CHART" ] || fail "bundle is missing charts/cyclopus-${CHART_VER}.tgz"
  CYCLOPUS_CHART_VER_ARGS=()
else
  CYCLOPUS_CHART="oci://ghcr.io/shepard-labs/charts/cyclopus"
  CYCLOPUS_CHART_VER_ARGS=(--version "$CHART_VER")
fi

VALUES_FILE=$( (umask 0077; mktemp) )
TMP_FILES="$TMP_FILES $VALUES_FILE"
cat > "$VALUES_FILE" <<EOF
image:
  repository: $(echo "$CYCLOPUS_IMAGE" | cut -d: -f1)
  tag: "$(echo "$CYCLOPUS_IMAGE" | cut -d: -f2)"
  pullPolicy: IfNotPresent
secrets:
  databaseUrl: "${DB_URL}"
  authTokenPepper: "${AUTH_TOKEN_PEPPER}"
  authBootstrapSecret: "${AUTH_BOOTSTRAP_SECRET}"
  # NOT the raw credential: the API expects a k8s-secret reference
  # (internal/secretref, "k8s-secret://<namespace>/<name>#<key>") and resolves
  # it at boot; the referenced key holds the raw "user:password" value.
  openobserveCredential: "k8s-secret://${CYCLOPUS_NAMESPACE}/openobserve-auth#credential"
  # Same k8s-secret reference shape as openobserveCredential above. Empty when
  # Beszel superuser provisioning (just above) didn't succeed — MetricsEnabled()
  # then correctly reports metrics as disabled rather than reaching Beszel
  # unauthenticated.
  beszelCredential: "${BESZEL_CREDENTIAL_SECRETREF}"
config:
  kubernetes:
    previewHostnameSuffix: "${HOSTNAME_SUFFIX}"
    customDomainCnameTarget: "${HOSTNAME_SUFFIX}"
    certManagerClusterIssuer: "letsencrypt-production"
  observability:
    # In-cluster ClusterIP Services, NOT public ingresses (§8: "never exposed
    # directly"). Config validation accepts plaintext http:// only for a
    # Kubernetes .svc address (internal/adapters/observability/url.go); this
    # traffic never leaves the cluster network and network-policy.yaml confines
    # it to this namespace.
    openobserveBaseUrl: "http://openobserve.${CYCLOPUS_NAMESPACE}.svc:5080"
    # Org/stream "default": where the Collector's otlphttp exporter lands
    # telemetry (infra/observability/otel-collector.yaml).
    openobserveOrganization: "default"
    openobserveLogStream: "default"
    openobserveTraceStream: "default"
    beszelBaseUrl: "http://beszel.${CYCLOPUS_NAMESPACE}.svc:8090"
ingress:
  enabled: true
  className: traefik
  host: "${CONSOLE_HOST}"
  clusterIssuer: "letsencrypt-production"
EOF

if ! helm_deployed "$CYCLOPUS_RELEASE" "$CYCLOPUS_NAMESPACE"; then
  helm install "$CYCLOPUS_RELEASE" "$CYCLOPUS_CHART" \
    "${CYCLOPUS_CHART_VER_ARGS[@]}" \
    --namespace "$CYCLOPUS_NAMESPACE" --create-namespace \
    -f "$VALUES_FILE" \
    --atomic --wait --timeout 5m
else
  helm upgrade "$CYCLOPUS_RELEASE" "$CYCLOPUS_CHART" \
    "${CYCLOPUS_CHART_VER_ARGS[@]}" \
    --namespace "$CYCLOPUS_NAMESPACE" \
    -f "$VALUES_FILE" \
    --atomic --wait --timeout 5m
fi
rm -f "$VALUES_FILE"

log "Waiting for Cyclopus to become ready..."
i=0; while [ $i -lt 60 ]; do
  $KUBECTL -n "$CYCLOPUS_NAMESPACE" rollout status "deploy/$CYCLOPUS_RELEASE" --timeout=10s >/dev/null 2>&1 && break
  i=$((i+1)); [ $i -lt 60 ] || fail "Cyclopus did not become ready"; sleep 2
done

# Observability was applied before the Cyclopus install; verify readiness here.
# Non-fatal: an observability add-on that is slow to start must not abort the
# install now that the platform itself is up. The four rollouts run in
# parallel with one shared deadline instead of 4 sequential 3-minute waits.
log "Checking observability stack readiness (parallel, non-fatal)..."
OBS_PIDS=""
for OBS_TARGET in deploy/openobserve deploy/beszel daemonset/opentelemetry-collector daemonset/beszel-agent; do
  ( $KUBECTL -n "$CYCLOPUS_NAMESPACE" rollout status "$OBS_TARGET" --timeout=3m >/dev/null 2>&1 || \
    warn "observability component ${OBS_TARGET#*/} not ready yet (non-fatal)" ) &
  OBS_PIDS="$OBS_PIDS $!"
done
for _p in $OBS_PIDS; do wait "$_p" || true; done

# --- WAL-G Postgres backups (§9.1) -------------------------------------------
# Stage the backup toolkit UNCONDITIONALLY (even without S3 creds) so the
# post-install wiring path (/opt/cyclopus/backup/configure_from_db.sh) needs no
# network. When all four WAL-G S3 inputs are set, additionally wire archiving +
# the daily base-backup timer now. Best-effort: failures warn and never fail
# the install — backups can always be wired post-install.
BACKUP_DIR=/opt/cyclopus/backup
WALG_WIRED=0
if (
  install -d -m 0755 "$BACKUP_DIR" &&
  if [ "$BUNDLE_MODE" = 1 ]; then
    cp "$BUNDLE_DIR"/backup/*.sh "$BACKUP_DIR/" && chmod 0755 "$BACKUP_DIR"/*.sh &&
    cp "$BUNDLE_DIR"/backup/cyclopus-walg-backup.service "$BUNDLE_DIR"/backup/cyclopus-walg-backup.timer "$BACKUP_DIR/" &&
    chmod 0644 "$BACKUP_DIR"/cyclopus-walg-backup.service "$BACKUP_DIR"/cyclopus-walg-backup.timer
  else
    for f in common.sh install_walg.sh backup.sh restore.sh configure_from_db.sh; do
      dl "$RAW_BASE/infra/backup/$f" -o "$BACKUP_DIR/$f" && chmod 0755 "$BACKUP_DIR/$f" || exit 1
    done &&
    for f in cyclopus-walg-backup.service cyclopus-walg-backup.timer; do
      dl "$RAW_BASE/infra/backup/$f" -o "$BACKUP_DIR/$f" && chmod 0644 "$BACKUP_DIR/$f" || exit 1
    done
  fi &&
  # common.sh's sibling fallback lets the staged scripts resolve their pins.
  cp "$RESOLVED_VERSIONS_ENV" "$BACKUP_DIR/versions.env" && chmod 0644 "$BACKUP_DIR/versions.env"
); then
  log "Backup toolkit staged at $BACKUP_DIR (scripts + systemd units + versions.env)"
else
  warn "backup toolkit staging incomplete (non-fatal); run infra/backup/configure_from_db.sh from a repo checkout instead"
fi

if [ -n "$WALG_S3_ENDPOINT" ] && [ -n "$WALG_S3_BUCKET" ] && [ -n "$AWS_ACCESS_KEY_ID" ] && [ -n "$AWS_SECRET_ACCESS_KEY" ]; then
  log "WAL-G S3 env present: wiring Postgres archiving + daily base backups..."
  WALG_S3_REGION=${WALG_S3_REGION:-us-east-1}
  WALG_S3_PREFIX_SUBPATH=${WALG_S3_PREFIX_SUBPATH:-postgres_backup}
  # walg.env is written by common.sh's write_walg_env (single source of truth:
  # charset validation + layout + 0600/0700 perms). The staged common.sh
  # resolves its pins via the sibling versions.env copied above; the four
  # walg_env-required vars are exported so the sourced functions see them.
  export WALG_S3_ENDPOINT WALG_S3_BUCKET WALG_S3_REGION WALG_S3_PREFIX_SUBPATH
  export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
  if (
    # shellcheck disable=SC1091
    . "$BACKUP_DIR/common.sh" &&
    require_root &&
    walg_env &&
    write_walg_env /etc/cyclopus/walg.env &&
    if [ "$BUNDLE_MODE" = 1 ]; then
      # Bundled wal-g tarball; checksum verification happens inside
      # install_walg.sh regardless of the local source (WALG_LOCAL_DIR).
      WALG_LOCAL_DIR="$BUNDLE_DIR/walg" sh "$BACKUP_DIR/install_walg.sh"
    else
      sh "$BACKUP_DIR/install_walg.sh"
    fi &&
    # archive_mode/wal_level are postmaster-start settings: full restart only
    # when archiving is not already active, otherwise a reload suffices
    # (idempotent installer re-runs do not bounce the DB).
    if [ "$(sudo -u postgres psql -d cyclopus -XAtqc 'SHOW archive_mode;' 2>/dev/null || true)" = on ]; then
      systemctl reload postgresql || true
    else
      systemctl restart postgresql || exit 1
    fi &&
    sed "s|@INSTALL_DIR@|$BACKUP_DIR|g" "$BACKUP_DIR/cyclopus-walg-backup.service" > /etc/systemd/system/cyclopus-walg-backup.service &&
    sed "s|@INSTALL_DIR@|$BACKUP_DIR|g" "$BACKUP_DIR/cyclopus-walg-backup.timer" > /etc/systemd/system/cyclopus-walg-backup.timer &&
    chmod 0644 /etc/systemd/system/cyclopus-walg-backup.service /etc/systemd/system/cyclopus-walg-backup.timer &&
    systemctl daemon-reload &&
    systemctl enable --now cyclopus-walg-backup.timer &&
    systemctl is-enabled cyclopus-walg-backup.timer | grep -q enabled
  ); then
    WALG_WIRED=1
    log "WAL-G archiving + daily base backups enabled (cyclopus-walg-backup.timer)"
  else
    warn "WAL-G backup wiring failed (non-fatal). Create a storage destination, then run: $BACKUP_DIR/configure_from_db.sh (see infra/backup/README.md)"
  fi
fi

if [ -n "$BESZEL_CREDENTIAL_SECRETREF" ]; then
  BESZEL_STATUS_NOTE="Auto-provisioned; Cyclopus already authenticates as this account for operator metrics."
else
  BESZEL_STATUS_NOTE="WARNING: automatic provisioning failed; operator metrics are disabled. Retry: kubectl -n ${CYCLOPUS_NAMESPACE} exec deploy/beszel -- /beszel superuser upsert ${BESZEL_ADMIN_EMAIL} '<password above>', then kubectl -n ${CYCLOPUS_NAMESPACE} create secret generic beszel-auth --from-literal=credential='${BESZEL_ADMIN_EMAIL}:<password above>' and helm upgrade ${CYCLOPUS_RELEASE} -n ${CYCLOPUS_NAMESPACE} --reuse-values --set secrets.beszelCredential=k8s-secret://${CYCLOPUS_NAMESPACE}/beszel-auth#credential <chart>"
fi

# Write secrets to a 0600 file created with a tight umask (#6) — no stdout leak (#2).
( umask 0077; cat > "$SECRETS_FILE" <<EOF
Cyclopus Installation Secrets
Generated: $(date -u +"%Y-%m-%d %H:%M:%S UTC")
======================================

Console (UI + API): https://${CONSOLE_HOST}

Bootstrap Secret (use once to claim admin):
${AUTH_BOOTSTRAP_SECRET}

Token Pepper (HMAC key for all tokens):
${AUTH_TOKEN_PEPPER}

Database Password (PostgreSQL cyclopus user):
${DB_PASSWORD}

OpenObserve (logs/traces backend — no public URL by design, §8)
  Read logs and traces in the Cyclopus console; the API queries this backend
  server-side and scopes every query to your org.
  In-cluster only: http://openobserve.${CYCLOPUS_NAMESPACE}.svc:5080
  To reach its own UI, port-forward from a trusted machine:
    kubectl -n ${CYCLOPUS_NAMESPACE} port-forward svc/openobserve 5080:5080
  User: admin@cyclopus.local
  Password: ${OO_PASSWORD}

Beszel (metrics backend — no public URL by design, browsing it is never required)
  In-cluster only: http://beszel.${CYCLOPUS_NAMESPACE}.svc:8090
  To reach its own UI, port-forward from a trusted machine:
    kubectl -n ${CYCLOPUS_NAMESPACE} port-forward svc/beszel 8090:8090
  Admin login: ${BESZEL_ADMIN_EMAIL} / ${BESZEL_ADMIN_PASSWORD}
  ${BESZEL_STATUS_NOTE}
  This node (${NODE_IP}:45876) was auto-registered as a Beszel system.
  To register additional nodes later (no UI needed):
    /opt/cyclopus/beszel/register_node.sh <node-name> <node-ip>
  Hub keypair (generated at install; private key is NOT in the cluster):
    Public key (already given to the agents via the beszel-agent-key secret):
    ${BESZEL_PUBLIC_KEY}
    Private key (import into the hub as /beszel_data/id_ed25519 and restart it):
${BESZEL_PRIVATE_KEY}

======================================
IMPORTANT: Save these to your password manager, then delete this file.
EOF
)

INSTALL_OK=1

log ""
log "=========================================="
log "Cyclopus Installation Complete!"
log "=========================================="
log ""
log "📍 Console (UI + API): https://${CONSOLE_HOST}"
log "   (TLS via Let's Encrypt production; first cert may take ~60s to issue)"
log ""
log "🔐 Secrets saved to: $SECRETS_FILE  (chmod 0600)"
log "   View:   cat $SECRETS_FILE"
log "   Delete: rm $SECRETS_FILE   (after saving to a password manager)"
log ""
log "Next steps:"
log "  1. cat $SECRETS_FILE"
log "  2. Open https://${CONSOLE_HOST} and claim the admin account with the bootstrap secret"
log "  3. kubectl -n $CYCLOPUS_NAMESPACE get pods"
log "  4. Logs, traces, and metrics: read them in the Cyclopus console — OpenObserve and"
log "     Beszel are in-cluster only by design (§8) and have no public URL."
log "  5. Beszel admin + operator metrics were auto-provisioned; to reach either backend UI"
log "     directly: kubectl -n $CYCLOPUS_NAMESPACE port-forward svc/openobserve 5080:5080 (or svc/beszel 8090:8090)"
log "     - To register additional nodes later: /opt/cyclopus/beszel/register_node.sh <name> <ip>"
log "     - Hub public key + private key (hub import), and troubleshooting if provisioning failed: $SECRETS_FILE"
if [ "$CYCLOPUS_CONTROL_PLANE_TAINT" != "true" ]; then
  log "  6. Optional (multi-node only): k3s kubectl taint nodes <node> cyclopus.app/role=control-plane:NoSchedule"
fi
if [ "$WALG_WIRED" = 1 ]; then
  log "  7. Backups: WAL-G archiving ACTIVE; daily base backups via cyclopus-walg-backup.timer"
  log "     (systemctl list-timers cyclopus-walg-backup.timer; manual run: $BACKUP_DIR/backup.sh)"
else
  log "  7. Backups: create a storage destination (Settings/API → storage_destinations, purpose postgres_backup),"
  log "     then on this host: $BACKUP_DIR/configure_from_db.sh (see docs/decisions + infra/backup/README.md)"
fi
log ""
log "=========================================="
