#!/bin/sh
# Checksum-verified gVisor (runsc) install + k3s containerd runtime-handler
# registration for the `gvisor` RuntimeClass (§2.2, §7.1).
#
# gVisor must be installed per node that runs app pods (VPS2-4, and VPS1 if it
# ever schedules sandboxed pods). Because §7.1's validating policy is
# fail-closed, a node missing runsc produces pods that fail to start rather than
# pods that run unsandboxed — loud, which is the intent.
#
# Idempotent: re-running with runsc already at the pinned release is a no-op.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/bootstrap/common.sh
. "$SCRIPT_DIR/common.sh"

require_root
detect_distro

case "$(uname -m)" in
  aarch64 | arm64) GVISOR_ARCH=aarch64 ;;
  x86_64 | amd64) GVISOR_ARCH=x86_64 ;;
  *) fail "unsupported architecture for gVisor: $(uname -m)" ;;
esac

BASE_URL="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE#release-}/${GVISOR_ARCH}"

# Pinned upstream sha512 constants (versions.yaml components.gvisor.checksums),
# fetched from the release .sha512 sidecars and embedded so we verify against a
# reviewed value rather than a same-origin sidecar downloaded at install time.
case "$GVISOR_ARCH" in
  x86_64)
    GVISOR_SHA512_runsc='ab99ea1b0e2d169ec95473ea6c44abdac9b6b63d9c483f898487fd2b3c32d63bfa9ea104a3d5eed217b90cfc880ceb7a1130a9f7daef6e50656b6e028a8f52e3'
    GVISOR_SHA512_shim='3d6834562006a9b47f5ae9124eb45a14616886ec481ada528cf8cba570a1fc32d2f74082eae510f9bf790c6eb94b54f811919ea0f12fb19e051181219ada48fc'
    ;;
  aarch64)
    GVISOR_SHA512_runsc='c1a654739a11dadcb6e314ac29f458e7bc8befb5f86259bc55fe090c91cd64ee2254f1b70e3962c3b71a127de612839b0e0915b248d46fcf2a315e995650179b'
    GVISOR_SHA512_shim='f788b2445cffa925cef17d11f0ba9a4d57b409f9b54b236b296ed0f1068c151e2bee60a145cb6ad519ae984ab549fd748f1c524ba42e5d8808146ea5c43bc731'
    ;;
  *) fail "no pinned gVisor checksum for arch $GVISOR_ARCH" ;;
esac

log "installing gVisor runsc $GVISOR_RELEASE ($GVISOR_ARCH)"
if command -v runsc >/dev/null 2>&1 && runsc --version 2>/dev/null | grep -q "${GVISOR_RELEASE#release-}"; then
  log "runsc already at $GVISOR_RELEASE"
else
  tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

  # Verify each artifact against the PINNED sha512 constant above, not against a
  # sidecar downloaded from the same origin (which an attacker who controls the
  # binary could also forge).
  for artifact in runsc containerd-shim-runsc-v1; do
    retry 3 curl -fsSLo "$tmp_dir/$artifact" "$BASE_URL/$artifact" || fail "download failed: $artifact"
    case "$artifact" in
      runsc) _expected=$GVISOR_SHA512_runsc ;;
      containerd-shim-runsc-v1) _expected=$GVISOR_SHA512_shim ;;
    esac
    _actual=$(sha512sum "$tmp_dir/$artifact" | cut -d ' ' -f 1)
    [ "$_actual" = "$_expected" ] || fail "gVisor checksum mismatch for $artifact: expected $_expected, observed $_actual"
  done

  $SUDO install -m 0755 "$tmp_dir/runsc" /usr/local/bin/runsc
  $SUDO install -m 0755 "$tmp_dir/containerd-shim-runsc-v1" /usr/local/bin/containerd-shim-runsc-v1
fi
command -v runsc >/dev/null 2>&1 || fail "runsc not on PATH after install"

# Register the runsc runtime handler with k3s's embedded containerd. k3s reads
# an operator-supplied config template and merges it into its generated config.
log "registering gvisor runtime handler with k3s containerd"
$SUDO install -d -m 0755 /var/lib/rancher/k3s/agent/etc/containerd
TEMPLATE=/var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl
$SUDO tee "$TEMPLATE" >/dev/null <<'TOML'
{{ template "base" . }}

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "/etc/containerd/runsc.toml"
TOML

$SUDO install -d -m 0755 /etc/containerd
$SUDO tee /etc/containerd/runsc.toml >/dev/null <<'TOML'
[runsc_config]
  platform = "systrap"
TOML

# Restart k3s so the merged containerd config takes effect (agent or server).
if systemctl list-unit-files 2>/dev/null | grep -q '^k3s\.service'; then
  $SUDO systemctl restart k3s || warn "k3s server restart failed; restart manually to load runsc"
elif systemctl list-unit-files 2>/dev/null | grep -q '^k3s-agent\.service'; then
  $SUDO systemctl restart k3s-agent || warn "k3s-agent restart failed; restart manually to load runsc"
else
  warn "no k3s unit found yet; runtime handler will apply on next k3s start"
fi

log "gVisor installed; register the gvisor RuntimeClass with register_runtimeclass.sh"
