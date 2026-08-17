#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
STATE_DIR="$ROOT/.cyclopus-dev"
CLUSTER_NAME="${CYCLOPUS_DEV_CLUSTER:-cyclopus-dev}"
DATABASE_NAME="${CYCLOPUS_DEV_DATABASE:-cyclopus_dev}"
TAILWIND_VERSION="v4.3.3"
TAILWIND_RELEASE_URL="https://github.com/tailwindlabs/tailwindcss/releases/download/$TAILWIND_VERSION"

tailwind_platform_asset() {
  local os="${1:-$(uname -s)}"
  local arch="${2:-$(uname -m)}"

  case "$os:$arch" in
    Darwin:arm64)
      printf '%s %s\n' "$TAILWIND_RELEASE_URL/tailwindcss-macos-arm64" 'cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d'
      ;;
    Darwin:x86_64)
      printf '%s %s\n' "$TAILWIND_RELEASE_URL/tailwindcss-macos-x64" '7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1'
      ;;
    Linux:arm64|Linux:aarch64)
      printf '%s %s\n' "$TAILWIND_RELEASE_URL/tailwindcss-linux-arm64" '55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195'
      ;;
    Linux:x86_64|Linux:amd64)
      printf '%s %s\n' "$TAILWIND_RELEASE_URL/tailwindcss-linux-x64" 'dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a'
      ;;
    *)
      printf 'unsupported Tailwind platform: %s/%s\n' "$os" "$arch" >&2
      return 1
      ;;
  esac
}

tailwind_sha256_tool() {
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s\n' sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    printf '%s\n' shasum
  else
    printf 'missing required checksum command: sha256sum or shasum\n' >&2
    return 1
  fi
}

