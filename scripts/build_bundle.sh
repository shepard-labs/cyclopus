#!/usr/bin/env bash
# Build the Cyclopus offline install bundle for a release tag.
#
# Usage: scripts/build_bundle.sh <tag>   (e.g. v0.5.0)
#
# Produces in the current directory:
#   cyclopus-bundle-<tag>-linux-amd64.tar.zst
#   cyclopus-bundle-<tag>-linux-amd64.tar.zst.sha256
#
# The bundle lets scripts/install.sh install with a SINGLE download: all helm
# charts, all container images (incl. the k3s airgap image set), the k3s/helm
# binaries, the checksum-verified gVisor + WAL-G artifacts, and the
# observability/backup manifests and scripts.
#
# Bundle layout:
#   charts/*.tgz                          pinned helm charts (incl. cyclopus)
#   images/k3s-airgap-images-amd64.tar.zst  k3s release airgap image set
#   images/platform-images.tar            docker save of all workload images
#   bin/k3s, bin/k3s-install.sh, bin/helm
#   gvisor/{runsc,containerd-shim-runsc-v1}
#   walg/wal-g-pg-20.04-amd64.tar.gz
#   observability/*.yaml                  applied by install.sh
#   observability/register_beszel_node.sh staged to /opt/cyclopus/beszel by install.sh
#   backup/*                              staged to /opt/cyclopus/backup
#   bootstrap/*                           gVisor installer + common.sh
#   versions.env                          pin SSOT snapshot
#
# Runs in CI (ubuntu-latest) on amd64 only: the cyclopus image is built
# linux/amd64-only, so an arm64 bundle could not be complete. Requires:
# docker, helm, zstd, ruby, curl.
set -euo pipefail

