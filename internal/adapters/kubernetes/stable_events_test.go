package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cyclopus-dev/cyclopus/internal/store"
)

func captureRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode slog record: %v", err)
		}
		records = append(records, record)
	}
	return records
}

func findEvent(records []map[string]any, name string) map[string]any {
	for _, record := range records {
		if record["event"] == name {
			return record
		}
	}
	return nil
}

func bufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func TestStableEventDeployStartedAndFailed(t *testing.T) {
	var buf bytes.Buffer
	envelope := deployEnvelope(t, "none")
	execution := deployExecution(t, envelope)
	objects, _ := BuildDesiredObjects(execution, envelope, nil)
	// Selector drift forces a terminal failWithoutMutation path.
	current := objects.Deployment.DeepCopy()
	current.Spec.Selector.MatchLabels["drift"] = "true"
	client := fake.NewSimpleClientset(current)
	adapter := newForwardAdapter(t, client, newFakeForwardStore(execution))
	adapter.logger = bufferLogger(&buf)

	if _, err := adapter.ExecuteDeploy(context.Background(), execution); err != nil {
		t.Fatal(err)
	}

	records := captureRecords(t, &buf)
	started := findEvent(records, "deploy.started")
	if started == nil {
		t.Fatal("deploy.started not emitted")
	}
	if started["msg"] != "deploy.started" || started["release_id"] != "release" ||
		started["environment_id"] != "environment" || started["org_id"] != "org" {
		t.Fatalf("deploy.started fields = %+v", started)
	}
	failed := findEvent(records, "deploy.failed")
	if failed == nil {
		t.Fatal("deploy.failed not emitted")
	}
	if failed["msg"] != "deploy.failed" || failed["release_id"] != "release" ||
		failed["environment_id"] != "environment" || failed["org_id"] != "org" {
		t.Fatalf("deploy.failed fields = %+v", failed)
	}
	if _, ok := failed["duration_ms"]; !ok {
		t.Fatalf("deploy.failed missing duration_ms: %+v", failed)
	}
	if _, ok := failed["condition"]; !ok {
		t.Fatalf("deploy.failed missing condition: %+v", failed)
	}
}

func TestStableEventDeploySucceeded(t *testing.T) {
	var buf bytes.Buffer
	envelope := deployEnvelope(t, "none")
	execution := deployExecution(t, envelope)
	objects, _ := BuildDesiredObjects(execution, envelope, nil)
	// Pre-create Service/Deployment/Ingress; the update reactor promotes the
	// Deployment to a ready status so observeForward completes and the release
	// is finalized live.
	existing := objects.Deployment.DeepCopy()
	client := fake.NewSimpleClientset(objects.Service.DeepCopy(), existing, objects.Ingress.DeepCopy())
	client.PrependReactor("update", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		update := action.(clienttesting.UpdateAction).GetObject().(*appsv1.Deployment)
		update.Generation = 2
		update.Status = readyStatus(2)
		update.Annotations[ReleaseAnnotation] = execution.ReleaseID
		update.Annotations[ConfigHashAnnotation] = execution.FailedRelease.ConfigHash
		return false, nil, nil
	})
	dataStore := newFakeForwardStore(execution)
	adapter := newForwardAdapter(t, client, dataStore)
	adapter.logger = bufferLogger(&buf)

	if _, err := adapter.ExecuteDeploy(context.Background(), execution); err != nil {
		t.Fatalf("ExecuteDeploy() error = %v", err)
	}
	if dataStore.liveCalls != 1 {
		t.Fatalf("liveCalls = %d, want 1", dataStore.liveCalls)
	}

	records := captureRecords(t, &buf)
	succeeded := findEvent(records, "deploy.succeeded")
	if succeeded == nil {
		t.Fatal("deploy.succeeded not emitted")
	}
	if succeeded["msg"] != "deploy.succeeded" || succeeded["release_id"] != "release" ||
		succeeded["environment_id"] != "environment" || succeeded["org_id"] != "org" {
		t.Fatalf("deploy.succeeded fields = %+v", succeeded)
	}
	if _, ok := succeeded["duration_ms"]; !ok {
		t.Fatalf("deploy.succeeded missing duration_ms: %+v", succeeded)
	}
	if findEvent(records, "deploy.failed") != nil {
		t.Fatal("deploy.failed emitted on a successful deploy")
	}
}

func runReconcileWithLogger(t *testing.T, buf *bytes.Buffer, rs *fakeReconcileStore, client *fake.Clientset, run *store.ReconciliationRun) {
	t.Helper()
	adapter := &RecoveryAdapter{
		config: Config{PollInterval: time.Nanosecond, ConflictRetries: 3, APITimeout: time.Second},
		client: client,
		store:  rs,
		logger: bufferLogger(buf),
	}
	if err := adapter.ExecuteReconciliation(context.Background(), run, "worker-one"); err != nil {
		t.Fatalf("ExecuteReconciliation() error = %v", err)
	}
}

func TestStableEventReconcileInvoked(t *testing.T) {
	var buf bytes.Buffer
	release := reconcileTestRelease(t)
	client := fake.NewSimpleClientset()
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run-one", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true, Prune: false}

	runReconcileWithLogger(t, &buf, rs, client, run)

	event := findEvent(captureRecords(t, &buf), "reconcile.invoked")
	if event == nil {
		t.Fatal("reconcile.invoked not emitted")
	}
	if event["msg"] != "reconcile.invoked" || event["run_id"] != "run-one" || event["org_id"] != "org" ||
		event["scope_kind"] != "environment" || event["dry_run"] != true || event["prune"] != false {
		t.Fatalf("reconcile.invoked fields = %+v", event)
	}
}

func TestStableEventRuntimeClassDriftDetected(t *testing.T) {
	var buf bytes.Buffer
	release := reconcileTestRelease(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "tenant",
		Labels: map[string]string{ManagedByLabel: ManagedByValue}}, Spec: corev1.PodSpec{}}
	client := fake.NewSimpleClientset(pod)
	rs := &fakeReconcileStore{env: reconcileEnv(), release: release, app: store.App{RuntimeClass: "gvisor"}}
	run := &store.ReconciliationRun{ID: "run", OrgID: "org", ScopeKind: "environment", ScopeID: "environment", DryRun: true}

	runReconcileWithLogger(t, &buf, rs, client, run)

	event := findEvent(captureRecords(t, &buf), "runtime_class.drift_detected")
	if event == nil {
		t.Fatal("runtime_class.drift_detected not emitted")
	}
	if event["msg"] != "runtime_class.drift_detected" || event["namespace"] != "tenant" ||
		event["pod"] != "app-pod" || event["expected"] != "gvisor" || event["observed"] != "" {
		t.Fatalf("runtime_class.drift_detected fields = %+v", event)
	}
}
