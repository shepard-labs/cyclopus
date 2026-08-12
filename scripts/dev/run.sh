#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ENV_FILE="$ROOT/.cyclopus-dev/env"

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

# Unauthenticated component preview routes for headless design review.
export CYCLOPUS_UI_PREVIEW=1

cd "$ROOT"
exec air -c .air.toml
