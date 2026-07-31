#!/bin/sh
set -eu

DIR="$(dirname -- "$0")/integration"

case "${1:-}" in
  deploy)
    exec sh "$DIR/deploy_orbstack.sh"
    ;;
  security)
    exec sh "$DIR/security_orbstack.sh"
    ;;
  install)
    exec sh "$DIR/install_orbstack.sh"
    ;;
  install-ubuntu)
    exec env INSTALL_DISTRO=ubuntu sh "$DIR/install_orbstack.sh"
    ;;
  install-debian)
    # orbstack-debian gate: run the Debian install scenario (M5).
    exec env INSTALL_DISTRO=debian sh "$DIR/install_orbstack.sh"
    ;;
  recovery|disaster-recovery)
    exec sh "$DIR/dr_orbstack.sh"
    ;;
  dr-ubuntu)
    exec env DR_DISTRO=ubuntu sh "$DIR/dr_orbstack.sh"
    ;;
  dr-debian)
    # orbstack-debian gate: run the Debian disaster-recovery scenario (M5).
    exec env DR_DISTRO=debian sh "$DIR/dr_orbstack.sh"
    ;;
  ovh-public-cloud|ovh-vps-constrained|ovh-capabilities)
    # These require live OVH Public Cloud credentials, unavailable here. Each
    # emits well-formed BLOCKED gate evidence and exits with the BLOCKED code (2)
    # rather than faking a pass. The offline tofu validate/fmt logic they would
    # run is covered by scripts/test_infrastructure.sh (the unit gate).
    exec sh "$DIR/ovh_blocked.sh" "$1"
    ;;
  runtime|hostname-provider) ;;
  *)
    printf '%s\n' "usage: $0 {deploy|security|runtime|hostname-provider|install|install-ubuntu|install-debian|ovh-public-cloud|ovh-vps-constrained|ovh-capabilities|recovery|disaster-recovery|dr-ubuntu|dr-debian}" >&2
    exit 64
    ;;
esac

printf '%s\n' "BLOCKED: integration contract '$1' is defined, but its phase implementation and test infrastructure do not exist yet." >&2
exit 2
