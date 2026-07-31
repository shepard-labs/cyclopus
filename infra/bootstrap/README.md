# Cyclopus node bootstrap

Idempotent POSIX `sh` scripts that turn a fresh Ubuntu 24.04 or Debian 12 VPS
into a Cyclopus cluster node. All downloads are checksum-verified, reusing the
pattern from `scripts/integration/deploy_guest.sh`. Every version is pinned to
`versions.yaml` (k3s `v1.33.13+k3s1`, gVisor `release-20260727.0`).

| Script | Role |
|---|---|
| `common.sh` | Shared helpers (sourced): `require_root`, `detect_distro`, `download_verified`, `retry`. |
| `base_packages.sh` | Base packages on Ubuntu 24.04 / Debian 12. |
| `k3s_server.sh` | Control-plane (VPS1): hardened k3s server, `--disable=traefik`, secrets encryption on, single-stack IPv4, control-plane taint, hardened kubelet/API flags (§2.2, §7, §9). |
| `k3s_agent.sh` | Data-plane (VPS2-4): hardened k3s agent joining via `K3S_URL`/`K3S_TOKEN`. |
| `install_gvisor.sh` | Checksum-verified `runsc` + containerd runtime-handler registration for the `gvisor` RuntimeClass (§7.1). |
| `register_runtimeclass.sh` | Registers the `gvisor` and `runc` RuntimeClasses in the cluster. |
| `floating_ip.sh` | Node-local attach/detach of the ingress floating IP (§9.1). |

## Node-join checklist

**Control plane (VPS1):**

```sh
FLOATING_IP=203.0.113.7 sh infra/bootstrap/base_packages.sh
FLOATING_IP=203.0.113.7 sh infra/bootstrap/k3s_server.sh
sh infra/bootstrap/register_runtimeclass.sh
# node-token: /var/lib/rancher/k3s/server/node-token
```

**Each data-plane node (VPS2-4) — gVisor is per-node, not automatic (§2.2):**

```sh
sh infra/bootstrap/base_packages.sh
K3S_URL=https://203.0.113.7:6443 K3S_TOKEN=<node-token> sh infra/bootstrap/k3s_agent.sh
sh infra/bootstrap/install_gvisor.sh      # required, or app pods fail closed
```

## Hardening summary (§7, §2.2)

- `--disable=traefik` (Traefik managed by the Helm chart), `--disable=servicelb`.
- Secrets encryption at rest ON (§7.3).
- Single-stack IPv4 (`cluster-cidr`/`service-cidr` IPv4 only; `flannel-ipv6-masq: false`) (§7.2).
- Control-plane taint `cyclopus.app/role=control-plane:NoSchedule` + node label (§2.2).
- Kubelet: `protect-kernel-defaults`, `streaming-connection-idle-timeout=5m`, `make-iptables-util-chains`.
- API server: `anonymous-auth=false`, bounded `event-ttl`.
- gVisor `runsc` registered as a containerd runtime handler; validating Kyverno
  policy (§7.1) makes a missing handler fail closed.

## DR reattachment

The floating IP move on OVH is authoritative via the OpenStack API (see
`infra/opentofu` and `infra/backup/dr_runbook.sh`); `floating_ip.sh attach`
handles the node-local address bind so Traefik answers immediately.
