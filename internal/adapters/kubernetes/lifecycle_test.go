package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPreviewProvisionBlockedKeepsPendingLabelAndResourceBounds(t *testing.T) {
	client := fake.NewSimpleClientset()
	persisted := &fakeLifecycleStore{}
	adapter, err := NewLifecycleAdapter(client, persisted, nil, nil, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	job := previewLifecycleJob()
	if err := adapter.ExecuteEnvironmentLifecycle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	namespace, err := client.CoreV1().Namespaces().Get(context.Background(), job.Namespace, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[PoliciesPendingLabel] != "true" || persisted.blockCode != "policy_provisioner_unavailable" || persisted.completedProvision != 0 {
		t.Fatalf("namespace=%v block=%s completed=%d", namespace.Labels, persisted.blockCode, persisted.completedProvision)
	}
	quota, err := client.CoreV1().ResourceQuotas(job.Namespace).Get(context.Background(), "cyclopus-preview", metav1.GetOptions{})
	limitCPU := quota.Spec.Hard["limits.cpu"]
	requestCPU := quota.Spec.Hard["requests.cpu"]
	if err != nil || quota.Spec.Hard.Pods().Value() != 1 || limitCPU.MilliValue() != 200 || requestCPU.MilliValue() != 100 {
		t.Fatalf("quota=%+v err=%v", quota.Spec.Hard, err)
	}
	limits, err := client.CoreV1().LimitRanges(job.Namespace).Get(context.Background(), "cyclopus-preview", metav1.GetOptions{})
	if err != nil || limits.Spec.Limits[0].Default.Cpu().MilliValue() != 200 || limits.Spec.Limits[0].DefaultRequest.Cpu().MilliValue() != 100 {
		t.Fatalf("limit range=%+v err=%v", limits.Spec, err)
	}
}

func TestPreviewProvisionVerifiedPolicyRemovesPendingLabelAndMarksReady(t *testing.T) {
	client := fake.NewSimpleClientset()
	persisted := &fakeLifecycleStore{}
	adapter, _ := NewLifecycleAdapter(client, persisted, successfulPolicy{}, staticHostname("preview.example.test"), time.Millisecond)
	job := previewLifecycleJob()
	if err := adapter.ExecuteEnvironmentLifecycle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	namespace, _ := client.CoreV1().Namespaces().Get(context.Background(), job.Namespace, metav1.GetOptions{})
	if _, exists := namespace.Labels[PoliciesPendingLabel]; exists || persisted.completedProvision != 1 || persisted.hostname != "preview.example.test" {
		t.Fatalf("labels=%v completed=%d hostname=%q", namespace.Labels, persisted.completedProvision, persisted.hostname)
	}
	found := false
	for _, checkpoint := range persisted.checkpoints {
		found = found || checkpoint == "resource_bounds_applied"
	}
	if !found {
		t.Fatalf("preview checkpoints = %v, want resource_bounds_applied recorded", persisted.checkpoints)
	}
}

func TestNetworkPolicyProvisionerCreatesAndVerifiesRequiredPolicies(t *testing.T) {
	client := fake.NewSimpleClientset()
	job := previewLifecycleJob()
	if err := NewNetworkPolicyProvisioner(client, nil).ProvisionAndVerify(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"default-deny-ingress", "default-deny-egress"} {
		policy, err := client.NetworkingV1().NetworkPolicies(job.Namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil || !ownedByEnvironment(policy.Labels, job.EnvironmentID) {
			t.Fatalf("policy %s = %+v, %v", name, policy, err)
		}
	}
	egress, _ := client.NetworkingV1().NetworkPolicies(job.Namespace).Get(context.Background(), "default-deny-egress", metav1.GetOptions{})
	if len(egress.Spec.Egress) != 4 || egress.Spec.Egress[3].To[0].IPBlock == nil {
		t.Fatalf("egress policy = %+v", egress.Spec.Egress)
	}
}

func TestNetworkPolicyProvisionerMergesConfiguredEgressExceptCIDRs(t *testing.T) {
	client := fake.NewSimpleClientset()
	job := previewLifecycleJob()
	// "10.0.0.0/8" is already a default and must be de-duplicated; the node
	// public CIDR "203.0.113.0/24" must be appended after the 5 defaults.
	provisioner := NewNetworkPolicyProvisioner(client, []string{"203.0.113.0/24", "10.0.0.0/8"})
	if err := provisioner.ProvisionAndVerify(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	egress, err := client.NetworkingV1().NetworkPolicies(job.Namespace).Get(context.Background(), "default-deny-egress", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	block := egress.Spec.Egress[3].To[0].IPBlock
	if block == nil {
		t.Fatalf("egress IPBlock is nil: %+v", egress.Spec.Egress)
	}
	want := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8", "203.0.113.0/24"}
	if len(block.Except) != len(want) {
		t.Fatalf("Except = %v, want %v", block.Except, want)
	}
	for i, cidr := range want {
		if block.Except[i] != cidr {
			t.Fatalf("Except[%d] = %q, want %q (full: %v)", i, block.Except[i], cidr, block.Except)
		}
	}
	dupes := 0
	for _, cidr := range block.Except {
		if cidr == "10.0.0.0/8" {
			dupes++
		}
	}
	if dupes != 1 {
		t.Fatalf("expected 10.0.0.0/8 exactly once, got %d times: %v", dupes, block.Except)
	}
}

func TestNetworkPolicyProvisionerDefaultExceptCIDRsWithoutExtras(t *testing.T) {
	client := fake.NewSimpleClientset()
	job := previewLifecycleJob()
	if err := NewNetworkPolicyProvisioner(client, nil).ProvisionAndVerify(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	egress, err := client.NetworkingV1().NetworkPolicies(job.Namespace).Get(context.Background(), "default-deny-egress", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	block := egress.Spec.Egress[3].To[0].IPBlock
	want := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8"}
	if block == nil || len(block.Except) != len(want) {
		t.Fatalf("Except = %+v, want exactly the 5 defaults %v", block, want)
	}
	for i, cidr := range want {
		if block.Except[i] != cidr {
			t.Fatalf("Except[%d] = %q, want %q (full: %v)", i, block.Except[i], cidr, block.Except)
		}
	}
}

func TestPreviewHostnameProviderUsesPreviewSlugAndConfiguredSuffix(t *testing.T) {
	provider, err := NewSuffixHostnameProvider("previews.example.test")
	if err != nil {
		t.Fatal(err)
	}
	hostname, err := provider.PreviewHostname(context.Background(), previewLifecycleJob())
	if err != nil || hostname != "calm-otter-0001.previews.example.test" {
		t.Fatalf("hostname = %q, %v", hostname, err)
	}
}

func TestPreviewProvisionRestoresPendingLabelWhenDurableCompletionFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	persisted := &fakeLifecycleStore{completeProvisionErr: errors.New("database unavailable")}
	adapter, _ := NewLifecycleAdapter(client, persisted, successfulPolicy{}, staticHostname("preview.example.test"), time.Millisecond)
	job := previewLifecycleJob()
	if err := adapter.ExecuteEnvironmentLifecycle(context.Background(), job); !errors.Is(err, persisted.completeProvisionErr) {
		t.Fatalf("error = %v", err)
	}
	namespace, err := client.CoreV1().Namespaces().Get(context.Background(), job.Namespace, metav1.GetOptions{})
	if err != nil || namespace.Labels[PoliciesPendingLabel] != "true" {
		t.Fatalf("namespace labels = %v, %v", namespace.Labels, err)
	}
}

func TestPreviewTeardownNamespaceDeletionIsIdempotentAndWaitsAbsent(t *testing.T) {
	client := fake.NewSimpleClientset()
	persisted := &fakeLifecycleStore{}
	adapter, _ := NewLifecycleAdapter(client, persisted, nil, nil, time.Millisecond)
	job := previewLifecycleJob()
	job.Kind = "teardown"
	if err := adapter.ExecuteEnvironmentLifecycle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), job.Namespace, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("namespace remained: %v", err)
	}
	if persisted.completedTeardown != 1 {
		t.Fatalf("completed teardown = %d", persisted.completedTeardown)
	}
	if err := adapter.ExecuteEnvironmentLifecycle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewTeardownRejectsUnownedNamespace(t *testing.T) {
	job := previewLifecycleJob()
	client := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: job.Namespace}})
	adapter, _ := NewLifecycleAdapter(client, &fakeLifecycleStore{}, nil, nil, time.Millisecond)
	job.Kind = "teardown"
	if err := adapter.ExecuteEnvironmentLifecycle(context.Background(), job); !errors.Is(err, ErrResourceOwnership) {
		t.Fatalf("error = %v", err)
	}
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), job.Namespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("unowned namespace was deleted: %v", err)
	}
}

