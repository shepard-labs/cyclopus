#!/bin/bash
# Cyclopus VPS Installer
# One-line install (like Coolify):
#   curl -fsSL https://raw.githubusercontent.com/shepard-labs/cyclopus/main/scripts/install.sh | bash
#
# Zero-config install for Ubuntu 24.04 / Debian 12 on a fresh VPS.
# Detects public IP, configures sslip.io hostnames, generates secrets,
# installs PostgreSQL + k3s + gVisor + Kyverno + Traefik + cert-manager + KEDA + Cyclopus.
#
# TLS: cert-manager issues Let's Encrypt PRODUCTION certificates automatically for
# every app/preview/custom-domain Ingress and the control-plane console. Traefik is
# the ingress controller; TLS terminates at Traefik (the pod stays plaintext inside
# the cluster). No hand-rolled ACME.
#
# Optional env vars:
#   CYCLOPUS_VERSION     image/chart version (default: latest)
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
CYCLOPUS_RELEASE=${CYCLOPUS_RELEASE:-cyclopus}
CYCLOPUS_NAMESPACE=${CYCLOPUS_NAMESPACE:-cyclopus}
CYCLOPUS_IMAGE=${CYCLOPUS_IMAGE:-ghcr.io/shepard-labs/cyclopus:${CYCLOPUS_VERSION}}
LETSENCRYPT_EMAIL=${LETSENCRYPT_EMAIL:-}
CYCLOPUS_CONTROL_PLANE_TAINT=${CYCLOPUS_CONTROL_PLANE_TAINT:-false}
# Optional WAL-G backup wiring (see header docs; all four must be set to wire).
WALG_S3_ENDPOINT=${WALG_S3_ENDPOINT:-}
WALG_S3_BUCKET=${WALG_S3_BUCKET:-}
WALG_S3_REGION=${WALG_S3_REGION:-}
AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-}
AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-}

# Canonical raw base for bootstrap scripts (single source of truth for gVisor, #7).
RAW_BASE="https://raw.githubusercontent.com/shepard-labs/cyclopus/${CYCLOPUS_VERSION}"
[ "$CYCLOPUS_VERSION" = "latest" ] && RAW_BASE="https://raw.githubusercontent.com/shepard-labs/cyclopus/main"

KUBECTL="k3s kubectl"

# ---- global cleanup / failure handling (#16) -------------------------------
SECRETS_FILE="/root/.cyclopus-install-secrets"
STATE_FILE="/root/.cyclopus-install.env"
INSTALL_OK=0
TMP_FILES=""
cleanup() {
  # Remove any temp values files regardless of outcome.
  for f in $TMP_FILES; do rm -f "$f" 2>/dev/null || true; done
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
  # Sourcing hardening: every line must be a comment or a KEY="base64" pair
  # (both BESZEL_* keys are base64-encoded, so one charset whitelist covers
  # everything). Anything else — including $( )/` ` injection that would run
  # at source time — aborts before sourcing. Also turns a set -u abort on
  # garbage content into a clear message.
  if grep -vqE '^(#.*|(AUTH_BOOTSTRAP_SECRET|AUTH_TOKEN_PEPPER|DB_PASSWORD|OO_PASSWORD|BESZEL_PUBLIC_KEY|BESZEL_PRIVATE_KEY)="[A-Za-z0-9+/=]*")$' "$STATE_FILE"; then
    fail "STATE_FILE $STATE_FILE contains unexpected content (possible tampering); not sourcing"
  fi
  # shellcheck disable=SC1090
  . "$STATE_FILE"
  _missing=""
  for _v in AUTH_BOOTSTRAP_SECRET AUTH_TOKEN_PEPPER DB_PASSWORD OO_PASSWORD BESZEL_PUBLIC_KEY BESZEL_PRIVATE_KEY; do
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
  DB_PASSWORD=$(openssl rand -base64 32 | tr -d '\n')
  OO_PASSWORD=$(openssl rand -base64 24 | tr -d '\n' | tr '/+' 'xy')
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
BESZEL_PUBLIC_KEY="$(printf '%s' "$BESZEL_PUBLIC_KEY" | base64 | tr -d '\n')"
BESZEL_PRIVATE_KEY="$(printf '%s' "$BESZEL_PRIVATE_KEY" | base64 | tr -d '\n')"
EOF
  )
  log "Generated new secrets (saved to $STATE_FILE)"
