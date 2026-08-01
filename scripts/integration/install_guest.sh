#!/bin/sh
# In-VM install scenario: run the Phase 5 node bootstrap + helm install end to
# end and assert the API becomes ready, the gvisor RuntimeClass exists, and
# secrets encryption is enabled (workflow phase-5 install contract).
set -eu

ROOT=${CYCLOPUS_INSTALL_ROOT:?CYCLOPUS_INSTALL_ROOT is required}
EXPECTED_DISTRO=${CYCLOPUS_INSTALL_DISTRO:?CYCLOPUS_INSTALL_DISTRO is required}
K3S_VERSION='v1.33.13+k3s1'
HELM_VERSION=v3.18.3
PG_MAJOR=18
DB_USER=cyclopus
DB_NAME=cyclopus_install
DB_PASSWORD=cyclopus-install-test
RELEASE=cyclopus
NAMESPACE=cyclopus

step() { printf '\n==> install: %s\n' "$1"; }
fail() { printf 'install: FAILED: %s\n' "$*" >&2; exit 1; }
retry() { attempts=$1; shift; count=1; until "$@"; do [ "$count" -ge "$attempts" ] && return 1; count=$((count + 1)); sleep 3; done; }
encryption_enabled() { sudo k3s secrets-encrypt status 2>&1 | grep -q '^Encryption Status: Enabled$'; }

. /etc/os-release
[ "$ID" = "$EXPECTED_DISTRO" ] || fail "expected $EXPECTED_DISTRO, observed $ID"
case "$EXPECTED_DISTRO:$VERSION_ID" in ubuntu:24.04 | debian:12) ;; *) fail "unsupported profile $EXPECTED_DISTRO $VERSION_ID" ;; esac
command -v sudo >/dev/null 2>&1 || fail "sudo is required"
sudo -n true || fail "passwordless sudo is required"

cd "$ROOT"

step "base packages (infra/bootstrap/base_packages.sh)"
sudo -E sh "$ROOT/infra/bootstrap/base_packages.sh" || fail "base package bootstrap failed"

step "installing PostgreSQL $PG_MAJOR for the control plane"
export DEBIAN_FRONTEND=noninteractive
sudo install -d -m 0755 /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | sudo gpg --dearmor --yes -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
printf 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt %s-pgdg main\n' "$VERSION_CODENAME" | sudo tee /etc/apt/sources.list.d/pgdg.list >/dev/null
retry 3 sudo apt-get update -y || fail "PGDG metadata update failed"
retry 3 sudo apt-get install -y "postgresql-$PG_MAJOR" "postgresql-client-$PG_MAJOR" || fail "PostgreSQL install failed"
sudo systemctl enable --now postgresql
retry 20 sudo -u postgres pg_isready -q || fail "PostgreSQL did not become ready"
sudo -u postgres psql -X -v ON_ERROR_STOP=1 --set=db_user="$DB_USER" --set=db_name="$DB_NAME" --set=db_password="$DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'db_user', :'db_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'db_user') \gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'db_name', :'db_user')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'db_name') \gexec
SQL

step "hardened k3s server via infra/bootstrap/k3s_server.sh"
sudo -E env CONTROL_PLANE_TAINT='' NODE_LABEL_ROLE='cyclopus.app/role=control-plane' \
  sh "$ROOT/infra/bootstrap/k3s_server.sh" || fail "k3s server bootstrap failed"
# Single-node install: allow the control-plane node to also run the API pod by
# not tainting it (CONTROL_PLANE_TAINT='' above) but keep the role label so the
# chart's nodeSelector still matches.

step "asserting k3s hardening invariants"
retry 40 sudo k3s kubectl get --raw=/readyz >/dev/null || fail "k3s API not ready"
sudo k3s kubectl -n kube-system get helmchart traefik >/dev/null 2>&1 && fail "bundled Traefik exists"
retry 10 encryption_enabled || fail "secrets encryption is not enabled"

# Single-node install: remove the control-plane taint so Kyverno and other
# platform components can schedule on this node (same node runs everything).
sudo k3s kubectl taint nodes --all "cyclopus.app/role-" 2>/dev/null || true

