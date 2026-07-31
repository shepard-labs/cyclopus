# Cyclopus OVH Public Cloud install (OpenTofu)

Provisions the infrastructure Cyclopus installs onto: a control-plane node
(VPS1), data-plane nodes (VPS2-4), a detachable ingress floating IP, an
S3-compatible object storage bucket for `storage_destinations`, and a
default-deny, single-stack IPv4 network (§7.2, §9, §9.1). Terraform/OpenTofu is
never in the live request path — it exists only to get Cyclopus installed (§9).

## Pinned tooling

| Tool | Version | Source |
|---|---|---|
| OpenTofu | `1.10.1` | `versions.yaml` → `components.opentofu.pin` |
| `ovh/ovh` provider | `2.0.0` | pinned in `versions.tf` |
| `terraform-provider-openstack/openstack` provider | `3.0.0` | pinned in `versions.tf` |

OVH Public Cloud is an OpenStack region, so instances, networking, and object
storage are driven through the OpenStack provider, while OVH-specific resources
(the S3 credential user, private vRack network) use the OVH provider.

## Required credentials (environment only — never hardcoded)

The providers read all secrets from the environment. Nothing sensitive is
committed. `tofu fmt`/`tofu validate` do **not** need these; only `plan`/`apply`
against live OVH do.

OVH provider (create keys at <https://api.ovh.com/createToken/>):

```sh
export OVH_ENDPOINT=ovh-eu
export OVH_APPLICATION_KEY=...
export OVH_APPLICATION_SECRET=...
export OVH_CONSUMER_KEY=...
```

OpenStack provider (download the OpenStack RC file from the OVH Public Cloud
"Users & Roles" panel and `source` it):

```sh
export OS_AUTH_URL=https://auth.cloud.ovh.net/v3
export OS_IDENTITY_API_VERSION=3
export OS_USERNAME=...
export OS_PASSWORD=...
export OS_TENANT_ID=...        # OVH Public Cloud project ID
export OS_TENANT_NAME=...
export OS_REGION_NAME=GRA11
```

## Required variables

| Variable | Description |
|---|---|
| `ovh_service_name` | OVH Public Cloud project ID (cloud project service name). |
| `ssh_public_key` | SSH public key authorized on every node for bootstrap. |

See `variables.tf` for the full set; most have sane defaults.

## Constrained OVH VPS profile and capacity gate

The `*_profile` variables carry the **asserted capacity** (vCPU / RAM / disk) of
the OVH flavor you selected. The compute module asserts each against a hard
floor (`min_control_plane`, `min_data_plane`) with a `lifecycle.precondition`.
If a flavor is too small to run hardened k3s + Postgres 18 + gVisor, `tofu plan`
**fails** with a message naming the deficient dimension — it never provisions an
unusable cluster.

```
control-plane flavor b3-8 is too small: have 1 vCPU / 2GB RAM / 40GB disk,
need at least 2 / 4 / 40. Choose a larger OVH flavor or correct
control_plane_profile.
```

`examples/constrained.tfvars` is the `ovh_vps_constrained` overlay: a minimal
budget flavor that still meets the floor. Lower any dimension below the floor to
see the gate fail.

## Usage

```sh
tofu init
tofu fmt -check                     # style gate (also run by the local unit gate)
tofu validate                       # schema gate (also run by the local unit gate)
tofu plan  -var-file=terraform.tfvars
tofu apply -var-file=terraform.tfvars
```

The local Phase 5 unit gate (`scripts/test_infrastructure.sh`) runs
`tofu fmt -check` + `tofu init -backend=false` + `tofu validate` here without
contacting OVH. Live `plan`/`apply` (the `ovh-public-cloud` /
`ovh-vps-constrained` integration gates) require the credentials above and are
marked **blocked** in environments where they are unavailable.

## Outputs

- `node_ips`, `control_plane_ip`, `data_plane_ips`
- `floating_ip`, `floating_ip_dashed` (for `{ip-dashed}.sslip.io`)
- `kubeconfig_path_hint`
- `bucket_name`, `bucket_endpoint`, `bucket_region`
- `bucket_access_key_id`, `bucket_secret_access_key` (sensitive)

## Ingress topology: floating IP → Traefik

The reserved ingress floating IP (`floating_ip` output) is associated to the
**control-plane node (VPS1)** and is the sslip.io-embedded address that must
survive DR (§9.1). For inbound traffic on that IP to reach Traefik, the ingress
data path must terminate on the same node that holds the floating IP:

- Traefik is the **k3s-embedded** ServiceLB ingress (not part of
  `charts/cyclopus`). By default k3s ServiceLB (`svclb`) runs a DaemonSet that
  binds host ports 80/443 on **every** node, so whichever node holds the
  floating IP already answers on 80/443 — traffic on the FIP reaches Traefik
  with no extra wiring. This is the simplest correct topology and the one the
  security-group rules (80/443 from `0.0.0.0/0`) assume.
- The floating IP is intentionally pinned to VPS1: `infra/bootstrap/floating_ip.sh`
  binds it node-locally so Traefik answers immediately after a DR reattach, and
  `infra/backup/dr_runbook.sh` re-associates it to the replacement control-plane
  node. Because ServiceLB listens on all nodes, pinning the FIP to VPS1 (which
  runs the API + Postgres) keeps the ingress path deterministic.

**Alternatives**, if k3s ServiceLB is disabled (`--disable=servicelb`) or Traefik
is deployed as its own chart:

- Run Traefik as a **DaemonSet** (or pin its pod to the FIP-holding node with a
  nodeSelector/affinity) so port 80/443 is served on VPS1, OR
- Bind the FIP to the Traefik Service via **`externalIPs: [<floating_ip>]`** so
  kube-proxy routes traffic addressed to the FIP into the Traefik Service.

Any of these keeps the FIP → Traefik path intact across a DR node rebuild.

## State backend

Left to the installer (local file, remote S3-compatible, or otherwise) — not
prescribed (§9). No `backend {}` block is committed, so the local unit gate runs
`tofu init -backend=false`.