fi

# Install PostgreSQL
log "Installing PostgreSQL $PG_MAJOR..."
if ! command -v psql >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y curl ca-certificates gnupg lsb-release openssh-client
  install -d /usr/share/postgresql-common/pgdg
  dl https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
  echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list
  apt-get update -qq
  apt-get install -y postgresql-"$PG_MAJOR"
else
  log "PostgreSQL already installed, skipping"
fi

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
systemctl restart postgresql
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

# Install k3s with hardening. #10: download installer, then execute (no curl|sh).
log "Installing k3s $K3S_VERSION with hardening..."
K3S_INSTALLER=/tmp/k3s-install.sh
dl https://get.k3s.io -o "$K3S_INSTALLER"
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
rm -f "$K3S_INSTALLER"
systemctl enable --now k3s

log "Waiting for k3s API..."
i=0; while [ $i -lt 60 ]; do
  $KUBECTL get --raw=/readyz >/dev/null 2>&1 && break
  i=$((i+1)); [ $i -lt 60 ] || fail "k3s API did not become ready"; sleep 2
done
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
GVISOR_SH=/tmp/install_gvisor.sh
dl "$RAW_BASE/infra/bootstrap/install_gvisor.sh" -o "$GVISOR_SH"
dl "$RAW_BASE/infra/bootstrap/common.sh" -o /tmp/common.sh
# Reuse the versions.env resolved above so local-checkout pins are honored.
cp "$RESOLVED_VERSIONS_ENV" /tmp/versions.env
TMP_FILES="$TMP_FILES /tmp/versions.env"
( cd /tmp && CYCLOPUS_VERSIONS_ENV=/tmp/versions.env sh "$GVISOR_SH" ) || fail "gVisor bootstrap failed"
rm -f "$GVISOR_SH" /tmp/common.sh /tmp/versions.env
systemctl restart k3s
i=0; while [ $i -lt 60 ]; do
  $KUBECTL get --raw=/readyz >/dev/null 2>&1 && break
  i=$((i+1)); [ $i -lt 60 ] || fail "k3s API not ready after gVisor"; sleep 2
done

log "Registering gVisor RuntimeClass..."
$KUBECTL apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
$KUBECTL get runtimeclass gvisor >/dev/null 2>&1 || fail "gvisor RuntimeClass missing"

# Install Helm (#8 bounded downloads)
log "Installing Helm $HELM_VERSION..."
if ! command -v helm >/dev/null 2>&1; then
  HELM_TAR="helm-${HELM_VERSION}-linux-${GO_ARCH}.tar.gz"
  dl "https://get.helm.sh/$HELM_TAR" -o /tmp/helm.tar.gz
  dl "https://get.helm.sh/${HELM_TAR}.sha256sum" -o /tmp/helm.sha256
  echo "$(awk '{print $1}' /tmp/helm.sha256)  /tmp/helm.tar.gz" | sha256sum -c - || fail "Helm checksum mismatch"
  tar -C /tmp -xzf /tmp/helm.tar.gz
  install -m 0755 /tmp/linux-${GO_ARCH}/helm /usr/local/bin/helm
  rm -rf /tmp/helm.tar.gz /tmp/helm.sha256 /tmp/linux-${GO_ARCH}
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

