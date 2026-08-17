#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=setup.sh
source "$SCRIPT_DIR/setup.sh"

TEST_TMP_PARENT=/var/folders/4r/t9xgfxbx1glcf1f0h482klp80000gn/T/opencode
if [[ ! -d "$TEST_TMP_PARENT" ]]; then
  printf 'required test sandbox parent does not exist: %s\n' "$TEST_TMP_PARENT" >&2
  exit 1
fi
if [[ ! -w "$TEST_TMP_PARENT" ]]; then
  printf 'required test sandbox parent is not writable: %s\n' "$TEST_TMP_PARENT" >&2
  exit 1
fi
TEST_TMP_ROOT=$(mktemp -d "$TEST_TMP_PARENT/cyclopus-setup-test.XXXXXX")
trap 'rm -rf "$TEST_TMP_ROOT"' EXIT

assert_eq() {
  local want="$1"
  local got="$2"
  local message="$3"
  if [[ "$got" != "$want" ]]; then
    printf 'assertion failed: %s\nwant: %s\n got: %s\n' "$message" "$want" "$got" >&2
    return 1
  fi
}

assert_platform() {
  local os="$1"
  local arch="$2"
  local asset="$3"
  local hash="$4"
  local got

  got=$(tailwind_platform_asset "$os" "$arch")
  assert_eq \
    "$TAILWIND_RELEASE_URL/$asset $hash" \
    "$got" \
    "$os/$arch selects the approved asset and literal checksum"
}

test_platform_matrix() {
  assert_platform Darwin arm64 tailwindcss-macos-arm64 \
    cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d
  assert_platform Darwin x86_64 tailwindcss-macos-x64 \
    7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1
  assert_platform Linux arm64 tailwindcss-linux-arm64 \
    55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195
  assert_platform Linux aarch64 tailwindcss-linux-arm64 \
    55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195
  assert_platform Linux x86_64 tailwindcss-linux-x64 \
    dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a
  assert_platform Linux amd64 tailwindcss-linux-x64 \
    dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a
}

test_unsupported_platforms_are_rejected() {
  if tailwind_platform_asset FreeBSD x86_64 >/dev/null 2>&1; then
    printf 'unsupported operating system was accepted\n' >&2
    return 1
  fi
  if tailwind_platform_asset Linux riscv64 >/dev/null 2>&1; then
    printf 'unsupported architecture was accepted\n' >&2
    return 1
  fi
}

write_version_binary() {
  local path="$1"
  local output="$2"

  cat > "$path" <<BINARY
#!/usr/bin/env bash
if [[ "\${1:-}" == "--help" ]]; then
  printf '%s\\n' '$output'
fi
BINARY
  chmod 755 "$path"
}

test_version_matching_is_exact() {
  local case_dir="$TEST_TMP_ROOT/version-matching"
  local binary="$case_dir/tailwindcss"
  local approved_hash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  local failures=0

  mkdir -p "$case_dir"
  TEST_ACTUAL_HASH=$approved_hash
  write_version_binary "$binary" '≈ tailwindcss v4.3.3'
  if ! tailwind_binary_is_approved "$binary" "$approved_hash" test-checksum-tool; then
    printf 'exact Tailwind v4.3.3 version was rejected\n' >&2
    failures=$((failures + 1))
  fi

  write_version_binary "$binary" '≈ tailwindcss v4.3.30'
  if tailwind_binary_is_approved "$binary" "$approved_hash" test-checksum-tool; then
    printf 'Tailwind v4.3.30 was accepted as exact v4.3.3\n' >&2
    failures=$((failures + 1))
  fi

  write_version_binary "$binary" 'arbitrary text mentioning ≈ tailwindcss v4.3.3 is not a version response'
  if tailwind_binary_is_approved "$binary" "$approved_hash" test-checksum-tool; then
    printf 'arbitrary text was accepted as the Tailwind v4.3.3 version contract\n' >&2
    failures=$((failures + 1))
  fi

  ((failures == 0))
}

uname() {
  case "${1:-}" in
    -s) printf '%s\n' "$TEST_OS" ;;
    -m) printf '%s\n' "$TEST_ARCH" ;;
    *) return 1 ;;
  esac
}

curl() {
  local url=''
  local output=''

  while (($#)); do
    case "$1" in
      -o)
        output="$2"
        shift 2
        ;;
      http://*|https://*)
        url="$1"
        shift
        ;;
      *)
        shift
        ;;
    esac
  done

  [[ -n "$url" && -n "$output" ]] || return 1
  TEST_CURL_CALLS=$((TEST_CURL_CALLS + 1))
  if [[ "$url" == "$TAILWIND_RELEASE_URL/sha256sums.txt" ]]; then
    printf '%s  ./%s\n' "$TEST_MANIFEST_HASH" "$TEST_ASSET_NAME" > "$output"
  else
    assert_eq "$TAILWIND_RELEASE_URL/$TEST_ASSET_NAME" "$url" 'installer downloads the selected release asset'
    cat > "$output" <<'BINARY'
#!/usr/bin/env bash
if [[ "${1:-}" == "--help" ]]; then
  printf '≈ tailwindcss v4.3.3\n'
fi
BINARY
  fi
}

tailwind_sha256() {
  if grep -Fq 'UNTRUSTED_EXISTING_BINARY' "$1"; then
    printf '%s\n' 'ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff'
    return
  fi
  printf '%s\n' "$TEST_ACTUAL_HASH"
}

