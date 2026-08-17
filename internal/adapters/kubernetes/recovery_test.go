package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestRecoverExpiredDeployRestoresSnapshotPreservingMetadataAndFinalizes(t *testing.T) {
	previous := testDeployment("old-image")
	failed := testDeployment("bad-image")
	current := failed.DeepCopy()
	current.ResourceVersion = "12"
	current.Generation = 3
	current.Labels = map[string]string{"external": "preserved", ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment"}
	current.Annotations = map[string]string{"external/annotation": "preserved"}
	current.Finalizers = []string{"external/finalizer"}
	current.Status = readyStatus(3)

	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, previous))
	client := fake.NewSimpleClientset(recoveryObjects(t, recoveryStore, current)...)
	client.PrependReactor("update", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updated := action.(clienttesting.UpdateAction).GetObject().(*appsv1.Deployment)
		updated.Generation = 4
		updated.Status = readyStatus(4)
		return false, nil, nil
	})
	adapter := newTestAdapter(t, client, recoveryStore)

	if _, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease); err != nil {
		t.Fatalf("RecoverExpiredDeploy() error = %v", err)
	}
	got, err := client.AppsV1().Deployments("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "old-image" || got.Labels["external"] != "preserved" || got.Annotations["external/annotation"] != "preserved" || got.Finalizers[0] != "external/finalizer" {
		t.Fatalf("restored deployment = %#v", got)
	}
	if got.Annotations[ReleaseAnnotation] != "previous" || got.Annotations[ConfigHashAnnotation] != "previous-hash" || got.Annotations[DeployFenceAnnotation] == "" {
		t.Fatalf("restored identity annotations = %#v", got.Annotations)
	}
	if recoveryStore.finalizeCalls != 1 || recoveryStore.finalizedLease.Fence != recoveryStore.lease.Fence {
		t.Fatalf("finalization = calls:%d lease:%+v latest:%+v", recoveryStore.finalizeCalls, recoveryStore.finalizedLease, recoveryStore.lease)
	}
}

func TestRecoverExpiredDeployPreservesAutoscalerAndAdmissionFields(t *testing.T) {
	previous := testDeployment("old-image")
	previous.Spec.Replicas = int32Pointer(2)
	failed := testDeployment("bad-image")
	current := failed.DeepCopy()
	current.ResourceVersion = "12"
	current.Generation = 3
	current.Spec.Replicas = int32Pointer(7)
	current.Spec.Template.Spec.NodeSelector = map[string]string{"admission": "injected"}
	current.Status = appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 7, UpdatedReplicas: 7, ReadyReplicas: 7, AvailableReplicas: 7}

	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, previous))
	client := fake.NewSimpleClientset(recoveryObjects(t, recoveryStore, current)...)
	client.PrependReactor("update", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updated := action.(clienttesting.UpdateAction).GetObject().(*appsv1.Deployment)
		updated.Generation = 4
		updated.Status = appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2}
		return false, nil, nil
	})
	adapter := newTestAdapter(t, client, recoveryStore)
	if _, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease); err != nil {
		t.Fatalf("RecoverExpiredDeploy() error = %v", err)
	}
	got, err := client.AppsV1().Deployments("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 2 || got.Spec.Template.Spec.NodeSelector["admission"] != "injected" {
		t.Fatalf("runtime fields were overwritten: %#v", got.Spec)
	}
}

func TestRecoverExpiredDeployRecreatesMissingOwnedPreviousDeployment(t *testing.T) {
	previous := testDeployment("old-image")
	failed := testDeployment("bad-image")
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		created := action.(clienttesting.CreateAction).GetObject().(*appsv1.Deployment).DeepCopy()
		created.Generation = 1
		created.Status = readyStatus(1)
		if err := client.Tracker().Create(appsv1.SchemeGroupVersion.WithResource("deployments"), created, created.Namespace); err != nil {
			return true, nil, err
		}
		return true, created, nil
	})
	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, previous))
	adapter := newTestAdapter(t, client, recoveryStore)
	if _, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease); err != nil {
		t.Fatalf("RecoverExpiredDeploy() error = %v", err)
	}
	got, err := client.AppsV1().Deployments("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil || got.Spec.Template.Spec.Containers[0].Image != "old-image" {
		t.Fatalf("recreated deployment = %#v, %v", got, err)
	}
}