# Install Traefik as a host-network DaemonSet binding ports 80/443 directly on the
# node. This is REQUIRED for Let's Encrypt HTTP-01: the ACME server connects to
# http://<host>:80/.well-known/acme-challenge/... — a NodePort (30080) is not
# reachable by Let's Encrypt, so certs would never issue. hostNetwork puts Traefik
# on the node's :80/:443 so both ACME and normal traffic work.
log "Installing Traefik ingress controller v$TRAEFIK_CHART_VERSION (hostNetwork :80/:443)..."
helm repo add traefik https://traefik.github.io/charts 2>/dev/null || true
helm repo update traefik
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
_rc=0
converge_chart traefik kube-system "$TRAEFIK_CHART_VERSION" || _rc=$?
case "$_rc" in
  1)
    helm install traefik traefik/traefik \
      --version "$TRAEFIK_CHART_VERSION" \
      --namespace kube-system \
      "${TRAEFIK_SET_ARGS[@]}" \
      --timeout 10m
    ;;
  2)
    log "Traefik chart not at pin — upgrading to $TRAEFIK_CHART_VERSION"
    helm upgrade traefik traefik/traefik \
      --version "$TRAEFIK_CHART_VERSION" \
      --namespace kube-system \
      "${TRAEFIK_SET_ARGS[@]}" \
      --cleanup-on-fail --timeout 10m
    ;;
esac
assert_chart_version traefik kube-system "$TRAEFIK_CHART_VERSION"

# #13: verify Traefik is actually reachable on host :80 (ACME HTTP-01 depends on it).
i=0; while [ $i -lt 60 ]; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:80/ 2>/dev/null || echo 000)
  # Any HTTP response (404 from Traefik with no route is fine) means it's listening.
  [ "$code" != "000" ] && break
  i=$((i+1)); [ $i -lt 60 ] || fail "Traefik is not listening on host :80 (ACME HTTP-01 would fail)"; sleep 2
done
log "Traefik listening on host :80/:443 (ACME HTTP-01 reachable)"

# Install Kyverno
log "Installing Kyverno $KYVERNO_VERSION..."
helm repo add kyverno https://kyverno.github.io/kyverno/ 2>/dev/null || true
helm repo update kyverno
_rc=0
converge_chart kyverno kyverno "$KYVERNO_CHART_VERSION" || _rc=$?
case "$_rc" in
  1)
    helm install kyverno kyverno/kyverno \
      --version "$KYVERNO_CHART_VERSION" \
      --namespace kyverno --create-namespace \
      --wait --timeout 5m
    ;;
  2)
    log "Kyverno chart not at pin — upgrading to $KYVERNO_CHART_VERSION"
    # Upgrade passes identical value flags as install by design (no --reuse-values).
    # Note: helm never upgrades the chart's crds/ directory — a pin bump with CRD
    # schema changes needs a manual CRD apply (out of scope).
    helm upgrade kyverno kyverno/kyverno \
      --version "$KYVERNO_CHART_VERSION" \
      --namespace kyverno \
      --cleanup-on-fail --wait --timeout 5m
    ;;
esac
assert_chart_version kyverno kyverno "$KYVERNO_CHART_VERSION"

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

# Install cert-manager
log "Installing cert-manager $CERT_MANAGER_VERSION..."
helm repo add jetstack https://charts.jetstack.io 2>/dev/null || true
helm repo update jetstack
_rc=0
converge_chart cert-manager cert-manager "$CERT_MANAGER_VERSION" || _rc=$?
case "$_rc" in
  1)
    helm install cert-manager jetstack/cert-manager \
      --version "$CERT_MANAGER_VERSION" \
      --namespace cert-manager --create-namespace \
      --set crds.enabled=true \
      --wait --timeout 5m
    ;;
  2)
    log "cert-manager chart not at pin — upgrading to $CERT_MANAGER_VERSION"
    # Upgrade passes identical value flags as install by design (no --reuse-values).
    helm upgrade cert-manager jetstack/cert-manager \
      --version "$CERT_MANAGER_VERSION" \
      --namespace cert-manager \
      --set crds.enabled=true \
      --cleanup-on-fail --wait --timeout 5m
    ;;
esac
assert_chart_version cert-manager cert-manager "$CERT_MANAGER_VERSION"

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

