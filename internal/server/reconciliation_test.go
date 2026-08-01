package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestReconciliationProtoProjectsDurableContract(t *testing.T) {
	createdAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	startedAt := createdAt.Add(time.Second)
	completedAt := startedAt.Add(2 * time.Second)
	errorCode := "scan_partial"
	errorMessage := "one object could not be observed"
	run := store.ReconciliationRun{
		ID: "run-id", OrgID: "org-id", ScopeKind: "environment",
		ScopeAppID: stringPointerForTest("app-id"), ScopeEnvironmentID: stringPointerForTest("environment-id"),
		Categories: []string{"workload", "network_policy"}, Status: "failed", DryRun: true,
		CreatedAt: createdAt, StartedAt: &startedAt, CompletedAt: &completedAt,
		ErrorCode: &errorCode, ErrorMessage: &errorMessage,
		Plan: json.RawMessage(`{
			"create":[{"object":{"kind":"Service","namespace":"app","name":"api"},"desired_hash":"desired"}],
			"actions":[{"action":"patch","object_kind":"Deployment","namespace":"app","name":"api","observed_hash":"observed"},{"action":"prune","object":"stale"}]
		}`),
		Report: json.RawMessage(`{
			"orphans":[{"kind":"Service","namespace":"app","name":"stale"}],
			"pruned":[],"planned":3,"applied":0,"dry_run":true,
			"findings":[
				{"type":"runtime_class_drift","category":"workload","status":"drifted","resource_name":"pods/api","summary":"runtime class differs","expected_runtime_class":"gvisor","observed_runtime_class":"runc","observed_at":"2026-07-29T12:00:02Z"},
				{"type":"network_policy_drift","category":"network_policy","status":"missing","resource_name":"namespaces/app","summary":"required policies are missing","missing_policies":["default-deny","allow-dns"]}
			]
		}`),
	}

	projected, err := reconciliationProto(run)
	if err != nil {
		t.Fatal(err)
	}
	if projected.GetStartedAt() == nil || !projected.GetStartedAt().AsTime().Equal(startedAt) ||
		projected.GetErrorCode() != errorCode || projected.GetErrorMessage() != errorMessage {
		t.Fatalf("timestamps/errors = %+v", projected)
	}
	if len(projected.GetPlan().GetCreate()) != 1 || len(projected.GetPlan().GetPatch()) != 1 || len(projected.GetPlan().GetDelete()) != 1 {
		t.Fatalf("plan = %+v", projected.GetPlan())
	}
	if projected.GetPlan().GetDelete()[0].GetObject().GetName() != "stale" {
		t.Fatalf("delete operation = %+v", projected.GetPlan().GetDelete()[0])
	}
	if len(projected.GetReport().GetOrphans()) != 1 || projected.GetReport().GetPlanned() != 3 || !projected.GetReport().GetDryRun() {
		t.Fatalf("report = %+v", projected.GetReport())
	}
	if len(projected.GetFindings()) != 2 || projected.GetFindings()[0].GetRuntimeClassDrift().GetExpectedRuntimeClass() != "gvisor" ||
		projected.GetFindings()[1].GetNetworkPolicyDrift().GetMissingPolicies()[1] != "allow-dns" {
		t.Fatalf("findings = %+v", projected.GetFindings())
	}
}

func TestReconciliationProtoRejectsUnknownTypedData(t *testing.T) {
	_, err := reconciliationProto(store.ReconciliationRun{Plan: json.RawMessage(`{"actions":[{"action":"replace"}]}`)})
	if err == nil {
		t.Fatal("unknown reconciliation action was accepted")
	}
	_, err = reconciliationProto(store.ReconciliationRun{Report: json.RawMessage(`{"findings":[{"type":"unknown"}]}`)})
	if err == nil {
		t.Fatal("unknown reconciliation finding was accepted")
	}
}

func TestReconciliationEvidenceCursorIsRunScoped(t *testing.T) {
	scope := pageScope("reconciliation_evidence", "org-one", "run-one")
	token := encodeReconciliationEvidenceCursor(scope, 42)
	sequence, err := reconciliationEvidenceCursor(token, scope)
	if err != nil || sequence != 42 {
		t.Fatalf("cursor = %d, %v", sequence, err)
	}
	if _, err := reconciliationEvidenceCursor(token, pageScope("reconciliation_evidence", "org-one", "run-two")); err == nil {
		t.Fatal("cursor from another run was accepted")
	}
	if _, err := reconciliationEvidenceCursor(token, pageScope("reconciliation_evidence", "org-two", "run-one")); err == nil {
		t.Fatal("cursor from another organization was accepted")
	}
}

func TestReconciliationEvidenceProto(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	value, err := reconciliationEvidenceProto(store.ReconciliationEvidence{
		Sequence: 7, Kind: "runtime_class_drift", ObjectKind: stringPointerForTest("Pod"),
		ObjectNamespace: stringPointerForTest("app"), ObjectName: stringPointerForTest("api-123"),
		Action: stringPointerForTest("observe"), Details: json.RawMessage(`{"expected":"gvisor","observed":"runc"}`), ObservedAt: observedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.GetSequence() != 7 || value.GetObject().GetKind() != "Pod" || value.GetDetails().GetStructValue().GetFields()["expected"].GetStringValue() != "gvisor" ||
		value.GetObservedAt() == nil || !value.GetObservedAt().AsTime().Equal(observedAt) {
		t.Fatalf("evidence = %+v", value)
	}
	if value.GetAction() != "observe" || value.GetKind() != "runtime_class_drift" {
		t.Fatalf("evidence vocabulary = %+v", value)
	}
}

func stringPointerForTest(value string) *string { return &value }