func TestRecoverExpiredEnvChangeVerifiesPreviousImmutableSecretBeforeDeploymentRollback(t *testing.T) {
	previous := testDeployment("old-image")
	previous.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "runtime-r1"}},
	}}
	failed := testDeployment("bad-image")
	state := recoveryState(t, failed, previous)
	previousEnvelope, err := store.DecodeReleaseSnapshot(state.PreviousDeploymentSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	previousEnvelope.EnvVarRevision = &store.EnvVarRevision{Version: store.EnvVarRevisionVersion, ID: "revision-one", Revision: 1, Variables: []store.ReleaseEnvironment{{
		Key: "DATABASE_URL", SecretRef: "k8s-secret://tenant/runtime-r1#DATABASE_URL", IsSecret: true,
	}}}
	previousEnvelope.EnvVarRevision.Hash = store.ComputeEnvVarRevisionHash(previousEnvelope.EnvVarRevision.Variables)
	previousEnvelope.DesiredState.Secret = testEnvSecretIntent(previousEnvelope)
	previousEnvelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{previousEnvelope.DesiredState.Secret.Identity.Name}
	previousEnvelope.DesiredState.Deployment.PodAnnotations[EnvRevisionAnnotation] = "revision-one"
	previousEnvelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = previousEnvelope.EnvVarRevision.Hash
	state.PreviousDeploymentSnapshot, _, err = store.EncodeReleaseSnapshot(previousEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	immutable := true
	secretData := map[string][]byte{"DATABASE_URL": []byte("old")}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: previousEnvelope.DesiredState.Secret.Identity.Name, Namespace: "tenant",
		Labels: map[string]string{ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment"}, Annotations: map[string]string{
			EnvRevisionAnnotation: "revision-one", ConfigHashAnnotation: "previous-hash", ReleaseAnnotation: "previous",
			SecretContentHashAnnotation: ComputeSecretDataHash(secretData),
		}}, Immutable: &immutable, Data: secretData}
	current := failed.DeepCopy()
	current.Generation = 2
	current.Status = readyStatus(2)
	recoveryStore := newFakeRecoveryStore(state)
	recoveryStore.lease.FailedRelease.Kind = "env_change"
	recoveryStore.lease.PreviousRelease.EnvVarRevisionID = stringPointer("revision-one")
	client := fake.NewSimpleClientset(append([]runtime.Object{managedTestNamespace(), managedTestSecretRole(), secret}, recoveryObjects(t, recoveryStore, current)...)...)
	adapter := newTestAdapter(t, client, recoveryStore)

	if _, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease); err != nil {
		t.Fatalf("RecoverExpiredDeploy() error = %v", err)
	}
	actions := client.Actions()
	secretGet, deploymentUpdate := -1, -1
	for index, action := range actions {
		if action.GetVerb() == "get" && action.GetResource().Resource == "secrets" {
			secretGet = index
		}
		if action.GetVerb() == "update" && action.GetResource().Resource == "deployments" {
			deploymentUpdate = index
		}
	}
	if secretGet < 0 || deploymentUpdate < 0 || secretGet > deploymentUpdate {
		t.Fatalf("actions = %#v, secret verification must precede deployment update", actions)
	}
}

func TestRecoverExpiredEnvChangeRejectsPreviousSecretHashMismatchBeforeDeploymentRollback(t *testing.T) {
	previous := testDeployment("old-image")
	failed := testDeployment("bad-image")
	state := recoveryState(t, failed, previous)
	previousEnvelope, err := store.DecodeReleaseSnapshot(state.PreviousDeploymentSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	previousEnvelope.EnvVarRevision = &store.EnvVarRevision{Version: store.EnvVarRevisionVersion, ID: "revision-one", Revision: 1, Variables: []store.ReleaseEnvironment{{
		Key: "DATABASE_URL", SecretRef: "k8s-secret://tenant/runtime-r1#DATABASE_URL", IsSecret: true,
	}}}
	previousEnvelope.EnvVarRevision.Hash = store.ComputeEnvVarRevisionHash(previousEnvelope.EnvVarRevision.Variables)
	previousEnvelope.DesiredState.Secret = testEnvSecretIntent(previousEnvelope)
	previousEnvelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{previousEnvelope.DesiredState.Secret.Identity.Name}
	previousEnvelope.DesiredState.Deployment.PodAnnotations[EnvRevisionAnnotation] = "revision-one"
	previousEnvelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = previousEnvelope.EnvVarRevision.Hash
	state.PreviousDeploymentSnapshot, _, err = store.EncodeReleaseSnapshot(previousEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	immutable := true
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: previousEnvelope.DesiredState.Secret.Identity.Name, Namespace: "tenant",
		Labels: map[string]string{ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment"}, Annotations: map[string]string{
			EnvRevisionAnnotation: "revision-one", ConfigHashAnnotation: "wrong-hash", ReleaseAnnotation: "stale-release",
			SecretContentHashAnnotation: "wrong-content-hash",
		}}, Immutable: &immutable}
	client := fake.NewSimpleClientset(managedTestNamespace(), managedTestSecretRole(), secret, failed.DeepCopy())
	recoveryStore := newFakeRecoveryStore(state)
	recoveryStore.lease.FailedRelease.Kind = "env_change"
	recoveryStore.lease.PreviousRelease.EnvVarRevisionID = stringPointer("revision-one")
	adapter := newTestAdapter(t, client, recoveryStore)

	_, err = adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease)
	if !errors.Is(err, ErrCompensationUnproven) || countResourceActions(client.Actions(), "update", "deployments") != 0 {
		t.Fatalf("error = %v, deployment updates = %d", err, countActions(client.Actions(), "update"))
	}
}

