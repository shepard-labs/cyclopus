variable "project_name" {
  type = string
}

variable "region" {
  type = string
}

variable "ovh_service_name" {
  type = string
}

variable "control_plane_id" {
  description = "Instance the failover/floating IP is initially attached to (VPS1)."
  type        = string
}