step "installing gVisor + registering RuntimeClasses"
sudo -E sh "$ROOT/infra/bootstrap/install_gvisor.sh" || fail "gVisor install failed"
retry 20 sudo k3s kubectl get --raw=/readyz >/dev/null || fail "k3s API not ready after gVisor"
sudo -E env KUBECTL='sudo k3s kubectl' sh "$ROOT/infra/bootstrap/register_runtimeclass.sh" || fail "RuntimeClass registration failed"
sudo k3s kubectl get runtimeclass gvisor >/dev/null 2>&1 || fail "gvisor RuntimeClass missing"

step "installing docker (test-only build dependency)"
retry 3 sudo apt-get install -y docker.io || fail "docker.io install failed"
sudo systemctl enable --now docker

step "installing checksum-verified latest Go 1.26 stable and building the Cyclopus image"
case "$(uname -m)" in aarch64 | arm64) go_arch=arm64 ;; x86_64 | amd64) go_arch=amd64 ;; *) fail "unsupported arch" ;; esac
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
retry 3 curl -fsSLo "$tmp_dir/go.json" 'https://go.dev/dl/?mode=json&include=all' || fail "Go manifest download failed"
go_version=$(jq -er '[.[] | select(.stable == true and (.version | startswith("go1.26.")))] | first | .version' "$tmp_dir/go.json") || fail "Go 1.26 not found"
go_archive="${go_version}.linux-${go_arch}.tar.gz"
expected=$(jq -er --arg v "$go_version" --arg f "$go_archive" '.[] | select(.version==$v) | .files[] | select(.filename==$f) | .sha256' "$tmp_dir/go.json") || fail "Go checksum missing"
retry 3 curl -fsSLo "$tmp_dir/$go_archive" "https://go.dev/dl/$go_archive" || fail "Go download failed"
[ "$(sha256sum "$tmp_dir/$go_archive" | cut -d ' ' -f 1)" = "$expected" ] || fail "Go checksum mismatch"
if [ ! -x /usr/local/go/bin/go ] || [ "$(/usr/local/go/bin/go version | cut -d ' ' -f 3)" != "$go_version" ]; then sudo rm -rf /usr/local/go; sudo tar -C /usr/local -xzf "$tmp_dir/$go_archive"; fi
export PATH=/usr/local/go/bin:$PATH

step "building Cyclopus binary and OCI image, importing into containerd"
CGO_ENABLED=0 go build -o "$tmp_dir/cyclopus" ./cmd/cyclopus || fail "cyclopus build failed"
image_dir="$tmp_dir/img"
mkdir -p "$image_dir/rootfs/usr/local/bin"
cp "$tmp_dir/cyclopus" "$image_dir/rootfs/usr/local/bin/cyclopus"
# Use k3s ctr to build a minimal image from the binary via a Dockerfile-less
# approach: package into a tar and import a scratch-based image.
cat > "$tmp_dir/Dockerfile" <<'DOCKER'
FROM gcr.io/distroless/static-debian12:nonroot
COPY cyclopus /usr/local/bin/cyclopus
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cyclopus"]
DOCKER
if command -v docker >/dev/null 2>&1; then
  ( cd "$tmp_dir" && sudo docker build -t ghcr.io/cyclopus/cyclopus:0.1.0 . ) || fail "image build failed"
  sudo docker save ghcr.io/cyclopus/cyclopus:0.1.0 | sudo k3s ctr images import - || fail "image import failed"
else
  # No docker: import the distroless base then it is not trivially layerable via
  # ctr alone. Fall back to a nerdctl build if present, else skip image build and
  # run the binary as a hostPath — record that the packaged-image path needs docker/nerdctl.
  command -v nerdctl >/dev/null 2>&1 || fail "docker or nerdctl required to build the Cyclopus image"
  ( cd "$tmp_dir" && sudo nerdctl --namespace k8s.io build -t ghcr.io/cyclopus/cyclopus:0.1.0 . ) || fail "nerdctl image build failed"
fi

step "helm install (infra path) via installed Helm $HELM_VERSION"
if ! command -v helm >/dev/null 2>&1; then
  helm_tar="helm-${HELM_VERSION}-linux-${go_arch}.tar.gz"
  retry 3 curl -fsSLo "$tmp_dir/$helm_tar" "https://get.helm.sh/$helm_tar" || fail "helm download failed"
  retry 3 curl -fsSLo "$tmp_dir/helm.sha256" "https://get.helm.sh/$helm_tar.sha256sum" || fail "helm checksum download failed"
  [ "$(sha256sum "$tmp_dir/$helm_tar" | cut -d ' ' -f 1)" = "$(awk '{print $1; exit}' "$tmp_dir/helm.sha256")" ] || fail "helm checksum mismatch"
  tar -C "$tmp_dir" -xzf "$tmp_dir/$helm_tar"
  sudo install -m 0755 "$tmp_dir/linux-${go_arch}/helm" /usr/local/bin/helm