configure_linux_x64_download() {
  TEST_OS=Linux
  TEST_ARCH=x86_64
  TEST_ASSET_NAME=tailwindcss-linux-x64
  TEST_MANIFEST_HASH=dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a
  TEST_ACTUAL_HASH=$TEST_MANIFEST_HASH
  TEST_CURL_CALLS=0
}

test_existing_executable_with_wrong_checksum_is_reinstalled() {
  local case_dir="$TEST_TMP_ROOT/untrusted-existing"
  local destination="$case_dir/tailwindcss"

  mkdir -p "$case_dir"
  cat > "$destination" <<'UNTRUSTED_BINARY'
#!/usr/bin/env bash
# UNTRUSTED_EXISTING_BINARY
if [[ "${1:-}" == "--help" ]]; then
  printf '≈ tailwindcss v4.3.3\n'
fi
UNTRUSTED_BINARY
  chmod 755 "$destination"

  configure_linux_x64_download
  install_tailwind "$destination"

  if grep -Fq 'UNTRUSTED_EXISTING_BINARY' "$destination"; then
    printf 'existing executable with the wrong checksum was trusted instead of reinstalled\n' >&2
    return 1
  fi
  if ((TEST_CURL_CALLS != 2)); then
    printf 'expected asset and checksum downloads when replacing untrusted executable, got %s calls\n' "$TEST_CURL_CALLS" >&2
    return 1
  fi
  if [[ ! -x "$destination" ]] ||
    ! tailwind_binary_is_approved "$destination" "$TEST_MANIFEST_HASH" test-checksum-tool; then
    printf 'replacement for untrusted executable is not executable Tailwind v4.3.3\n' >&2
    return 1
  fi
}

test_checksum_mismatch_preserves_existing_binary() {
  local case_dir="$TEST_TMP_ROOT/checksum-mismatch"
  local destination="$case_dir/tailwindcss"
  local before="$case_dir/before"

  mkdir -p "$case_dir"
  cat > "$destination" <<'OLD_BINARY'
#!/usr/bin/env bash
printf 'tailwindcss v3.4.0\n'
OLD_BINARY
  chmod 755 "$destination"
  cp "$destination" "$before"

  configure_linux_x64_download
  TEST_ACTUAL_HASH=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
  if install_tailwind "$destination" >/dev/null 2>&1; then
    printf 'installation succeeded despite a checksum mismatch\n' >&2
    return 1
  fi
  if ! cmp -s "$before" "$destination"; then
    printf 'checksum failure changed the existing destination binary\n' >&2
    return 1
  fi
}

test_verified_installation_is_executable_and_current() {
  local case_dir="$TEST_TMP_ROOT/success"
  local destination="$case_dir/bin/tailwindcss"
  local version_output

  configure_linux_x64_download
  install_tailwind "$destination"

  if [[ ! -x "$destination" ]]; then
    printf 'verified installation did not produce an executable destination\n' >&2
    return 1
  fi
  version_output=$("$destination" --help 2>&1)
  assert_eq '≈ tailwindcss v4.3.3' "$version_output" 'installed binary satisfies the exact v4.3.3 contract'
  if ! tailwind_binary_is_approved "$destination" "$TEST_MANIFEST_HASH" test-checksum-tool; then
    printf 'installed binary was not recognized as Tailwind v4.3.3\n' >&2
    return 1
  fi
  if compgen -G "$case_dir/bin/.tailwindcss.*.tmp" >/dev/null ||
    compgen -G "$case_dir/bin/.tailwind-download.*" >/dev/null; then
    printf 'successful installation left staging artifacts beside the destination\n' >&2
    return 1
  fi
}

test_ensure_env_var_is_idempotent_and_non_clobbering() {
  local env_file="$TEST_TMP_ROOT/env"

  printf "export CYCLOPUS_EXTERNAL_URL='https://custom.example'\n" > "$env_file"
  ensure_env_var "$env_file" CYCLOPUS_EXTERNAL_URL http://127.0.0.1:8081
  ensure_env_var "$env_file" CYCLOPUS_KUBERNETES_INSTANCE_NAMESPACE cyclopus
  ensure_env_var "$env_file" CYCLOPUS_KUBERNETES_INSTANCE_NAMESPACE cyclopus

  assert_eq 1 "$(grep -c '^export CYCLOPUS_EXTERNAL_URL=' "$env_file")" 'existing env key is not duplicated'
  assert_eq "export CYCLOPUS_EXTERNAL_URL='https://custom.example'" "$(grep '^export CYCLOPUS_EXTERNAL_URL=' "$env_file")" 'existing env value is not clobbered'
  assert_eq 1 "$(grep -c '^export CYCLOPUS_KUBERNETES_INSTANCE_NAMESPACE=' "$env_file")" 'new env key is appended once'
}

run_test() {
  local name="$1"
  if "$name"; then
    printf 'ok - %s\n' "$name"
  else
    printf 'not ok - %s\n' "$name" >&2
    TEST_FAILURES=$((TEST_FAILURES + 1))
  fi
}

TEST_FAILURES=0
run_test test_platform_matrix
run_test test_unsupported_platforms_are_rejected
run_test test_version_matching_is_exact
run_test test_existing_executable_with_wrong_checksum_is_reinstalled
run_test test_checksum_mismatch_preserves_existing_binary
run_test test_verified_installation_is_executable_and_current
run_test test_ensure_env_var_is_idempotent_and_non_clobbering

if ((TEST_FAILURES > 0)); then
  printf '%s test group(s) failed\n' "$TEST_FAILURES" >&2
  exit 1
fi
