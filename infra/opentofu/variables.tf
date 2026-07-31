# Input variables for the Cyclopus OVH Public Cloud install.
#
# Credentials are never hardcoded. The OVH and OpenStack providers read their
# secrets from the standard environment variables documented in README.md
# (OVH_ENDPOINT / OVH_APPLICATION_KEY / OVH_APPLICATION_SECRET /
# OVH_CONSUMER_KEY and OS_* for OpenStack). Only non-secret selectors are
# declared here.

variable "project_name" {
  description = "Name prefix applied to all provisioned resources."
  type        = string
  default     = "cyclopus"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}[a-z0-9]$", var.project_name))
    error_message = "project_name must be a DNS-safe lowercase label (letters, digits, hyphens)."
  }
}

variable "region" {
  description = "OVH Public Cloud (OpenStack) region, e.g. GRA11, SBG5, BHS5."
  type        = string
  default     = "GRA11"
}

variable "ovh_service_name" {
  description = "OVH Public Cloud project ID (cloud project service name)."
  type        = string
}

variable "image_name" {
  description = "Base OS image for every node. v1 supports Ubuntu 24.04 and Debian 12 only."
  type        = string
  default     = "Ubuntu 24.04"

  validation {
    condition     = contains(["Ubuntu 24.04", "Debian 12"], var.image_name)
    error_message = "image_name must be exactly 'Ubuntu 24.04' or 'Debian 12' (the only bootstrap-supported profiles)."
  }
}

variable "ssh_public_key" {
  description = "SSH public key authorized on every node for bootstrap access."
  type        = string
}

# --- Constrained OVH VPS profile ---------------------------------------------
#
# The capacity gate is enforced against the REAL flavor capacity resolved by a
# data "openstack_compute_flavor_v2" lookup in the compute module and compared
# against the hard floors (var.min_*). A flavor too small to run a hardened k3s
# control plane plus Postgres 18 plus gVisor makes `tofu plan` fail with a clear
# message rather than provisioning an unusable cluster. The *_profile objects
# below are an OPTIONAL documentation override of operator intent only; they are
# not the enforcement source. This is the constrained-profile capacity gate
# referenced by phases.yaml's ovh_vps_constrained integration profile.
variable "control_plane_flavor" {
  description = "OpenStack flavor name for the control-plane node (VPS1)."
  type        = string
  default     = "b3-8"
}

variable "data_plane_flavor" {
  description = "OpenStack flavor name for the data-plane nodes (VPS2-4)."
  type        = string
  default     = "b3-8"
}

variable "control_plane_profile" {
  description = <<-EOT
    OPTIONAL documentation override recording the operator's expected capacity of
    control_plane_flavor. NOT the enforcement source: the capacity gate compares
    the REAL flavor capacity (data.openstack_compute_flavor_v2) against
    var.min_control_plane. Defaults to null (unused).
  EOT
  type = object({
    vcpus     = number
    memory_gb = number
    disk_gb   = number
  })
  default = null
}

variable "data_plane_profile" {
  description = "OPTIONAL documentation override of each data-plane flavor's expected capacity. Not the enforcement source (see control_plane_profile)."
  type = object({
    vcpus     = number
    memory_gb = number
    disk_gb   = number
  })
  default = null
}

variable "min_control_plane" {
  description = "Hard capacity floor for the control-plane node. Do not lower without spec review."
  type = object({
    vcpus     = number
    memory_gb = number
    disk_gb   = number
  })
  default = {
    vcpus     = 2
    memory_gb = 4
    disk_gb   = 40
  }
}

variable "min_data_plane" {
  description = "Hard capacity floor for each data-plane node. Do not lower without spec review."
  type = object({
    vcpus     = number
    memory_gb = number
    disk_gb   = number
  })
  default = {
    vcpus     = 2
    memory_gb = 4
    disk_gb   = 40
  }
}

variable "data_plane_count" {
  description = "Number of data-plane (worker) nodes, VPS2-4 by default."
  type        = number
  default     = 3

  validation {
    condition     = var.data_plane_count >= 1 && var.data_plane_count <= 8
    error_message = "data_plane_count must be between 1 and 8 for a v1 single-control-plane install."
  }
}

variable "private_network_cidr" {
  description = "Single-stack IPv4 CIDR for the private cluster network (§7.2)."
  type        = string
  default     = "10.42.0.0/16"

  validation {
    condition     = can(cidrhost(var.private_network_cidr, 0)) && !can(regex(":", var.private_network_cidr))
    error_message = "private_network_cidr must be a valid single-stack IPv4 CIDR (no IPv6)."
  }
}

variable "api_allowed_cidrs" {
  description = "CIDRs permitted to reach the k3s API (6443) and SSH (22). Keep this private (§9)."
  type        = list(string)
  default     = ["10.42.0.0/16"]
}

variable "backup_bucket_name" {
  description = "S3-compatible bucket for storage_destinations (postgres_backup + observability_backing)."
  type        = string
  default     = "cyclopus-backups"

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.backup_bucket_name))
    error_message = "backup_bucket_name must be a valid S3 bucket name (3-63 chars, lowercase)."
  }
}
