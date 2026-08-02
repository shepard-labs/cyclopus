#!/bin/sh
# Generate versions.env from versions.yaml (single source of truth for pins).
# Writes to the path given as $1 (default: <repo root>/versions.env); pass "-"
# to print to stdout.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT=${1:-"$ROOT/versions.env"}

generate() {
  ruby - "$ROOT/versions.yaml" <<'RUBY'
require "yaml"

path = ARGV.fetch(0)
doc = YAML.safe_load(File.read(path), aliases: false)
components = doc["components"] || abort("versions.yaml: missing components")

get = lambda do |name, field|
  value = components.dig(name, field)
  abort("versions.yaml: missing #{name}.#{field}") if value.nil? || value.to_s.empty?
  value.to_s
end
strip_v = ->(value) { value.sub(/\Av/, "") }

lines = [
  ["K3S_VERSION", get.call("k3s", "pin")],
  ["HELM_VERSION", get.call("helm", "pin")],
  ["TRAEFIK_CHART_VERSION", get.call("traefik", "chart_pin")],
  ["KYVERNO_VERSION", get.call("kyverno", "pin")],
  ["KYVERNO_CHART_VERSION", get.call("kyverno", "chart_pin")],
  ["CERT_MANAGER_VERSION", get.call("cert_manager", "pin")],
  ["KEDA_VERSION", strip_v.call(get.call("keda", "pin"))],
  ["KEDA_HTTP_ADDON_VERSION", strip_v.call(get.call("keda_http_add_on", "pin"))],
  ["PG_MAJOR", get.call("postgres", "pin")],
  ["GO_VERSION", get.call("go", "toolchain_pin")],
  ["GVISOR_RELEASE", get.call("gvisor", "pin")],
  ["WAL_G_VERSION", get.call("wal_g", "pin")],
  ["OTEL_COLLECTOR_VERSION", strip_v.call(get.call("opentelemetry_collector", "pin"))],
  ["OPENOBSERVE_VERSION", get.call("openobserve", "pin")],
  ["BESZEL_VERSION", strip_v.call(get.call("beszel", "pin"))],
]

puts "# GENERATED from versions.yaml by scripts/generate_versions_env.sh — do not edit by hand"
lines.each { |name, value| puts "#{name}='#{value}'" }
RUBY
}

if [ "$OUT" = "-" ]; then
  generate
else
  generate > "$OUT"
fi
