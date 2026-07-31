output "address" {
  description = "Reserved ingress floating IP address (the sslip.io-embedded IP)."
  value       = openstack_networking_floatingip_v2.ingress.address
}

output "dashed_address" {
  description = "Floating IP with dots replaced by dashes, for {ip-dashed}.sslip.io hostnames."
  value       = replace(openstack_networking_floatingip_v2.ingress.address, ".", "-")
}
