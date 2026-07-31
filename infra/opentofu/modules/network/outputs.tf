output "network_id" {
  description = "OpenStack private network ID for the cluster."
  value       = ovh_cloud_project_network_private.cluster.id
}

output "subnet_id" {
  description = "Private subnet ID (single-stack IPv4)."
  value       = ovh_cloud_project_network_private_subnet.cluster.id
}

output "security_group_id" {
  description = "Default-deny security group applied to every node."
  value       = openstack_networking_secgroup_v2.cluster.id
}

output "security_group_name" {
  description = "Security group name for instance attachment."
  value       = openstack_networking_secgroup_v2.cluster.name
}
