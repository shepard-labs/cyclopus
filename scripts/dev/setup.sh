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

main() {
  local command

for command in docker k3d kubectl helm psql createdb openssl; do
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

if ! k3d cluster list --no-headers | cut -d' ' -f1 | grep -qx "$CLUSTER_NAME"; then
  k3d cluster create "$CLUSTER_NAME" \
    --servers 1 \
    --agents 1 \
    --port '80:80@loadbalancer' \
    --port '443:443@loadbalancer' \
    --wait
fi

k3d kubeconfig get "$CLUSTER_NAME" > "$STATE_DIR/kubeconfig"
chmod 600 "$STATE_DIR/kubeconfig"
export KUBECONFIG="$STATE_DIR/kubeconfig"

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

helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null 2>&1 || true
helm repo update kyverno >/dev/null
if ! kubectl get deployment kyverno-admission-controller -n kyverno >/dev/null 2>&1; then
  helm install kyverno kyverno/kyverno \
    --version 3.4.2 \
    --namespace kyverno \
    --create-namespace \
    --wait \
    --timeout 5m
else
  kubectl rollout status deployment/kyverno-admission-controller -n kyverno --timeout=2m
fi

helm template cyclopus "$ROOT/charts/cyclopus" \
  --set secrets.databaseUrl='postgres://render-only/unused' \
  --set secrets.authTokenPepper='render-only-token-pepper-32-characters' \
  --set secrets.authBootstrapSecret='render-only-bootstrap-secret-32-chars' \
  --set config.kubernetes.previewHostnameSuffix='127.0.0.1.sslip.io' \
  --set kyverno.policies.defaultRuntimeClass=gvisor \
  --show-only templates/kyverno-policies.yaml | kubectl apply -f -

if ! psql -d postgres -Atc "select 1 from pg_database where datname='$DATABASE_NAME'" | grep -qx 1; then
  createdb "$DATABASE_NAME"
fi

if [[ ! -f "$STATE_DIR/env" ]]; then
  token_pepper=$(openssl rand -hex 32)
  bootstrap_secret=$(openssl rand -hex 32)
  cat > "$STATE_DIR/env" <<EOF
export CYCLOPUS_DATABASE_URL='postgres://localhost/$DATABASE_NAME?sslmode=disable'
export CYCLOPUS_HTTP_ADDRESS='127.0.0.1:8081'
export CYCLOPUS_AUTH_TOKEN_PEPPER='$token_pepper'
export CYCLOPUS_AUTH_BOOTSTRAP_SECRET='$bootstrap_secret'
export CYCLOPUS_KUBECONFIG='$STATE_DIR/kubeconfig'
export CYCLOPUS_KUBERNETES_PREVIEW_HOSTNAME_SUFFIX='127.0.0.1.sslip.io'
export CYCLOPUS_KUBERNETES_CERT_MANAGER_CLUSTER_ISSUER=''
export CYCLOPUS_LOG_LEVEL='debug'
EOF
  chmod 600 "$STATE_DIR/env"
fi

printf '\nLocal infrastructure is ready.\n'
printf 'Start Cyclopus: %s/scripts/dev/run.sh\n' "$ROOT"
printf 'UI: http://127.0.0.1:8081\n'
printf 'Bootstrap token: '
sed -n "s/^export CYCLOPUS_AUTH_BOOTSTRAP_SECRET='\(.*\)'$/\1/p" "$STATE_DIR/env"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
