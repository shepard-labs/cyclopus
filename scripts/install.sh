#!/bin/sh
# Cyclopus VPS installer — single-node control-plane bootstrap.
#
# ONE-LINER:
#   curl -fsSL https://raw.githubusercontent.com/cyclopus-dev/cyclopus/main/scripts/install.sh | sh
#
# Set the required env vars before piping (see CONFIGURATION below).
# Run the script directly (not piped) for interactive prompts instead.
#
# CONFIGURATION (environment variables)
# ─────────────────────────────────────
# Required:
#   CYCLOPUS_DB_URL                  Postgres DSN
#                                    postgres://user:pass@host:5432/db?sslmode=require
#   CYCLOPUS_AUTH_BOOTSTRAP_SECRET   >=32 char random string
#                                    First request to /auth/bootstrap claims admin.
#                                    generate: openssl rand -hex 32
#   CYCLOPUS_AUTH_TOKEN_PEPPER       >=32 char random string
#                                    HMAC pepper for auth tokens.
#                                    WARNING: rotating this invalidates all sessions.
#                                    generate: openssl rand -hex 32
#   CYCLOPUS_PREVIEW_HOSTNAME_SUFFIX DNS suffix for preview environments
#                                    e.g. .preview.example.com
#                                    Wildcard DNS *.preview.example.com must point here.
#   CYCLOPUS_CNAME_TARGET            CNAME target for custom domains
#                                    e.g. preview.example.com
#   CYCLOPUS_OPENOBSERVE_URL         https:// base URL of your OpenObserve instance
#   CYCLOPUS_BESZEL_URL              https:// base URL of your Beszel instance
#
# Optional:
#   CYCLOPUS_VERSION    Release tag to install, e.g. v0.2.0 (default: latest)
#   CYCLOPUS_RELEASE    Helm release name (default: cyclopus)
#   CYCLOPUS_NAMESPACE  Kubernetes namespace (default: cyclopus)
#
# REQUIREMENTS
# ─────────────────────────────────────
#   - Ubuntu 24.04 or Debian 12, x86_64 or arm64
#   - Root or passwordless sudo
#   - Postgres reachable from this host (CYCLOPUS_DB_URL)
#   - Wildcard DNS *.{PREVIEW_HOSTNAME_SUFFIX} → this server's public IP
#
# MULTI-NODE
# ─────────────────────────────────────
#   This script sets up a single-node control plane.
#   To add worker nodes, run infra/bootstrap/k3s_agent.sh on each agent host
#   after this script completes. The node token is saved to /var/lib/rancher/k3s/server/node-token.

set -eu

# ── pinned versions ────────────────────────────────────────────────────────────
# Keep in sync with infra/bootstrap/common.sh and charts/cyclopus/Chart.yaml.
K3S_VERSION='v1.33.13+k3s1'
HELM_VERSION='v3.18.3'
KYVERNO_VERSION='v1.14.2'
KYVERNO_CHART_VERSION='3.4.2'

CYCLOPUS_RELEASE=${CYCLOPUS_RELEASE:-cyclopus}
CYCLOPUS_NAMESPACE=${CYCLOPUS_NAMESPACE:-cyclopus}

# ── helpers ────────────────────────────────────────────────────────────────────

step()  { printf '\n==> cyclopus-install: %s\n' "$1"; }
info()  { printf '    %s\n' "$1"; }
fail()  { printf '\ncyclopus-install: ERROR: %s\n' "$*" >&2; exit 1; }

retry() {
  _n=$1; shift
  _i=1
  until "$@"; do
    [ "$_i" -ge "$_n" ] && return 1
    _i=$((_i + 1)); sleep 3
  done
}

is_tty() { [ -t 0 ]; }

# Require an env var. Prompts interactively if stdin is a TTY and the var is unset.
require_env() {
  _var="$1"; _label="$2"; _secret="${3:-no}"
  eval "_val=\${$_var:-}"
  if [ -z "$_val" ]; then
    if is_tty; then
      if [ "$_secret" = "yes" ]; then
        # stty may not be available in all minimal installs; fall back to plain read.
        if command -v stty >/dev/null 2>&1; then
          printf '    %s [%s] (hidden): ' "$_label" "$_var"
          stty -echo 2>/dev/null; read -r _val; stty echo 2>/dev/null; printf '\n'
        else
          printf '    %s [%s]: ' "$_label" "$_var"
          read -r _val
        fi
      else
        printf '    %s [%s]: ' "$_label" "$_var"
        read -r _val
      fi
      eval "$_var=\$_val"
      eval "_val=\${$_var:-}"
    fi
    [ -n "$_val" ] || fail "$_var is required. Set it before running:\n  export $_var=<value>"
  fi
}

arch() {
  case "$(uname -m)" in
    x86_64 | amd64)  printf 'amd64' ;;
    aarch64 | arm64) printf 'arm64' ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac
}

# ── root check ─────────────────────────────────────────────────────────────────

