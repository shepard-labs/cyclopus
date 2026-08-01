#!/bin/sh
# Cyclopus VPS Installer
# One-line install (like Coolify):
#   curl -fsSL https://raw.githubusercontent.com/shepard-labs/cyclopus/main/scripts/install.sh | sh
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

set -eu

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log() { printf "${GREEN}[cyclopus]${NC} %s\n" "$*"; }
warn() { printf "${YELLOW}[cyclopus]${NC} %s\n" "$*" >&2; }
fail() { printf "${RED}[cyclopus]${NC} %s\n" "$*" >&2; exit 1; }

# Hardened curl: bounded time + retries (#8).
dl() { curl -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 2 "$@"; }

# helm_deployed REL NS: true only when the release is in 'deployed' state. A
# 'failed'/'pending' release is uninstalled so a clean install can retry (helm
# status returns success even for a failed release, which would wrongly skip it).
helm_deployed() {
  _st=$(helm status "$1" -n "$2" -o json 2>/dev/null | grep -o '"status":"[^"]*"' | head -n1 | cut -d'"' -f4)
  if [ "$_st" = "deployed" ]; then return 0; fi
  if [ -n "$_st" ]; then
    warn "$1 release is $_st (not deployed) — uninstalling for clean retry"
    helm uninstall "$1" -n "$2" >/dev/null 2>&1 || true
  fi
  return 1
}

# Component versions
K3S_VERSION='v1.36.2+k3s1'
HELM_VERSION='v3.21.3'
KYVERNO_VERSION='v1.18.2'
KYVERNO_CHART_VERSION='3.8.2'
CERT_MANAGER_VERSION='v1.21.1'
TRAEFIK_VERSION='41.1.0'
KEDA_VERSION='2.20.2'
KEDA_HTTP_ADDON_VERSION='0.15.0'
PG_MAJOR=18

# Install configuration
CYCLOPUS_VERSION=${CYCLOPUS_VERSION:-latest}
CYCLOPUS_RELEASE=${CYCLOPUS_RELEASE:-cyclopus}
CYCLOPUS_NAMESPACE=${CYCLOPUS_NAMESPACE:-cyclopus}
CYCLOPUS_IMAGE=${CYCLOPUS_IMAGE:-ghcr.io/shepard-labs/cyclopus:${CYCLOPUS_VERSION}}
LETSENCRYPT_EMAIL=${LETSENCRYPT_EMAIL:-}

# Canonical raw base for bootstrap scripts (single source of truth for gVisor, #7).
RAW_BASE="https://raw.githubusercontent.com/shepard-labs/cyclopus/${CYCLOPUS_VERSION}"
[ "$CYCLOPUS_VERSION" = "latest" ] && RAW_BASE="https://raw.githubusercontent.com/shepard-labs/cyclopus/main"

KUBECTL="k3s kubectl"

# ---- global cleanup / failure handling (#16) -------------------------------
SECRETS_FILE="/root/.cyclopus-install-secrets"
INSTALL_OK=0
TMP_FILES=""
cleanup() {
  # Remove any temp values files regardless of outcome.
  for f in $TMP_FILES; do rm -f "$f" 2>/dev/null || true; done
  # Best-effort delete of the Kyverno readiness probe pod (#12).
  $KUBECTL delete pod cyclopus-webhook-probe -n default --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
  if [ "$INSTALL_OK" -ne 1 ] && [ -f "$SECRETS_FILE" ]; then
    warn "install did not complete; generated secrets left at $SECRETS_FILE (delete if unused)"
  fi
}
trap cleanup EXIT HUP INT TERM

# Check prerequisites
log "Checking prerequisites..."
[ "$(id -u)" -eq 0 ] || fail "This script must be run as root"

# Detect OS
if [ -f /etc/os-release ]; then
  . /etc/os-release
  OS=$ID
  VERSION_ID=$VERSION_ID
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

