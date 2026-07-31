# Private, single-stack IPv4 network + firewall for the Cyclopus cluster (§7.2).
#
# OVH Public Cloud private networks (vRack) are modeled as OpenStack networks.
# Security groups default-deny and only open the k3s API, SSH, and public
# ingress (80/443) required by Traefik.

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

resource "ovh_cloud_project_network_private" "cluster" {
  service_name = var.ovh_service_name
  name         = "${var.project_name}-net"
  regions      = [var.region]
  vlan_id      = 100
}

resource "ovh_cloud_project_network_private_subnet" "cluster" {
  service_name = var.ovh_service_name
  network_id   = ovh_cloud_project_network_private.cluster.id
  region       = var.region
  start        = cidrhost(var.private_network_cidr, 10)
  end          = cidrhost(var.private_network_cidr, 250)
  network      = var.private_network_cidr
  dhcp         = true
  no_gateway   = false
}

# Default-deny security group: only explicitly-added rules below are allowed.
resource "openstack_networking_secgroup_v2" "cluster" {
  name        = "${var.project_name}-sg"
  description = "Cyclopus cluster: default-deny, IPv4 only (§7.2)"
  region      = var.region
}

# SSH + k3s API restricted to private/administrative CIDRs only.
resource "openstack_networking_secgroup_rule_v2" "ssh" {
  for_each          = toset(var.api_allowed_cidrs)
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 22
  port_range_max    = 22
  remote_ip_prefix  = each.value
  security_group_id = openstack_networking_secgroup_v2.cluster.id
  region            = var.region
}

resource "openstack_networking_secgroup_rule_v2" "k3s_api" {
  for_each          = toset(var.api_allowed_cidrs)
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 6443
  port_range_max    = 6443
  remote_ip_prefix  = each.value
  security_group_id = openstack_networking_secgroup_v2.cluster.id
  region            = var.region
}

# Public HTTP/HTTPS ingress for Traefik on the floating IP.
resource "openstack_networking_secgroup_rule_v2" "http" {
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 80
  port_range_max    = 80
  remote_ip_prefix  = "0.0.0.0/0"
  security_group_id = openstack_networking_secgroup_v2.cluster.id
  region            = var.region
}

resource "openstack_networking_secgroup_rule_v2" "https" {
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 443
  port_range_max    = 443
  remote_ip_prefix  = "0.0.0.0/0"
  security_group_id = openstack_networking_secgroup_v2.cluster.id
  region            = var.region
}

# Intra-cluster traffic between nodes on the private subnet.
resource "openstack_networking_secgroup_rule_v2" "intra_cluster" {
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  remote_ip_prefix  = var.private_network_cidr
  security_group_id = openstack_networking_secgroup_v2.cluster.id
  region            = var.region
}

# flannel pod network (k3s default CNI) VXLAN overlay. Without this the TCP-only
# intra_cluster rule silently drops the encapsulated pod traffic, leaving pods
# unable to reach each other across nodes. 8472/udp is the default flannel VXLAN
# port; 51820/51821 cover the wireguard-native backend if it is selected. All
# scoped to the private CIDR (§7.2).
resource "openstack_networking_secgroup_rule_v2" "flannel_vxlan" {
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "udp"
  port_range_min    = 8472
  port_range_max    = 8472
  remote_ip_prefix  = var.private_network_cidr
  security_group_id = openstack_networking_secgroup_v2.cluster.id
  region            = var.region
}

resource "openstack_networking_secgroup_rule_v2" "flannel_wireguard" {
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "udp"
  port_range_min    = 51820
  port_range_max    = 51821
  remote_ip_prefix  = var.private_network_cidr
  security_group_id = openstack_networking_secgroup_v2.cluster.id
  region            = var.region
}
