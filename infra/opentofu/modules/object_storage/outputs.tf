output "bucket_name" {
  description = "S3-compatible bucket for postgres_backup + observability_backing."
  value       = openstack_objectstorage_container_v1.backups.name
}

output "endpoint" {
  description = "S3-compatible endpoint for the object storage region."
  value       = "https://s3.${var.region}.io.cloud.ovh.net"
}

output "region" {
  description = "Object storage region (S3 region hint for WAL-G)."
  value       = var.region
}

output "access_key_id" {
  description = "S3 access key for storage_destinations. Sensitive."
  value       = ovh_cloud_project_user_s3_credential.storage.access_key_id
  sensitive   = true
}

output "secret_access_key" {
  description = "S3 secret key for storage_destinations. Sensitive."
  value       = ovh_cloud_project_user_s3_credential.storage.secret_access_key
  sensitive   = true
}
