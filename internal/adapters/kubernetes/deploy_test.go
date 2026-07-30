package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestPreviewDesiredObjectsForceOneReplicaAndDeleteOwnedHPA(t *testing.T) {
	envelope := deployEnvelope(t, "none")
	envelope.DesiredState.Deployment.Replicas = 1
	execution := deployExecution(t, envelope)
	objects, err := BuildDesiredObjects(execution, envelope, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *objects.Deployment.Spec.Replicas != 1 || objects.HPA != nil || objects.DeleteHPA != "app" {
		t.Fatalf("preview objects = %+v", objects)
	}
}

func TestDeployDesiredObjectsBuildAutoscalingV2HPAAndRejectKEDA(t *testing.T) {
	envelope := deployEnvelope(t, "hpa")
	execution := deployExecution(t, envelope)
	objects, err := BuildDesiredObjects(execution, envelope, nil)
	if err != nil || objects.HPA == nil || objects.HPA.APIVersion != "autoscaling/v2" || objects.HPA.Spec.MaxReplicas != 3 {
		t.Fatalf("HPA = %+v, %v", objects.HPA, err)
	}
	envelope.DesiredState.Scaling = store.ScalingIntent{Version: 1, Kind: "keda", Name: "app", KEDA: &store.KEDAIntent{Mode: "http", MinReplicas: 0, MaxReplicas: 3, TargetCPUPercent: 70}}
	envelope.DesiredState.Deployment.Replicas = 0
	if _, err := BuildDesiredObjects(execution, envelope, nil); !errors.Is(err, ErrKEDAUnsupported) {
		t.Fatalf("KEDA error = %v", err)
	}
}

func TestDeployApplyPreservesServiceAllocationIngressAnnotationsAndAdmissionFields(t *testing.T) {
	envelope := deployEnvelope(t, "hpa")
	execution := deployExecution(t, envelope)
	objects, err := BuildDesiredObjects(execution, envelope, nil)
	if err != nil {
		t.Fatal(err)
	}
	currentService := objects.Service.DeepCopy()
	currentService.ResourceVersion = "1"
	currentService.Spec.ClusterIP = "10.0.0.7"
	currentService.Spec.ClusterIPs = []string{"10.0.0.7"}
	currentService.Spec.Ports[0].NodePort = 30080
	currentIngress := objects.Ingress.DeepCopy()
	currentIngress.ResourceVersion = "1"
	currentIngress.Annotations["external.example/keep"] = "yes"
	currentDeployment := objects.Deployment.DeepCopy()
	currentDeployment.ResourceVersion = "1"
	currentDeployment.Spec.Template.Spec.NodeSelector = map[string]string{"admission": "kept"}
	currentHPA := objects.HPA.DeepCopy()
	currentHPA.ResourceVersion = "1"
	client := fake.NewSimpleClientset(currentService, currentIngress, currentDeployment, currentHPA)
	dataStore := newFakeForwardStore(execution)
	adapter := newForwardAdapter(t, client, dataStore)
	if err := adapter.applyDesired(context.Background(), &execution, objects); err != nil {
		t.Fatal(err)
	}
	service, _ := client.CoreV1().Services("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	ingress, _ := client.NetworkingV1().Ingresses("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	deployment, _ := client.AppsV1().Deployments("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	if service.Spec.ClusterIP != "10.0.0.7" || service.Spec.Ports[0].NodePort != 30080 || ingress.Annotations["external.example/keep"] != "yes" || deployment.Spec.Template.Spec.NodeSelector["admission"] != "kept" {
		t.Fatalf("preservation failed: service=%+v ingress=%+v deployment=%+v", service.Spec, ingress.Annotations, deployment.Spec.Template.Spec)
	}
}

func TestDeployFenceRejectsNewerAndUnownedResourcesBeforeMutation(t *testing.T) {
	envelope := deployEnvelope(t, "hpa")
	execution := deployExecution(t, envelope)
	objects, _ := BuildDesiredObjects(execution, envelope, nil)
	for _, test := range []struct {
		name string
		edit func(*corev1.Service)
		want error
	}{
		{name: "newer fence", edit: func(service *corev1.Service) {
			service.Annotations[DeployFenceAnnotation] = execution.Fence.Add(time.Hour).Format(time.RFC3339Nano)
		}, want: ErrNewerDeployFence},
		{name: "unowned", edit: func(service *corev1.Service) { delete(service.Labels, EnvironmentIDLabel) }, want: ErrResourceOwnership},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := objects.Service.DeepCopy()
			service.ResourceVersion = "1"
			test.edit(service)
			client := fake.NewSimpleClientset(service)
			adapter := newForwardAdapter(t, client, newFakeForwardStore(execution))
			err := adapter.applyService(context.Background(), &execution, objects.Service)
			if !errors.Is(err, test.want) || countActions(client.Actions(), "update") != 0 {
				t.Fatalf("error=%v updates=%d", err, countActions(client.Actions(), "update"))
			}
		})
	}
}

