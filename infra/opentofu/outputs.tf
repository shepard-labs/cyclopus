output "control_plane_ip" {
  description = "Control-plane node (VPS1) IPv4 address."
  value       = module.compute.control_plane_ip
}

output "data_plane_ips" {
  description = "Data-plane node (VPS2-4) IPv4 addresses."
  value       = module.compute.data_plane_ips
}

output "node_ips" {
  description = "All node IPv4 addresses, control plane first."
  value       = module.compute.node_ips
}

output "floating_ip" {
  description = "Ingress floating IP; the sslip.io-embedded address that survives DR (§9.1)."
  value       = module.floating_ip.address
}

output "floating_ip_dashed" {
  description = "Floating IP for {ip-dashed}.sslip.io default hostnames."
  value       = module.floating_ip.dashed_address
}

output "kubeconfig_path_hint" {
  description = "Where to fetch the k3s kubeconfig from once the control-plane node is bootstrapped."
  value       = "scp root@${module.compute.control_plane_ip}:/etc/rancher/k3s/k3s.yaml ./kubeconfig  # then rewrite server: https://${module.floating_ip.address}:6443"
}

output "bucket_name" {
  description = "storage_destinations S3 bucket name."
  value       = module.object_storage.bucket_name
}

output "bucket_endpoint" {
  description = "storage_destinations S3-compatible endpoint."
  value       = module.object_storage.endpoint
}

output "bucket_region" {
  description = "storage_destinations S3 region hint."
  value       = module.object_storage.region
}

output "bucket_access_key_id" {
  description = "S3 access key for storage_destinations (WAL-G + OpenObserve). Sensitive."
  value       = module.object_storage.access_key_id
  sensitive   = true
}

output "bucket_secret_access_key" {
  description = "S3 secret key for storage_destinations. Sensitive."
  value       = module.object_storage.secret_access_key
  sensitive   = true
}
