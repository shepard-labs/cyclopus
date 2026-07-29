#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
backup=$(mktemp -d)
trap 'rm -rf "$root/gen/go"; cp -R "$backup/go" "$root/gen/go"; rm -rf "$backup"' EXIT HUP INT TERM

cp -R "$root/gen/go" "$backup/go"
cd "$root"
go run github.com/bufbuild/buf/cmd/buf@v1.57.0 generate

if ! diff -ru "$backup/go" "$root/gen/go"; then
  printf '%s\n' 'generated protobuf files are stale; run: go run github.com/bufbuild/buf/cmd/buf@v1.57.0 generate' >&2
  exit 1
fi
