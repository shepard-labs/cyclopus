#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
STATE_DIR="$ROOT/.cyclopus-dev"
CLUSTER_NAME="${CYCLOPUS_DEV_CLUSTER:-cyclopus-dev}"
DATABASE_NAME="${CYCLOPUS_DEV_DATABASE:-cyclopus_dev}"

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