func previewLifecycleJob() store.EnvironmentLifecycleJob {
	return store.EnvironmentLifecycleJob{ID: "job", OrgID: "org", EnvironmentID: "environment", Kind: "provision", EnvironmentKind: "preview", Namespace: "preview-calm-otter-0001", App: store.App{CPURequestMillicores: 100, CPULimitMillicores: 200, MemoryRequestMiB: 128, MemoryLimitMiB: 256}}
}

func productionLifecycleJob() store.EnvironmentLifecycleJob {
	hostname := "whoami.example.test"
	return store.EnvironmentLifecycleJob{ID: "job", OrgID: "org", EnvironmentID: "environment", Kind: "provision", EnvironmentKind: "production", Namespace: "whoami-production", PlannedHostname: &hostname, App: store.App{CPURequestMillicores: 100, CPULimitMillicores: 200, MemoryRequestMiB: 128, MemoryLimitMiB: 256}}
}

func TestProductionProvisionUsesPlannedHostnameWithoutHostnameProvider(t *testing.T) {
	client := fake.NewSimpleClientset()
	persisted := &fakeLifecycleStore{}
	adapter, err := NewLifecycleAdapter(client, persisted, successfulPolicy{}, nil, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	job := productionLifecycleJob()
	if err := adapter.ExecuteEnvironmentLifecycle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	namespace, err := client.CoreV1().Namespaces().Get(context.Background(), job.Namespace, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := namespace.Labels[PoliciesPendingLabel]; exists || persisted.completedProvision != 1 || persisted.hostname != *job.PlannedHostname {
		t.Fatalf("labels=%v completed=%d hostname=%q", namespace.Labels, persisted.completedProvision, persisted.hostname)
	}
	// Spec §4.3: ResourceQuota/LimitRange are preview-only; a production
	// namespace must not be capped (HPA/ScaledObject would be defeated).
	if quota, err := client.CoreV1().ResourceQuotas(job.Namespace).Get(context.Background(), "cyclopus-preview", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("production quota should not exist: quota=%+v err=%v", quota.Spec.Hard, err)
	}
	if limits, err := client.CoreV1().LimitRanges(job.Namespace).Get(context.Background(), "cyclopus-preview", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("production limit range should not exist: limits=%+v err=%v", limits.Spec, err)
	}
	// Checkpoint honesty: bounds were skipped, so production checkpoints must
	// not claim resource_bounds_applied.
	for _, checkpoint := range persisted.checkpoints {
		if checkpoint == "resource_bounds_applied" {
			t.Fatalf("production provision recorded resource_bounds_applied without applying bounds: checkpoints=%v", persisted.checkpoints)
		}
	}
	if len(persisted.checkpoints) == 0 || persisted.checkpoints[0] != "namespace_pending" {
		t.Fatalf("checkpoints = %v, want first checkpoint namespace_pending", persisted.checkpoints)
	}
}

func TestProductionProvisionMissingPlannedHostnameBlocksFailClosed(t *testing.T) {
	client := fake.NewSimpleClientset()
	persisted := &fakeLifecycleStore{}
	adapter, err := NewLifecycleAdapter(client, persisted, successfulPolicy{}, nil, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	job := productionLifecycleJob()
	job.PlannedHostname = nil
	if err := adapter.ExecuteEnvironmentLifecycle(context.Background(), job); err != nil {
		t.Fatalf("blocking should not surface as a job error: %v", err)
	}
	if persisted.blockCode != "planned_hostname_missing" || persisted.completedProvision != 0 {
		t.Fatalf("nil planned_hostname must block fail-closed: block=%q completed=%d", persisted.blockCode, persisted.completedProvision)
	}
	namespace, err := client.CoreV1().Namespaces().Get(context.Background(), job.Namespace, metav1.GetOptions{})
	if err != nil || namespace.Labels[PoliciesPendingLabel] != "true" {
		t.Fatalf("blocked namespace should keep the pending label: labels=%v err=%v", namespace.Labels, err)
	}
}

type fakeLifecycleStore struct {
	blockCode            string
	completedProvision   int
	completedTeardown    int
	hostname             string
	checkpoints          []string
	completeProvisionErr error
}

func (s *fakeLifecycleStore) CheckpointEnvironmentLifecycle(_ context.Context, _ store.EnvironmentLifecycleJob, checkpoint string) error {
	s.checkpoints = append(s.checkpoints, checkpoint)
	return nil
}
func (s *fakeLifecycleStore) BlockEnvironmentLifecycle(_ context.Context, _ store.EnvironmentLifecycleJob, _, code, _ string) error {
	s.blockCode = code
	return nil
}
func (s *fakeLifecycleStore) CompleteEnvironmentProvision(_ context.Context, _ store.EnvironmentLifecycleJob, hostname *string) error {
	if s.completeProvisionErr != nil {
		return s.completeProvisionErr
	}
	s.completedProvision++
	if hostname != nil {
		s.hostname = *hostname
	}
	return nil
}
func (s *fakeLifecycleStore) CompleteEnvironmentTeardown(context.Context, store.EnvironmentLifecycleJob) error {
	s.completedTeardown++
	return nil
}

type successfulPolicy struct{}

func (successfulPolicy) ProvisionAndVerify(context.Context, store.EnvironmentLifecycleJob) error {
	return nil
}

type staticHostname string

func (value staticHostname) PreviewHostname(context.Context, store.EnvironmentLifecycleJob) (string, error) {
	return string(value), nil
}
