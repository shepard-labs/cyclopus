#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
CHART="$ROOT/charts/cyclopus"
HELM="${HELM:-helm}"

fail() { printf 'not ok - %s\n' "$1" >&2; exit 1; }
command -v "$HELM" >/dev/null 2>&1 || fail "helm not found on PATH"
command -v ruby >/dev/null 2>&1 || fail "ruby not found on PATH"

grep -q 'cyclopus-github-app Secret is retained' "$CHART/templates/NOTES.txt" || fail "NOTES omit retained credential guidance"
grep -q 'back up and restore' "$CHART/templates/NOTES.txt" || fail "NOTES omit credential migration guidance"
grep -q 'database GitHub App state has been reset' "$CHART/templates/NOTES.txt" || fail "NOTES omit safe manual deletion prerequisite"
printf 'ok - NOTES explain retained credential cleanup and migration without exposing credentials\n'

rendered=$(mktemp)
fixture_dir=$(mktemp -d)
trap 'rm -f "$rendered"; rm -rf "$fixture_dir"' EXIT

"$HELM" template cyclopus "$CHART" --namespace cyclopus-system \
  --set secrets.databaseUrl=postgres://u:p@h/db \
  --set secrets.authTokenPepper=x \
  --set ingress.enabled=true \
  --set ingress.host=cyclopus.example.test \
  --set config.externalUrl=https://cyclopus.example.test >"$rendered" || fail "secure ingress render failed"

ruby -ryaml - "$rendered" <<'RUBY' || exit 1
documents = YAML.load_stream(File.read(ARGV.fetch(0))).compact
ingress = documents.find { |doc| doc["kind"] == "Ingress" }
middleware = documents.find { |doc| doc["kind"] == "Middleware" }
github_secret = documents.find { |doc| doc["kind"] == "Secret" && doc.dig("metadata", "name") == "cyclopus-github-app" }
abort "not ok - TLS ingress missing" unless ingress.dig("spec", "tls", 0, "hosts") == ["cyclopus.example.test"]
abort "not ok - HTTPS redirect middleware missing" unless middleware.dig("spec", "redirectScheme") == {"scheme" => "https", "permanent" => true}
abort "not ok - ingress does not reference redirect middleware" unless ingress.dig("metadata", "annotations", "traefik.ingress.kubernetes.io/router.middlewares")&.end_with?("-https-redirect@kubernetescrd")
abort "not ok - install does not render the deterministic GitHub credential Secret" unless github_secret
abort "not ok - GitHub credential Secret is not retained on uninstall" unless github_secret.dig("metadata", "annotations", "helm.sh/resource-policy") == "keep"
puts "ok - TLS ingress enforces permanent HTTP-to-HTTPS redirect"
puts "ok - install renders GitHub credential Secret with Helm uninstall retention policy"
RUBY

if "$HELM" template cyclopus "$CHART" --namespace cyclopus-system \
  --set secrets.databaseUrl=postgres://u:p@h/db --set secrets.authTokenPepper=x \
  --set ingress.enabled=true --set ingress.host=cyclopus.example.test \
  --set config.externalUrl=https://other.example.test >"$rendered" 2>/dev/null; then
  fail "ingress accepted an external URL that does not match its TLS host"
fi
printf 'ok - ingress rejects external URL and TLS host mismatch\n'

mkdir -p "$fixture_dir/templates"
cp "$CHART/Chart.yaml" "$fixture_dir/Chart.yaml"
cp "$CHART/values.yaml" "$fixture_dir/values.yaml"
cp "$CHART/templates/_helpers.tpl" "$fixture_dir/templates/_helpers.tpl"
cp "$CHART/templates/secret.yaml" "$fixture_dir/templates/secret.yaml"
cat >"$fixture_dir/templates/lookup-fixture.yaml" <<'EOF'
{{- define "fixture.lookup" -}}{{- end -}}
EOF

# Helm template cannot seed lookup without a cluster. Exercise the same ownership
# contract with a temporary chart copy whose lookup call is replaced by a fixture;
# credentials stay in files and are never printed by this test.
ruby - "$fixture_dir/templates/secret.yaml" <<'RUBY'
path = ARGV.fetch(0)
text = File.read(path)
fixture = '.Values.lookupFixture | default dict'
text.sub!('lookup "v1" "Secret" .Release.Namespace "cyclopus-github-app"', fixture)
File.write(path, text)
RUBY

