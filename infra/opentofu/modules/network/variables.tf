variable "project_name" {
  type = string
}

variable "region" {
  type = string
}

variable "ovh_service_name" {
  type = string
}

variable "private_network_cidr" {
  type = string
}

variable "api_allowed_cidrs" {
  type = list(string)
}
