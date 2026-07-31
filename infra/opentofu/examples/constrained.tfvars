# Constrained OVH VPS profile overlay (phases.yaml ovh_vps_constrained).
#
# The capacity gate is enforced at `tofu plan` against the REAL capacity of the
# selected flavor (resolved by data.openstack_compute_flavor_v2 in the compute
# module) versus the hard floors in var.min_control_plane / var.min_data_plane.
# If the chosen flavor is smaller than a floor, the precondition fails the plan
# with a message naming the deficient dimension. The *_profile objects here are
# an optional documentation record of the flavor's expected capacity, not the
# enforcement source.
#
# b3-8 is OVH Public Cloud's smallest general-purpose flavor: 2 vCPU / 8 GB RAM
# / 50 GB disk. That satisfies the default floors (2 vCPU / 4 GB / 40 GB), so
# this overlay plans cleanly; the documented profile below matches the real
# b3-8 capacity so the two are self-consistent.

ovh_service_name = "REPLACE_WITH_OVH_CLOUD_PROJECT_ID"
ssh_public_key   = "ssh-ed25519 AAAA... replace@with.your.key"

region     = "GRA11"
image_name = "Debian 12"

control_plane_flavor  = "b3-8"
control_plane_profile = { vcpus = 2, memory_gb = 8, disk_gb = 50 }

data_plane_flavor  = "b3-8"
data_plane_profile = { vcpus = 2, memory_gb = 8, disk_gb = 50 }
data_plane_count   = 2
