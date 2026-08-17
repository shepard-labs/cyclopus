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

"$HELM" template cyclopus "$CHART" --namespace cyclopus-system --kube-version 1.34.0 \
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

runtime_role = cluster_roles.find { |role| role.dig("metadata", "name").end_with?("-reconcile") }
runtime_bounds_rules = runtime_role.fetch("rules").select { |candidate| (candidate.fetch("resources", []) & %w[resourcequotas limitranges]).any? }
abort "not ok - runtime resource bounds access is not read-only" unless runtime_bounds_rules.length == 1 && runtime_bounds_rules.first.fetch("verbs", []).sort == %w[get list watch]
provisioner_role = cluster_roles.find { |role| role.dig("metadata", "name").end_with?("-provisioner") }
provisioner_bounds_rules = provisioner_role.fetch("rules").select { |candidate| (candidate.fetch("resources", []) & %w[resourcequotas limitranges]).any? }
abort "not ok - provisioner resource bounds mutation rule missing" unless provisioner_bounds_rules.length == 1 && provisioner_bounds_rules.first.fetch("verbs", []).sort == %w[create get update]

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
puts "ok - runtime quota/limit access is read-only and provisioner mutation remains admission-gated"

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

bounds_policy = documents.find { |doc| doc["kind"] == "ValidatingAdmissionPolicy" && doc.dig("metadata", "name").end_with?("preview-resource-bounds") }
abort "not ok - preview resource bounds admission policy missing" unless bounds_policy
abort "not ok - preview resource bounds policy is not fail-closed" unless bounds_policy.dig("spec", "failurePolicy") == "Fail"
rules = bounds_policy.dig("spec", "matchConstraints", "resourceRules")
abort "not ok - preview resource bounds policy match is incomplete" unless rules == [{"apiGroups" => [""], "apiVersions" => ["v1"], "operations" => ["CREATE", "UPDATE"], "resources" => ["resourcequotas", "limitranges"]}]
match_conditions = bounds_policy.dig("spec", "matchConditions")
abort "not ok - preview resource bounds policy lacks scoped match condition" unless match_conditions&.length == 1
match_expression = match_conditions.first.fetch("expression", "")
%w[request.userInfo.username cyclopus-preview namespaceObject app.kubernetes.io/managed-by].each do |fragment|
  abort "not ok - preview resource bounds match condition lacks #{fragment}" unless match_expression.include?(fragment)
end
expressions = bounds_policy.fetch("spec").fetch("validations").map { |validation| validation.fetch("expression") }
required_fragments = [
  "system:serviceaccount:cyclopus-system:cyclopus-provisioner", "namespaceObject != null", "^preview-",
  "cyclopus.dev/namespace-owner", "cyclopus.dev/namespace-uid", "cyclopus.dev/environment-id",
  "cyclopus.dev/org-id", "cyclopus.dev/app-id", 'object.metadata.name == "cyclopus-preview"',
  'object.metadata.labels.size() == 4', 'oldObject.metadata.labels.size() == 4',
  'oldObject.metadata.labels.size() == 5', '"cyclopus.app/policies-pending" in oldObject.metadata.labels',
  'request.operation != "UPDATE"', 'request.resource.resource != "resourcequotas"',
  '!has(object.spec.scopes)', '!has(object.spec.scopeSelector)', 'object.spec.hard.size() == 5', 'object.spec.hard["pods"] == quantity("1")',
  'request.resource.resource != "limitranges"', 'object.spec.limits.size() == 1',
  'object.spec.limits[0].type == "Container"', '!has(object.spec.limits[0].min)',
  '!has(object.spec.limits[0].max)', '!has(object.spec.limits[0].maxLimitRequestRatio)'
]
required_fragments.each do |fragment|
  abort "not ok - preview resource bounds policy lacks #{fragment}" unless expressions.any? { |expression| expression.include?(fragment) }
end
abort 'not ok - preview resource bounds policy uses invalid has(map[index]) syntax' if expressions.any? { |expression| expression.include?('has(oldObject.metadata.labels["cyclopus.app/policies-pending"])') }
binding = documents.find { |doc| doc["kind"] == "ValidatingAdmissionPolicyBinding" && doc.dig("metadata", "name").end_with?("preview-resource-bounds") }
abort "not ok - preview resource bounds binding missing or non-denying" unless binding&.dig("spec", "validationActions") == ["Deny"]

namespace = {
  "metadata" => {"name" => "preview-calm-otter-0001", "uid" => "namespace-uid",
    "labels" => {"app.kubernetes.io/managed-by" => "cyclopus", "cyclopus.dev/environment-id" => "environment", "cyclopus.dev/org-id" => "org", "cyclopus.dev/app-id" => "app"},
    "annotations" => {"cyclopus.dev/namespace-owner" => "environment", "cyclopus.dev/namespace-uid" => "namespace-uid"}}
}
quota = {"kind" => "ResourceQuota", "metadata" => {"name" => "cyclopus-preview", "namespace" => namespace.dig("metadata", "name"), "labels" => namespace.dig("metadata", "labels").dup},
  "spec" => {"hard" => {"pods" => "1", "requests.cpu" => "100m", "limits.cpu" => "200m", "requests.memory" => "128Mi", "limits.memory" => "256Mi"}}}
