#!/bin/sh
# In-VM node-pressure scenario: install Cyclopus the way a prod VPS user would
# (the published one-line installer + release bundle — NO build from source),
# then drive realistic telemetry ingestion + OpenObserve compaction and sample
# node memory to see whether the byo_vps floor holds without OOM-kills or
# evictions.
#
# The point of the run is to validate workflow/run.yaml's byo_vps memory floor.
# The VM is sized to that floor by the host script (node_pressure_orbstack.sh);
# this guest does the install, the load, and the measurement, and writes a
# schema-shaped evidence JSON to /tmp/node_pressure_evidence.json.
#
# Env (all optional):
#   CYCLOPUS_VERSION   release tag to install (default: v0.1.6)
#   LOAD_SECONDS       sustained-load window (default: 900 = 15 min)
#   SAMPLE_INTERVAL    seconds between memory samples (default: 5)
#   SPEW_REPLICAS      log-spewer pods driving ingestion (default: 8)
#   SPEW_SLEEP         seconds between log bursts per spewer (default: 0.05)
set -eu

CYCLOPUS_VERSION=${CYCLOPUS_VERSION:-v0.1.6}
LOAD_SECONDS=${LOAD_SECONDS:-900}
SAMPLE_INTERVAL=${SAMPLE_INTERVAL:-5}
SPEW_REPLICAS=${SPEW_REPLICAS:-8}
SPEW_SLEEP=${SPEW_SLEEP:-0.05}
NAMESPACE=${CYCLOPUS_NAMESPACE:-cyclopus}
NP_WORKDIR=${NP_WORKDIR:-/tmp}
mkdir -p "$NP_WORKDIR"
EVIDENCE="$NP_WORKDIR/node_pressure_evidence.json"
SAMPLES="$NP_WORKDIR/node_pressure_samples.csv"
INSTALL_LOG="$NP_WORKDIR/node_pressure_install.log"
K="sudo k3s kubectl"

step() { printf '\n==> node-pressure: %s\n' "$1"; }
warn() { printf 'node-pressure: WARN: %s\n' "$*" >&2; }
fail() {
  # Emit a failure evidence file before exiting so the host always has a verdict.
  reason=$1
  write_evidence failed "$reason"
  printf 'node-pressure: FAILED: %s\n' "$reason" >&2
  exit 1
}

# write_evidence STATUS REASON: emit the evidence JSON from whatever globals are
# currently set (missing ones default to 0/empty). Safe to call at any point.
write_evidence() {
  _status=$1; _reason=${2:-}
  cat > "$EVIDENCE" <<JSON
{
  "scenario": "node-pressure",
  "profile": "byo_vps",
  "status": "${_status}",
  "reason": "${_reason}",
  "cyclopus_version": "${CYCLOPUS_VERSION}",
  "node": {
    "vcpu": $(nproc 2>/dev/null || echo 0),
    "mem_total_mib": ${MEM_TOTAL_MIB:-0},
    "arch": "$(uname -m)",
    "enforcement": "${ENFORCEMENT:-unknown}"
  },
  "load": {
    "load_seconds": ${LOAD_SECONDS},
    "spew_replicas": ${SPEW_REPLICAS},
    "sample_interval_s": ${SAMPLE_INTERVAL}
  },
  "install_ok": ${INSTALL_OK:-false},
  "measurements": {
    "baseline_mem_used_mib": ${BASELINE_MEM_MIB:-0},
    "peak_mem_used_mib": ${PEAK_MEM_MIB:-0},
    "peak_mem_pct": ${PEAK_MEM_PCT:-0},
    "peak_swap_used_mib": ${PEAK_SWAP_MIB:-0},
    "oomkilled_events": ${OOMKILLED:-0},
    "evicted_pods": ${EVICTED:-0},
    "restarts_during_load": ${RESTARTS:-0},
    "node_memory_pressure_ever": ${MEMPRESSURE:-false},
    "openobserve_compactions_observed": ${COMPACTIONS:-0},
    "samples": $(wc -l < "$SAMPLES" 2>/dev/null | tr -d ' ' || echo 0)
  },
  "verdict": "${_status}"
}
JSON
  printf 'node-pressure: evidence written: %s (status=%s)\n' "$EVIDENCE" "$_status" >&2
}

