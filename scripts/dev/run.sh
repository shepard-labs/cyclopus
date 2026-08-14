#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ENV_FILE="$ROOT/.cyclopus-dev/env"
TAILWIND="$ROOT/.cyclopus-dev/bin/tailwindcss"
TAILWIND_VERSION="v4.3.3"

tailwind_binary_version() {
  local binary="$1"
  local line

  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    if [[ "$line" == '≈ tailwindcss '* ]]; then
      printf '%s\n' "${line#≈ tailwindcss }"
      return 0
    elif [[ "$line" == 'tailwindcss '* ]]; then
      printf '%s\n' "${line#tailwindcss }"
      return 0
    fi
    return 1
  done < <("$binary" --help 2>&1)
  return 1
}

if [[ ! -f "$ENV_FILE" ]]; then
  printf 'local environment is not configured; run scripts/dev/setup.sh first\n' >&2
  exit 1
fi

for command in air templ; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'missing required command: %s\n' "$command" >&2
    exit 1
  fi
done

set -a
source "$ENV_FILE"
set +a

if [[ ! -x "$TAILWIND" ]] || [[ "$(tailwind_binary_version "$TAILWIND" || true)" != "$TAILWIND_VERSION" ]]; then
  printf 'Tailwind CSS %s is not installed; run scripts/dev/setup.sh first\n' "$TAILWIND_VERSION" >&2
  exit 1
fi

cd "$ROOT"

tailwind_pid=''
air_pid=''

terminate_children() {
  local pid

  while IFS= read -r pid; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done < <(jobs -pr)
}

wait_for_children() {
  local pid

  while IFS= read -r pid; do
    [[ -n "$pid" ]] && wait "$pid" 2>/dev/null || true
  done < <(jobs -p)
}

cleanup() {
  trap - EXIT INT TERM
  terminate_children
  wait_for_children
}

handle_signal() {
  local status="$1"

  trap - INT TERM
  terminate_children
  wait_for_children
  exit "$status"
}

trap cleanup EXIT
trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM

$ROOT/.cyclopus-dev/bin/tailwindcss -i "$ROOT/internal/ui/static/css/input.css" -o "$ROOT/internal/ui/static/css/output.css" --minify --watch &
tailwind_pid=$!
air -c .air.toml &
air_pid=$!

while kill -0 "$tailwind_pid" 2>/dev/null && kill -0 "$air_pid" 2>/dev/null; do
  sleep 1
done

tailwind_running=0
air_running=0
kill -0 "$tailwind_pid" 2>/dev/null && tailwind_running=1
kill -0 "$air_pid" 2>/dev/null && air_running=1
tailwind_status=0
air_status=0
set +e
if ((tailwind_running == 0 && air_running == 1)); then
  wait "$tailwind_pid"
  status=$?
  kill "$air_pid" 2>/dev/null
  wait "$air_pid"
elif ((air_running == 0 && tailwind_running == 1)); then
  wait "$air_pid"
  status=$?
  kill "$tailwind_pid" 2>/dev/null
  wait "$tailwind_pid"
else
  wait "$tailwind_pid"
  tailwind_status=$?
  wait "$air_pid"
  air_status=$?
  status="$tailwind_status"
  if ((air_status != 0)); then
    status="$air_status"
  fi
fi
set -e
exit "$status"
