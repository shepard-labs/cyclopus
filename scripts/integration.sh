#!/bin/sh
set -eu

case "${1:-}" in
  recovery|disaster-recovery)
    exec sh "$(dirname -- "$0")/integration/recovery_orbstack.sh"
    ;;
  deploy|security|runtime|hostname-provider|install|ovh-public-cloud|ovh-vps-constrained|ovh-capabilities) ;;
  *)
    printf '%s\n' "usage: $0 {deploy|security|runtime|hostname-provider|install|ovh-public-cloud|ovh-vps-constrained|ovh-capabilities|recovery|disaster-recovery}" >&2
    exit 64
    ;;
esac

printf '%s\n' "BLOCKED: integration contract '$1' is defined, but its phase implementation and test infrastructure do not exist yet." >&2
exit 2
