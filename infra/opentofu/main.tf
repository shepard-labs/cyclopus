# Cyclopus OVH Public Cloud install — root module.
#
# Wires the four reusable submodules:
#   network        — private single-stack IPv4 net + default-deny firewall (§7.2)
#   compute        — VPS1 control plane + VPS2-4 data plane, with the constrained
#                    OVH VPS capacity gate (§2.2)
#   floating_ip    — detachable ingress IP that survives DR (§9.1)
#   object_storage — S3-compatible bucket for storage_destinations (§9.1)

module "network" {
  source = "./modules/network"

  project_name         = var.project_name
  region               = var.region
  ovh_service_name     = var.ovh_service_name
  private_network_cidr = var.private_network_cidr
  api_allowed_cidrs    = var.api_allowed_cidrs
}

module "compute" {
  source = "./modules/compute"

  project_name        = var.project_name
  region              = var.region
  image_name          = var.image_name
  ssh_public_key      = var.ssh_public_key
  network_id          = module.network.network_id
  security_group_name = module.network.security_group_name

  control_plane_flavor  = var.control_plane_flavor
  data_plane_flavor     = var.data_plane_flavor
  data_plane_count      = var.data_plane_count
  control_plane_profile = var.control_plane_profile
  data_plane_profile    = var.data_plane_profile
  min_control_plane     = var.min_control_plane
  min_data_plane        = var.min_data_plane
}

module "floating_ip" {
  source = "./modules/floating_ip"

  project_name     = var.project_name
  region           = var.region
  ovh_service_name = var.ovh_service_name
  control_plane_id = module.compute.control_plane_id
}

module "object_storage" {
  source = "./modules/object_storage"

  project_name     = var.project_name
  region           = var.region
  ovh_service_name = var.ovh_service_name
  bucket_name      = var.backup_bucket_name
}