func TestEnvSnapshotImmutableSecretIsIdempotentAndNeverOverwritten(t *testing.T) {
	envelope := deployEnvelope(t, "none")
	hash := store.ComputeEnvVarRevisionHash([]store.ReleaseEnvironment{{Key: "TOKEN", SecretRef: "k8s-secret://tenant/source#TOKEN", IsSecret: true}})
	name := store.EnvDestinationSecretName("app", "revision")
	envelope.EnvVarRevision = &store.EnvVarRevision{Version: 1, ID: "revision", Revision: 1, Hash: hash, Variables: []store.ReleaseEnvironment{{Key: "TOKEN", SecretRef: "k8s-secret://tenant/source#TOKEN", IsSecret: true}}}
	envelope.DesiredState.Secret = &store.SecretIntent{Version: 1, Identity: store.ObjectIdentity{Name: name, Namespace: "tenant", Labels: envelope.DesiredState.Deployment.Identity.Labels}, Immutable: true, Entries: []store.SecretEntryIntent{{Key: "TOKEN", SecretRef: "k8s-secret://tenant/source#TOKEN"}}}
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{name}
	envelope.DesiredState.Deployment.PodAnnotations[EnvRevisionAnnotation] = "revision"
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = hash
	execution := deployExecution(t, envelope)
	objects, err := BuildDesiredObjects(execution, envelope, map[string][]byte{"TOKEN": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(objects.Secret.DeepCopy())
	adapter := newForwardAdapter(t, client, newFakeForwardStore(execution))
	if err := adapter.applySecret(context.Background(), &execution, objects.Secret); err != nil {
		t.Fatalf("idempotent Secret apply: %v", err)
	}
	mismatch := objects.Secret.DeepCopy()
	mismatch.Data["TOKEN"] = []byte("different")
	if err := adapter.applySecret(context.Background(), &execution, mismatch); !errors.Is(err, ErrImmutableSecretMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
	if countActions(client.Actions(), "update") != 0 {
		t.Fatal("immutable Secret was updated")
	}
}

func TestEnvSnapshotDestinationSecretReusedAcrossOrdinaryReleasesIgnoresLegacyReleaseIdentity(t *testing.T) {
	envelope := envDeployEnvelope(t)
	first := deployExecution(t, envelope)
	objects, err := BuildDesiredObjects(first, envelope, map[string][]byte{"TOKEN": []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	legacy := objects.Secret.DeepCopy()
	legacy.Annotations[ReleaseAnnotation] = "older-release"
	legacy.Annotations[ConfigHashAnnotation] = strings.Repeat("f", 64)
	client := fake.NewSimpleClientset(legacy)
	second := first
	second.ReleaseID = "ordinary-release"
	second.FailedRelease.ID = second.ReleaseID
	second.FailedRelease.ConfigHash = strings.Repeat("e", 64)
	adapter := newForwardAdapter(t, client, newFakeForwardStore(second))
	if err := adapter.applySecret(context.Background(), &second, objects.Secret); err != nil {
		t.Fatalf("revision Secret reuse rejected: %v", err)
	}
	stored, _ := client.CoreV1().Secrets("tenant").Get(context.Background(), legacy.Name, metav1.GetOptions{})
	if stored.Annotations[ReleaseAnnotation] != "older-release" || stored.Annotations[ConfigHashAnnotation] != strings.Repeat("f", 64) {
		t.Fatalf("legacy identity was rewritten: %v", stored.Annotations)
	}
}

func TestDeployNamedHTTPPortProtectsMixedRevisionRollingPods(t *testing.T) {
	envelope := deployEnvelope(t, "none")
	envelope.DesiredState.Deployment.Container.Port = 9191
	envelope.DesiredState.Deployment.Container.ReadinessProbe.Port = 9191
	envelope.DesiredState.Service.TargetPort = 9191
	execution := deployExecution(t, envelope)
	objects, err := BuildDesiredObjects(execution, envelope, nil)
	if err != nil {
		t.Fatal(err)
	}
	port := objects.Deployment.Spec.Template.Spec.Containers[0].Ports[0]
	if port.Name != "http" || port.ContainerPort != 9191 || objects.Service.Spec.Ports[0].TargetPort.StrVal != "http" || objects.Service.Spec.Ports[0].TargetPort.Type != intstr.String {
		t.Fatalf("deployment port=%+v service target=%+v", port, objects.Service.Spec.Ports[0].TargetPort)
	}
}

func TestDeployApplyOrderSecretServiceDeploymentIngressScaling(t *testing.T) {
	envelope := deployEnvelope(t, "hpa")
	hash := store.ComputeEnvVarRevisionHash([]store.ReleaseEnvironment{{Key: "TOKEN", SecretRef: "k8s-secret://tenant/source#TOKEN", IsSecret: true}})
	name := store.EnvDestinationSecretName("app", "revision")
	envelope.EnvVarRevision = &store.EnvVarRevision{Version: 1, ID: "revision", Revision: 1, Hash: hash, Variables: []store.ReleaseEnvironment{{Key: "TOKEN", SecretRef: "k8s-secret://tenant/source#TOKEN", IsSecret: true}}}
	envelope.DesiredState.Secret = &store.SecretIntent{Version: 1, Identity: store.ObjectIdentity{Name: name, Namespace: "tenant", Labels: envelope.DesiredState.Deployment.Identity.Labels}, Immutable: true, Entries: []store.SecretEntryIntent{{Key: "TOKEN", SecretRef: "k8s-secret://tenant/source#TOKEN"}}}
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{name}
	envelope.DesiredState.Deployment.PodAnnotations[EnvRevisionAnnotation] = "revision"
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = hash
	execution := deployExecution(t, envelope)
	objects, _ := BuildDesiredObjects(execution, envelope, map[string][]byte{"TOKEN": []byte("secret")})
	client := fake.NewSimpleClientset()
	adapter := newForwardAdapter(t, client, newFakeForwardStore(execution))
	if err := adapter.applyDesired(context.Background(), &execution, objects); err != nil {
		t.Fatal(err)
	}
	var created []string
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" {
			created = append(created, action.GetResource().Resource)
		}
	}
	want := []string{"secrets", "services", "deployments", "ingresses", "horizontalpodautoscalers"}
	if !reflect.DeepEqual(created, want) {
		t.Fatalf("create order = %#v, want %#v", created, want)
	}
}

func TestDeployRolloutReadinessRequiresIdentityAndProgressDeadlineIsTerminal(t *testing.T) {
	envelope := deployEnvelope(t, "none")
	execution := deployExecution(t, envelope)
	objects, _ := BuildDesiredObjects(execution, envelope, nil)
	ready := objects.Deployment.DeepCopy()
	ready.Generation = 2
	ready.Status = readyStatus(2)
	client := fake.NewSimpleClientset(ready)
	adapter := newForwardAdapter(t, client, newFakeForwardStore(execution))
	if err := adapter.observeForward(context.Background(), &execution, objects.Deployment); err != nil {
		t.Fatal(err)
	}
	terminal := ready.DeepCopy()
	terminal.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}}
	client = fake.NewSimpleClientset(terminal)
	adapter = newForwardAdapter(t, client, newFakeForwardStore(execution))
	if err := adapter.observeForward(context.Background(), &execution, objects.Deployment); !errors.Is(err, ErrRolloutTerminal) {
		t.Fatalf("terminal error = %v", err)
	}
}

func TestDeployTerminalRolloutPermanentCompensationFailureQuarantinesRenewedFence(t *testing.T) {
	envelope := deployEnvelope(t, "none")
	execution := deployExecution(t, envelope)
	originalFence := execution.Fence
	execution.PreviousRelease = &store.ImmutableReleaseSnapshot{
		ID: "previous", ConfigHash: strings.Repeat("b", 64), ConfigSnapshot: json.RawMessage(`{"version":99}`),
	}
	objects, err := BuildDesiredObjects(execution, envelope, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminal := objects.Deployment.DeepCopy()
	terminal.Generation = 2
	terminal.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
	}}}
	client := fake.NewSimpleClientset(terminal)
	dataStore := newFakeForwardStore(execution)
	adapter := newForwardAdapter(t, client, dataStore)
	result, err := adapter.ExecuteDeploy(context.Background(), execution)
	if err != nil {
		t.Fatalf("terminal compensation failure was not quarantined: %v", err)
	}
	if dataStore.quarantineCalls != 1 || dataStore.quarantineCode != "forward_permanent_after_mutation" ||
		!dataStore.quarantineLease.Fence.After(originalFence) || !result.Lease.Fence.Equal(dataStore.quarantineLease.Fence) {
		t.Fatalf("quarantine calls=%d code=%q lease=%+v result=%+v", dataStore.quarantineCalls,
			dataStore.quarantineCode, dataStore.quarantineLease, result.Lease)
	}
	if dataStore.failedCalls != 0 || dataStore.liveCalls != 0 {
		t.Fatalf("terminal unproven compensation finalized live=%d failed=%d", dataStore.liveCalls, dataStore.failedCalls)
	}
}

