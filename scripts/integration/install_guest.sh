#!/bin/sh
# In-VM install scenario: run the Phase 5 node bootstrap + helm install end to
# end and assert the API becomes ready, the gvisor RuntimeClass exists, and
# secrets encryption is enabled (workflow phase-5 install contract).
set -eu

ROOT=${CYCLOPUS_INSTALL_ROOT:?CYCLOPUS_INSTALL_ROOT is required}
EXPECTED_DISTRO=${CYCLOPUS_INSTALL_DISTRO:?CYCLOPUS_INSTALL_DISTRO is required}
# Pinned component versions come from versions.env (generated from versions.yaml).
# shellcheck disable=SC1091
. "$ROOT/versions.env"
DB_USER=cyclopus
DB_NAME=cyclopus_install
DB_PASSWORD=cyclopus-install-test
RELEASE=cyclopus
NAMESPACE=cyclopus

step() { printf '\n==> install: %s\n' "$1"; }
fail() { printf 'install: FAILED: %s\n' "$*" >&2; exit 1; }
retry() { attempts=$1; shift; count=1; until "$@"; do [ "$count" -ge "$attempts" ] && return 1; count=$((count + 1)); sleep 3; done; }
encryption_enabled() { sudo k3s secrets-encrypt status 2>&1 | grep -q '^Encryption Status: Enabled$'; }
# helm_deployed: true only when the release exists AND is in 'deployed' state.
# A 'failed'/'pending' release must NOT be skipped — uninstall it so a clean
# install can proceed (helm status returns success even for failed releases).
helm_deployed() {
  _st=$(helm status "$1" -n "$2" -o json 2>/dev/null | grep -o '"status":"[^"]*"' | head -n1 | cut -d'"' -f4)
  if [ "$_st" = "deployed" ]; then return 0; fi
  if [ -n "$_st" ]; then
    printf 'install: %s release is %s (not deployed) — uninstalling for clean retry\n' "$1" "$_st"
    helm uninstall "$1" -n "$2" >/dev/null 2>&1 || true
  fi
  return 1
}

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

export KUBECONFIG="$tmp_dir/kubeconfig"
sudo install -m 0600 /etc/rancher/k3s/k3s.yaml "$KUBECONFIG"
sudo chown "$(id -u):$(id -g)" "$KUBECONFIG"

step "enabling unprivileged low-port binding (net.ipv4.ip_unprivileged_port_start=0)"
echo 'net.ipv4.ip_unprivileged_port_start=0' | sudo tee /etc/sysctl.d/99-cyclopus-lowports.conf >/dev/null
sudo sysctl -q -w net.ipv4.ip_unprivileged_port_start=0

step "installing Traefik ingress controller v$TRAEFIK_CHART_VERSION (hostNetwork :80/:443)"
helm repo add traefik https://traefik.github.io/charts >/dev/null 2>&1 || true
helm repo update traefik >/dev/null 2>&1 || true
if helm_deployed traefik kube-system; then
  printf 'install: Traefik already installed, skipping\n'
else
  # hostNetwork binds host :80/:443 — required so Let's Encrypt HTTP-01 can reach
  # the challenge (a NodePort is not reachable by the ACME server). No --wait: cold
  # VM image pulls can exceed helm's timeout; the explicit poll below is truth.
  helm install traefik traefik/traefik \
    --version "$TRAEFIK_CHART_VERSION" \
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
    --timeout 10m || fail "Traefik install failed"
fi

step "waiting for Traefik DaemonSet ready + host :80 (ACME HTTP-01 reachability)"
_traefik_ready() {
  nready=$(sudo k3s kubectl -n kube-system get ds traefik -o jsonpath='{.status.numberReady}' 2>/dev/null || echo 0)
  [ "${nready:-0}" -ge 1 ] 2>/dev/null || return 1
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:80/ 2>/dev/null || echo 000)
  [ "$code" != "000" ]
}
retry 80 _traefik_ready || fail "Traefik DaemonSet not ready / not listening on host :80"

step "installing Kyverno $KYVERNO_VERSION (required before Cyclopus — §7.1 fail-closed sequencing)"
helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null 2>&1 || true
helm repo update kyverno >/dev/null 2>&1 || true
if helm_deployed kyverno kyverno; then
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

step "installing cert-manager $CERT_MANAGER_VERSION"
helm repo add jetstack https://charts.jetstack.io 2>/dev/null || true
helm repo update jetstack
if ! helm_deployed cert-manager cert-manager; then
  helm install cert-manager jetstack/cert-manager \
    --version "$CERT_MANAGER_VERSION" \
    --namespace cert-manager --create-namespace \
    --set crds.enabled=true \
    --wait --timeout 5m || fail "cert-manager install failed"
else
  printf 'install: cert-manager already installed, skipping\n'
fi

step "waiting for cert-manager webhook"
_certmanager_ready() {
  sudo k3s kubectl -n cert-manager get deployment cert-manager-webhook \
    -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null | grep -q '^True$'
}
retry 40 _certmanager_ready || fail "cert-manager webhook did not become ready"

step "creating Let's Encrypt production ClusterIssuer (matches Coolify behavior)"
sudo k3s kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-production
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: test@cyclopus.local
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
    email: test@cyclopus.local
    privateKeySecretRef:
      name: letsencrypt-staging-key
    solvers:
    - http01:
        ingress:
          class: traefik
EOF

