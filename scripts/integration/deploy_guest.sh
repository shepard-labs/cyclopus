#!/bin/sh
set -eu

ROOT=${CYCLOPUS_DEPLOY_ROOT:?CYCLOPUS_DEPLOY_ROOT is required}
EXPECTED_DISTRO=${CYCLOPUS_DEPLOY_DISTRO:?CYCLOPUS_DEPLOY_DISTRO is required}
K3S_VERSION='v1.33.13+k3s1'
PG_MAJOR=18
DB_USER=cyclopus
DB_NAME=cyclopus_test
DB_PASSWORD=cyclopus-deploy-test
IMAGE_TAG=${CYCLOPUS_DEPLOY_TEST_IMAGE_TAG:-nginx:1.27.5-alpine}

step() { printf '\n==> deploy: %s\n' "$1"; }
fail() { printf '%s\n' "deploy: FAILED: $*" >&2; exit 1; }
retry() { attempts=$1; shift; count=1; until "$@"; do [ "$count" -ge "$attempts" ] && return 1; count=$((count+1)); sleep 3; done; }
encryption_enabled() { sudo k3s secrets-encrypt status 2>&1 | grep -q '^Encryption Status: Enabled$'; }

. /etc/os-release
[ "$ID" = "$EXPECTED_DISTRO" ] || fail "expected $EXPECTED_DISTRO, observed $ID"
case "$EXPECTED_DISTRO:$VERSION_ID" in ubuntu:24.04|debian:12) ;; *) fail "unsupported profile $EXPECTED_DISTRO $VERSION_ID" ;; esac
command -v sudo >/dev/null 2>&1 || fail "sudo is required"
sudo -n true || fail "passwordless sudo is required"

step "installing base packages and build toolchain"
export DEBIAN_FRONTEND=noninteractive
retry 3 sudo apt-get update -y || fail "apt metadata update failed"
retry 3 sudo apt-get install -y build-essential ca-certificates coreutils curl gnupg jq ruby tar || fail "base package installation failed"

step "installing PostgreSQL $PG_MAJOR"
sudo install -d -m 0755 /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | sudo gpg --dearmor --yes -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
printf 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt %s-pgdg main\n' "$VERSION_CODENAME" | sudo tee /etc/apt/sources.list.d/pgdg.list >/dev/null
retry 3 sudo apt-get update -y || fail "PGDG metadata update failed"
retry 3 sudo apt-get install -y "postgresql-$PG_MAJOR" "postgresql-client-$PG_MAJOR" || fail "PostgreSQL installation failed"
sudo systemctl enable --now postgresql
retry 20 sudo -u postgres pg_isready -q || fail "PostgreSQL did not become ready"
server_version=$(sudo -u postgres psql -XAtqc 'SHOW server_version_num')
case "$server_version" in 18????) ;; *) fail "PostgreSQL 18 required, observed $server_version" ;; esac
sudo -u postgres psql -X -v ON_ERROR_STOP=1 --set=db_user="$DB_USER" --set=db_name="$DB_NAME" --set=db_password="$DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'db_user', :'db_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'db_user') \gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'db_name', :'db_user')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'db_name') \gexec
SQL

step "installing checksum-verified latest Go 1.26 stable"
case "$(uname -m)" in aarch64|arm64) go_arch=arm64 ;; x86_64|amd64) go_arch=amd64 ;; *) fail "unsupported architecture $(uname -m)" ;; esac
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
retry 3 curl -fsSLo "$tmp_dir/go-downloads.json" 'https://go.dev/dl/?mode=json&include=all' || fail "Go manifest download failed"
go_version=$(jq -er '[.[] | select(.stable == true and (.version | startswith("go1.26.")))] | first | .version' "$tmp_dir/go-downloads.json") || fail "latest stable Go 1.26 release was not found"
go_archive="${go_version}.linux-${go_arch}.tar.gz"
expected_checksum=$(jq -er --arg version "$go_version" --arg filename "$go_archive" '.[] | select(.version==$version) | .files[] | select(.filename==$filename) | .sha256' "$tmp_dir/go-downloads.json") || fail "Go checksum was not found"
retry 3 curl -fsSLo "$tmp_dir/$go_archive" "https://go.dev/dl/$go_archive" || fail "Go archive download failed"
actual_checksum=$(sha256sum "$tmp_dir/$go_archive" | cut -d ' ' -f 1)
[ "$actual_checksum" = "$expected_checksum" ] || fail "Go checksum mismatch"
if [ ! -x /usr/local/go/bin/go ] || [ "$(/usr/local/go/bin/go version | cut -d ' ' -f 3)" != "$go_version" ]; then sudo rm -rf /usr/local/go; sudo tar -C /usr/local -xzf "$tmp_dir/$go_archive"; fi
export PATH=/usr/local/go/bin:$PATH
[ "$(go version | cut -d ' ' -f 3)" = "$go_version" ] || fail "$go_version was not installed"

