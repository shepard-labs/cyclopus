# Root version and provider pins for the Cyclopus OVH Public Cloud install.
#
# OpenTofu is pinned to versions.yaml components.opentofu.pin (1.10.1). The OVH
# and OpenStack providers are pinned to reviewed releases; OVH Public Cloud is
# an OpenStack region, so instances/network/storage are driven through the
# OpenStack provider while OVH-specific resources (failover/floating IP,
# S3-compatible object storage credentials) use the OVH provider.
terraform {
  required_version = "= 1.10.1"

  required_providers {
    ovh = {
      source  = "ovh/ovh"
      version = "= 2.0.0"
    }
    openstack = {
      source  = "terraform-provider-openstack/openstack"
      version = "= 3.0.0"
    }
  }
}