func TestDeployTerminalRolloutRestoresCompletePreviousDesiredStateBeforeFinalization(t *testing.T) {
	for _, test := range []struct {
		name            string
		previousScaling string
		failedScaling   string
		wantHPA         bool
	}{
		{name: "HPA to none", previousScaling: "hpa", failedScaling: "none", wantHPA: true},
		{name: "none to HPA", previousScaling: "none", failedScaling: "hpa", wantHPA: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			previousEnvelope := compensationEnvelope(t, test.previousScaling, "previous")
			failedEnvelope := compensationEnvelope(t, test.failedScaling, "failed")
			previousSnapshot, previousHash, err := store.EncodeReleaseSnapshot(previousEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			failedSnapshot, failedHash, err := store.EncodeReleaseSnapshot(failedEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			fence := time.Now().UTC().Truncate(time.Microsecond)
			execution := store.DeployExecution{OrgID: "org", EnvironmentID: "environment", ReleaseID: "failed", Namespace: "tenant", PlannedHostname: "failed.example.test", Fence: fence, ExpiresAt: fence.Add(time.Hour), Timeout: time.Minute,
				FailedRelease:   store.ImmutableReleaseSnapshot{ID: "failed", EnvironmentID: "environment", ConfigSnapshot: failedSnapshot, ConfigHash: failedHash},
				PreviousRelease: &store.ImmutableReleaseSnapshot{ID: "previous", EnvironmentID: "environment", ConfigSnapshot: previousSnapshot, ConfigHash: previousHash},
			}
			failedObjects, err := BuildDesiredObjects(execution, failedEnvelope, nil)
			if err != nil {
				t.Fatal(err)
			}
			terminal := failedObjects.Deployment.DeepCopy()
			terminal.Generation = 2
			terminal.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}}
			objects := []runtime.Object{failedObjects.Service.DeepCopy(), terminal, failedObjects.Ingress.DeepCopy()}
			if failedObjects.HPA != nil {
				objects = append(objects, failedObjects.HPA.DeepCopy())
			}
			client := fake.NewSimpleClientset(objects...)
			deploymentUpdates := 0
			client.PrependReactor("update", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
				deploymentUpdates++
				update := action.(clienttesting.UpdateAction).GetObject().(*appsv1.Deployment)
				if deploymentUpdates > 1 {
					update.Generation = terminal.Generation + 1
					update.Status = readyStatus(update.Generation)
				}
				return false, nil, nil
			})
			dataStore := newFakeForwardStore(execution)
			dataStore.actionCount = func() int { return len(client.Actions()) }
			adapter := newForwardAdapter(t, client, dataStore)
			result, err := adapter.ExecuteDeploy(context.Background(), execution)
			if err != nil {
				t.Fatal(err)
			}
			if dataStore.failedCalls != 1 || dataStore.quarantineCalls != 0 {
				t.Fatalf("failed=%d quarantine=%d reason=%s", dataStore.failedCalls, dataStore.quarantineCalls, dataStore.quarantineReason)
			}
			assertCompensatedObjects(t, context.Background(), client, previousEnvelope, *execution.PreviousRelease, result.Lease, test.wantHPA)
			lastMutation := 0
			for index, action := range client.Actions() {
				if action.GetVerb() == "update" || action.GetVerb() == "create" || action.GetVerb() == "delete" {
					lastMutation = index
				}
			}
			if dataStore.finalizeActionCount <= lastMutation {
				t.Fatalf("finalization action=%d before final mutation=%d actions=%v", dataStore.finalizeActionCount, lastMutation, client.Actions())
			}
		})
	}
}