TAG=${1:?usage: build_bundle.sh <tag> (e.g. v0.5.0)}
case "$TAG" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "build_bundle: tag must look like vX.Y.Z, got: $TAG" >&2; exit 1 ;;
esac
CHART_VER=${TAG#v}

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

log() { printf '\n==> bundle: %s\n' "$*"; }
fail() { printf 'bundle: FAILED: %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null || fail "docker required"
command -v helm >/dev/null || fail "helm required"
command -v zstd >/dev/null || fail "zstd required"
command -v ruby >/dev/null || fail "ruby required"
command -v curl >/dev/null || fail "curl required"

# Pins from the SSOT (same source install.sh uses).
# shellcheck disable=SC1091
. "$ROOT/versions.env"
: "${K3S_VERSION:?}" "${HELM_VERSION:?}" "${TRAEFIK_CHART_VERSION:?}" \
  "${KYVERNO_CHART_VERSION:?}" "${CERT_MANAGER_VERSION:?}" "${KEDA_VERSION:?}" \
  "${KEDA_HTTP_ADDON_VERSION:?}" "${GVISOR_RELEASE:?}" "${WAL_G_VERSION:?}"

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT
B="$WORKDIR/bundle"
mkdir -p "$B"/{charts,images,bin,gvisor,walg,observability,backup,bootstrap}

# ---------------------------------------------------------------- charts ----
log "pulling pinned helm charts"
helm pull oci://ghcr.io/shepard-labs/charts/cyclopus --version "$CHART_VER" -d "$B/charts"
helm repo add traefik https://traefik.github.io/charts
helm repo add kyverno https://kyverno.github.io/kyverno/
helm repo add jetstack https://charts.jetstack.io
helm repo add kedacore https://kedacore.github.io/charts
helm repo update
helm pull traefik/traefik --version "$TRAEFIK_CHART_VERSION" -d "$B/charts"
helm pull kyverno/kyverno --version "$KYVERNO_CHART_VERSION" -d "$B/charts"
helm pull jetstack/cert-manager --version "$CERT_MANAGER_VERSION" -d "$B/charts"
helm pull kedacore/keda --version "$KEDA_VERSION" -d "$B/charts"
helm pull kedacore/keda-add-ons-http --version "$KEDA_HTTP_ADDON_VERSION" -d "$B/charts"

# ---------------------------------------------------------------- images ----
# Render each chart with EXACTLY the --set values install.sh uses, extract
# every image ref, and pull them. Extraction-from-render is self-covering: any
# image a pod would need is visible in the rendered manifests.
log "rendering charts to extract image refs"
TRAEFIK_SET_ARGS=(
  --set deployment.kind=DaemonSet --set hostNetwork=true --set service.enabled=false
  --set updateStrategy.rollingUpdate.maxUnavailable=1 --set updateStrategy.rollingUpdate.maxSurge=0
  --set ports.web.port=80 --set ports.web.hostPort=80
  --set ports.websecure.port=443 --set ports.websecure.hostPort=443
  --set resources.requests.cpu=50m --set resources.requests.memory=64Mi
  --set resources.limits.memory=128Mi
)
KEDA_HTTP_SET_ARGS=(
  --set operator.resources.requests.cpu=50m --set operator.resources.requests.memory=64Mi
  --set operator.resources.limits.memory=128Mi --set interceptor.resources.requests.cpu=50m
  --set interceptor.resources.requests.memory=64Mi --set interceptor.resources.limits.memory=128Mi
  --set scaler.resources.requests.cpu=50m --set scaler.resources.requests.memory=64Mi
  --set scaler.resources.limits.memory=128Mi
)
{
  helm template traefik "$B/charts/traefik-${TRAEFIK_CHART_VERSION}.tgz" -n kube-system "${TRAEFIK_SET_ARGS[@]}"
  helm template kyverno "$B/charts/kyverno-${KYVERNO_CHART_VERSION}.tgz" -n kyverno
  helm template cert-manager "$B/charts/cert-manager-${CERT_MANAGER_VERSION}.tgz" -n cert-manager --set crds.enabled=true
  helm template keda "$B/charts/keda-${KEDA_VERSION}.tgz" -n keda
  helm template keda-add-ons-http "$B/charts/keda-add-ons-http-${KEDA_HTTP_ADDON_VERSION}.tgz" -n keda "${KEDA_HTTP_SET_ARGS[@]}"
  cat "$ROOT"/infra/observability/*.yaml
} | grep -oE 'image: ["'\'']?[a-z0-9][a-z0-9./_-]+[a-z0-9@:/._+-]*' \
  | sed -E 's/^image: ["'\'']?//' | sort -u > "$WORKDIR/images.txt"

# The cyclopus control-plane image itself (built by the release job).
echo "ghcr.io/shepard-labs/cyclopus:${TAG}" >> "$WORKDIR/images.txt"
sort -u "$WORKDIR/images.txt" -o "$WORKDIR/images.txt"
log "images to bundle:"; sed 's/^/    /' "$WORKDIR/images.txt"

log "pulling images"
# Registries (esp. public.ecr.aws and docker.io) rate-limit anonymous pulls
# from shared CI egress IPs — retry with backoff before giving up.
pull_with_retry() {
  _img=$1
  _attempt=1
  until docker pull --platform linux/amd64 --quiet "$_img" >/dev/null; do
    [ "$_attempt" -ge 4 ] && return 1
    log "pull failed for $_img (attempt $_attempt/4); backing off"
    sleep $((20 * _attempt))
    _attempt=$((_attempt + 1))
  done
}
while IFS= read -r img; do
  if ! pull_with_retry "$img"; then
    # openobserve is mirrored to Docker Hub by the same publisher; pull the
    # mirror and retag to the ECR name the manifests reference (containerd
    # matches pre-imported images by exact name at pod time).
    case "$img" in
      public.ecr.aws/zinclabs/openobserve:*)
        _mirror="docker.io/openobserve/openobserve:${img##*:}"
        log "ECR rate-limited; pulling mirror $_mirror and retagging to $img"
        pull_with_retry "$_mirror" || fail "docker pull failed (both ECR and Hub mirror): $img"
        docker tag "$_mirror" "$img" || fail "docker tag failed: $_mirror -> $img"
        ;;
      *) fail "docker pull failed: $img" ;;
    esac
  fi
done < "$WORKDIR/images.txt"

log "saving images to platform-images.tar"
# shellcheck disable=SC2046
docker save -o "$B/images/platform-images.tar" $(tr '\n' ' ' < "$WORKDIR/images.txt")

# k3s airgap image set, as published by the k3s release.
log "downloading k3s airgap images + binary ($K3S_VERSION)"
K3S_DL="https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}"
curl -fsSLo "$B/images/k3s-airgap-images-amd64.tar.zst" "$K3S_DL/k3s-airgap-images-amd64.tar.zst" \
  || fail "k3s airgap images download failed"
curl -fsSLo "$B/bin/k3s" "$K3S_DL/k3s" || fail "k3s binary download failed"
curl -fsSLo "$WORKDIR/sha256sum-amd64.txt" "$K3S_DL/sha256sum-amd64.txt" || fail "k3s checksums download failed"
chmod 0755 "$B/bin/k3s"
grep -E ' (k3s|k3s-airgap-images-amd64\.tar\.zst)$' "$WORKDIR/sha256sum-amd64.txt" > "$WORKDIR/k3s-checksums.txt" \
  || fail "k3s checksums file missing expected entries"
( cd "$B/bin" && grep ' k3s$' "$WORKDIR/k3s-checksums.txt" | sha256sum -c - ) || fail "k3s binary checksum mismatch"
( cd "$B/images" && grep 'k3s-airgap-images-amd64\.tar\.zst$' "$WORKDIR/k3s-checksums.txt" | sha256sum -c - ) \
  || fail "k3s airgap images checksum mismatch"
curl -fsSLo "$B/bin/k3s-install.sh" https://get.k3s.io || fail "k3s installer download failed"

# ------------------------------------------------------------------ helm ----
log "downloading helm $HELM_VERSION"
HELM_TAR="helm-${HELM_VERSION}-linux-amd64.tar.gz"
curl -fsSLo "$WORKDIR/helm.tar.gz" "https://get.helm.sh/${HELM_TAR}" || fail "helm download failed"
curl -fsSLo "$WORKDIR/helm.sha256sum" "https://get.helm.sh/${HELM_TAR}.sha256sum" || fail "helm checksum download failed"
echo "$(awk '{print $1}' "$WORKDIR/helm.sha256sum")  $WORKDIR/helm.tar.gz" | sha256sum -c - || fail "helm checksum mismatch"
tar -C "$WORKDIR" -xzf "$WORKDIR/helm.tar.gz"
install -m 0755 "$WORKDIR/linux-amd64/helm" "$B/bin/helm"

# ---------------------------------------------------------------- gvisor ----
# Verify against the pinned sha512 constants in versions.yaml (same values
# install_gvisor.sh embeds) rather than same-origin sidecars.
log "downloading gVisor $GVISOR_RELEASE (x86_64)"
read -r GVISOR_SHA512_RUNSC GVISOR_SHA512_SHIM <<EOF
$(ruby -ryaml -e 'c=YAML.safe_load(File.read(ARGV[0]))["components"]["gvisor"]["checksums"]["sha512"]["x86_64"]; puts "#{c["runsc"]} #{c["containerd-shim-runsc-v1"]}"' "$ROOT/versions.yaml")
EOF
GVISOR_BASE="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE#release-}/x86_64"
for artifact in runsc containerd-shim-runsc-v1; do
  curl -fsSLo "$B/gvisor/$artifact" "$GVISOR_BASE/$artifact" || fail "gVisor download failed: $artifact"
done
echo "$GVISOR_SHA512_RUNSC  $B/gvisor/runsc" | sha512sum -c - || fail "gVisor runsc checksum mismatch"
echo "$GVISOR_SHA512_SHIM  $B/gvisor/containerd-shim-runsc-v1" | sha512sum -c - || fail "gVisor shim checksum mismatch"
chmod 0755 "$B/gvisor/runsc" "$B/gvisor/containerd-shim-runsc-v1"

# ------------------------------------------------------------------ walg ----
log "downloading WAL-G $WAL_G_VERSION (amd64)"
WALG_SHA256=$(ruby -ryaml -e 'puts YAML.safe_load(File.read(ARGV[0]))["components"]["wal_g"]["checksums"]["sha256"]["amd64"]' "$ROOT/versions.yaml")
WALG_TARBALL="wal-g-pg-20.04-amd64.tar.gz"
curl -fsSLo "$B/walg/$WALG_TARBALL" "https://github.com/wal-g/wal-g/releases/download/${WAL_G_VERSION}/${WALG_TARBALL}" \
  || fail "WAL-G download failed"
echo "$WALG_SHA256  $B/walg/$WALG_TARBALL" | sha256sum -c - || fail "WAL-G checksum mismatch"

# --------------------------------------------------------------- statics ----
log "copying manifests, scripts, versions.env"
cp "$ROOT"/infra/observability/openobserve.yaml "$ROOT"/infra/observability/beszel.yaml \
  "$ROOT"/infra/observability/otel-collector.yaml "$ROOT"/infra/observability/beszel-agent.yaml \
  "$ROOT"/infra/observability/network-policy.yaml \
  "$ROOT"/infra/observability/register_beszel_node.sh \
  "$B/observability/"
cp "$ROOT"/infra/backup/common.sh "$ROOT"/infra/backup/install_walg.sh \
  "$ROOT"/infra/backup/backup.sh "$ROOT"/infra/backup/restore.sh \
  "$ROOT"/infra/backup/configure_from_db.sh \
  "$ROOT"/infra/backup/cyclopus-walg-backup.service "$ROOT"/infra/backup/cyclopus-walg-backup.timer \
  "$B/backup/"
cp "$ROOT"/infra/bootstrap/install_gvisor.sh "$ROOT"/infra/bootstrap/common.sh "$B/bootstrap/"
cp "$ROOT"/versions.env "$B/versions.env"

# ------------------------------------------------------------------ pack ----
OUT="cyclopus-bundle-${TAG}-linux-amd64.tar.zst"
log "packing $OUT"
tar -C "$WORKDIR" -cf - bundle | zstd -T0 -19 -o "$OUT"
sha256sum "$OUT" > "${OUT}.sha256"
log "done: $OUT ($(du -h "$OUT" | cut -f1))"