fi

KYVERNO_VERSION=v1.14.2
KYVERNO_CHART_VERSION=3.4.2

export KUBECONFIG="$tmp_dir/kubeconfig"
sudo install -m 0600 /etc/rancher/k3s/k3s.yaml "$KUBECONFIG"
sudo chown "$(id -u):$(id -g)" "$KUBECONFIG"

step "installing Kyverno $KYVERNO_VERSION (required before Cyclopus — §7.1 fail-closed sequencing)"
helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null 2>&1 || true
helm repo update kyverno >/dev/null 2>&1 || true
if helm status kyverno -n kyverno >/dev/null 2>&1; then
  printf 'install: Kyverno already installed, skipping\n'
else
  helm install kyverno kyverno/kyverno \
    --version "$KYVERNO_CHART_VERSION" \
    --namespace kyverno --create-namespace \
    --wait --timeout 5m || fail "Kyverno install failed"
fi
# Wait for Kyverno admission webhook to have endpoints before installing Cyclopus.
_kyverno_ready() {
  sudo k3s kubectl -n kyverno get endpoints kyverno-svc \
    -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null | grep -q '.'
}
retry 40 _kyverno_ready || fail "Kyverno webhook endpoint not ready"
# Point the pod at the host's Postgres via the node IP.
node_ip=$(ip -o -4 addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -n1)
db_url="postgres://$DB_USER:$DB_PASSWORD@$node_ip:5432/$DB_NAME?sslmode=disable"
sudo sed -i "s/^#\?listen_addresses.*/listen_addresses = '*'/" "/etc/postgresql/$PG_MAJOR/main/postgresql.conf" || true
printf 'host %s %s %s/0 md5\n' "$DB_NAME" "$DB_USER" "$node_ip" | sudo tee -a "/etc/postgresql/$PG_MAJOR/main/pg_hba.conf" >/dev/null
sudo systemctl restart postgresql
retry 20 sudo -u postgres pg_isready -q || fail "Postgres not ready after reconfigure"

helm upgrade --install "$RELEASE" "$ROOT/charts/cyclopus" \
  --namespace "$NAMESPACE" --create-namespace \
  --set image.pullPolicy=Never \
  --set "controlPlane.nodeSelector.cyclopus\.app/role=control-plane" \
  --set secrets.databaseUrl="$db_url" \
  --set secrets.authTokenPepper="install-test-pepper-value-32bytes!!" \
  --set secrets.authBootstrapSecret="install-test-bootstrap-secret-32b!" \
  --set config.insecurePublicListeners=true \
  --set config.kubernetes.previewHostnameSuffix="test.cyclopus.local" \
  --set config.kubernetes.customDomainCnameTarget="test.cyclopus.local" \
  --set config.observability.openobserveBaseUrl="https://openobserve.test.local" \
  --set config.observability.beszelBaseUrl="https://beszel.test.local" \
  --wait --timeout 5m || fail "helm install failed"

step "asserting the API becomes ready"
retry 30 sudo k3s kubectl -n "$NAMESPACE" rollout status "deploy/$RELEASE" --timeout=30s || fail "Cyclopus API did not become ready"
pod=$(sudo k3s kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=cyclopus -o name | head -n1)
[ -n "$pod" ] || fail "no Cyclopus pod found"
sudo k3s kubectl -n "$NAMESPACE" exec "$pod" -- wget -qO- http://127.0.0.1:8080/readyz >/dev/null 2>&1 \
  || sudo k3s kubectl -n "$NAMESPACE" get "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' | grep -q True \
  || fail "API readiness probe not satisfied"

step "final invariants: gvisor RuntimeClass + secrets encryption"
sudo k3s kubectl get runtimeclass gvisor >/dev/null 2>&1 || fail "gvisor RuntimeClass missing"
encryption_enabled || fail "secrets encryption not enabled"

step "install scenario passed on $EXPECTED_DISTRO $VERSION_ID"
