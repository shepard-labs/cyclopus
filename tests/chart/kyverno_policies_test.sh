#!/bin/sh
# Render assertion for the Kyverno fail-closed compute-isolation policies
# (spec §7.1, §4.3). Kyverno admission cannot run under `helm template`, so this
# asserts the rendered ClusterPolicy manifests carry the load-bearing
# invariants: both policies present, fail-closed, Enforce, the pending guard,
# the managed-by namespace selector, and the gvisor sandbox class. Also asserts
# the enabled=false toggle removes the ClusterPolicies entirely.
#
# Exit codes: 0 all assertions passed, 1 an assertion failed.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
CHART="$ROOT/charts/cyclopus"
HELM="${HELM:-helm}"
assertions=0

fail() { printf 'not ok - %s\n' "$1" >&2; exit 1; }
pass() { assertions=$((assertions + 1)); printf 'ok - %s\n' "$1"; }

command -v "$HELM" >/dev/null 2>&1 || fail "helm not found on PATH"

# Secrets the chart requires to render at all (they do not affect the kyverno
# policy output; the infra unit gate passes the same placeholders).
SECRETS="--set secrets.databaseUrl=postgres://u:p@h/db --set secrets.authTokenPepper=x"

# --- default render (policies enabled) ---------------------------------------
# shellcheck disable=SC2086
rendered=$("$HELM" template cyclopus "$CHART" --kube-version 1.33.13 $SECRETS \
  --show-only templates/kyverno-policies.yaml 2>/dev/null) \
  || fail "helm template (default) failed to render kyverno-policies.yaml"

contains() {
  printf '%s\n' "$rendered" | grep -q -- "$2" || fail "$1 (missing: $2)"
  pass "$1"
}

# Exactly two ClusterPolicy documents.
count=$(printf '%s\n' "$rendered" | grep -c '^kind: ClusterPolicy' || true)
[ "$count" -eq 2 ] || fail "expected exactly 2 ClusterPolicy docs, got $count"
pass "renders exactly two ClusterPolicy documents"

# Both policy names.
contains "mutating policy name present" "name: cyclopus-inject-runtimeclass"
contains "validating policy name present" "name: cyclopus-enforce-sandbox"

# Fail-closed (§7.1): failurePolicy: Fail on both Kyverno ClusterPolicies AND
# on the ValidatingAdmissionPolicy backstop (3 occurrences total).
fp=$(printf '%s\n' "$rendered" | grep -c 'failurePolicy: Fail' || true)
[ "$fp" -eq 3 ] || fail "expected failurePolicy: Fail on both ClusterPolicies + the VAP backstop, got $fp"
pass "both ClusterPolicies and the VAP are fail-closed (failurePolicy: Fail x3)"

# Enforce on the validating policy — per-rule failureAction (the non-deprecated
# form). Both validate rules (sandbox-class + pending-guard) must enforce.
fa=$(printf '%s\n' "$rendered" | grep -c 'failureAction: Enforce' || true)
[ "$fa" -eq 2 ] || fail "expected failureAction: Enforce on both validate rules, got $fa"
pass "both validate rules enforce (failureAction: Enforce x2)"
# The deprecated spec-level field must NOT be used (future audit-mode regression).
if printf '%s\n' "$rendered" | grep -q 'validationFailureAction:'; then
  fail "deprecated spec-level validationFailureAction present; use per-rule failureAction"
fi
pass "no deprecated spec-level validationFailureAction"

# §4.3 step-2 pending guard.
contains "pending-policies guard label matched" "cyclopus.app/policies-pending"

# managed-by namespace selector scoping both policies to app namespaces.
contains "managed-by namespace selector present" "app.kubernetes.io/managed-by: cyclopus"

# Sandbox RuntimeClass default/allowed.
contains "gvisor sandbox class present" "gvisor"

# Kyverno v1.14 API kind/apiVersion.
contains "kyverno v1 apiVersion" "apiVersion: kyverno.io/v1"

# Admission-operation scoping (isolation verification, 2026-08-13): the
# sandbox rule must be CREATE/UPDATE-only so a non-compliant pod stays
# deletable, and the pending guard must be CREATE-only.
ops_create_update=$(printf '%s\n' "$rendered" | grep -c '                - UPDATE' || true)
[ "$ops_create_update" -eq 1 ] || fail "expected exactly one CREATE/UPDATE-scoped rule, got $ops_create_update"
pass "sandbox rule scoped to CREATE/UPDATE (deletes stay possible)"

# Webhook-free backstop (§7.1): a ValidatingAdmissionPolicy + binding that keep
# enforcing while the Kyverno webhooks are absent (graceful shutdown/restart).
contains "VAP backstop present" "kind: ValidatingAdmissionPolicy$"
contains "VAP binding present" "kind: ValidatingAdmissionPolicyBinding$"
contains "VAP apiVersion" "apiVersion: admissionregistration.k8s.io/v1"
contains "VAP bound to policy by name" "policyName: cyclopus-enforce-sandbox"
contains "VAP denies on violation" "validationActions: \[Deny\]"
contains "VAP requires a sandbox runtime class" 'object.spec.runtimeClassName in'

# --- toggle off (policies disabled) ------------------------------------------
# shellcheck disable=SC2086
disabled=$("$HELM" template cyclopus "$CHART" --kube-version 1.33.13 $SECRETS \
  --set kyverno.policies.enabled=false 2>/dev/null) \
  || fail "helm template (enabled=false) failed"
if printf '%s\n' "$disabled" | grep -q '^kind: ClusterPolicy'; then
  fail "kyverno.policies.enabled=false must render NO ClusterPolicy"
fi
pass "kyverno.policies.enabled=false renders no ClusterPolicy (toggle works)"
if printf '%s\n' "$disabled" | grep -q '^kind: ValidatingAdmissionPolicy'; then
  fail "kyverno.policies.enabled=false must render NO ValidatingAdmissionPolicy"
fi
pass "kyverno.policies.enabled=false renders no VAP backstop (toggle works)"

printf 'kyverno policy render tests passed: %s assertions\n' "$assertions"