func TestRecoverExpiredFirstDeployMissingObjectTerminallyFailsFromPersistedCheckpoint(t *testing.T) {
	failed := testDeployment("bad-image")
	client := fake.NewSimpleClientset()
	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, nil))
	adapter := newTestAdapter(t, client, recoveryStore)
	_, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease)
	if err != nil || recoveryStore.finalizeCalls != 1 || recoveryStore.checkpoint != "first_deployment_missing" || countActions(client.Actions(), "create") != 0 {
		t.Fatalf("error=%v finalize=%d checkpoint=%q creates=%d", err, recoveryStore.finalizeCalls, recoveryStore.checkpoint, countActions(client.Actions(), "create"))
	}
}

func TestRecoverExpiredDeployRejectsUnownedDeployment(t *testing.T) {
	previous := testDeployment("old-image")
	failed := testDeployment("bad-image")
	current := failed.DeepCopy()
	delete(current.Labels, EnvironmentIDLabel)
	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, previous))
	client := fake.NewSimpleClientset(recoveryObjects(t, recoveryStore, current)...)
	adapter := newTestAdapter(t, client, recoveryStore)
	claimedFence := recoveryStore.lease.Fence
	result, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease)
	if !errors.Is(err, ErrCompensationUnproven) || countResourceActions(client.Actions(), "update", "deployments") != 0 {
		t.Fatalf("error=%v deployment updates=%d", err, countResourceActions(client.Actions(), "update", "deployments"))
	}
	if !result.Lease.Fence.Equal(recoveryStore.lease.Fence) || result.Lease.Fence.Equal(claimedFence) {
		t.Fatalf("result fence = %v, claimed = %v latest = %v", result.Lease.Fence, claimedFence, recoveryStore.lease.Fence)
	}
}

func TestRecoverExpiredDeployRetriesOptimisticConflict(t *testing.T) {
	previous := testDeployment("old-image")
	failed := testDeployment("bad-image")
	current := failed.DeepCopy()
	current.ResourceVersion = "12"
	current.Generation = 3
	current.Status = readyStatus(3)
	client := fake.NewSimpleClientset(current)
	conflicts := 1
	client.PrependReactor("update", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			return false, nil, nil
		}
		conflicts--
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "app", errors.New("resource version conflict"))
	})
	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, previous))
	for _, object := range recoveryObjects(t, recoveryStore, current)[1:] {
		_ = client.Tracker().Add(object)
	}
	adapter := newTestAdapter(t, client, recoveryStore)

	if _, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease); err != nil {
		t.Fatalf("RecoverExpiredDeploy() error = %v", err)
	}
	if countResourceActions(client.Actions(), "update", "deployments") != 2 || recoveryStore.finalizeCalls != 1 {
		t.Fatalf("deployment updates = %d, finalize calls = %d", countResourceActions(client.Actions(), "update", "deployments"), recoveryStore.finalizeCalls)
	}
}

func TestRecoverExpiredFirstDeployDoesNotMutateAndFinalizesOnlyOnProvenFailure(t *testing.T) {
	failed := testDeployment("bad-image")
	current := failed.DeepCopy()
	current.ResourceVersion = "5"
	current.Generation = 2
	current.Status = appsv1.DeploymentStatus{
		ObservedGeneration: 2,
		Conditions:         []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: "False", Reason: "ProgressDeadlineExceeded"}},
	}
	client := fake.NewSimpleClientset(current)
	mutations := 0
	client.PrependReactor("update", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		mutations++
		return false, nil, nil
	})
	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, nil))
	adapter := newTestAdapter(t, client, recoveryStore)

	if _, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease); err != nil {
		t.Fatalf("RecoverExpiredDeploy() error = %v", err)
	}
	if mutations != 0 || recoveryStore.finalizeCalls != 1 {
		t.Fatalf("mutations = %d, finalize calls = %d", mutations, recoveryStore.finalizeCalls)
	}
}

