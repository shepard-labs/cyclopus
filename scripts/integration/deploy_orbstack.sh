#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DISTRO=${DEPLOY_DISTRO:-${RECOVERY_DISTRO:-ubuntu}}

case "$DISTRO" in
  ubuntu)
    IMAGE=${DEPLOY_IMAGE:-ubuntu:24.04}
    VM=${DEPLOY_VM_NAME:-cyclopus-deploy-ubuntu}
    ;;
  debian)
    IMAGE=${DEPLOY_IMAGE:-debian:12}
    VM=${DEPLOY_VM_NAME:-cyclopus-deploy-debian}
    ;;
  *)
    printf '%s\n' "deploy: DEPLOY_DISTRO must be ubuntu or debian, got '$DISTRO'" >&2
    exit 64
    ;;
esac

command -v orb >/dev/null 2>&1 || {
  printf '%s\n' "deploy: OrbStack CLI 'orb' is required" >&2
  exit 2
}

vm_exists=false
if orb info "$VM" >/dev/null 2>&1; then
  vm_exists=true
fi
if [ "$vm_exists" = true ]; then
  observed=$(orb -m "$VM" sh -c '. /etc/os-release && printf %s "$ID"')
  observed_version=$(orb -m "$VM" sh -c '. /etc/os-release && printf %s "$VERSION_ID"')
  expected_version=24.04
  [ "$DISTRO" = debian ] && expected_version=12
  if [ "$observed" != "$DISTRO" ] || [ "$observed_version" != "$expected_version" ] || [ "${DEPLOY_RECREATE:-0}" = 1 ]; then
    printf '%s\n' "deploy: recreating $VM (observed $observed $observed_version)"
    orb delete --force "$VM"
    vm_exists=false
  else
    printf '%s\n' "deploy: reusing $DISTRO $observed_version VM $VM"
  fi
fi
if [ "$vm_exists" = false ]; then
  printf '%s\n' "deploy: creating $IMAGE VM $VM"
  orb create --cpus "${DEPLOY_CPUS:-4}" --memory "${DEPLOY_MEMORY:-6G}" --disk "${DEPLOY_DISK:-30G}" "$IMAGE" "$VM"
fi

stage=/tmp/cyclopus-deploy-source
printf '%s\n' "deploy: staging dirty worktree and testing in $VM"
orb -m "$VM" rm -rf "$stage"
COPYFILE_DISABLE=1 tar -C "$ROOT" --exclude='.git' --exclude='.codegraph' -cf - . |
  orb -m "$VM" sh -c "mkdir -p '$stage' && tar -C '$stage' -xf -"
orb -m "$VM" env CYCLOPUS_DEPLOY_ROOT="$stage" CYCLOPUS_DEPLOY_DISTRO="$DISTRO" \
  sh "$stage/scripts/integration/deploy_guest.sh"