func TestDeployCompensationRestoreFailureQuarantinesWithoutFinalization(t *testing.T) {
	previousEnvelope := compensationEnvelope(t, "none", "previous")
	failedEnvelope := compensationEnvelope(t, "hpa", "failed")
	previousSnapshot, previousHash, _ := store.EncodeReleaseSnapshot(previousEnvelope)
	failedSnapshot, failedHash, _ := store.EncodeReleaseSnapshot(failedEnvelope)
	execution := deployExecution(t, failedEnvelope)
	execution.ReleaseID = "failed"
	execution.FailedRelease = store.ImmutableReleaseSnapshot{ID: "failed", EnvironmentID: "environment", ConfigSnapshot: failedSnapshot, ConfigHash: failedHash}
	execution.PreviousRelease = &store.ImmutableReleaseSnapshot{ID: "previous", EnvironmentID: "environment", ConfigSnapshot: previousSnapshot, ConfigHash: previousHash}
	failedObjects, _ := BuildDesiredObjects(execution, failedEnvelope, nil)
	terminal := failedObjects.Deployment.DeepCopy()
	terminal.Generation = 2
	terminal.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}}
	client := fake.NewSimpleClientset(failedObjects.Service.DeepCopy(), terminal, failedObjects.Ingress.DeepCopy(), failedObjects.HPA.DeepCopy())
	serviceUpdates := 0
	client.PrependReactor("update", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
		serviceUpdates++
		if serviceUpdates > 1 {
			return true, nil, errors.New("permanent restore failure")
		}
		return false, nil, nil
	})
	dataStore := newFakeForwardStore(execution)
	adapter := newForwardAdapter(t, client, dataStore)
	if _, err := adapter.ExecuteDeploy(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	if dataStore.failedCalls != 0 || dataStore.quarantineCalls != 1 {
		t.Fatalf("failed=%d quarantine=%d", dataStore.failedCalls, dataStore.quarantineCalls)
	}
}