# Install KEDA
log "Installing KEDA v$KEDA_VERSION..."
helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
helm repo update kedacore
_rc=0
converge_chart keda keda "$KEDA_VERSION" || _rc=$?
case "$_rc" in
  1)
    helm install keda kedacore/keda \
      --version "$KEDA_VERSION" \
      --namespace keda --create-namespace \
      --wait --timeout 5m
    ;;
  2)
    log "KEDA chart not at pin — upgrading to $KEDA_VERSION"
    # Upgrade passes identical value flags as install by design (no --reuse-values).
    helm upgrade keda kedacore/keda \
      --version "$KEDA_VERSION" \
      --namespace keda \
      --cleanup-on-fail --wait --timeout 5m
    ;;
esac
assert_chart_version keda keda "$KEDA_VERSION"

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
_rc=0
converge_chart keda-add-ons-http keda "$KEDA_HTTP_ADDON_VERSION" || _rc=$?
case "$_rc" in
  1)
    helm install keda-add-ons-http kedacore/keda-add-ons-http \
      --version "$KEDA_HTTP_ADDON_VERSION" \
      --namespace keda \
      "${KEDA_HTTP_SET_ARGS[@]}" \
      --wait --timeout 10m
    ;;
  2)
    log "KEDA HTTP Add-on chart not at pin — upgrading to $KEDA_HTTP_ADDON_VERSION"
    helm upgrade keda-add-ons-http kedacore/keda-add-ons-http \
      --version "$KEDA_HTTP_ADDON_VERSION" \
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

# Install Cyclopus. #2: secrets go in a 0600 values file (umask), never in argv.
log "Installing Cyclopus..."
DB_URL="postgres://cyclopus:${DB_PASSWORD}@${NODE_IP}:5432/cyclopus?sslmode=require"

helm repo add cyclopus https://shepard-labs.github.io/charts 2>/dev/null || true
helm repo update cyclopus 2>/dev/null || true

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
  # Hub admin is created interactively post-install; token is added via the UI.
  beszelToken: ""
config:
  kubernetes:
    previewHostnameSuffix: "${HOSTNAME_SUFFIX}"
    customDomainCnameTarget: "${HOSTNAME_SUFFIX}"
    certManagerClusterIssuer: "letsencrypt-production"
  observability:
    # HTTPS is enforced by config validation (internal/config
    # Observability.validate rejects plaintext http://), so these point at the
    # Traefik-terminated ingresses, not the in-cluster http services.
    openobserveBaseUrl: "https://oo.${HOSTNAME_SUFFIX}"
    # Org/stream "default": where the Collector's otlphttp exporter lands
    # telemetry (infra/observability/otel-collector.yaml).
    openobserveOrganization: "default"
    openobserveLogStream: "default"
    openobserveTraceStream: "default"
    beszelBaseUrl: "https://beszel.${HOSTNAME_SUFFIX}"
ingress:
  enabled: true
  className: traefik
  host: "${CONSOLE_HOST}"
  clusterIssuer: "letsencrypt-production"
EOF

if ! helm_deployed "$CYCLOPUS_RELEASE" "$CYCLOPUS_NAMESPACE"; then
  helm install "$CYCLOPUS_RELEASE" cyclopus/cyclopus \
    --namespace "$CYCLOPUS_NAMESPACE" --create-namespace \
    -f "$VALUES_FILE" \
    --atomic --wait --timeout 5m
else
  helm upgrade "$CYCLOPUS_RELEASE" cyclopus/cyclopus \
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

# write_tls_ingress NAME HOST PORT: emit a Traefik Ingress (with cert-manager
# Let's Encrypt production TLS) routing HOST to service NAME:PORT.
write_tls_ingress() {
  cat <<EOF
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: $1
  namespace: ${CYCLOPUS_NAMESPACE}
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-production
spec:
  ingressClassName: traefik
  tls:
  - secretName: $1-tls
    hosts:
    - $2
  rules:
  - host: $2
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: $1
            port:
              number: $3
EOF
}