func TestRecoverExpiredFirstDeployDoesNotFinalizeWithoutTerminalFailure(t *testing.T) {
	failed := testDeployment("bad-image")
	current := failed.DeepCopy()
	current.ResourceVersion = "5"
	current.Generation = 2
	current.Status = readyStatus(2)
	client := fake.NewSimpleClientset(current)
	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, nil))
	adapter := newTestAdapter(t, client, recoveryStore)

	_, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease)
	if err != nil || recoveryStore.adoptCalls != 1 || recoveryStore.finalizeCalls != 0 || recoveryStore.checkpoint != "late_ready_observed" {
		t.Fatalf("RecoverExpiredDeploy() error = %v, adopt=%d finalize=%d checkpoint=%q", err, recoveryStore.adoptCalls, recoveryStore.finalizeCalls, recoveryStore.checkpoint)
	}
}

func TestRecoverExpiredDeployStopsWhenLeaseFenceIsLost(t *testing.T) {
	previous := testDeployment("old-image")
	failed := testDeployment("bad-image")
	client := fake.NewSimpleClientset(failed.DeepCopy())
	recoveryStore := newFakeRecoveryStore(recoveryState(t, failed, previous))
	recoveryStore.renewErrAt = 2
	adapter := newTestAdapter(t, client, recoveryStore)

	_, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease)
	if !errors.Is(err, store.ErrLeaseNotOwned) || recoveryStore.finalizeCalls != 0 {
		t.Fatalf("RecoverExpiredDeploy() error = %v, finalize calls = %d", err, recoveryStore.finalizeCalls)
	}
	if updates := countActions(client.Actions(), "update"); updates != 0 {
		t.Fatalf("deployment updates = %d", updates)
	}
}

func TestRecoverExpiredDeployRejectsSnapshotBeforeMutation(t *testing.T) {
	client := fake.NewSimpleClientset(testDeployment("bad-image"))
	state := RecoveryState{Namespace: "tenant", DeploymentName: "app", FailureReason: "failed", FailedDeploymentSnapshot: json.RawMessage(`{"version":99}`)}
	recoveryStore := newFakeRecoveryStore(state)
	adapter := newTestAdapter(t, client, recoveryStore)

	_, err := adapter.RecoverExpiredDeploy(context.Background(), recoveryStore.lease)
	if !errors.Is(err, ErrRecoveryStateInvalid) || len(client.Actions()) != 0 || recoveryStore.renewCalls != 0 || recoveryStore.finalizeCalls != 0 {
		t.Fatalf("error = %v actions=%d renew=%d finalize=%d", err, len(client.Actions()), recoveryStore.renewCalls, recoveryStore.finalizeCalls)
	}
}

