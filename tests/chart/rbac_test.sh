#!/bin/sh
# Helm contract test for least-privilege GitHub App credential Secret RBAC.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
CHART="$ROOT/charts/cyclopus"
HELM="${HELM:-helm}"

fail() { printf 'not ok - %s\n' "$1" >&2; exit 1; }

command -v "$HELM" >/dev/null 2>&1 || fail "helm not found on PATH"
command -v ruby >/dev/null 2>&1 || fail "ruby not found on PATH"

rendered=$(mktemp)
telemetry_rendered=$(mktemp)
webhook_rendered=$(mktemp)
trap 'rm -f "$rendered" "$telemetry_rendered" "$webhook_rendered"' EXIT

"$HELM" template cyclopus "$CHART" --namespace cyclopus-system \
  --set secrets.databaseUrl=postgres://u:p@h/db \
  --set secrets.authTokenPepper=x \
  --set config.externalUrl=https://cyclopus.example.test >"$rendered" \
  || fail "helm template failed"

ruby -ryaml - "$rendered" <<'RUBY' || exit 1
path = ARGV.fetch(0)
documents = YAML.load_stream(File.read(path)).compact
abort "not ok - rendered no Kubernetes objects" if documents.empty?

cluster_roles = documents.select { |doc| doc["kind"] == "ClusterRole" }
cluster_secret_rules = cluster_roles.flat_map { |role| role.fetch("rules", []) }
  .select { |rule| rule.fetch("resources", []).include?("secrets") }
abort "not ok - ClusterRole Secret access is not create-only" unless cluster_secret_rules.length == 1 && cluster_secret_rules.first.fetch("verbs", []) == ["create"]
abort "not ok - ClusterRole grants Secret list/watch/read/mutation" unless (cluster_secret_rules.first.fetch("verbs", []) & %w[get list watch update patch delete]).empty?

roles = documents.select { |doc| doc["kind"] == "Role" }
secret_rules = roles.flat_map { |role| role.fetch("rules", []) }
  .select { |rule| rule.fetch("resources", []).include?("secrets") }
abort "not ok - expected exactly one namespaced Secret rule" unless secret_rules.length == 1

rule = secret_rules.first
abort "not ok - Secret rule is not limited to cyclopus-github-app" unless rule["resourceNames"] == ["cyclopus-github-app"]
abort "not ok - Secret rule verbs exceed get/update/patch" unless rule.fetch("verbs", []).sort == %w[get patch update]
abort "not ok - Secret rule grants list/watch" unless (rule.fetch("verbs", []) & %w[list watch]).empty?

role = roles.find { |candidate| candidate.fetch("rules", []).include?(rule) }
abort "not ok - Secret Role is not release-namespace scoped" unless role.dig("metadata", "namespace") == "cyclopus-system"

secret = documents.find { |doc| doc["kind"] == "Secret" && doc.dig("metadata", "name") == "cyclopus-github-app" }
abort "not ok - deterministic GitHub App Secret is not pre-created" unless secret
abort "not ok - GitHub App Secret is not release-namespace scoped" unless secret.dig("metadata", "namespace") == "cyclopus-system"
abort "not ok - rendered GitHub App Secret contains credential data" unless secret["data"] == {}
abort "not ok - rendered GitHub App Secret contains stringData" if secret.key?("stringData")

puts "ok - ClusterRole Secret access is create-only"
puts "ok - Role permits only get/update/patch on cyclopus-github-app"
puts "ok - unrelated Secrets are excluded by exact resourceNames"
puts "ok - chart pre-creates an empty, namespace-scoped credential Secret"

policy = documents.find { |doc| doc["kind"] == "ValidatingAdmissionPolicy" && doc.dig("metadata", "name").end_with?("managed-secret-create") }
abort "not ok - managed Secret create admission policy missing" unless policy
expression = policy.dig("spec", "validations", 0, "expression").to_s
abort "not ok - managed Secret admission lacks namespace ownership constraint" unless expression.include?("namespaceObject") && expression.include?("managed-by")
abort "not ok - managed Secret admission lacks deterministic name/immutable constraints" unless expression.include?("-env-") && expression.include?("immutable")
puts "ok - destination Secret create is admission-constrained"

rbac_policy = documents.find { |doc| doc["kind"] == "ValidatingAdmissionPolicy" && doc.dig("metadata", "name").end_with?("secret-reader-rbac") }
abort "not ok - source Secret RBAC admission policy missing" unless rbac_policy
rbac_expressions = rbac_policy.fetch("spec").fetch("validations").map { |validation| validation.fetch("expression", "") }.join(" ")
abort "not ok - source Secret Role is not exact-name/get-only constrained" unless rbac_expressions.include?("resourceNames") && rbac_expressions.include?('rule.verbs == ["get"]')
abort "not ok - source Secret RoleBinding identity is not constrained" unless rbac_expressions.include?("cyclopus-source-secret-reader") && rbac_expressions.include?("ServiceAccount")
puts "ok - source Secret reads are exact-name and admission-constrained"
RUBY

