# Provider configuration. NO secrets here.
#
# Both providers read credentials from the environment (see README.md):
#   OVH provider       — OVH_ENDPOINT, OVH_APPLICATION_KEY,
#                        OVH_APPLICATION_SECRET, OVH_CONSUMER_KEY
#   OpenStack provider — OS_AUTH_URL, OS_USERNAME, OS_PASSWORD,
#                        OS_TENANT_ID (project ID), OS_REGION_NAME, etc.
#
# `tofu validate` and `tofu fmt` do not require these to be set; only apply/plan
# against real infrastructure does. The local unit gate runs validate + fmt with
# `-backend=false` and never contacts OVH.

provider "ovh" {
  endpoint = "ovh-eu"
}

provider "openstack" {
  region = var.region
}
