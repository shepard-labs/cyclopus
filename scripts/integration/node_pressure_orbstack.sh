#!/bin/sh
# Node-pressure scenario (OrbStack host side): provision a VM sized to the
# byo_vps memory floor in workflow/run.yaml, then run node_pressure_guest.sh
# inside it (prod one-line install + realistic ingestion/compaction load +
# memory sampling) and copy the evidence JSON back to the repo.
#
# Sizing comes straight from workflow/run.yaml integration_profiles.byo_vps.limits
# (vcpu / memory_mib / disk_gib). The VM is created at exactly that floor so the
# run measures the floor, not a roomier box.
#
# NOTE ON ARCH: the published Cyclopus image + release bundle are linux/amd64
# only. On an Apple-Silicon (arm64) host this VM is therefore an *emulated*
# amd64 machine so the prod installer runs without building from source. CPU is
# slow under emulation; memory footprint — the thing under test — is what we
# sample.
#
# Env:
#   NP_VM        VM name           (default: cyclopus-nodepressure)
#   NP_IMAGE     base image        (default: ubuntu:24.04)
#   NP_ARCH      VM architecture   (default: amd64 — matches the amd64-only image)
#   NP_RECREATE  1 = delete+recreate the VM even if it exists
#   LOAD_SECONDS / SPEW_REPLICAS / CYCLOPUS_VERSION  passed through to the guest
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
RUN_YAML="$ROOT/workflow/run.yaml"
VM=${NP_VM:-cyclopus-nodepressure}
IMAGE=${NP_IMAGE:-ubuntu:24.04}
ARCH=${NP_ARCH:-amd64}

fail() { printf 'node-pressure: FAILED: %s\n' "$*" >&2; exit 1; }

command -v orb >/dev/null 2>&1 || fail "OrbStack CLI 'orb' is required"

# Read the floor from run.yaml so the VM tracks the source of truth.
floor() { awk -v k="$1" '$1==k":" {gsub(/[^0-9]/,"",$2); print $2; exit}' "$RUN_YAML"; }
VCPU=$(floor vcpu); MEM_MIB=$(floor memory_mib); DISK_GIB=$(floor disk_gib)
[ -n "$VCPU" ] && [ -n "$MEM_MIB" ] && [ -n "$DISK_GIB" ] \
  || fail "could not read byo_vps limits (vcpu/memory_mib/disk_gib) from $RUN_YAML"
MEM_G=$(awk -v m="$MEM_MIB" 'BEGIN{printf (m%1024==0)?"%dG":"%dM", (m%1024==0)?m/1024:m}')
printf 'node-pressure: byo_vps floor = %s vCPU / %s MiB (%s) / %s GiB, arch=%s\n' \
  "$VCPU" "$MEM_MIB" "$MEM_G" "$DISK_GIB" "$ARCH"

# (Re)create the VM at the floor.
if orb info "$VM" >/dev/null 2>&1; then
  if [ "${NP_RECREATE:-0}" = 1 ]; then
    printf 'node-pressure: recreating %s\n' "$VM"; orb delete --force "$VM"
  else
    printf 'node-pressure: reusing existing VM %s\n' "$VM"
  fi
fi
if ! orb info "$VM" >/dev/null 2>&1; then
  printf 'node-pressure: creating %s (%s, %s)\n' "$VM" "$IMAGE" "$ARCH"
  orb create --arch "$ARCH" --cpus "$VCPU" --memory "$MEM_G" --disk "${DISK_GIB}G" "$IMAGE" "$VM" \
    || fail "orb create failed"
fi

# Stage the guest harness AND the installer (so the run exercises the local
# install.sh — including fixes — against the pinned release bundle).
printf 'node-pressure: staging guest harness + installer into %s\n' "$VM"
orb -m "$VM" mkdir -p /tmp/np
cat "$ROOT/scripts/integration/node_pressure_guest.sh" \
  | orb -m "$VM" sh -c 'cat > /tmp/np/node_pressure_guest.sh'
cat "$ROOT/scripts/install.sh" \
  | orb -m "$VM" sh -c 'cat > /tmp/np/install.sh'

printf 'node-pressure: running guest harness in %s (this is long under emulation)\n' "$VM"
orb -m "$VM" env \
  CYCLOPUS_VERSION="${CYCLOPUS_VERSION:-v0.1.6}" \
  LOAD_SECONDS="${LOAD_SECONDS:-900}" \
  SPEW_REPLICAS="${SPEW_REPLICAS:-8}" \
  sh /tmp/np/node_pressure_guest.sh

# Copy evidence back.
EVID_DIR="$ROOT/.workflow/evidence/node-pressure"
mkdir -p "$EVID_DIR"
orb -m "$VM" cat /tmp/node_pressure_evidence.json > "$EVID_DIR/node-pressure.json" 2>/dev/null \
  || fail "no evidence produced in $VM"
orb -m "$VM" cat /tmp/node_pressure_samples.csv > "$EVID_DIR/samples.csv" 2>/dev/null || true
printf 'node-pressure: evidence copied to %s\n' "$EVID_DIR/node-pressure.json"
cat "$EVID_DIR/node-pressure.json"