step "installing hardened k3s $K3S_VERSION"
sudo install -d -m 0755 /etc/rancher/k3s
printf '%s\n' 'disable:' '  - traefik' 'secrets-encryption: true' | sudo tee /etc/rancher/k3s/config.yaml >/dev/null
if ! command -v k3s >/dev/null 2>&1 || [ "$(k3s --version | sed -n '1s/^k3s version \([^ ]*\).*/\1/p')" != "$K3S_VERSION" ]; then retry 3 curl -sfL https://get.k3s.io -o "$tmp_dir/install-k3s.sh" || fail "k3s installer download failed"; sudo env INSTALL_K3S_VERSION="$K3S_VERSION" sh "$tmp_dir/install-k3s.sh"; fi
sudo systemctl enable k3s
sudo systemctl restart k3s
retry 40 sudo k3s kubectl get --raw=/readyz >/dev/null || fail "k3s API did not become ready"
[ "$(k3s --version | sed -n '1s/^k3s version \([^ ]*\).*/\1/p')" = "$K3S_VERSION" ] || fail "wrong k3s version"
sudo k3s kubectl -n kube-system get helmchart traefik >/dev/null 2>&1 && fail "bundled Traefik exists"
retry 10 encryption_enabled || fail "k3s secrets encryption is not enabled"
sudo install -m 0600 /etc/rancher/k3s/k3s.yaml /tmp/cyclopus-deploy-kubeconfig
sudo chown "$(id -u):$(id -g)" /tmp/cyclopus-deploy-kubeconfig
printf '%s\n' \
  'apiVersion: node.k8s.io/v1' \
  'kind: RuntimeClass' \
  'metadata:' \
  '  name: runc' \
  'handler: runc' | sudo k3s kubectl apply -f - >/dev/null

step "probing multi-architecture HTTP readiness image and pinning observed digest"
retry 3 sudo k3s crictl pull "$IMAGE_TAG" >/dev/null || fail "readiness image pull failed"
image_repo=${IMAGE_TAG%:*}
case "$image_repo" in
  */*) ;;
  *) image_repo="library/$image_repo" ;;
esac
case "$image_repo" in
  *.*/*|*:*/*) ;;
  *) image_repo="docker.io/$image_repo" ;;
esac
image_digest=
for ref in $(sudo k3s ctr images list -q); do
  case "$ref" in
    "$image_repo"@sha256:*) image_digest=$ref; break ;;
  esac
done
[ -n "$image_digest" ] || fail "observed image digest was not available after pull"
export CYCLOPUS_DEPLOY_TEST_IMAGE="$image_digest"

export POSTGRES_TEST_URL="postgres://$DB_USER:$DB_PASSWORD@127.0.0.1/$DB_NAME?sslmode=disable"
export CYCLOPUS_TEST_DATABASE_URL="$POSTGRES_TEST_URL"
export POSTGRES_CONTRACT_URL="$POSTGRES_TEST_URL"
export KUBECONFIG=/tmp/cyclopus-deploy-kubeconfig
cd "$ROOT"

step "running unit, PostgreSQL, race, vet, and generation checks"
go test -count=1 ./... || fail "unit tests failed"
go test -count=1 -tags integration ./... || fail "PostgreSQL integration tests failed"
go test -race -count=1 ./... || fail "race tests failed"
go vet ./... || fail "go vet failed"
./scripts/verify_generation.sh || fail "generation verification failed"
./scripts/validate_workflow.sh || fail "workflow validation failed"
./scripts/validate_postgres_contract.sh || fail "PostgreSQL contract validation failed"

step "running live k3s forward and recovery suites"
sudo k3s kubectl get namespaces -o name | grep '^namespace/cyclopus-forward-' | xargs -r sudo k3s kubectl delete --wait=false >/dev/null
sudo k3s kubectl get namespaces -o name | grep '^namespace/cyclopus-recovery-' | xargs -r sudo k3s kubectl delete --wait=false >/dev/null
go test -tags k3s_integration ./internal/adapters/kubernetes -run '^TestForwardDeploy' -count=1 -v || fail "live forward deploy tests failed"
go test -tags k3s_integration ./internal/adapters/kubernetes -run '^TestRecovery' -count=1 -v || fail "live recovery tests failed"

step "deploy scenario passed on $EXPECTED_DISTRO $VERSION_ID"