# Memory accounting: the node's real ceiling is the cgroup memory.max, NOT
# /proc/meminfo. On a cgroup-capped host (e.g. an OrbStack machine, whose guest
# /proc/meminfo reports the shared VM total) meminfo would wildly understate
# utilization; on a real VPS memory.max is "max" and we fall back to meminfo.
CG=/sys/fs/cgroup
cg_bytes() { v=$(cat "$CG/$1" 2>/dev/null || echo max); [ "$v" = max ] && echo 0 || echo "$v"; }
cg_used_mib() { u=$(cat "$CG/memory.current" 2>/dev/null || echo 0); echo $((u/1048576)); }
CAP_BYTES=$(cg_bytes memory.max)
if [ "${CAP_BYTES:-0}" -gt 0 ]; then
  MEM_TOTAL_MIB=$((CAP_BYTES/1048576))
  ENFORCEMENT="cgroup memory.max (${MEM_TOTAL_MIB} MiB) — whole-machine cap; kubelet eviction/MemoryPressure do NOT engage (kubelet sees host meminfo), so the verdict rests on cgroup OOM-kills"
else
  MEM_TOTAL_MIB=$(awk '/MemTotal/ {printf "%d", $2/1024}' /proc/meminfo)
  ENFORCEMENT="/proc/meminfo (${MEM_TOTAL_MIB} MiB) — real node RAM; kubelet eviction signals are meaningful"
fi
INSTALL_OK=false

# ---- preflight -------------------------------------------------------------
step "preflight"
. /etc/os-release
[ "$ID" = ubuntu ] || warn "expected ubuntu, observed $ID (continuing)"
command -v sudo >/dev/null 2>&1 || fail "sudo is required"
sudo -n true 2>/dev/null || fail "passwordless sudo is required"
command -v curl >/dev/null 2>&1 || { sudo apt-get update -y && sudo apt-get install -y curl; }
printf 'node-pressure: node = %s vCPU / %s MiB / %s arch\n' "$(nproc)" "$MEM_TOTAL_MIB" "$(uname -m)"

# ---- prod install (the one-line VPS installer, release bundle, no source) ----
step "installing Cyclopus $CYCLOPUS_VERSION via the VPS installer (release bundle, no source build)"
# Prefer a staged installer (the host stages scripts/install.sh so the run picks
# up local fixes); otherwise fetch the released one. Either way the installer
# pulls the pinned RELEASE bundle (images+charts) — nothing is built from source.
INSTALLER="$NP_WORKDIR/install.sh"
if [ ! -f "$INSTALLER" ]; then
  curl -fsSL "https://raw.githubusercontent.com/shepard-labs/cyclopus/${CYCLOPUS_VERSION}/scripts/install.sh" \
    -o "$INSTALLER" || fail "could not download install.sh $CYCLOPUS_VERSION"
fi
# Run from a dir WITHOUT versions.env so the installer pulls the tag's pins to
# match the downloaded bundle. Capture the exit code DIRECTLY — a pipe to tee
# would mask the installer's status under POSIX sh.
( cd "$NP_WORKDIR" && sudo -E env \
    CYCLOPUS_VERSION="$CYCLOPUS_VERSION" \
    LETSENCRYPT_EMAIL="node-pressure@cyclopus.local" \
    bash "$INSTALLER" ) > "$INSTALL_LOG" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then
  INSTALL_OK=true
else
  tail -n 40 "$INSTALL_LOG" >&2 || true
  fail "VPS installer exited $rc — see $INSTALL_LOG"
fi