if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || fail "run as root or install sudo first"
  sudo -n true 2>/dev/null       || fail "passwordless sudo required (or run as root)"
  SUDO=sudo
else
  SUDO=''
fi

# ── distro check ───────────────────────────────────────────────────────────────

[ -r /etc/os-release ] || fail "/etc/os-release not found"
# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-}:${VERSION_ID:-}" in
  ubuntu:24.04 | debian:12) ;;
  *) fail "unsupported OS: ${ID:-unknown} ${VERSION_ID:-unknown}. Supported: Ubuntu 24.04, Debian 12" ;;
esac

# ── resolve version ────────────────────────────────────────────────────────────

step "resolving Cyclopus version"
if [ -z "${CYCLOPUS_VERSION:-}" ]; then
  info "CYCLOPUS_VERSION not set — fetching latest release from GitHub"
  CYCLOPUS_VERSION=$(curl -fsSL \
    -H "Accept: application/vnd.github+json" \
    https://api.github.com/repos/cyclopus-dev/cyclopus/releases/latest \
    | grep '"tag_name"' | head -n1 | cut -d '"' -f 4)
  [ -n "$CYCLOPUS_VERSION" ] || fail "could not resolve latest release; set CYCLOPUS_VERSION explicitly"
fi
info "version: $CYCLOPUS_VERSION"
# Strip leading 'v' for semver values used in Helm --version.
CHART_VERSION="${CYCLOPUS_VERSION#v}"

# ── required configuration ─────────────────────────────────────────────────────

step "collecting required configuration"
require_env CYCLOPUS_DB_URL                 "Postgres DSN (postgres://user:pass@host:5432/db)"
require_env CYCLOPUS_AUTH_BOOTSTRAP_SECRET  "Auth bootstrap secret (>=32 chars)"         yes
require_env CYCLOPUS_AUTH_TOKEN_PEPPER      "Auth token pepper (>=32 chars)"              yes
require_env CYCLOPUS_PREVIEW_HOSTNAME_SUFFIX "Preview hostname DNS suffix (e.g. .preview.example.com)"
require_env CYCLOPUS_CNAME_TARGET           "Custom domain CNAME target (e.g. preview.example.com)"
require_env CYCLOPUS_OPENOBSERVE_URL        "OpenObserve base URL (https://...)"
require_env CYCLOPUS_BESZEL_URL             "Beszel base URL (https://...)"

# ── download bootstrap scripts ─────────────────────────────────────────────────

step "downloading bootstrap scripts at $CYCLOPUS_VERSION"
BOOTSTRAP_DIR=$(mktemp -d)
trap 'rm -rf "$BOOTSTRAP_DIR"' EXIT HUP INT TERM

RAW="https://raw.githubusercontent.com/cyclopus-dev/cyclopus/${CYCLOPUS_VERSION}/infra/bootstrap"
mkdir -p "$BOOTSTRAP_DIR/infra/bootstrap"
for _script in common.sh base_packages.sh k3s_server.sh install_gvisor.sh register_runtimeclass.sh; do
  retry 3 curl -fsSLo "$BOOTSTRAP_DIR/infra/bootstrap/$_script" "$RAW/$_script" \
    || fail "failed to download infra/bootstrap/$_script"
done

# ── base packages ──────────────────────────────────────────────────────────────

step "installing base packages"
$SUDO sh "$BOOTSTRAP_DIR/infra/bootstrap/base_packages.sh" \
  || fail "base package install failed"

# ── k3s control plane ─────────────────────────────────────────────────────────

step "bootstrapping k3s $K3S_VERSION"
# CONTROL_PLANE_TAINT='': single-node install — no taint so platform components
# (Kyverno, etc.) can schedule on the same node as the Cyclopus API pod.
# NODE_LABEL_ROLE: labels the node so the chart's nodeSelector still matches.
$SUDO env \
  CONTROL_PLANE_TAINT='' \
  NODE_LABEL_ROLE='cyclopus.app/role=control-plane' \
  sh "$BOOTSTRAP_DIR/infra/bootstrap/k3s_server.sh" \
  || fail "k3s bootstrap failed"

step "waiting for k3s API"
retry 40 $SUDO k3s kubectl get --raw=/readyz >/dev/null \
  || fail "k3s API did not become ready"

_enc_ok() { $SUDO k3s secrets-encrypt status 2>&1 | grep -q '^Encryption Status: Enabled$'; }
retry 20 _enc_ok || fail "k3s secrets encryption not enabled"

# ── gVisor ────────────────────────────────────────────────────────────────────

step "installing gVisor + RuntimeClasses"
$SUDO sh "$BOOTSTRAP_DIR/infra/bootstrap/install_gvisor.sh" \
  || fail "gVisor install failed"
retry 10 $SUDO k3s kubectl get --raw=/readyz >/dev/null \
  || fail "k3s API not ready after gVisor install"
$SUDO env KUBECTL="$SUDO k3s kubectl" \
  sh "$BOOTSTRAP_DIR/infra/bootstrap/register_runtimeclass.sh" \
  || fail "RuntimeClass registration failed"