cat >"$fixture_dir/owned.yaml" <<'EOF'
lookupFixture:
  metadata:
    labels:
      app.kubernetes.io/managed-by: Helm
      unrelated: keep
    annotations:
      meta.helm.sh/release-name: cyclopus
      meta.helm.sh/release-namespace: cyclopus-system
      unrelated: keep
  data:
    private-key.pem: cHJpdmF0ZQ==
    webhook-secret: d2ViaG9vaw==
EOF
"$HELM" template cyclopus "$fixture_dir" --namespace cyclopus-system -f "$fixture_dir/owned.yaml" \
  --set secrets.databaseUrl=postgres://u:p@h/db --set secrets.authTokenPepper=x >"$rendered" || fail "owned Secret upgrade render failed"
ruby -ryaml - "$rendered" <<'RUBY' || exit 1
secret = YAML.load_stream(File.read(ARGV.fetch(0))).compact.find { |doc| doc["kind"] == "Secret" && doc.dig("metadata", "name") == "cyclopus-github-app" }
abort "not ok - owned Secret disappeared" unless secret
abort "not ok - owned Secret data was not preserved" unless secret.fetch("data", {}).keys.sort == ["private-key.pem", "webhook-secret"]
abort "not ok - owned Secret metadata was not preserved" unless secret.dig("metadata", "labels", "unrelated") == "keep" && secret.dig("metadata", "annotations", "unrelated") == "keep"
abort "not ok - retained Secret missing Helm ownership" unless secret.dig("metadata", "annotations", "meta.helm.sh/release-name") == "cyclopus" && secret.dig("metadata", "annotations", "meta.helm.sh/release-namespace") == "cyclopus-system"
abort "not ok - upgrade/reinstall did not restore retention policy" unless secret.dig("metadata", "annotations", "helm.sh/resource-policy") == "keep"
retained_after_uninstall = secret.dig("metadata", "annotations", "helm.sh/resource-policy") == "keep" ? [secret] : []
abort "not ok - simulated uninstall would delete GitHub credential Secret" unless retained_after_uninstall == [secret]
puts "ok - upgrade preserves populated Secret data, ownership, runtime metadata, and retention policy"
puts "ok - simulated uninstall retains the credential Secret"
puts "ok - same-release reinstall safely adopts the retained Secret without clearing data"
RUBY

cat >"$fixture_dir/different-release.yaml" <<'EOF'
lookupFixture:
  metadata:
    labels:
      app.kubernetes.io/managed-by: Helm
    annotations:
      meta.helm.sh/release-name: previous-release
      meta.helm.sh/release-namespace: cyclopus-system
  data:
    private-key.pem: cHJpdmF0ZQ==
    webhook-secret: d2ViaG9vaw==
EOF
different_release_error="$fixture_dir/different-release-error.log"
if "$HELM" template cyclopus "$fixture_dir" --namespace cyclopus-system -f "$fixture_dir/different-release.yaml" \
  --set secrets.databaseUrl=postgres://u:p@h/db --set secrets.authTokenPepper=x >"$rendered" 2>"$different_release_error"; then
  fail "different-release retained Secret was accepted"
fi
grep -q 'not owned by Helm release cyclopus' "$different_release_error" || fail "different-release rejection was not actionable"
grep -q 'back it up and restore it with its database' "$different_release_error" || fail "different-release rejection omitted safe migration guidance"
printf 'ok - different-release reinstall rejects the retained Secret without rendering its data\n'

cat >"$fixture_dir/unowned.yaml" <<'EOF'
lookupFixture:
  metadata:
    labels:
      app.kubernetes.io/managed-by: other
EOF
error_log="$fixture_dir/unowned-error.log"
if "$HELM" template cyclopus "$fixture_dir" --namespace cyclopus-system -f "$fixture_dir/unowned.yaml" \
  --set secrets.databaseUrl=postgres://u:p@h/db --set secrets.authTokenPepper=x >"$rendered" 2>"$error_log"; then
  fail "unowned existing Secret was accepted"
fi
grep -q 'not owned by Helm release cyclopus' "$error_log" || fail "unowned Secret rejection was not actionable"
printf 'ok - unowned existing Secret is rejected with an ownership error\n'