func compensationEnvelope(t *testing.T, scaling, identity string) store.ReleaseSnapshotEnvelope {
	envelope := deployEnvelope(t, scaling)
	envelope.DesiredState.Deployment.Identity.Labels["cyclopus.dev/state"] = identity
	envelope.DesiredState.Deployment.PodLabels["cyclopus.dev/state"] = identity
	envelope.DesiredState.Service.Identity.Labels["cyclopus.dev/state"] = identity
	envelope.DesiredState.Ingress.Identity.Labels["cyclopus.dev/state"] = identity
	if scaling == "hpa" {
		envelope.DesiredState.Scaling.Name = "app"
	}
	if identity == "previous" {
		envelope.DesiredState.Deployment.Container.Image = "registry/previous@sha256:" + strings.Repeat("1", 64)
		envelope.DesiredState.Deployment.Container.Port = 8181
		envelope.DesiredState.Service.TargetPort = 8181
		envelope.DesiredState.Deployment.Container.Resources = store.ResourceIntent{CPURequestMillicores: 75, CPULimitMillicores: 250, MemoryRequestMiB: 96, MemoryLimitMiB: 384}
		envelope.DesiredState.Deployment.Container.ReadinessProbe = store.ReadinessIntent{Path: "/previous-ready", Port: 8181}
		envelope.DesiredState.Deployment.RuntimeClassName = "gvisor"
		envelope.DesiredState.Deployment.ProgressDeadlineSeconds = 77
		envelope.DesiredState.Deployment.Strategy = store.RollingUpdateIntent{Type: "RollingUpdate", MaxUnavailable: 0, MaxSurge: 1}
		envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{"previous-env"}
		envelope.DesiredState.Ingress.Hostname = "previous.example.test"
		envelope.DesiredState.Ingress.ServiceName = "app"
		envelope.DesiredState.Ingress.ServicePort = 80
		envelope.DesiredState.Ingress.TLS.SecretName = "previous-tls"
	} else {
		envelope.DesiredState.Deployment.Container.Image = "registry/failed@sha256:" + strings.Repeat("2", 64)
		envelope.DesiredState.Deployment.Container.Port = 9292
		envelope.DesiredState.Service.TargetPort = 9292
		envelope.DesiredState.Deployment.Container.Resources = store.ResourceIntent{CPURequestMillicores: 150, CPULimitMillicores: 500, MemoryRequestMiB: 192, MemoryLimitMiB: 768}
		envelope.DesiredState.Deployment.Container.ReadinessProbe = store.ReadinessIntent{Path: "/failed-ready", Port: 9292}
		envelope.DesiredState.Deployment.RuntimeClassName = "runc"
		envelope.DesiredState.Deployment.ProgressDeadlineSeconds = 99
		envelope.DesiredState.Deployment.Strategy = store.RollingUpdateIntent{Type: "RollingUpdate", MaxUnavailable: 0, MaxSurge: 1}
		envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{"failed-env"}
		envelope.DesiredState.Ingress.Hostname = "failed.example.test"
		envelope.DesiredState.Ingress.ServiceName = "app"
		envelope.DesiredState.Ingress.ServicePort = 80
		envelope.DesiredState.Ingress.TLS.SecretName = "failed-tls"
		envelope.DesiredState.Deployment.Identity.Labels["cyclopus.dev/failed-only"] = "remove"
		envelope.DesiredState.Service.Identity.Labels["cyclopus.dev/failed-only"] = "remove"
		envelope.DesiredState.Ingress.Identity.Labels["cyclopus.dev/failed-only"] = "remove"
		envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/failed-only"] = "remove"
	}
	return envelope
}

func assertCompensatedObjects(t *testing.T, ctx context.Context, client *fake.Clientset, envelope store.ReleaseSnapshotEnvelope, previous store.ImmutableReleaseSnapshot, lease store.DeployLease, wantHPA bool) {
	t.Helper()
	service, _ := client.CoreV1().Services("tenant").Get(ctx, "app", metav1.GetOptions{})
	deployment, _ := client.AppsV1().Deployments("tenant").Get(ctx, "app", metav1.GetOptions{})
	ingress, _ := client.NetworkingV1().Ingresses("tenant").Get(ctx, "app", metav1.GetOptions{})
	d := envelope.DesiredState.Deployment
	c := deployment.Spec.Template.Spec.Containers[0]
	if service.Spec.Ports[0].TargetPort.StrVal != "http" || c.Ports[0].Name != "http" || c.Ports[0].ContainerPort != d.Container.Port || c.Image != d.Container.Image || c.ReadinessProbe.HTTPGet.Path != d.Container.ReadinessProbe.Path || c.Resources.Requests.Cpu().MilliValue() != d.Container.Resources.CPURequestMillicores || *deployment.Spec.Template.Spec.RuntimeClassName != d.RuntimeClassName || *deployment.Spec.ProgressDeadlineSeconds != d.ProgressDeadlineSeconds || deployment.Spec.Template.Labels["cyclopus.dev/state"] != "previous" || c.EnvFrom[0].SecretRef.Name != "previous-env" {
		t.Fatalf("deployment/service not restored: service=%+v deployment=%+v", service.Spec, deployment.Spec)
	}
	if deployment.Labels["cyclopus.dev/failed-only"] != "" || service.Labels["cyclopus.dev/failed-only"] != "" || ingress.Labels["cyclopus.dev/failed-only"] != "" || deployment.Spec.Template.Annotations["cyclopus.dev/failed-only"] != "" || deployment.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal != d.Strategy.MaxUnavailable || deployment.Spec.Strategy.RollingUpdate.MaxSurge.IntVal != d.Strategy.MaxSurge {
		t.Fatalf("owned metadata/strategy not restored: deployment=%v service=%v ingress=%v strategy=%+v", deployment.Labels, service.Labels, ingress.Labels, deployment.Spec.Strategy)
	}
	if ingress.Spec.Rules[0].Host != envelope.DesiredState.Ingress.Hostname || ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name != envelope.DesiredState.Ingress.ServiceName || ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number != envelope.DesiredState.Ingress.ServicePort || ingress.Spec.TLS[0].SecretName != envelope.DesiredState.Ingress.TLS.SecretName {
		t.Fatalf("ingress not restored: %+v", ingress.Spec)
	}
	for name, annotations := range map[string]map[string]string{"service": service.Annotations, "ingress": ingress.Annotations} {
		if annotations[ReleaseAnnotation] != previous.ID || annotations[ConfigHashAnnotation] != previous.ConfigHash {
			t.Fatalf("%s identity not restored: %v", name, annotations)
		}
	}
	fence, err := time.Parse(time.RFC3339Nano, deployment.Annotations[DeployFenceAnnotation])
	if err != nil || fence.After(lease.Fence) || deployment.Annotations[ReleaseAnnotation] != previous.ID || deployment.Annotations[ConfigHashAnnotation] != previous.ConfigHash || deployment.Spec.Template.Annotations[ReleaseAnnotation] != previous.ID || deployment.Spec.Template.Annotations[ConfigHashAnnotation] != previous.ConfigHash {
		t.Fatalf("restored identity=%v template=%v", deployment.Annotations, deployment.Spec.Template.Annotations)
	}
	hpa, err := client.AutoscalingV2().HorizontalPodAutoscalers("tenant").Get(ctx, "app", metav1.GetOptions{})
	if wantHPA && err != nil {
		t.Fatal(err)
	}
	if wantHPA && (hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != envelope.DesiredState.Scaling.HPA.MinReplicas || hpa.Spec.MaxReplicas != envelope.DesiredState.Scaling.HPA.MaxReplicas) {
		t.Fatalf("HPA state not restored: %+v", hpa.Spec)
	}
	if wantHPA && (hpa.Annotations[ReleaseAnnotation] != previous.ID || hpa.Annotations[ConfigHashAnnotation] != previous.ConfigHash) {
		t.Fatalf("HPA identity not restored: %v", hpa.Annotations)
	}
	if !wantHPA && !apierrors.IsNotFound(err) {
		t.Fatalf("HPA remained: %+v %v", hpa, err)
	}
}

