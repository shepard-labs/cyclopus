# Detachable ingress address (§9.1). This is the sslip.io-embedded IP that makes
# default hostnames and their certificates survive a node rebuild: DR reattaches
# it to the replacement control-plane node rather than minting a new address.
#
# OVH Public Cloud exposes this as an OpenStack floating IP associated to the
# node's port. The association is a separate resource so DR can detach/reattach
# without destroying the reserved address.

terraform {
  required_providers {
    openstack = {
      source = "terraform-provider-openstack/openstack"
    }
  }
}

# The reserved floating IP itself. Kept independent of any node so its address
# is stable across rebuilds.
resource "openstack_networking_floatingip_v2" "ingress" {
  pool        = "ext-net"
  description = "${var.project_name} ingress floating IP (survives DR, §9.1)"
  region      = var.region
}

# Initial association to the control-plane node. DR reattaches by re-applying
# with control_plane_id set to the replacement instance.
data "openstack_networking_port_v2" "control_plane" {
  device_id = var.control_plane_id
  region    = var.region
}

resource "openstack_networking_floatingip_associate_v2" "ingress" {
  floating_ip = openstack_networking_floatingip_v2.ingress.address
  port_id     = data.openstack_networking_port_v2.control_plane.id
  region      = var.region
}
