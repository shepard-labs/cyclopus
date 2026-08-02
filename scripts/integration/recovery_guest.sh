#!/bin/sh
set -eu

ROOT=${CYCLOPUS_RECOVERY_ROOT:?CYCLOPUS_RECOVERY_ROOT is required}
EXPECTED_DISTRO=${CYCLOPUS_RECOVERY_DISTRO:?CYCLOPUS_RECOVERY_DISTRO is required}
# Pinned component versions come from versions.env (generated from versions.yaml).
# shellcheck disable=SC1091
. "$ROOT/versions.env"
DB_USER=cyclopus
DB_NAME=cyclopus_test
DB_PASSWORD=cyclopus-recovery-test

step() {
  printf '\n==> recovery: %s\n' "$1"
}

fail() {
  printf '%s\n' "recovery: FAILED: $*" >&2
  exit 1
}

retry() {
  attempts=$1
  shift
  count=1
  until "$@"; do
    if [ "$count" -ge "$attempts" ]; then
      return 1
    fi
    count=$((count + 1))
    sleep 3
  done
}

encryption_enabled() {
  sudo k3s secrets-encrypt status 2>&1 | grep -q '^Encryption Status: Enabled$'
}

. /etc/os-release
[ "$ID" = "$EXPECTED_DISTRO" ] || fail "expected $EXPECTED_DISTRO, observed $ID"
command -v sudo >/dev/null 2>&1 || fail "sudo is required"
sudo -n true || fail "passwordless sudo is required"

step "installing base packages"
export DEBIAN_FRONTEND=noninteractive
retry 3 sudo apt-get update -y || fail "apt metadata update failed"
retry 3 sudo apt-get install -y ca-certificates coreutils curl gnupg jq tar || fail "base package installation failed"

step "installing PostgreSQL $PG_MAJOR"
sudo install -d -m 0755 /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc |
  sudo gpg --dearmor --yes -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
printf 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt %s-pgdg main\n' "$VERSION_CODENAME" |
  sudo tee /etc/apt/sources.list.d/pgdg.list >/dev/null
retry 3 sudo apt-get update -y || fail "PGDG apt metadata update failed"
retry 3 sudo apt-get install -y "postgresql-$PG_MAJOR" "postgresql-client-$PG_MAJOR" || fail "PostgreSQL installation failed"
sudo systemctl enable --now postgresql
retry 20 sudo -u postgres pg_isready -q || fail "PostgreSQL did not become ready"
server_version=$(sudo -u postgres psql -XAtqc "SHOW server_version_num")
case "$server_version" in
  18????) ;;
  *) fail "PostgreSQL 18 required, observed server_version_num=$server_version" ;;
esac
sudo -u postgres psql -X -v ON_ERROR_STOP=1 --set=db_user="$DB_USER" --set=db_name="$DB_NAME" --set=db_password="$DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'db_user', :'db_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'db_user') \gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'db_name', :'db_user')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'db_name') \gexec
SQL

step "installing verified Go $GO_VERSION"
case "$(uname -m)" in
  aarch64|arm64) go_arch=arm64 ;;
  x86_64|amd64) go_arch=amd64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac
go_archive="go${GO_VERSION}.linux-${go_arch}.tar.gz"
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
retry 3 curl -fsSLo "$tmp_dir/$go_archive" "https://go.dev/dl/$go_archive" || fail "Go archive download failed"
retry 3 curl -fsSLo "$tmp_dir/go-downloads.json" 'https://go.dev/dl/?mode=json&include=all' || fail "Go checksum manifest download failed"
expected_checksum=$(jq -er --arg version "go$GO_VERSION" --arg filename "$go_archive" \
  '.[] | select(.version == $version) | .files[] | select(.filename == $filename) | .sha256' \
  "$tmp_dir/go-downloads.json") || fail "official checksum for $go_archive was not found"
actual_checksum=$(sha256sum "$tmp_dir/$go_archive" | cut -d ' ' -f 1)
[ "$actual_checksum" = "$expected_checksum" ] || fail "Go checksum mismatch: expected $expected_checksum, observed $actual_checksum"
if [ ! -x /usr/local/go/bin/go ] || [ "$(/usr/local/go/bin/go version | cut -d ' ' -f 3)" != "go$GO_VERSION" ]; then
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "$tmp_dir/$go_archive"
fi
export PATH=/usr/local/go/bin:$PATH
[ "$(go version | cut -d ' ' -f 3)" = "go$GO_VERSION" ] || fail "Go $GO_VERSION was not installed"

step "installing hardened k3s $K3S_VERSION"
sudo install -d -m 0755 /etc/rancher/k3s
printf '%s\n' 'disable:' '  - traefik' 'secrets-encryption: true' |
  sudo tee /etc/rancher/k3s/config.yaml >/dev/null
if ! command -v k3s >/dev/null 2>&1 || [ "$(k3s --version | sed -n '1s/^k3s version \([^ ]*\).*/\1/p')" != "$K3S_VERSION" ]; then
  retry 3 curl -sfL https://get.k3s.io -o "$tmp_dir/install-k3s.sh" || fail "k3s installer download failed"
  sudo env INSTALL_K3S_VERSION="$K3S_VERSION" sh "$tmp_dir/install-k3s.sh"
fi
sudo systemctl enable k3s
sudo systemctl restart k3s
retry 40 sudo k3s kubectl get --raw=/readyz >/dev/null || fail "k3s API did not become ready"
observed_k3s=$(k3s --version | sed -n '1s/^k3s version \([^ ]*\).*/\1/p')
[ "$observed_k3s" = "$K3S_VERSION" ] || fail "expected k3s $K3S_VERSION, observed $observed_k3s"
grep -Eq '^  - traefik$' /etc/rancher/k3s/config.yaml || fail "k3s is not configured with bundled Traefik disabled"
grep -Eq '^secrets-encryption: true$' /etc/rancher/k3s/config.yaml || fail "k3s is not configured with secrets encryption"
if sudo k3s kubectl -n kube-system get helmchart traefik >/dev/null 2>&1; then
  fail "bundled Traefik HelmChart exists despite --disable=traefik"
fi
retry 10 encryption_enabled || fail "k3s secrets encryption is not enabled"
sudo install -m 0600 /etc/rancher/k3s/k3s.yaml /tmp/cyclopus-recovery-kubeconfig
sudo chown "$(id -u):$(id -g)" /tmp/cyclopus-recovery-kubeconfig

export POSTGRES_TEST_URL="postgres://$DB_USER:$DB_PASSWORD@127.0.0.1/$DB_NAME?sslmode=disable"
export CYCLOPUS_TEST_DATABASE_URL="$POSTGRES_TEST_URL"
export KUBECONFIG=/tmp/cyclopus-recovery-kubeconfig

step "running production store tests"
cd "$ROOT"
go test -count=1 ./internal/store || fail "production store tests failed"
go test -count=1 -tags integration ./internal/store || fail "PostgreSQL integration tests failed"

step "running Kubernetes adapter tests"
go test -count=1 ./internal/adapters/kubernetes || fail "Kubernetes adapter tests failed"

step "running live k3s recovery integration test"
sudo k3s kubectl get namespaces -o name | grep '^namespace/cyclopus-recovery-' | xargs -r sudo k3s kubectl delete --wait=false >/dev/null
go test -tags k3s_integration ./internal/adapters/kubernetes -run '^TestRecoveryWorker' -count=1 -v || fail "live k3s PostgreSQL worker recovery integration tests failed"

step "recovery scenario passed on $EXPECTED_DISTRO"