limit = {"kind" => "LimitRange", "metadata" => quota.fetch("metadata").transform_values { |value| value.is_a?(Hash) ? value.dup : value },
  "spec" => {"limits" => [{"type" => "Container", "defaultRequest" => {"cpu" => "100m", "memory" => "128Mi"}, "default" => {"cpu" => "200m", "memory" => "256Mi"}}]}}
request = {"username" => "system:serviceaccount:cyclopus-system:cyclopus-provisioner", "operation" => "CREATE", "resource" => "resourcequotas", "namespace" => namespace, "object" => quota}

allowed = lambda do |fixture|
  ns = fixture.fetch("namespace").fetch("metadata")
  obj = fixture.fetch("object")
  labels = obj.dig("metadata", "labels") || {}
  ns_labels = ns.fetch("labels", {})
  ownership = %w[app.kubernetes.io/managed-by cyclopus.dev/environment-id cyclopus.dev/org-id cyclopus.dev/app-id]
  provisioner = fixture["username"] == "system:serviceaccount:cyclopus-system:cyclopus-provisioner"
  cyclopus_target = obj.dig("metadata", "name") == "cyclopus-preview" ||
    labels["app.kubernetes.io/managed-by"] == "cyclopus" ||
    (ns["name"].match?(/^preview-[a-z0-9]([-a-z0-9]*[a-z0-9])?$/) && ns_labels["app.kubernetes.io/managed-by"] == "cyclopus")
  return true unless provisioner || cyclopus_target
  return false unless fixture["username"] == "system:serviceaccount:cyclopus-system:cyclopus-provisioner"
  return false unless fixture["operation"] == "CREATE" || fixture["operation"] == "UPDATE"
  return false unless %w[resourcequotas limitranges].include?(fixture["resource"])
  return false unless ns["name"].match?(/^preview-[a-z0-9]([-a-z0-9]*[a-z0-9])?$/)
  return false unless ns_labels["app.kubernetes.io/managed-by"] == "cyclopus" && ownership.drop(1).all? { |key| !ns_labels[key].to_s.empty? }
  return false unless ns.dig("annotations", "cyclopus.dev/namespace-owner") == ns_labels["cyclopus.dev/environment-id"] && ns.dig("annotations", "cyclopus.dev/namespace-uid") == ns["uid"]
  return false unless obj.dig("metadata", "name") == "cyclopus-preview" && obj.dig("metadata", "namespace") == ns["name"] && labels.keys.sort == ownership.sort && ownership.all? { |key| labels[key] == ns_labels[key] }
  if fixture["operation"] == "UPDATE"
    old_labels = fixture.dig("oldObject", "metadata", "labels") || {}
    allowed_old_keys = old_labels.keys.sort == ownership.sort ||
      old_labels.keys.sort == (ownership + ["cyclopus.app/policies-pending"]).sort
    return false unless allowed_old_keys && ownership.all? { |key| old_labels[key] == labels[key] }
  end
  if fixture["resource"] == "resourcequotas"
    hard = obj.dig("spec", "hard") || {}
    return hard.keys.sort == %w[limits.cpu limits.memory pods requests.cpu requests.memory] && hard["pods"] == "1" && !obj.fetch("spec").key?("scopes") && !obj.fetch("spec").key?("scopeSelector")
  end
  limits = obj.dig("spec", "limits") || []
  limits.size == 1 && limits[0]["type"] == "Container" && limits[0].fetch("defaultRequest", {}).keys.sort == %w[cpu memory] && limits[0].fetch("default", {}).keys.sort == %w[cpu memory] && (limits[0].keys & %w[min max maxLimitRequestRatio]).empty?
end

abort "not ok - happy ResourceQuota fixture denied" unless allowed.call(Marshal.load(Marshal.dump(request)))
limit_request = Marshal.load(Marshal.dump(request)).merge("resource" => "limitranges", "object" => limit)
abort "not ok - happy LimitRange fixture denied" unless allowed.call(limit_request)
happy_update = Marshal.load(Marshal.dump(request))
happy_update["operation"] = "UPDATE"
happy_update["oldObject"] = Marshal.load(Marshal.dump(happy_update["object"]))
abort "not ok - happy ResourceQuota update fixture denied" unless allowed.call(happy_update)
happy_limit_update = Marshal.load(Marshal.dump(limit_request))
happy_limit_update["operation"] = "UPDATE"
happy_limit_update["oldObject"] = Marshal.load(Marshal.dump(happy_limit_update["object"]))
abort "not ok - happy LimitRange update fixture denied" unless allowed.call(happy_limit_update)