"$HELM" template cyclopus "$CHART" --namespace cyclopus-system \
  --set secrets.databaseUrl=postgres://u:p@h/db \
  --set secrets.authTokenPepper=x \
  --set config.externalUrl=https://cyclopus.example.test \
  --set-string config.observability.openobserveCredentialSecret=k8s-secret://cyclopus-system/telemetry-auth#credential \
  --set-string config.observability.beszelCredentialSecret=k8s-secret://cyclopus-system/telemetry-auth#identity \
  >"$telemetry_rendered" || fail "helm template with telemetry refs failed"

ruby -ryaml - "$telemetry_rendered" <<'RUBY' || exit 1
documents = YAML.load_stream(File.read(ARGV.fetch(0))).compact
role = documents.find { |doc| doc["kind"] == "Role" && doc.dig("metadata", "name").end_with?("telemetry-secrets") }
abort "not ok - telemetry Secret Role missing" unless role
rule = role.fetch("rules").fetch(0)
abort "not ok - telemetry Secret names were not merged" unless rule["resourceNames"] == ["telemetry-auth"]
abort "not ok - telemetry Secret Role is not get-only" unless rule["verbs"] == ["get"]
abort "not ok - telemetry Secret Role grants list/watch" unless (rule["verbs"] & %w[list watch]).empty?
puts "ok - telemetry Secret refs render one exact-name get-only Role"
RUBY

if "$HELM" template cyclopus "$CHART" --namespace cyclopus-system \
  --set secrets.databaseUrl=postgres://u:p@h/db \
  --set secrets.authTokenPepper=x \
  --set config.externalUrl=https://cyclopus.example.test \
  --set-string config.observability.openobserveCredentialSecret=k8s-secret://other/telemetry-auth#credential \
  >/dev/null 2>&1; then
  fail "cross-namespace telemetry Secret ref rendered successfully"
fi
printf '%s\n' "ok - cross-namespace telemetry Secret refs fail chart rendering"

"$HELM" template release "$CHART" --namespace cyclopus-system \
  --set secrets.databaseUrl=postgres://u:p@h/db \
  --set secrets.authTokenPepper=x \
  --set config.externalUrl=https://cyclopus.example.test \
  --set-string 'config.webhook.allowedSecretRefs[0]=k8s-secret://cyclopus-system/cyclopus-webhook-primary#secret' \
  --set-string 'config.webhook.allowedSecretRefs[1]=k8s-secret://cyclopus-system/cyclopus-webhook-primary#other' \
  >"$webhook_rendered" || fail "helm template with webhook refs failed"

ruby -ryaml - "$webhook_rendered" <<'RUBY' || exit 1
documents = YAML.load_stream(File.read(ARGV.fetch(0))).compact
role = documents.find { |doc| doc["kind"] == "Role" && doc.dig("metadata", "name") == "cyclopus-webhook-secret-reader" }
abort "not ok - webhook Secret Role missing" unless role
rule = role.fetch("rules").fetch(0)
abort "not ok - webhook Secret names were not deduplicated" unless rule["resourceNames"] == ["cyclopus-webhook-primary"]
abort "not ok - webhook Secret Role is not exact-name get-only" unless rule["verbs"] == ["get"]
abort "not ok - webhook Secret Role grants unrelated access" unless (rule["verbs"] & %w[list watch create update patch delete]).empty?
config = documents.find { |doc| doc["kind"] == "ConfigMap" }.fetch("data")
abort "not ok - webhook allowlist config missing" unless config["WEBHOOK_ALLOWED_SECRET_REFS"].include?("cyclopus-webhook-primary")
abort "not ok - webhook release ownership config missing" unless config["WEBHOOK_RELEASE_NAME"] == "release"
puts "ok - webhook Secret refs render one deduplicated exact-name get-only Role"
RUBY

for invalid in \
  'k8s-secret://other/cyclopus-webhook-primary#secret' \
  'k8s-secret://cyclopus-system/arbitrary#secret' \
  'not-a-reference'
do
  if "$HELM" template release "$CHART" --namespace cyclopus-system \
    --set secrets.databaseUrl=postgres://u:p@h/db \
    --set secrets.authTokenPepper=x \
    --set config.externalUrl=https://cyclopus.example.test \
    --set-string "config.webhook.allowedSecretRefs[0]=$invalid" \
    >/dev/null 2>&1; then
    fail "invalid webhook Secret ref $invalid rendered successfully"
  fi
done
printf '%s\n' "ok - malformed, cross-namespace, and arbitrary webhook Secret refs fail chart rendering"