step "waiting for the platform to settle (cyclopus API + observability)"
retry() { _n=$1; shift; _i=1; until "$@"; do [ "$_i" -ge "$_n" ] && return 1; _i=$((_i+1)); sleep 5; done; }
retry 60 sh -c "$K -n $NAMESPACE rollout status deploy/cyclopus --timeout=20s" \
  || fail "cyclopus API never became ready"
# Observability is the memory-dominant tier — wait for it, but non-fatally
# (install.sh treats it as a best-effort add-on).
$K -n "$NAMESPACE" rollout status deploy/openobserve --timeout=3m >/dev/null 2>&1 \
  || warn "openobserve not ready (continuing; load will still target it)"
$K -n "$NAMESPACE" rollout status daemonset/opentelemetry-collector --timeout=3m >/dev/null 2>&1 \
  || warn "otel-collector not ready (continuing)"

# ---- baseline --------------------------------------------------------------
step "recording baseline (post-install, pre-load)"
sleep 20
BASELINE_MEM_MIB=$(cg_used_mib)
: > "$SAMPLES"
printf 'node-pressure: baseline used = %s MiB / %s MiB (cgroup)\n' "$BASELINE_MEM_MIB" "$MEM_TOTAL_MIB"

# ---- load: realistic ingestion via a log-spewer the collector tails ---------
step "starting ingestion load: $SPEW_REPLICAS log-spewer pods (collector -> OpenObserve -> compaction)"
# A plain runc Deployment (no gVisor/Deploy path needed): each replica emits a
# steady stream of ~200B log lines. The OTel Collector DaemonSet tails
# /var/log/pods/*/*/*.log, ships to OpenObserve over OTLP, and OpenObserve's WAL
# fills and compacts under sustained volume — the exact ingest+compaction path
# whose memory footprint we are validating against the 4 GiB floor.
$K create namespace loadgen --dry-run=client -o yaml | $K apply -f - >/dev/null
cat <<YAML | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: log-spewer
  namespace: loadgen
spec:
  replicas: ${SPEW_REPLICAS}
  selector: { matchLabels: { app: log-spewer } }
  template:
    metadata: { labels: { app: log-spewer } }
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: spewer
          image: busybox:1.36
          resources:
            requests: { cpu: 10m, memory: 8Mi }
            limits: { cpu: 100m, memory: 32Mi }
          command: ["/bin/sh","-c"]
          args:
            - |
              i=0
              while true; do
                i=\$((i+1))
                echo "\$(date -u +%FT%TZ) level=info msg=node-pressure-load seq=\$i pod=\$HOSTNAME payload=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
                sleep ${SPEW_SLEEP}
              done
YAML
$K -n loadgen rollout status deploy/log-spewer --timeout=3m >/dev/null 2>&1 \
  || warn "log-spewer not fully ready (continuing; partial load still applies)"

