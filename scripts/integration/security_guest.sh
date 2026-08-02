#!/bin/sh
set -eu

ROOT=${CYCLOPUS_SECURITY_ROOT:?CYCLOPUS_SECURITY_ROOT is required}
# Pinned versions come from versions.env (generated from versions.yaml).
[ -f "$ROOT/versions.env" ] || { printf '%s\n' "security: FAILED: versions.env missing at $ROOT/versions.env" >&2; exit 1; }
# shellcheck disable=SC1091
. "$ROOT/versions.env"
DB_USER=${CYCLOPUS_SECURITY_DB_USER:-cyclopus_security}
DB_NAME=${CYCLOPUS_SECURITY_DB_NAME:-cyclopus_security_test}
DB_PASSWORD=${CYCLOPUS_SECURITY_DB_PASSWORD:-cyclopus-security-test}
PHASE_PACKAGES='./internal/auth ./internal/service ./internal/server ./internal/store ./internal/secretref'

step() {
  printf '\n==> security: %s\n' "$1"
}

fail() {
  printf '%s\n' "security: FAILED: $*" >&2
  exit 1
}

retry() {
  attempts=$1
  delay=$2
  description=$3
  shift 3
  count=1
  until "$@"; do
    status=$?
    if [ "$count" -ge "$attempts" ]; then
      printf '%s\n' "security: $description failed after $count attempts (exit $status)" >&2
      return "$status"
    fi
    printf '%s\n' "security: $description failed (attempt $count/$attempts); retrying in ${delay}s" >&2
    sleep "$delay"
    count=$((count + 1))
  done
}

run_gate() {
  description=$1
  shift
  step "$description"
  "$@" || fail "$description failed"
}

version_ge() {
  [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | sed -n '1p')" = "$2" ]
}

[ -r /etc/os-release ] || fail '/etc/os-release is unavailable'
# shellcheck disable=SC1091
. /etc/os-release
[ "$ID" = debian ] || fail "expected Debian, observed '$ID'"
[ "$VERSION_ID" = 12 ] || fail "expected Debian 12, observed '$VERSION_ID'"
printf '%s\n' "security: observed distro $PRETTY_NAME"

command -v sudo >/dev/null 2>&1 || fail 'sudo is required'
sudo -n true || fail 'passwordless sudo is required'

step 'installing base packages'
export DEBIAN_FRONTEND=noninteractive
retry 5 5 'Debian apt metadata update' sudo apt-get update -y || fail 'base apt metadata update failed'
retry 5 5 'base package installation' sudo apt-get install -y build-essential ca-certificates coreutils curl diffutils git gnupg jq tar || fail 'base package installation failed'

step "installing PostgreSQL $PG_MAJOR from PGDG"
sudo install -d -m 0755 /usr/share/postgresql-common/pgdg
pgdg_key_tmp=$(mktemp)
go_tmp=
cleanup() {
  rm -f "$pgdg_key_tmp"
  if [ -n "$go_tmp" ]; then
    rm -rf "$go_tmp"
  fi
}
trap cleanup EXIT HUP INT TERM
retry 5 5 'PGDG signing key download' curl -fsSLo "$pgdg_key_tmp" https://www.postgresql.org/media/keys/ACCC4CF8.asc || fail 'PGDG signing key download failed'
sudo gpg --dearmor --yes -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg "$pgdg_key_tmp"
printf 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt %s-pgdg main\n' "$VERSION_CODENAME" |
  sudo tee /etc/apt/sources.list.d/pgdg.list >/dev/null
retry 5 5 'PGDG apt metadata update' sudo apt-get update -y || fail 'PGDG apt metadata update failed'
retry 5 5 "PostgreSQL $PG_MAJOR installation" sudo apt-get install -y "postgresql-$PG_MAJOR" "postgresql-client-$PG_MAJOR" || fail "PostgreSQL $PG_MAJOR installation failed"
sudo systemctl enable --now postgresql || fail 'could not enable and start PostgreSQL'
retry 20 2 'PostgreSQL readiness check' sudo -u postgres pg_isready -q || fail 'PostgreSQL did not become ready'
server_version=$(sudo -u postgres psql -XAtqc 'SHOW server_version_num') || fail 'could not read PostgreSQL server version'
case "$server_version" in
  18????) ;;
  *) fail "PostgreSQL 18 required, observed server_version_num=$server_version" ;;
esac
printf '%s\n' "security: observed PostgreSQL server_version_num=$server_version"

sudo -u postgres psql -X -v ON_ERROR_STOP=1 --set=db_user="$DB_USER" --set=db_name="$DB_NAME" --set=db_password="$DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'db_user', :'db_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'db_user') \gexec
SELECT format('ALTER ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION', :'db_user', :'db_password') \gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'db_name', :'db_user')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'db_name') \gexec
SELECT format('ALTER DATABASE %I OWNER TO %I', :'db_name', :'db_user') \gexec
SQL

