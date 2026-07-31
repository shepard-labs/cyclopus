output "control_plane_id" {
  description = "Control-plane (VPS1) instance ID."
  value       = openstack_compute_instance_v2.control_plane.id
}

output "control_plane_ip" {
  description = "Control-plane node primary IPv4 address."
  value       = openstack_compute_instance_v2.control_plane.access_ip_v4
}

output "data_plane_ids" {
  description = "Data-plane (VPS2-4) instance IDs."
  value       = openstack_compute_instance_v2.data_plane[*].id
}

output "data_plane_ips" {
  description = "Data-plane node primary IPv4 addresses."
  value       = openstack_compute_instance_v2.data_plane[*].access_ip_v4
}

output "node_ips" {
  description = "All node IPv4 addresses, control plane first."
  value       = concat([openstack_compute_instance_v2.control_plane.access_ip_v4], openstack_compute_instance_v2.data_plane[*].access_ip_v4)
}
