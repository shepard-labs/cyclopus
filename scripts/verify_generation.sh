#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
workspace=$(mktemp -d)
trap 'rm -rf "$workspace"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

cp "$root/go.mod" "$root/go.sum" "$root/buf.yaml" "$root/buf.lock" "$root/buf.gen.yaml" "$workspace/"
cp -R "$root/api" "$workspace/api"
mkdir -p "$workspace/cmd" "$workspace/internal/apidocs"
cp -R "$root/cmd/gen-openapi" "$workspace/cmd/gen-openapi"
cd "$workspace"

remote_failure_is_transient() {
  if grep -Eiq '(^|[[:space:]])[^[:space:]]+\.proto:[0-9]+(:[0-9]+)?:|((parse|parsing|syntax|config|configuration|template)[^[:cntrl:]]*(error|invalid))|(invalid|malformed)[^[:cntrl:]]*(proto|config|configuration|template)|unknown([^[:cntrl:]]*)plugin|plugin[^[:cntrl:]]*(unknown|unsupported|invalid|not found)' "$1"; then
    return 1
  fi
  grep -Eiq 'rpc error: code = (ResourceExhausted|Unavailable|DeadlineExceeded)([^[:alnum:]]|$)|HTTP[^[:cntrl:]]*status([^[:alnum:]]+code)?[^[:alnum:]]+429([^0-9]|$)|(plugin[^[:cntrl:]]*download|download[^[:cntrl:]]*plugin)[^[:cntrl:]]*(HTTP[^[:cntrl:]]*)?(status([^[:alnum:]]+code)?[^[:alnum:]]+)?429([^0-9]|$)|lookup .*: (no such host|server misbehaving|temporary failure in name resolution)|(dial (tcp|udp)|connect): .*network is unreachable|dial (tcp|udp).*: (connect: )?connection timed out|dial (tcp|udp) .*: i/o timeout|(transport|connection): .*i/o timeout|TLS handshake timeout|connection (reset by peer|refused)|transport: unexpected EOF|remote[^:]*: unexpected EOF|(fetch|download|resolve)[^:]*: unexpected EOF|((buf[^:]* (fetch|download|resolve|transport)|(fetch|download|resolve|transport)[^:]* buf|plugin[^:]* download|download[^:]* plugin)[^:]*): EOF([[:space:]]|$)' "$1"
}

remote_fallback_eligible=0

generate_remote() {
  attempts=${VERIFY_GENERATION_REMOTE_ATTEMPTS:-3}
  case "$attempts" in
    ''|*[!0-9]*|0)
      printf '%s\n' 'VERIFY_GENERATION_REMOTE_ATTEMPTS must be a positive integer' >&2
      return 1
      ;;
  esac
  printf '%s\n' 'generation verification: validating remote Buf template' >&2
  if go run github.com/bufbuild/buf/cmd/buf@v1.57.0 config migrate --diff --buf-gen-yaml buf.gen.yaml >/dev/null; then
    :
  else
    template_status=$?
    printf '%s\n' 'generation verification: remote Buf template is invalid; local fallback will not run' >&2
    return "$template_status"
  fi
  if ruby "$root/scripts/derive_buf_gen_local.rb" buf.gen.yaml buf.gen.local.yaml local-plugin-installs; then
    :
  else
    template_status=$?
    printf '%s\n' 'generation verification: remote Buf template cannot be represented by the local fallback; local fallback will not run' >&2
    return "$template_status"
  fi
  attempt=1
  while [ "$attempt" -le "$attempts" ]; do
    printf 'generation verification: remote Buf generation (attempt %s/%s)\n' "$attempt" "$attempts" >&2
    if go run github.com/bufbuild/buf/cmd/buf@v1.57.0 generate >"$workspace/remote-generate.stdout" 2>"$workspace/remote-generate.stderr"; then
      cat "$workspace/remote-generate.stdout" >&2
      cat "$workspace/remote-generate.stderr" >&2
      return 0
    else
      remote_status=$?
    fi
    cat "$workspace/remote-generate.stdout" >&2
    cat "$workspace/remote-generate.stderr" >&2
    if ! remote_failure_is_transient "$workspace/remote-generate.stderr"; then
      printf '%s\n' 'generation verification: remote generation failed with a non-transient proto/config/plugin error; local fallback will not run' >&2
      return "$remote_status"
    fi
    attempt=$((attempt + 1))
    if [ "$attempt" -le "$attempts" ]; then
      sleep 2
    fi
  done
  remote_fallback_eligible=1
  return "$remote_status"
}

generate_local() {
  plugin_bin="$workspace/plugin-bin"
  mkdir -p "$plugin_bin"
  printf '%s\n' 'generation verification: installing exact local protoc plugins in isolated temporary GOBIN' >&2
  while IFS="$(printf '\t')" read -r module version; do
    GOBIN="$plugin_bin" go install "$module@$version"
  done <local-plugin-installs
  printf '%s\n' 'generation verification: local fallback generation' >&2
  PATH="$plugin_bin:$PATH" go run github.com/bufbuild/buf/cmd/buf@v1.57.0 generate --template buf.gen.local.yaml
}

if generate_remote; then
  :
else
  remote_status=$?
  if [ "$remote_fallback_eligible" -ne 1 ]; then
    exit "$remote_status"
  fi
  printf '%s\n' 'generation verification: recognized remote availability/rate-limit failure; using local fallback' >&2
  generate_local
fi

go run ./cmd/gen-openapi gen/openapi/cyclopus/v1/cyclopus.swagger.json internal/apidocs/openapi.json

differences=0

compare_directory() {
  label=$1
  expected=$2
  actual=$3
  if [ ! -d "$expected" ]; then
    printf 'generation verification: missing checked-in %s directory: %s\n' "$label" "$expected" >&2
    differences=1
  elif [ ! -d "$actual" ]; then
    printf 'generation verification: generation did not produce %s directory: %s\n' "$label" "$actual" >&2
    differences=1
  elif ! diff -ru "$expected" "$actual"; then
    printf 'generation verification: %s output differs\n' "$label" >&2
    differences=1
  fi
}

compare_file() {
  label=$1
  expected=$2
  actual=$3
  if [ ! -f "$expected" ]; then
    printf 'generation verification: missing checked-in %s file: %s\n' "$label" "$expected" >&2
    differences=1
  elif [ ! -f "$actual" ]; then
    printf 'generation verification: generation did not produce %s file: %s\n' "$label" "$actual" >&2
    differences=1
  elif ! diff -u "$expected" "$actual"; then
    printf 'generation verification: %s output differs\n' "$label" >&2
    differences=1
  fi
}

compare_directory 'Go' "$root/gen/go" "$workspace/gen/go"
compare_directory 'Swagger' "$root/gen/openapi" "$workspace/gen/openapi"
compare_file 'canonical OpenAPI' "$root/internal/apidocs/openapi.json" "$workspace/internal/apidocs/openapi.json"

if [ "$differences" -ne 0 ]; then
  printf '%s\n' 'generated API files are stale; regenerate protobuf, Swagger, and OpenAPI artifacts' >&2
  exit 1
fi