{"ResourceQuota" => happy_update, "LimitRange" => happy_limit_update}.each do |kind, update|
  legacy_cleanup = Marshal.load(Marshal.dump(update))
  legacy_cleanup.dig("oldObject", "metadata", "labels")["cyclopus.app/policies-pending"] = "true"
  abort "not ok - legacy #{kind} cleanup update fixture denied" unless allowed.call(legacy_cleanup)

  legacy_persisted = Marshal.load(Marshal.dump(legacy_cleanup))
  legacy_persisted.dig("object", "metadata", "labels")["cyclopus.app/policies-pending"] = "true"
  abort "not ok - legacy #{kind} pending label persisted" if allowed.call(legacy_persisted)

  unknown_fifth = Marshal.load(Marshal.dump(update))
  unknown_fifth.dig("oldObject", "metadata", "labels")["example.test/unknown"] = "true"
  abort "not ok - unknown fifth old #{kind} label was allowed" if allowed.call(unknown_fifth)

  ownership_change = Marshal.load(Marshal.dump(legacy_cleanup))
  ownership_change.dig("object", "metadata", "labels")["cyclopus.dev/org-id"] = "other"
  abort "not ok - legacy #{kind} ownership change was allowed" if allowed.call(ownership_change)
end
puts "ok - current and legacy ResourceQuota/LimitRange updates enforce cleanup and immutable ownership"

unrelated_admin = Marshal.load(Marshal.dump(request))
unrelated_admin["username"] = "admin@example.test"
unrelated_admin.dig("namespace", "metadata")["name"] = "default"
unrelated_admin.dig("namespace", "metadata")["labels"] = {}
unrelated_admin.dig("namespace", "metadata")["annotations"] = {}
unrelated_admin.dig("object", "metadata")["name"] = "team-quota"
unrelated_admin.dig("object", "metadata")["namespace"] = "default"
unrelated_admin.dig("object", "metadata")["labels"] = {"team" => "platform"}
abort "not ok - unrelated namespace admin request denied" unless allowed.call(unrelated_admin)
unrelated_controller = Marshal.load(Marshal.dump(unrelated_admin))
unrelated_controller["username"] = "system:serviceaccount:operators:quota-controller"
unrelated_controller.dig("object", "metadata")["name"] = "controller-quota"
abort "not ok - unrelated controller object denied" unless allowed.call(unrelated_controller)
puts "ok - unrelated admin/controller quota requests remain outside Cyclopus admission enforcement"
puts "ok - happy Cyclopus ResourceQuota and LimitRange create/update fixtures are allowed"

mutations = {
  "runtime service account on Cyclopus object" => ->(f) { f["username"] = "system:serviceaccount:cyclopus-system:cyclopus" },
  "arbitrary namespace" => ->(f) { f.dig("namespace", "metadata")["name"] = "default"; f.dig("object", "metadata")["namespace"] = "default" },
  "production namespace" => ->(f) { f.dig("namespace", "metadata")["name"] = "whoami-production"; f.dig("object", "metadata")["namespace"] = "whoami-production" },
  "unowned namespace" => ->(f) { f.dig("namespace", "metadata", "labels").delete("app.kubernetes.io/managed-by") },
  "wrong kind/resource" => ->(f) { f["resource"] = "configmaps" },
  "wrong fixed name" => ->(f) { f.dig("object", "metadata")["name"] = "arbitrary" },
  "extra label" => ->(f) { f.dig("object", "metadata", "labels")["extra.example.test/label"] = "forbidden" },
  "missing label" => ->(f) { f.dig("object", "metadata", "labels").delete("cyclopus.dev/org-id") },
  "mismatched labels" => ->(f) { f.dig("object", "metadata", "labels")["cyclopus.dev/app-id"] = "other" },
  "arbitrary quota rule" => ->(f) { f.dig("object", "spec", "hard")["services"] = "1" },
  "update ownership change" => ->(f) { f["operation"] = "UPDATE"; f["oldObject"] = Marshal.load(Marshal.dump(f["object"])); f.dig("object", "metadata", "labels")["cyclopus.dev/org-id"] = "other" }
}
mutations.each do |name, mutate|
  fixture = Marshal.load(Marshal.dump(request))
  mutate.call(fixture)
  abort "not ok - #{name} request fixture was allowed" if allowed.call(fixture)
end
arbitrary_limit = Marshal.load(Marshal.dump(limit_request))
arbitrary_limit.dig("object", "spec", "limits") << Marshal.load(Marshal.dump(arbitrary_limit.dig("object", "spec", "limits", 0)))
abort "not ok - arbitrary LimitRange rules fixture was allowed" if allowed.call(arbitrary_limit)
scoped_quota = Marshal.load(Marshal.dump(request))
scoped_quota.dig("object", "spec")["scopes"] = ["BestEffort"]
abort "not ok - scoped ResourceQuota fixture was allowed" if allowed.call(scoped_quota)
limited_range = Marshal.load(Marshal.dump(limit_request))
limited_range.dig("object", "spec", "limits", 0)["max"] = {"cpu" => "1"}
abort "not ok - LimitRange max fixture was allowed" if allowed.call(limited_range)
puts "ok - preview quota/limit policy denies mutated SA, namespace, production, kind, name, labels, rules, and update fixtures"
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