func readyStatus(generation int64) appsv1.DeploymentStatus {
	return appsv1.DeploymentStatus{ObservedGeneration: generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
}

func int32Pointer(value int32) *int32 { return &value }

func stringPointer(value string) *string { return &value }

func testEnvSecretIntent(envelope store.ReleaseSnapshotEnvelope) *store.SecretIntent {
	name := store.EnvDestinationSecretName(envelope.DesiredState.Deployment.Identity.Name, envelope.EnvVarRevision.ID)
	entries := make([]store.SecretEntryIntent, len(envelope.EnvVarRevision.Variables))
	for index, variable := range envelope.EnvVarRevision.Variables {
		entries[index] = store.SecretEntryIntent{Key: variable.Key, SecretRef: variable.SecretRef}
	}
	return &store.SecretIntent{Version: 1, Identity: store.ObjectIdentity{Name: name, Namespace: envelope.DesiredState.Deployment.Identity.Namespace, Labels: envelope.DesiredState.Deployment.Identity.Labels}, Immutable: true, Entries: entries}
}

func recoveryState(t *testing.T, failed, previous *appsv1.Deployment) RecoveryState {
	t.Helper()
	failed = failed.DeepCopy()
	failed.Annotations[ReleaseAnnotation] = "failed"
	failed.Annotations[ConfigHashAnnotation] = "failed-hash"
	failedSnapshot, err := EncodeDeploymentSnapshot(failed)
	if err != nil {
		t.Fatal(err)
	}
	state := RecoveryState{Namespace: "tenant", DeploymentName: "app", FailureReason: "ProgressDeadlineExceeded", FailedDeploymentSnapshot: failedSnapshot}
	if previous != nil {
		previous = previous.DeepCopy()
		previous.Annotations[ReleaseAnnotation] = "previous"
		previous.Annotations[ConfigHashAnnotation] = "previous-hash"
		state.PreviousDeploymentSnapshot, err = EncodeDeploymentSnapshot(previous)
		if err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func newTestAdapter(t *testing.T, client *fake.Clientset, recoveryStore *fakeRecoveryStore) *RecoveryAdapter {
	t.Helper()
	listKinds := map[schema.GroupVersionResource]string{scaledObjectGVR: "ScaledObjectList", httpScaledObjectGVR: "HTTPScaledObjectList"}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	allowDynamicResourceLists(dynamicClient)
	adapter, err := NewRecoveryAdapter(Config{PollInterval: time.Nanosecond, ConflictRetries: 3}, client, recoveryStore, WithDynamicClient(dynamicClient))
	if err != nil {
		t.Fatal(err)
	}
	adapter.now = func() time.Time { return time.Unix(100, 0) }
	return adapter
}

type fakeRecoveryStore struct {
	state          RecoveryState
	lease          store.DeployLease
	renewCalls     int
	renewErrAt     int
	finalizeCalls  int
	finalizedLease store.DeployLease
	checkpoint     string
	adoptCalls     int
}

func newFakeRecoveryStore(state RecoveryState) *fakeRecoveryStore {
	lease := store.DeployLease{
		OrgID: "org", EnvironmentID: "environment", ReleaseID: "release",
		Fence: time.Unix(1, 0), ExpiresAt: time.Now().Add(time.Minute), Namespace: state.Namespace, Timeout: time.Minute,
		FailedRelease: store.ImmutableReleaseSnapshot{ID: "failed", ConfigHash: "failed-hash", ConfigSnapshot: state.FailedDeploymentSnapshot},
	}
	if len(state.PreviousDeploymentSnapshot) > 0 {
		lease.PreviousRelease = &store.ImmutableReleaseSnapshot{ID: "previous", ConfigHash: "previous-hash", ConfigSnapshot: state.PreviousDeploymentSnapshot}
	}
	return &fakeRecoveryStore{state: state, lease: lease}
}

func recoveryObjects(t *testing.T, recoveryStore *fakeRecoveryStore, current *appsv1.Deployment) []runtime.Object {
	t.Helper()
	objects := []runtime.Object{current}
	if recoveryStore.lease.PreviousRelease == nil {
		return objects
	}
	envelope, err := store.DecodeReleaseSnapshot(recoveryStore.lease.PreviousRelease.ConfigSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	execution := recoveryStore.lease
	execution.ReleaseID = recoveryStore.lease.PreviousRelease.ID
	execution.FailedRelease = *recoveryStore.lease.PreviousRelease
	desired, err := BuildDesiredObjects(execution, envelope, nil)
	if err != nil {
		t.Fatal(err)
	}
	objects = append(objects, desired.Service, desired.Ingress)
	if desired.HPA != nil {
		objects = append(objects, desired.HPA)
	}
	return objects
}

func (f *fakeRecoveryStore) RenewDeployLease(_ context.Context, lease store.DeployLease) (store.DeployLease, error) {
	f.renewCalls++
	if f.renewErrAt == f.renewCalls {
		return store.DeployLease{}, store.ErrLeaseNotOwned
	}
	lease.Fence = lease.Fence.Add(time.Microsecond)
	lease.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
	f.lease = lease
	return lease, nil
}

func (f *fakeRecoveryStore) FinalizeRecoveredReleaseFailed(_ context.Context, lease store.DeployLease, _ string) (store.FinalizeReleaseResult, error) {
	f.finalizeCalls++
	f.finalizedLease = lease
	return store.FinalizeReleaseResult{}, nil
}

func (f *fakeRecoveryStore) CheckpointDeployRecovery(_ context.Context, lease store.DeployLease, checkpoint string) error {
	f.checkpoint = checkpoint
	f.lease.RecoveryCheckpoint = checkpoint
	return nil
}

func (f *fakeRecoveryStore) AdoptRecoveredReleaseLive(_ context.Context, lease store.DeployLease) (store.FinalizeReleaseResult, error) {
	f.adoptCalls++
	f.finalizedLease = lease
	return store.FinalizeReleaseResult{}, nil
}

func (*fakeRecoveryStore) QuarantineDeployRecovery(context.Context, store.DeployLease, string, string) error {
	return nil
}

func countActions(actions []clienttesting.Action, verb string) int {
	count := 0
	for _, action := range actions {
		if action.GetVerb() == verb {
			count++
		}
	}
	return count
}

func countResourceActions(actions []clienttesting.Action, verb, resource string) int {
	count := 0
	for _, action := range actions {
		if action.GetVerb() == verb && action.GetResource().Resource == resource {
			count++
		}
	}
	return count
}