step 'probing and installing a checksum-verified compatible Go release'
required_go=$(sed -n 's/^go[[:space:]][[:space:]]*\([0-9][0-9]*\.[0-9][0-9]*\).*$/\1/p' "$ROOT/go.mod")
[ -n "$required_go" ] || fail 'go.mod does not contain a supported go major.minor directive'
go_tmp=$(mktemp -d)
retry 5 5 'go.dev release metadata download' curl -fsSLo "$go_tmp/downloads.json" 'https://go.dev/dl/?mode=json&include=all' || fail 'Go release metadata download failed'
go_version=$(jq -r --arg required "$required_go" '
  def stable: (.stable == true) or ((.version | test("(beta|rc)")) | not);
  def tuple: .version | capture("^go(?<major>[0-9]+)\\.(?<minor>[0-9]+)\\.(?<patch>[0-9]+)$") |
    [(.major | tonumber), (.minor | tonumber), (.patch | tonumber)];
  ($required | split(".") | map(tonumber)) as $minimum |
  [ .[] | select(stable) | select(.version | test("^go[0-9]+\\.[0-9]+\\.[0-9]+$")) |
    select((tuple[0] > $minimum[0]) or (tuple[0] == $minimum[0] and tuple[1] >= $minimum[1])) |
    {version: (.version | ltrimstr("go")), order: tuple} ] |
  sort_by(.order) | last | .version // empty
' "$go_tmp/downloads.json") || fail 'could not parse official Go release metadata'
[ -n "$go_version" ] || fail "no stable Go release compatible with go.mod requirement $required_go is available from go.dev"
version_ge "$go_version" "$required_go" || fail "latest stable Go $go_version does not satisfy go.mod requirement $required_go"

case "$(uname -m)" in
  aarch64|arm64) go_arch=arm64 ;;
  x86_64|amd64) go_arch=amd64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac
go_archive="go${go_version}.linux-${go_arch}.tar.gz"
expected_checksum=$(jq -er --arg version "go$go_version" --arg filename "$go_archive" \
  '.[] | select(.version == $version) | .files[] | select(.filename == $filename) | .sha256' \
  "$go_tmp/downloads.json") || fail "official checksum for $go_archive was not found"
retry 5 5 "Go $go_version archive download" curl -fsSLo "$go_tmp/$go_archive" "https://go.dev/dl/$go_archive" || fail "Go $go_version archive download failed"
actual_checksum=$(sha256sum "$go_tmp/$go_archive" | cut -d ' ' -f 1)
[ "$actual_checksum" = "$expected_checksum" ] || fail "Go checksum mismatch: expected $expected_checksum, observed $actual_checksum"
if [ ! -x /usr/local/go/bin/go ] || [ "$(/usr/local/go/bin/go version | cut -d ' ' -f 3)" != "go$go_version" ]; then
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "$go_tmp/$go_archive"
fi
export PATH="/usr/local/go/bin:$PATH"
export GOTOOLCHAIN=local
export CGO_ENABLED=1
observed_go=$(go version | cut -d ' ' -f 3)
[ "$observed_go" = "go$go_version" ] || fail "expected Go $go_version after installation, observed $observed_go"
printf '%s\n' "security: go.mod requires >= $required_go; observed latest stable compatible Go $go_version"

export POSTGRES_TEST_URL="postgres://$DB_USER:$DB_PASSWORD@127.0.0.1/$DB_NAME?sslmode=disable"
export CYCLOPUS_TEST_DATABASE_URL="$POSTGRES_TEST_URL"
cd "$ROOT"

step 'warming Go module and protobuf tool caches'
retry 5 5 'Go module download' go mod download || fail 'Go module download failed'
retry 5 5 'Buf tool download' go run github.com/bufbuild/buf/cmd/buf@v1.57.0 --version || fail 'Buf tool download failed'

# Keep package variables word-split intentionally for portable /bin/sh invocation.
# shellcheck disable=SC2086
run_gate 'Phase 1 package unit tests' go test -count=1 $PHASE_PACKAGES
# shellcheck disable=SC2086
run_gate 'Phase 1 package PostgreSQL integration tests' go test -count=1 -tags integration $PHASE_PACKAGES
run_gate 'full repository tests' go test -count=1 ./...
# shellcheck disable=SC2086
run_gate 'Phase 1 race tests' go test -count=1 -race -tags integration $PHASE_PACKAGES
run_gate 'go vet' go vet ./...
run_gate 'protobuf generation verification' sh ./scripts/verify_generation.sh

step 'Debian 12 Phase 1 API security gate passed'