$SUDO k3s kubectl get runtimeclass gvisor >/dev/null 2>&1 \
  || fail "gvisor RuntimeClass missing after registration"

# ── Helm ──────────────────────────────────────────────────────────────────────

step "installing Helm $HELM_VERSION"
if ! command -v helm >/dev/null 2>&1; then
  _arch=$(arch)
  _helm_tar="helm-${HELM_VERSION}-linux-${_arch}.tar.gz"
  _tmp=$(mktemp -d)
  retry 3 curl -fsSLo "$_tmp/$_helm_tar" "https://get.helm.sh/$_helm_tar" \
    || fail "helm download failed"
  retry 3 curl -fsSLo "$_tmp/helm.sha256" "https://get.helm.sh/$_helm_tar.sha256sum" \
    || fail "helm checksum download failed"
  [ "$(sha256sum "$_tmp/$_helm_tar" | cut -d ' ' -f 1)" = "$(awk '{print $1; exit}' "$_tmp/helm.sha256")" ] \
    || fail "helm checksum mismatch"
  tar -C "$_tmp" -xzf "$_tmp/$_helm_tar"
  $SUDO install -m 0755 "$_tmp/linux-${_arch}/helm" /usr/local/bin/helm
  rm -rf "$_tmp"
fi

# Export the k3s kubeconfig so helm and kubectl commands work without sudo k3s.
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# ── Kyverno ───────────────────────────────────────────────────────────────────

step "installing Kyverno $KYVERNO_VERSION (chart $KYVERNO_CHART_VERSION)"
helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null 2>&1 || true
helm repo update kyverno >/dev/null 2>&1 || true

if helm status kyverno -n kyverno >/dev/null 2>&1; then
  info "Kyverno already installed, skipping"
else
  helm install kyverno kyverno/kyverno \
    --version "$KYVERNO_CHART_VERSION" \
    --namespace kyverno --create-namespace \
    --wait --timeout 5m \
    || fail "Kyverno install failed"
fi

step "waiting for Kyverno admission webhook"
_kyverno_ready() {
  kubectl -n kyverno get endpoints kyverno-svc \
    -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null | grep -q '.'
}
retry 40 _kyverno_ready || fail "Kyverno webhook endpoint did not become ready"

# ── Cyclopus ──────────────────────────────────────────────────────────────────

step "installing Cyclopus $CYCLOPUS_VERSION"
helm upgrade --install "$CYCLOPUS_RELEASE" \
  "oci://ghcr.io/cyclopus-dev/charts/cyclopus" \
  --version "$CHART_VERSION" \
  --namespace "$CYCLOPUS_NAMESPACE" --create-namespace \
  --set "controlPlane.nodeSelector.cyclopus\.app/role=control-plane" \
  --set "secrets.databaseUrl=${CYCLOPUS_DB_URL}" \
  --set "secrets.authBootstrapSecret=${CYCLOPUS_AUTH_BOOTSTRAP_SECRET}" \
  --set "secrets.authTokenPepper=${CYCLOPUS_AUTH_TOKEN_PEPPER}" \
  --set "config.kubernetes.previewHostnameSuffix=${CYCLOPUS_PREVIEW_HOSTNAME_SUFFIX}" \
  --set "config.kubernetes.customDomainCnameTarget=${CYCLOPUS_CNAME_TARGET}" \
  --set "config.observability.openobserveBaseUrl=${CYCLOPUS_OPENOBSERVE_URL}" \
  --set "config.observability.beszelBaseUrl=${CYCLOPUS_BESZEL_URL}" \
  --wait --timeout 5m \
  || fail "Cyclopus helm install failed"

# ── assertions ────────────────────────────────────────────────────────────────

step "verifying installation"
retry 30 kubectl -n "$CYCLOPUS_NAMESPACE" rollout status \
  "deploy/$CYCLOPUS_RELEASE" --timeout=30s \
  || fail "Cyclopus deployment did not become ready"

kubectl get runtimeclass gvisor >/dev/null 2>&1 \
  || fail "gvisor RuntimeClass missing"

_enc_ok || fail "secrets encryption not enabled"

# ── done ──────────────────────────────────────────────────────────────────────

printf '\n'
printf '==> cyclopus-install: installation complete\n'
printf '\n'
printf '    Cyclopus is running in namespace "%s" (release "%s").\n' \
  "$CYCLOPUS_NAMESPACE" "$CYCLOPUS_RELEASE"
printf '\n'
printf '    First run: POST /auth/bootstrap with CYCLOPUS_AUTH_BOOTSTRAP_SECRET\n'
printf '    to claim the admin account. Keep the control plane on a private\n'
printf '    network or behind a VPN until that step is complete.\n'
printf '\n'
printf '    Node join token (for agent nodes):\n'
printf '      %s\n' "$($SUDO cat /var/lib/rancher/k3s/server/node-token)"
printf '\n'