func TestDeployOptimisticConflictRetriesAndTransientCancellationDoesNotFinalize(t *testing.T) {
	envelope := deployEnvelope(t, "hpa")
	execution := deployExecution(t, envelope)
	objects, _ := BuildDesiredObjects(execution, envelope, nil)
	service := objects.Service.DeepCopy()
	service.ResourceVersion = "1"
	client := fake.NewSimpleClientset(service)
	conflicts := 1
	client.PrependReactor("update", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			return false, nil, nil
		}
		conflicts--
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "services"}, "app", errors.New("conflict"))
	})
	dataStore := newFakeForwardStore(execution)
	adapter := newForwardAdapter(t, client, dataStore)
	if err := adapter.applyService(context.Background(), &execution, objects.Service); err != nil || countActions(client.Actions(), "update") != 2 {
		t.Fatalf("conflict retry error=%v updates=%d", err, countActions(client.Actions(), "update"))
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.ExecuteDeploy(canceled, execution); err == nil {
		t.Fatal("canceled deploy succeeded")
	}
	if dataStore.liveCalls != 0 || dataStore.failedCalls != 0 {
		t.Fatalf("transient cancellation finalized live=%d failed=%d", dataStore.liveCalls, dataStore.failedCalls)
	}
}

func TestEnvSnapshotMutableSourceFailsBeforeAnyDestinationOrDeploymentMutation(t *testing.T) {
	envelope := envDeployEnvelope(t)
	execution := deployExecution(t, envelope)
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "tenant"}, Data: map[string][]byte{"TOKEN": []byte("secret")}}
	client := fake.NewSimpleClientset(source)
	dataStore := newFakeForwardStore(execution)
	adapter := newForwardAdapter(t, client, dataStore)
	if _, err := adapter.ExecuteDeploy(context.Background(), execution); err != nil {
		t.Fatalf("permanent source failure was not terminalized: %v", err)
	}
	if dataStore.failedCalls != 1 || countActions(client.Actions(), "create") != 0 || countActions(client.Actions(), "update") != 0 {
		t.Fatalf("failed=%d creates=%d updates=%d", dataStore.failedCalls, countActions(client.Actions(), "create"), countActions(client.Actions(), "update"))
	}
}

func TestEnvSnapshotImmutableSourceMaterializesStableContentAndRejectsMismatchedDestination(t *testing.T) {
	envelope := envDeployEnvelope(t)
	execution := deployExecution(t, envelope)
	immutable := true
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "tenant"}, Immutable: &immutable, Data: map[string][]byte{"TOKEN": []byte("secret")}}
	client := fake.NewSimpleClientset(source)
	adapter := newForwardAdapter(t, client, newFakeForwardStore(execution))
	data1, err := adapter.resolveSecretData(context.Background(), execution, envelope)
	if err != nil {
		t.Fatal(err)
	}
	data2, err := adapter.resolveSecretData(context.Background(), execution, envelope)
	if err != nil || !reflect.DeepEqual(data1, data2) {
		t.Fatalf("stable reads = %#v/%#v %v", data1, data2, err)
	}
	objects, err := BuildDesiredObjects(execution, envelope, data1)
	if err != nil {
		t.Fatal(err)
	}
	if objects.Secret.Annotations[SecretContentHashAnnotation] != ComputeSecretDataHash(data1) {
		t.Fatal("destination content hash missing")
	}
	mismatch := objects.Secret.DeepCopy()
	mismatch.Data["TOKEN"] = []byte("other")
	client = fake.NewSimpleClientset(source, mismatch)
	adapter = newForwardAdapter(t, client, newFakeForwardStore(execution))
	if err := adapter.preflightDesired(context.Background(), execution, objects); !errors.Is(err, ErrImmutableSecretMismatch) {
		t.Fatalf("mismatch error=%v", err)
	}
}