# Install the observability stack (OpenObserve + Beszel + OTel Collector +
# Beszel agents) into the Cyclopus namespace (§8). Raw manifests so the
# one-line install works without a repo checkout; ingress hosts derive from
# the same sslip.io suffix as the console. The manifests hardcode
# `namespace: cyclopus`; rewrite when CYCLOPUS_NAMESPACE differs — including
# the collector exporter DNS name and the filelog self-exclusion prefix,
# otherwise the collector tails its own logs (self-ingest loop).
log "Installing observability stack (OpenObserve + Beszel + OTel Collector + agents)..."
for OBS_NAME in openobserve beszel otel-collector beszel-agent; do
  OBS_MANIFEST=$(mktemp)
  TMP_FILES="$TMP_FILES $OBS_MANIFEST"
  dl "$RAW_BASE/infra/observability/$OBS_NAME.yaml" -o "$OBS_MANIFEST"
  if [ "$CYCLOPUS_NAMESPACE" != "cyclopus" ]; then
    sed -i "s/namespace: cyclopus/namespace: ${CYCLOPUS_NAMESPACE}/g; s/openobserve\.cyclopus\.svc/openobserve.${CYCLOPUS_NAMESPACE}.svc/g; s|/var/log/pods/cyclopus_|/var/log/pods/${CYCLOPUS_NAMESPACE}_|g" "$OBS_MANIFEST"
  fi
  $KUBECTL apply -f "$OBS_MANIFEST"
done
{
  write_tls_ingress openobserve "oo.${HOSTNAME_SUFFIX}" 5080
  write_tls_ingress beszel "beszel.${HOSTNAME_SUFFIX}" 8090
} | $KUBECTL apply -f -
# Non-fatal: an observability add-on that is slow to start must not abort the
# install now that the platform itself is up.
$KUBECTL -n "$CYCLOPUS_NAMESPACE" rollout status deploy/openobserve --timeout=3m || warn "observability component openobserve not ready yet (non-fatal)"
$KUBECTL -n "$CYCLOPUS_NAMESPACE" rollout status deploy/beszel --timeout=3m || warn "observability component beszel not ready yet (non-fatal)"
$KUBECTL -n "$CYCLOPUS_NAMESPACE" rollout status daemonset/opentelemetry-collector --timeout=3m || warn "observability component opentelemetry-collector not ready yet (non-fatal)"
$KUBECTL -n "$CYCLOPUS_NAMESPACE" rollout status daemonset/beszel-agent --timeout=3m || warn "observability component beszel-agent not ready yet (non-fatal)"

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
  for f in common.sh install_walg.sh backup.sh restore.sh configure_from_db.sh; do
    dl "$RAW_BASE/infra/backup/$f" -o "$BACKUP_DIR/$f" && chmod 0755 "$BACKUP_DIR/$f" || exit 1
  done &&
  for f in cyclopus-walg-backup.service cyclopus-walg-backup.timer; do
    dl "$RAW_BASE/infra/backup/$f" -o "$BACKUP_DIR/$f" && chmod 0644 "$BACKUP_DIR/$f" || exit 1
  done &&
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
    sh "$BACKUP_DIR/install_walg.sh" &&
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

OpenObserve (logs/traces UI): https://oo.${HOSTNAME_SUFFIX}
  User: admin@cyclopus.local
  Password: ${OO_PASSWORD}

Beszel (metrics UI): https://beszel.${HOSTNAME_SUFFIX}
  Create the admin account on first visit, then Add system for each node:
    ${NODE_IP}:45876   (hub connects TO agents on the node IP)
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
log "  4. OpenObserve: https://oo.${HOSTNAME_SUFFIX} (credentials in $SECRETS_FILE)"
log "  5. Beszel: https://beszel.${HOSTNAME_SUFFIX}"
log "     - Create the hub admin account, then Add system for each node: ${NODE_IP}:45876"
log "     - Hub public key + private key (hub import): $SECRETS_FILE"
log "     - Beszel API token for Cyclopus: add via the hub UI once the admin account exists"
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