# ---- sample loop -----------------------------------------------------------
step "sampling node memory for ${LOAD_SECONDS}s (interval ${SAMPLE_INTERVAL}s)"
PEAK_MEM_MIB=$BASELINE_MEM_MIB
PEAK_MEM_PCT=0
PEAK_SWAP_MIB=0
MEMPRESSURE=false
COMPACTIONS=0
elapsed=0
oo_pod=$($K -n "$NAMESPACE" get pods -l app=openobserve -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
while [ "$elapsed" -lt "$LOAD_SECONDS" ]; do
  used=$(cg_used_mib)
  swap=$(awk -v c="$(cat $CG/memory.swap.current 2>/dev/null || echo 0)" 'BEGIN{printf "%d", c/1048576}')
  pct=$(awk -v u="$used" -v t="$MEM_TOTAL_MIB" 'BEGIN{ if(t>0) printf "%d", (u*100)/t; else print 0 }')
  [ "$used" -gt "$PEAK_MEM_MIB" ] 2>/dev/null && PEAK_MEM_MIB=$used
  [ "$pct" -gt "$PEAK_MEM_PCT" ] 2>/dev/null && PEAK_MEM_PCT=$pct
  [ "${swap:-0}" -gt "$PEAK_SWAP_MIB" ] 2>/dev/null && PEAK_SWAP_MIB=$swap
  # Node MemoryPressure condition.
  mp=$($K get nodes -o jsonpath='{.items[0].status.conditions[?(@.type=="MemoryPressure")].status}' 2>/dev/null || echo Unknown)
  [ "$mp" = True ] && MEMPRESSURE=true
  printf '%s,%s,%s,%s,%s\n' "$elapsed" "$used" "$pct" "${swap:-0}" "$mp" >> "$SAMPLES"
  printf 'node-pressure: t=%4ss used=%5s MiB (%3s%%) swap=%4s MiB memPressure=%s\n' \
    "$elapsed" "$used" "$pct" "${swap:-0}" "$mp"
  elapsed=$((elapsed + SAMPLE_INTERVAL))
  sleep "$SAMPLE_INTERVAL"
done

# ---- teardown load + tally verdict signals ---------------------------------
step "stopping load and tallying OOM / eviction signals"
$K delete namespace loadgen --wait=false >/dev/null 2>&1 || true

# OOM-kills: count container terminations with reason OOMKilled across the
# platform namespaces (current + last state).
OOMKILLED=$($K get pods -A -o json 2>/dev/null \
  | grep -c '"reason": "OOMKilled"' || true)
OOMKILLED=${OOMKILLED:-0}
# Kernel-level OOM as a cross-check.
DMESG_OOM=$(sudo dmesg 2>/dev/null | grep -c -i 'out of memory\|oom-kill' || true)
# Evicted pods (kubelet node-pressure eviction).
EVICTED=$($K get pods -A -o json 2>/dev/null \
  | grep -c '"reason": "Evicted"' || true)
EVICTED=${EVICTED:-0}
# Restarts accumulated by platform pods during the run.
RESTARTS=$($K -n "$NAMESPACE" get pods -o jsonpath='{range .items[*]}{.status.containerStatuses[*].restartCount}{"\n"}{end}' 2>/dev/null \
  | awk '{s+=$1} END{print s+0}')
# OpenObserve compaction activity (best-effort: log lines mentioning compact).
if [ -n "${oo_pod:-}" ]; then
  COMPACTIONS=$($K -n "$NAMESPACE" logs "$oo_pod" --tail=-1 2>/dev/null \
    | grep -c -i 'compact' || true)
  COMPACTIONS=${COMPACTIONS:-0}
fi

printf 'node-pressure: peak=%s MiB (%s%%) baseline=%s MiB oomkilled=%s dmesg_oom=%s evicted=%s restarts=%s memPressureEver=%s compactions=%s\n' \
  "$PEAK_MEM_MIB" "$PEAK_MEM_PCT" "$BASELINE_MEM_MIB" "$OOMKILLED" "$DMESG_OOM" "$EVICTED" "$RESTARTS" "$MEMPRESSURE" "$COMPACTIONS"

# Verdict: the floor "holds" iff the install succeeded and the kernel/kubelet
# never had to kill or evict anything for memory, and the node never entered
# MemoryPressure. Peak memory is informational.
if [ "$INSTALL_OK" = true ] \
   && [ "${OOMKILLED:-0}" -eq 0 ] \
   && [ "${DMESG_OOM:-0}" -eq 0 ] \
   && [ "${EVICTED:-0}" -eq 0 ] \
   && [ "$MEMPRESSURE" = false ]; then
  write_evidence passed "byo_vps 4 GiB floor held under ingestion+compaction load"
  step "PASSED: 4 GiB floor held (no OOM-kills, no evictions, no MemoryPressure)"
else
  write_evidence failed "memory pressure signal(s) observed under load"
  step "FAILED: memory pressure observed — see evidence"
  exit 1
fi
