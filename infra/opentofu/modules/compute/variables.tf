variable "project_name" {
  type = string
}

variable "region" {
  type = string
}

variable "image_name" {
  type = string
}

variable "ssh_public_key" {
  type = string
}

variable "network_id" {
  type = string
}

variable "security_group_name" {
  type = string
}

variable "control_plane_flavor" {
  type = string
}

variable "data_plane_flavor" {
  type = string
}

variable "data_plane_count" {
  type = number
}

# Optional documentation override describing the operator's expectation of the
# chosen flavors' capacity. It is NOT the enforcement source: the capacity gate
# in main.tf compares the REAL flavor capacity (data.openstack_compute_flavor_v2)
# against the floors below. Kept only so operators can record intent; defaults
# to null when unused.
variable "control_plane_profile" {
  type = object({
    vcpus     = number
    memory_gb = number
    disk_gb   = number
  })
  default = null
}

variable "data_plane_profile" {
  type = object({
    vcpus     = number
    memory_gb = number
    disk_gb   = number
  })
  default = null
}

# Hard capacity floors the REAL flavor capacity must satisfy (the enforcement
# source for the precondition gate).
variable "min_control_plane" {
  type = object({
    vcpus     = number
    memory_gb = number
    disk_gb   = number
  })
}

variable "min_data_plane" {
  type = object({
    vcpus     = number
    memory_gb = number
    disk_gb   = number
  })
}