step "installing KEDA v$KEDA_VERSION"
helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
helm repo update kedacore
if ! helm_deployed keda keda; then
  helm install keda kedacore/keda \
    --version "$KEDA_VERSION" \
    --namespace keda --create-namespace \
    --wait --timeout 5m || fail "KEDA install failed"
else
  printf 'install: KEDA already installed, skipping\n'
fi

step "installing KEDA HTTP Add-on v$KEDA_HTTP_ADDON_VERSION"
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
    --wait --timeout 10m || fail "KEDA HTTP Add-on install failed"
else
  printf 'install: KEDA HTTP Add-on already installed, skipping\n'
fi

# Point the pod at the host's Postgres via the node IP, over SSL (#5: sslmode=require).
node_ip=$(ip -o -4 addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -n1)
db_url="postgres://$DB_USER:$DB_PASSWORD@$node_ip:5432/$DB_NAME?sslmode=require"
sudo sed -i "s/^#\?listen_addresses.*/listen_addresses = 'localhost,$node_ip'/" "/etc/postgresql/$PG_MAJOR/main/postgresql.conf" || true
if ! sudo grep -q "^ssl = on" "/etc/postgresql/$PG_MAJOR/main/postgresql.conf"; then
  printf "ssl = on\nssl_cert_file = '/etc/ssl/certs/ssl-cert-snakeoil.pem'\nssl_key_file = '/etc/ssl/private/ssl-cert-snakeoil.key'\n" \
    | sudo tee -a "/etc/postgresql/$PG_MAJOR/main/postgresql.conf" >/dev/null
fi
sudo usermod -aG ssl-cert postgres 2>/dev/null || true
# Pods connect FROM the pod network (k3s default 10.42.0.0/16), not the node IP,
# so the hba entry must allow the pod CIDR — over SSL with scram-sha-256.
pod_cidr=$(sudo k3s kubectl cluster-info dump 2>/dev/null | grep -m1 -oE 'cluster-cidr=[0-9./]+' | cut -d= -f2)
[ -n "$pod_cidr" ] || pod_cidr='10.42.0.0/16'
if ! sudo grep -q "hostssl $DB_NAME $DB_USER $pod_cidr scram-sha-256" "/etc/postgresql/$PG_MAJOR/main/pg_hba.conf"; then
  printf 'hostssl %s %s %s scram-sha-256\n' "$DB_NAME" "$DB_USER" "$pod_cidr" | sudo tee -a "/etc/postgresql/$PG_MAJOR/main/pg_hba.conf" >/dev/null
fi
sudo systemctl restart postgresql
retry 20 sudo -u postgres pg_isready -q || fail "Postgres not ready after reconfigure"

# #2/#6: secrets via a 0600 values file (umask), never in argv.
values_file=$( (umask 0077; mktemp) )
trap 'rm -f "$values_file"' EXIT HUP INT TERM
cat > "$values_file" <<EOF
image:
  pullPolicy: Never
controlPlane:
  nodeSelector:
    cyclopus.app/role: control-plane
secrets:
  databaseUrl: "${db_url}"
  authTokenPepper: "install-test-pepper-value-32bytes!!"
  authBootstrapSecret: "install-test-bootstrap-secret-32b!"
config:
  insecurePublicListeners: true
  kubernetes:
    previewHostnameSuffix: "test.cyclopus.local"
    customDomainCnameTarget: "test.cyclopus.local"
    certManagerClusterIssuer: "letsencrypt-production"
  observability:
    openobserveBaseUrl: "https://openobserve.test.local"
    beszelBaseUrl: "https://beszel.test.local"
ingress:
  enabled: true
  className: traefik
  host: "cyclopus.test.local"
  clusterIssuer: "letsencrypt-production"
EOF

helm upgrade --install "$RELEASE" "$ROOT/charts/cyclopus" \
  --namespace "$NAMESPACE" --create-namespace \
  -f "$values_file" \
  --wait --timeout 5m || fail "helm install failed"
rm -f "$values_file"

step "asserting the API becomes ready"
retry 30 sudo k3s kubectl -n "$NAMESPACE" rollout status "deploy/$RELEASE" --timeout=30s || fail "Cyclopus API did not become ready"
pod=$(sudo k3s kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=cyclopus -o name | head -n1)
[ -n "$pod" ] || fail "no Cyclopus pod found"
sudo k3s kubectl -n "$NAMESPACE" exec "$pod" -- wget -qO- http://127.0.0.1:8080/readyz >/dev/null 2>&1 \
  || sudo k3s kubectl -n "$NAMESPACE" get "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' | grep -q True \
  || fail "API readiness probe not satisfied"

step "asserting the control-plane console Ingress exists"
sudo k3s kubectl -n "$NAMESPACE" get ingress "$RELEASE-console" >/dev/null 2>&1 || fail "console Ingress missing"
issuer=$(sudo k3s kubectl -n "$NAMESPACE" get ingress "$RELEASE-console" -o jsonpath='{.metadata.annotations.cert-manager\.io/cluster-issuer}')
[ "$issuer" = "letsencrypt-production" ] || fail "console Ingress cluster-issuer is '$issuer', expected letsencrypt-production"

step "final invariants: gvisor RuntimeClass + secrets encryption"
sudo k3s kubectl get runtimeclass gvisor >/dev/null 2>&1 || fail "gvisor RuntimeClass missing"
encryption_enabled || fail "secrets encryption not enabled"

step "install scenario passed on $EXPECTED_DISTRO $VERSION_ID"
