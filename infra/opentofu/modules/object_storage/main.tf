# S3-compatible object storage for storage_destinations (§9.1):
#   - postgres_backup       — WAL-G base backups + WAL segments
#   - observability_backing — OpenObserve backing store
#
# OVH Public Cloud object storage is S3-compatible. A dedicated S3 user with
# scoped credentials is created so WAL-G and OpenObserve authenticate without
# reusing the OVH API credentials. Credentials are provider-generated and
# surfaced as sensitive outputs — never hardcoded.

terraform {
  required_providers {
    ovh = {
      source = "ovh/ovh"
    }
    openstack = {
      source = "terraform-provider-openstack/openstack"
    }
  }
}

resource "openstack_objectstorage_container_v1" "backups" {
  name   = var.bucket_name
  region = var.region
}

# Dedicated S3 user scoped to this project for backup/observability access.
resource "ovh_cloud_project_user" "storage" {
  service_name = var.ovh_service_name
  description  = "${var.project_name} storage_destinations S3 access"
  role_names   = ["objectstore_operator"]
}

resource "ovh_cloud_project_user_s3_credential" "storage" {
  service_name = var.ovh_service_name
  user_id      = ovh_cloud_project_user.storage.id
}