func TestDeployPreflightSelectorDriftFinalizesFailedWithoutMutationAndUnpausesAuthority(t *testing.T) {
	envelope := deployEnvelope(t, "none")
	execution := deployExecution(t, envelope)
	objects, _ := BuildDesiredObjects(execution, envelope, nil)
	current := objects.Deployment.DeepCopy()
	current.Spec.Selector.MatchLabels["drift"] = "true"
	current.Spec.Paused = true
	client := fake.NewSimpleClientset(current)
	dataStore := newFakeForwardStore(execution)
	adapter := newForwardAdapter(t, client, dataStore)
	if _, err := adapter.ExecuteDeploy(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	if dataStore.failedCalls != 1 || countActions(client.Actions(), "update") != 0 {
		t.Fatalf("failed=%d updates=%d", dataStore.failedCalls, countActions(client.Actions(), "update"))
	}
	current = objects.Deployment.DeepCopy()
	current.ResourceVersion = "1"
	current.Spec.Paused = true
	client = fake.NewSimpleClientset(current)
	adapter = newForwardAdapter(t, client, newFakeForwardStore(execution))
	if err := adapter.applyDeployment(context.Background(), &execution, objects.Deployment); err != nil {
		t.Fatal(err)
	}
	updated, _ := client.AppsV1().Deployments("tenant").Get(context.Background(), "app", metav1.GetOptions{})
	if updated.Spec.Paused {
		t.Fatal("managed Deployment remained paused")
	}
}

func TestDeployObservationBoundLeavesDurableDeployingWithoutFailureFinalization(t *testing.T) {
	envelope := deployEnvelope(t, "none")
	execution := deployExecution(t, envelope)
	execution.Timeout = 5 * time.Millisecond
	execution.ExpiresAt = time.Now().Add(time.Second)
	objects, _ := BuildDesiredObjects(execution, envelope, nil)
	current := objects.Deployment.DeepCopy()
	current.Generation = 2
	client := fake.NewSimpleClientset(current)
	dataStore := newFakeForwardStore(execution)
	adapter := newForwardAdapter(t, client, dataStore)
	adapter.config.PollInterval = time.Millisecond
	if _, err := adapter.ExecuteDeploy(context.Background(), execution); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bound error=%v", err)
	}
	if dataStore.failedCalls != 0 || dataStore.liveCalls != 0 {
		t.Fatalf("finalized live=%d failed=%d", dataStore.liveCalls, dataStore.failedCalls)
	}
}