tailwind_sha256() {
  local tool="${2:-}"

  if [[ -z "$tool" ]]; then
    tool=$(tailwind_sha256_tool) || return 1
  fi
  if [[ "$tool" == sha256sum ]]; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

verify_tailwind_download() {
  local asset_path="$1"
  local sums_path="$2"
  local asset_name="$3"
  local expected_hash="$4"
  local expected_line="$expected_hash  ./$asset_name"
  local checksum_tool="${5:-}"
  local actual_hash

  if ! grep -Fqx "$expected_line" "$sums_path"; then
    printf 'Tailwind checksum manifest does not contain the approved entry for %s\n' "$asset_name" >&2
    return 1
  fi

  actual_hash=$(tailwind_sha256 "$asset_path" "$checksum_tool") || return 1
  if [[ "$actual_hash" != "$expected_hash" ]]; then
    printf 'Tailwind checksum mismatch for %s\n' "$asset_name" >&2
    return 1
  fi
}

tailwind_binary_version() {
  local binary="$1"
  local line

  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    if [[ "$line" == '≈ tailwindcss '* ]]; then
      printf '%s\n' "${line#≈ tailwindcss }"
      return 0
    elif [[ "$line" == 'tailwindcss '* ]]; then
      printf '%s\n' "${line#tailwindcss }"
      return 0
    fi
    return 1
  done < <("$binary" --help 2>&1)
  return 1
}

tailwind_version_is_current() {
  local actual_version

  [[ -x "$1" ]] || return 1
  actual_version=$(tailwind_binary_version "$1") || return 1
  [[ "$actual_version" == "$TAILWIND_VERSION" ]]
}

tailwind_binary_is_approved() {
  local binary="$1"
  local expected_hash="$2"
  local checksum_tool="$3"
  local actual_hash actual_version

  [[ -x "$binary" ]] || return 1
  actual_hash=$(tailwind_sha256 "$binary" "$checksum_tool") || return 1
  [[ "$actual_hash" == "$expected_hash" ]] || return 1
  actual_version=$(tailwind_binary_version "$binary") || return 1
  [[ "$actual_version" == "$TAILWIND_VERSION" ]]
}

install_tailwind() {
  local install_path="${1:-$STATE_DIR/bin/tailwindcss}"
  local platform asset_url asset_name expected_hash checksum_tool download_dir asset_path sums_path staged_path

  platform=$(tailwind_platform_asset)
  asset_url=${platform%% *}
  asset_name=${asset_url##*/}
  expected_hash=${platform##* }
  checksum_tool=$(tailwind_sha256_tool) || return 1

  if tailwind_binary_is_approved "$install_path" "$expected_hash" "$checksum_tool"; then
    return 0
  fi

  if ! command -v curl >/dev/null 2>&1; then
    printf 'missing required command for Tailwind download: curl\n' >&2
    return 1
  fi

  mkdir -p "$(dirname "$install_path")"
  download_dir=$(mktemp -d "$(dirname "$install_path")/.tailwind-download.XXXXXX")
  asset_path="$download_dir/$asset_name"
  sums_path="$download_dir/sha256sums.txt"
  staged_path="$(dirname "$install_path")/.tailwindcss.$$.tmp"

  if ! curl --fail --silent --show-error --location "$asset_url" -o "$asset_path" ||
    ! curl --fail --silent --show-error --location "$TAILWIND_RELEASE_URL/sha256sums.txt" -o "$sums_path" ||
    ! verify_tailwind_download "$asset_path" "$sums_path" "$asset_name" "$expected_hash" "$checksum_tool"; then
    rm -rf "$download_dir" "$staged_path"
    return 1
  fi
  cp "$asset_path" "$staged_path"
  chmod 755 "$staged_path"
  if ! tailwind_binary_is_approved "$staged_path" "$expected_hash" "$checksum_tool"; then
    printf 'downloaded Tailwind binary is not the approved %s release\n' "$TAILWIND_VERSION" >&2
    rm -rf "$download_dir" "$staged_path"
    return 1
  fi
  mv -f "$staged_path" "$install_path"
  rm -rf "$download_dir"
}

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

prepare_helm_release() {
  local release="$1" namespace="$2" status
  status=$(helm status "$release" -n "$namespace" -o json 2>/dev/null | grep -o '"status":"[^"]*"' | head -n1 | cut -d'"' -f4 || true)
  if [[ -n "$status" && "$status" != deployed ]]; then
    warn "$release release is $status; uninstalling it for a clean retry"
    helm uninstall "$release" -n "$namespace" >/dev/null 2>&1 || true
  fi
}

assert_chart_version() {
  local release="$1" namespace="$2" expected="$3" actual
  actual=$(helm list -n "$namespace" -o json | ruby -rjson -e '
    release, input = ARGV.shift, JSON.parse(STDIN.read)
    row = input.find { |item| item.fetch("name") == release }
    abort unless row
    puts row.fetch("chart").sub(/^#{Regexp.escape(release)}-/, "")
  ' "$release") || fail "$release: cannot read installed chart version"
  [[ "$actual" == "$expected" ]] || fail "$release chart version is $actual, expected $expected"
}

ensure_env_var() {
  local file="$1" key="$2" value="$3"
  if ! grep -q "^export ${key}=" "$file" 2>/dev/null; then
    printf "export %s='%s'\n" "$key" "$value" >> "$file"
  fi
}

ensure_hosts_entry() {
  local host="$1"
  grep -qF "$host" /etc/hosts 2>/dev/null && return 0
  printf 'Adding %s to /etc/hosts (requires administrator approval)...\n' "$host" >&2
  if [[ "$(uname -s)" == Darwin ]] && osascript -e "do shell script \"echo 127.0.0.1 $host >> /etc/hosts\" with administrator privileges"; then
    log "Added $host to /etc/hosts"
  elif printf '127.0.0.1 %s\n' "$host" | sudo tee -a /etc/hosts >/dev/null; then
    log "Added $host to /etc/hosts"
  else
    warn "Could not add $host; add this line manually: 127.0.0.1 $host"
  fi
}

main() {
  local command state_file oo_password oo_credential oo_basic_auth secret_file

for command in docker k3d kubectl helm psql createdb openssl ssh-keygen ruby; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'missing required command: %s\n' "$command" >&2
    exit 1
  fi
done

if ! docker info >/dev/null 2>&1; then
  printf 'Docker is not running. Start OrbStack or Docker Desktop and retry.\n' >&2
  exit 1
fi

mkdir -p "$STATE_DIR"
install_tailwind

[[ -f "$ROOT/versions.env" ]] || fail "versions.env is missing; run scripts/generate_versions_env.sh"
# shellcheck disable=SC1091
source "$ROOT/versions.env"

if k3d cluster list --no-headers | cut -d' ' -f1 | grep -qx "$CLUSTER_NAME"; then
  probe_kubeconfig=$(mktemp "${TMPDIR:-/tmp}/cyclopus-kubeconfig.XXXXXX")
  k3d kubeconfig get "$CLUSTER_NAME" > "$probe_kubeconfig"
  if KUBECONFIG="$probe_kubeconfig" kubectl get helmchart traefik -n kube-system >/dev/null 2>&1; then
    rm -f "$probe_kubeconfig"
    fail "legacy cluster uses bundled Traefik; run: k3d cluster delete $CLUSTER_NAME, then rerun setup.sh"
  fi
  rm -f "$probe_kubeconfig"
else
  log "Creating k3d cluster $CLUSTER_NAME with bundled Traefik disabled"
  k3d cluster create "$CLUSTER_NAME" \
    --servers 1 \
    --agents 1 \
    --port '80:80@loadbalancer' \
    --port '443:443@loadbalancer' \
    --k3s-arg '--disable=traefik@server:*' \
    --wait
fi

k3d kubeconfig get "$CLUSTER_NAME" > "$STATE_DIR/kubeconfig"
chmod 600 "$STATE_DIR/kubeconfig"
export KUBECONFIG="$STATE_DIR/kubeconfig"

# Some k3s releases leave the Traefik CRD HelmChart after --disable=traefik.
kubectl delete helmchart traefik-crd -n kube-system --ignore-not-found=true >/dev/null 2>&1 || true

# k3d has runc but not gVisor. Alias the production default RuntimeClass to runc
# so local deployments exercise the real Kubernetes flow on macOS.
kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: runc
handler: runc
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
  annotations:
    cyclopus.dev/local-runtime-alias: runc
handler: runc
YAML

helm repo add traefik https://traefik.github.io/charts >/dev/null 2>&1 || true
helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null 2>&1 || true
helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
helm repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
helm repo update >/dev/null

prepare_helm_release traefik kube-system
helm upgrade --install traefik traefik/traefik \
  --version "$TRAEFIK_CHART_VERSION" \
  --namespace kube-system \
  --set resources.requests.cpu=50m \
  --set resources.requests.memory=64Mi \
  --set resources.limits.memory=128Mi \
  --wait --timeout 5m
assert_chart_version traefik kube-system "$TRAEFIK_CHART_VERSION"

prepare_helm_release kyverno kyverno
helm upgrade --install kyverno kyverno/kyverno \
  --version "$KYVERNO_CHART_VERSION" \
  --namespace kyverno --create-namespace \
  --set-json 'config.webhooks={"namespaceSelector":{"matchLabels":{"app.kubernetes.io/managed-by":"cyclopus"}}}' \
  --set admissionController.replicas=2 \
  --set-json 'admissionController.podDisruptionBudget={"enabled":true,"minAvailable":1}' \
  --wait --timeout 5m
assert_chart_version kyverno kyverno "$KYVERNO_CHART_VERSION"

log 'Waiting for Kyverno webhook'
for ((i=0; i<120; i++)); do
  ready=$(kubectl -n kyverno get deployment kyverno-admission-controller -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
  if [[ "${ready:-0}" -gt 0 ]] && kubectl run cyclopus-webhook-probe -n default --image=alpine --restart=Never --dry-run=server >/dev/null 2>&1; then
    break
  fi
  ((i == 119)) && fail 'Kyverno webhook did not become ready'
  sleep 2
done

prepare_helm_release cert-manager cert-manager
helm upgrade --install cert-manager jetstack/cert-manager \
  --version "$CERT_MANAGER_VERSION" \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true \
  --wait --timeout 5m
assert_chart_version cert-manager cert-manager "$CERT_MANAGER_VERSION"

prepare_helm_release keda keda
helm upgrade --install keda kedacore/keda \
  --version "$KEDA_VERSION" \
  --namespace keda --create-namespace \
  --wait --timeout 5m
assert_chart_version keda keda "$KEDA_VERSION"

prepare_helm_release keda-add-ons-http keda
helm upgrade --install keda-add-ons-http kedacore/keda-add-ons-http \
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
assert_chart_version keda-add-ons-http keda "$KEDA_HTTP_ADDON_VERSION"

helm template cyclopus "$ROOT/charts/cyclopus" \
  --set secrets.databaseUrl='postgres://render-only/unused' \
  --set secrets.authTokenPepper='render-only-token-pepper-32-characters' \
  --set secrets.authBootstrapSecret='render-only-bootstrap-secret-32-chars' \
  --set config.externalUrl='http://127.0.0.1:8081' \
  --set config.kubernetes.previewHostnameSuffix='127.0.0.1.sslip.io' \
  --set kyverno.policies.defaultRuntimeClass=gvisor \
  --show-only templates/kyverno-policies.yaml | kubectl apply -f -

if ! psql -d postgres -Atc "select 1 from pg_database where datname='$DATABASE_NAME'" | grep -qx 1; then
  createdb "$DATABASE_NAME"
fi

log 'Installing observability stack'
kubectl create namespace cyclopus --dry-run=client -o yaml | kubectl apply -f - >/dev/null

if ! kubectl get secret openobserve-auth -n cyclopus >/dev/null 2>&1; then
  oo_password="Cyclopus!0$(openssl rand -base64 24 | tr -d '\n' | tr '/+' 'xy')"
  oo_credential="admin@cyclopus.local:${oo_password}"
  oo_basic_auth=$(printf '%s' "$oo_credential" | base64 | tr -d '\n')
  secret_file=$(mktemp "${TMPDIR:-/tmp}/cyclopus-openobserve-secret.XXXXXX")
  chmod 600 "$secret_file"
  cat > "$secret_file" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: openobserve-auth
  namespace: cyclopus
type: Opaque
stringData:
  password: "${oo_password}"
  credential: "${oo_credential}"
  basic-auth: "${oo_basic_auth}"
EOF
  kubectl apply -f "$secret_file" >/dev/null
  rm -f "$secret_file"
fi

if ! kubectl get secret beszel-agent-key -n cyclopus >/dev/null 2>&1; then
  beszel_key=$(mktemp "${TMPDIR:-/tmp}/cyclopus-beszel-key.XXXXXX")
  rm -f "$beszel_key"
  ssh-keygen -t ed25519 -f "$beszel_key" -N '' -C cyclopus-beszel >/dev/null 2>&1
  kubectl -n cyclopus create secret generic beszel-agent-key \
    --from-literal=public="$(<"$beszel_key.pub")" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  rm -f "$beszel_key" "$beszel_key.pub"
fi

for manifest in openobserve beszel otel-collector beszel-agent network-policy; do
  kubectl apply -f "$ROOT/infra/observability/${manifest}.yaml" >/dev/null
done

kubectl -n cyclopus rollout status deployment/openobserve --timeout=5m || warn 'OpenObserve is not ready yet'
kubectl -n cyclopus rollout status deployment/beszel --timeout=5m || warn 'Beszel is not ready yet'
kubectl -n cyclopus rollout status daemonset/opentelemetry-collector --timeout=5m || warn 'OTel Collector is not ready yet'
kubectl -n cyclopus rollout status daemonset/beszel-agent --timeout=5m || warn 'Beszel agent is not ready yet'

if ! kubectl get secret beszel-auth -n cyclopus >/dev/null 2>&1; then
  beszel_email='beszel-admin@cyclopus.local'
  beszel_password=$(openssl rand -base64 32 | tr -d '\n')
  beszel_pod=$(kubectl -n cyclopus get pods -l app=beszel -o jsonpath='{.items[0].metadata.name}')
  provisioned=0
  for ((i=0; i<10; i++)); do
    if kubectl -n cyclopus exec "$beszel_pod" -- /beszel superuser upsert "$beszel_email" "$beszel_password" >/dev/null 2>&1; then
      provisioned=1
      break
    fi
    sleep 3
  done
  if [[ "$provisioned" == 1 ]]; then
    kubectl -n cyclopus create secret generic beszel-auth \
      --from-literal=credential="${beszel_email}:${beszel_password}" \
      --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  else
    warn 'Beszel superuser provisioning failed; metrics remain disabled'
  fi
fi

if [[ ! -f "$STATE_DIR/env" ]]; then
  token_pepper=$(openssl rand -hex 32)
  bootstrap_secret=$(openssl rand -hex 32)
  cat > "$STATE_DIR/env" <<EOF
export CYCLOPUS_DATABASE_URL='postgres://localhost/$DATABASE_NAME?sslmode=disable'
export CYCLOPUS_HTTP_ADDRESS='127.0.0.1:8081'
export CYCLOPUS_EXTERNAL_URL='http://127.0.0.1:8081'
export CYCLOPUS_AUTH_TOKEN_PEPPER='$token_pepper'
export CYCLOPUS_AUTH_BOOTSTRAP_SECRET='$bootstrap_secret'
export CYCLOPUS_KUBECONFIG='$STATE_DIR/kubeconfig'
export CYCLOPUS_KUBERNETES_INSTANCE_NAMESPACE='cyclopus'
export CYCLOPUS_KUBERNETES_PREVIEW_HOSTNAME_SUFFIX='127.0.0.1.sslip.io'
export CYCLOPUS_KUBERNETES_CERT_MANAGER_CLUSTER_ISSUER=''
export CYCLOPUS_LOG_LEVEL='debug'
EOF
  chmod 600 "$STATE_DIR/env"
fi

ensure_env_var "$STATE_DIR/env" CYCLOPUS_EXTERNAL_URL http://127.0.0.1:8081
ensure_env_var "$STATE_DIR/env" CYCLOPUS_KUBERNETES_INSTANCE_NAMESPACE cyclopus
ensure_env_var "$STATE_DIR/env" CYCLOPUS_OBSERVABILITY_OPENOBSERVE_BASE_URL http://openobserve.cyclopus.svc:5080
ensure_env_var "$STATE_DIR/env" CYCLOPUS_OBSERVABILITY_OPENOBSERVE_ORGANIZATION default
ensure_env_var "$STATE_DIR/env" CYCLOPUS_OBSERVABILITY_OPENOBSERVE_LOG_STREAM default
ensure_env_var "$STATE_DIR/env" CYCLOPUS_OBSERVABILITY_OPENOBSERVE_TRACE_STREAM default
ensure_env_var "$STATE_DIR/env" CYCLOPUS_OBSERVABILITY_OPENOBSERVE_CREDENTIAL_SECRET k8s-secret://cyclopus/openobserve-auth#credential
if kubectl get secret beszel-auth -n cyclopus >/dev/null 2>&1; then
  ensure_env_var "$STATE_DIR/env" CYCLOPUS_OBSERVABILITY_BESZEL_BASE_URL http://beszel.cyclopus.svc:8090
  ensure_env_var "$STATE_DIR/env" CYCLOPUS_OBSERVABILITY_BESZEL_CREDENTIAL_SECRET k8s-secret://cyclopus/beszel-auth#credential
fi

ensure_hosts_entry openobserve.cyclopus.svc
ensure_hosts_entry beszel.cyclopus.svc

printf '\nLocal infrastructure is ready.\n'
printf 'Start Cyclopus: %s/scripts/dev/run.sh\n' "$ROOT"
printf 'UI: http://127.0.0.1:8081\n'
printf 'Bootstrap token: '
sed -n "s/^export CYCLOPUS_AUTH_BOOTSTRAP_SECRET='\(.*\)'$/\1/p" "$STATE_DIR/env"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
