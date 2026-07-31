# Control-plane (VPS1) and data-plane (VPS2-4) OVH Public Cloud instances.
#
# The constrained OVH VPS profile is enforced here: each instance carries a
# lifecycle precondition asserting the flavor's declared capacity meets the
# hard floor. If a flavor is too small, `tofu plan` fails with a clear message
# naming the deficient dimension, rather than provisioning a cluster that
# cannot run k3s + Postgres 18 + gVisor.

terraform {
  required_providers {
    openstack = {
      source = "terraform-provider-openstack/openstack"
    }
  }
}

# Real flavor lookups. These resolve the actual OVH/OpenStack flavor capacity at
# PLAN time (against the live API). At `tofu validate` the data sources are not
# read, so validate stays offline and needs no credentials. The preconditions
# below compare the REAL vcpus/ram/disk against the floors — this is the
# enforcement source, not the operator-supplied profile object (H1).
data "openstack_compute_flavor_v2" "control_plane" {
  name   = var.control_plane_flavor
  region = var.region
}

data "openstack_compute_flavor_v2" "data_plane" {
  name   = var.data_plane_flavor
  region = var.region
}

locals {
  # OpenStack reports RAM in MB and disk in GB. Convert RAM MB -> GB (floor) for
  # comparison against the GB floors.
  cp_vcpus     = data.openstack_compute_flavor_v2.control_plane.vcpus
  cp_memory_gb = floor(data.openstack_compute_flavor_v2.control_plane.ram / 1024)
  cp_disk_gb   = data.openstack_compute_flavor_v2.control_plane.disk
  dp_vcpus     = data.openstack_compute_flavor_v2.data_plane.vcpus
  dp_memory_gb = floor(data.openstack_compute_flavor_v2.data_plane.ram / 1024)
  dp_disk_gb   = data.openstack_compute_flavor_v2.data_plane.disk

  # Capacity gate driven by the REAL flavor capacity (not the asserted profile).
  control_plane_ok = (
    local.cp_vcpus >= var.min_control_plane.vcpus &&
    local.cp_memory_gb >= var.min_control_plane.memory_gb &&
    local.cp_disk_gb >= var.min_control_plane.disk_gb
  )
  data_plane_ok = (
    local.dp_vcpus >= var.min_data_plane.vcpus &&
    local.dp_memory_gb >= var.min_data_plane.memory_gb &&
    local.dp_disk_gb >= var.min_data_plane.disk_gb
  )
  control_plane_gate_message = format(
    "control-plane flavor %s is too small: OVH reports %d vCPU / %dGB RAM / %dGB disk, need at least %d / %d / %d. Choose a larger OVH flavor.",
    var.control_plane_flavor,
    local.cp_vcpus, local.cp_memory_gb, local.cp_disk_gb,
    var.min_control_plane.vcpus, var.min_control_plane.memory_gb, var.min_control_plane.disk_gb,
  )
  data_plane_gate_message = format(
    "data-plane flavor %s is too small: OVH reports %d vCPU / %dGB RAM / %dGB disk, need at least %d / %d / %d. Choose a larger OVH flavor.",
    var.data_plane_flavor,
    local.dp_vcpus, local.dp_memory_gb, local.dp_disk_gb,
    var.min_data_plane.vcpus, var.min_data_plane.memory_gb, var.min_data_plane.disk_gb,
  )
}

resource "openstack_compute_keypair_v2" "cluster" {
  name       = "${var.project_name}-key"
  public_key = var.ssh_public_key
  region     = var.region
}

# VPS1 — k3s server / control plane. Tainted at bootstrap so only Cyclopus API
# and Postgres land here (§2.2).
resource "openstack_compute_instance_v2" "control_plane" {
  name            = "${var.project_name}-cp-1"
  image_name      = var.image_name
  flavor_name     = var.control_plane_flavor
  key_pair        = openstack_compute_keypair_v2.cluster.name
  security_groups = [var.security_group_name]
  region          = var.region

  network {
    uuid = var.network_id
  }

  lifecycle {
    precondition {
      condition     = local.control_plane_ok
      error_message = local.control_plane_gate_message
    }
  }
}

# VPS2-4 — k3s agents running customer app pods under gVisor.
resource "openstack_compute_instance_v2" "data_plane" {
  count           = var.data_plane_count
  name            = "${var.project_name}-dp-${count.index + 2}"
  image_name      = var.image_name
  flavor_name     = var.data_plane_flavor
  key_pair        = openstack_compute_keypair_v2.cluster.name
  security_groups = [var.security_group_name]
  region          = var.region

  network {
    uuid = var.network_id
  }

  lifecycle {
    precondition {
      condition     = local.data_plane_ok
      error_message = local.data_plane_gate_message
    }
  }
}