func TestRollbackLegacyEnvSnapshotUsesUpgradedDestinationWithoutChangingReleaseHash(t *testing.T) {
	envelope := envDeployEnvelope(t)
	envelope.DesiredState.Secret = nil
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = nil
	legacyBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	legacyHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	execution := deployExecution(t, deployEnvelope(t, "none"))
	execution.PreviousRelease = &store.ImmutableReleaseSnapshot{ID: "previous", ConfigSnapshot: legacyBytes, ConfigHash: legacyHash, EnvVarRevisionID: deployStringPointer("revision")}
	decoded, err := store.DecodeReleaseSnapshot(execution.PreviousRelease.ConfigSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := DecodeDeploymentSnapshot(execution.PreviousRelease.ConfigSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.DesiredState.Secret == nil || len(previous.Spec.Template.Spec.Containers[0].EnvFrom) != 1 || previous.Spec.Template.Spec.Containers[0].EnvFrom[0].SecretRef.Name != decoded.DesiredState.Secret.Identity.Name || execution.PreviousRelease.ConfigHash != legacyHash {
		t.Fatalf("legacy rollback decoded=%+v envFrom=%+v hash=%s", decoded.DesiredState.Secret, previous.Spec.Template.Spec.Containers[0].EnvFrom, execution.PreviousRelease.ConfigHash)
	}
}

func envDeployEnvelope(t *testing.T) store.ReleaseSnapshotEnvelope {
	envelope := deployEnvelope(t, "none")
	variables := []store.ReleaseEnvironment{{Key: "TOKEN", SecretRef: "k8s-secret://tenant/source#TOKEN", IsSecret: true}}
	hash := store.ComputeEnvVarRevisionHash(variables)
	envelope.EnvVarRevision = &store.EnvVarRevision{Version: 1, ID: "revision", Revision: 1, Hash: hash, Variables: variables}
	name := store.EnvDestinationSecretName("app", "revision")
	envelope.DesiredState.Secret = &store.SecretIntent{Version: 1, Identity: store.ObjectIdentity{Name: name, Namespace: "tenant", Labels: envelope.DesiredState.Deployment.Identity.Labels}, Immutable: true, Entries: []store.SecretEntryIntent{{Key: "TOKEN", SecretRef: variables[0].SecretRef}}}
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{name}
	envelope.DesiredState.Deployment.PodAnnotations[EnvRevisionAnnotation] = "revision"
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = hash
	return envelope
}

func deployStringPointer(value string) *string { return &value }

func deployEnvelope(t *testing.T, scaling string) store.ReleaseSnapshotEnvelope {
	t.Helper()
	labels := map[string]string{ManagedByLabel: ManagedByValue, EnvironmentIDLabel: "environment", "app.kubernetes.io/name": "app"}
	selector := map[string]string{EnvironmentIDLabel: "environment", "app.kubernetes.io/name": "app"}
	identity := store.ObjectIdentity{Name: "app", Namespace: "tenant", Labels: labels}
	snapshot := store.ReleaseSnapshotEnvelope{Version: store.ReleaseSnapshotVersion, DesiredState: store.DesiredStateSnapshot{Version: 1,
		Deployment: store.DeploymentIntent{Version: 1, Identity: identity, Selector: selector, Replicas: 1, Strategy: store.RollingUpdateIntent{Type: "RollingUpdate", MaxUnavailable: 0, MaxSurge: 1}, PodLabels: labels, PodAnnotations: map[string]string{ReleaseAnnotation: store.ReleaseIDAnnotationTemplate, ConfigHashAnnotation: store.ConfigHashAnnotationTemplate}, RuntimeClassName: "gvisor", ProgressDeadlineSeconds: 60, Container: store.ContainerIntent{Name: "app", Image: "registry/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Port: 8080, Resources: store.ResourceIntent{CPURequestMillicores: 100, CPULimitMillicores: 200, MemoryRequestMiB: 128, MemoryLimitMiB: 256}, ReadinessProbe: store.ReadinessIntent{Path: "/ready", Port: 8080}}},
		Service:    store.ServiceIntent{Version: 1, Identity: identity, Selector: selector, Port: 80, TargetPort: 8080},
		Ingress:    store.IngressIntent{Version: 1, Identity: identity, Hostname: "app.example.test", Path: "/", ServiceName: "app", ServicePort: 80, TLS: store.TLSIntent{Enabled: true, SecretName: "app-tls"}},
		Scaling:    store.ScalingIntent{Version: 1, Kind: scaling},
	}}
	if scaling == "hpa" {
		snapshot.DesiredState.Scaling.Name = "app"
		snapshot.DesiredState.Scaling.HPA = &store.HPAIntent{MinReplicas: 1, MaxReplicas: 3, TargetCPUPercent: 70}
	}
	if _, _, err := store.EncodeReleaseSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func deployExecution(t *testing.T, envelope store.ReleaseSnapshotEnvelope) store.DeployExecution {
	t.Helper()
	encoded, hash, err := store.EncodeReleaseSnapshot(envelope)
	if err != nil {
		t.Fatal(err)
	}
	fence := time.Now().UTC().Truncate(time.Microsecond)
	return store.DeployExecution{OrgID: "org", EnvironmentID: "environment", ReleaseID: "release", Namespace: "tenant", PlannedHostname: "app.example.test", Fence: fence, ExpiresAt: fence.Add(time.Hour), Timeout: time.Minute, FailedRelease: store.ImmutableReleaseSnapshot{ID: "release", EnvironmentID: "environment", ConfigSnapshot: encoded, ConfigHash: hash}}
}

func newForwardAdapter(t *testing.T, client *fake.Clientset, dataStore *fakeForwardStore) *RecoveryAdapter {
	t.Helper()
	adapter, err := NewAdapter(Config{PollInterval: time.Nanosecond, ConflictRetries: 3}, client, dataStore)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

type fakeForwardStore struct {
	lease               store.DeployLease
	liveCalls           int
	failedCalls         int
	quarantineCalls     int
	quarantineLease     store.DeployLease
	quarantineCode      string
	quarantineReason    string
	actionCount         func() int
	finalizeActionCount int
}

func newFakeForwardStore(lease store.DeployLease) *fakeForwardStore {
	return &fakeForwardStore{lease: lease}
}

func (f *fakeForwardStore) RenewDeployLease(_ context.Context, lease store.DeployLease) (store.DeployLease, error) {
	lease.Fence = lease.Fence.Add(time.Microsecond)
	lease.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
	f.lease = lease
	return lease, nil
}
func (*fakeForwardStore) CheckpointDeployRecovery(context.Context, store.DeployLease, string) error {
	return nil
}
func (*fakeForwardStore) AdoptRecoveredReleaseLive(context.Context, store.DeployLease) (store.FinalizeReleaseResult, error) {
	return store.FinalizeReleaseResult{}, nil
}
func (f *fakeForwardStore) FinalizeRecoveredReleaseFailed(context.Context, store.DeployLease, string) (store.FinalizeReleaseResult, error) {
	f.failedCalls++
	return store.FinalizeReleaseResult{}, nil
}
func (f *fakeForwardStore) FinalizeReleaseLive(context.Context, store.FinalizeReleaseParams) (store.FinalizeReleaseResult, error) {
	f.liveCalls++
	return store.FinalizeReleaseResult{}, nil
}
func (f *fakeForwardStore) FinalizeReleaseFailed(context.Context, store.FailReleaseParams) (store.FinalizeReleaseResult, error) {
	f.failedCalls++
	if f.actionCount != nil {
		f.finalizeActionCount = f.actionCount()
	}
	return store.FinalizeReleaseResult{}, nil
}
func (f *fakeForwardStore) QuarantineDeployRecovery(_ context.Context, lease store.DeployLease, code, reason string) error {
	f.quarantineCalls++
	f.quarantineLease = lease
	f.quarantineCode = code
	f.quarantineReason = reason
	return nil
}

var _ = autoscalingv2.HorizontalPodAutoscaler{}
var _ = networkingv1.Ingress{}