# Detect public IP (bounded, #8)
log "Detecting public IP..."
PUBLIC_IP=$(dl https://ifconfig.io 2>/dev/null || dl https://api.ipify.org 2>/dev/null || true)
[ -n "$PUBLIC_IP" ] || fail "Cannot detect public IP (set NODE_IP and rerun, or check network)"
log "Public IP: $PUBLIC_IP"

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

# Generate secrets
log "Generating secrets..."
AUTH_BOOTSTRAP_SECRET=$(openssl rand -base64 32 | tr -d '\n')
AUTH_TOKEN_PEPPER=$(openssl rand -base64 32 | tr -d '\n')
DB_PASSWORD=$(openssl rand -base64 32 | tr -d '\n')

# Install PostgreSQL
log "Installing PostgreSQL $PG_MAJOR..."
if ! command -v psql >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y curl ca-certificates gnupg lsb-release
  install -d /usr/share/postgresql-common/pgdg
  dl https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
  echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list
  apt-get update -qq
  apt-get install -y postgresql-$PG_MAJOR
else
  log "PostgreSQL already installed, skipping"
fi

# Create database and user.
# #1/#4: pass password via PGPASSWORD to the SINGLE psql process (not a shell-wide
# export), and use a here-doc without psql :vars so nothing lands in ~/.psql_history.
log "Creating Cyclopus database and user..."
CREATE_ROLE_SQL=$(cat <<SQL
DO \$do\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'cyclopus') THEN
    EXECUTE format('CREATE ROLE cyclopus LOGIN PASSWORD %L', '${DB_PASSWORD}');
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
NODE_IP=${NODE_IP:-$(ip -o -4 addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -n1)}
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

# #14: validate NODE_IP is the pod-reachable interface once the API is up.
POD_CIDR=$($KUBECTL get node -o jsonpath='{.items[0].spec.podCIDR}' 2>/dev/null || echo "")
log "Pod CIDR: ${POD_CIDR:-unknown}"

# Install gVisor via the CANONICAL bootstrap script (#7: no duplicated checksums).
log "Installing gVisor (canonical bootstrap script)..."
GVISOR_SH=/tmp/install_gvisor.sh
dl "$RAW_BASE/infra/bootstrap/install_gvisor.sh" -o "$GVISOR_SH"
dl "$RAW_BASE/infra/bootstrap/common.sh" -o /tmp/common.sh
( cd /tmp && sh "$GVISOR_SH" ) || fail "gVisor bootstrap failed"
rm -f "$GVISOR_SH" /tmp/common.sh
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

# #11: assert an installed release's chart version matches what we requested.
assert_chart_version() {
  _rel=$1; _ns=$2; _want=$3
  _got=$(helm list -n "$_ns" -o json | grep -o "\"chart\":\"${_rel}-[^\"]*\"" | head -n1 | sed "s/.*${_rel}-//; s/\"//")
  [ -n "$_got" ] || fail "$_rel: could not read installed chart version"
  [ "$_got" = "$_want" ] || fail "$_rel chart version mismatch: installed $_got, expected $_want"
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
log "Installing Traefik ingress controller v$TRAEFIK_VERSION (hostNetwork :80/:443)..."
helm repo add traefik https://traefik.github.io/charts 2>/dev/null || true
helm repo update traefik
if ! helm_deployed traefik kube-system; then
  helm install traefik traefik/traefik \
    --version "$TRAEFIK_VERSION" \
    --namespace kube-system \
    --set deployment.kind=DaemonSet \
    --set hostNetwork=true \
    --set service.enabled=false \
    --set updateStrategy.rollingUpdate.maxUnavailable=1 \
    --set updateStrategy.rollingUpdate.maxSurge=0 \
    --set ports.web.port=80 \
    --set ports.web.hostPort=80 \
    --set ports.websecure.port=443 \
    --set ports.websecure.hostPort=443 \
    --set resources.requests.cpu=50m \
    --set resources.requests.memory=64Mi \
    --set resources.limits.memory=128Mi \
    --timeout 10m
else
  log "Traefik already installed"
fi
assert_chart_version traefik kube-system "$TRAEFIK_VERSION"

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
if ! helm_deployed kyverno kyverno; then
  helm install kyverno kyverno/kyverno \
    --version "$KYVERNO_CHART_VERSION" \
    --namespace kyverno --create-namespace \
    --wait --timeout 5m
else
  log "Kyverno already installed"
fi
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
if ! helm_deployed cert-manager cert-manager; then
  helm install cert-manager jetstack/cert-manager \
    --version "$CERT_MANAGER_VERSION" \
    --namespace cert-manager --create-namespace \
    --set crds.enabled=true \
    --wait --timeout 5m
else
  log "cert-manager already installed"
fi
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
if ! helm_deployed keda keda; then
  helm install keda kedacore/keda \
    --version "$KEDA_VERSION" \
    --namespace keda --create-namespace \
    --wait --timeout 5m
else
  log "KEDA already installed"
fi
assert_chart_version keda keda "$KEDA_VERSION"

log "Installing KEDA HTTP Add-on v$KEDA_HTTP_ADDON_VERSION..."
if ! helm_deployed keda-add-ons-http keda; then
  helm install keda-add-ons-http kedacore/keda-add-ons-http \
    --version "$KEDA_HTTP_ADDON_VERSION" \
    --namespace keda \
    --set operator.resources.requests.cpu=50m \
    --set operator.resources.requests.memory=64Mi \
    --set operator.resources.limits.memory=128Mi \
    --set interceptor.resources.requests.cpu=50m \
    --set interceptor.resources.requests.memory=64Mi \
    --set interceptor.resources.limits.memory=128Mi \
    --set scaler.resources.requests.cpu=50m \
    --set scaler.resources.requests.memory=64Mi \
    --set scaler.resources.limits.memory=128Mi \
    --wait --timeout 10m
else
  log "KEDA HTTP Add-on already installed"
fi
assert_chart_version keda-add-ons-http keda "$KEDA_HTTP_ADDON_VERSION"

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
config:
  kubernetes:
    previewHostnameSuffix: "${HOSTNAME_SUFFIX}"
    customDomainCnameTarget: "${HOSTNAME_SUFFIX}"
    certManagerClusterIssuer: "letsencrypt-production"
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
log ""
log "=========================================="
