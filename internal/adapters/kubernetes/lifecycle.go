package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

func intstrPointer(value int) *intstr.IntOrString {
	port := intstr.FromInt(value)
	return &port
}

func protocolPointer(value corev1.Protocol) *corev1.Protocol { return &value }

// observabilityNamespace is the namespace of the OTel Collector the
// app→collector OTLP egress rule must reach. scripts/install.sh allows the
// platform namespace to differ from the default ("cyclopus") via
// CYCLOPUS_NAMESPACE, so read the override from the environment (§7.2)
// instead of hardcoding the default.
func observabilityNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("CYCLOPUS_OBSERVABILITY_NAMESPACE")); ns != "" {
		return ns
	}
	return "cyclopus"
}

const PoliciesPendingLabel = "cyclopus.app/policies-pending"

var (
	ErrPolicyProvisionerUnavailable = errors.New("Phase 3 policy provisioner is unavailable")
	ErrPolicyVerificationFailed     = errors.New("Phase 3 policy verification did not succeed")
	ErrHostnameProviderUnavailable  = errors.New("Phase 3 hostname provider is unavailable")
)

type PolicyProvisioner interface {
	ProvisionAndVerify(context.Context, store.EnvironmentLifecycleJob) error
}

type HostnameProvider interface {
	PreviewHostname(context.Context, store.EnvironmentLifecycleJob) (string, error)
	ProductionHostname(hostnameLabel string) (string, error)
}

type NetworkPolicyProvisioner struct {
	client              kubernetes.Interface
	egressExcludedCIDRs []string
}

// defaultEgressExceptCIDRs are the baseline private/link-local/loopback ranges
// carved out of the default-deny-egress 0.0.0.0/0 rule. These are
// load-bearing (169.254.0.0/16 covers cloud metadata; 127.0.0.0/8 loopback) and
// must never be dropped. Operator-supplied node public IP CIDRs are appended to
// this baseline via mergeEgressExceptCIDRs.
var defaultEgressExceptCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8"}

// mergeEgressExceptCIDRs returns the baseline default CIDRs followed by the
// operator-supplied extras, de-duplicated and in deterministic order (defaults
// first in their existing order, then extras in their given order, skipping any
// already present). Determinism matters because the reconcile drift check and
// the provisioner update path compare NetworkPolicy specs.
func mergeEgressExceptCIDRs(extras []string) []string {
	seen := make(map[string]struct{}, len(defaultEgressExceptCIDRs)+len(extras))
	merged := make([]string, 0, len(defaultEgressExceptCIDRs)+len(extras))
	for _, cidr := range append(append([]string{}, defaultEgressExceptCIDRs...), extras...) {
		if _, ok := seen[cidr]; ok {
			continue
		}
		seen[cidr] = struct{}{}
		merged = append(merged, cidr)
	}
	return merged
}

func NewNetworkPolicyProvisioner(client kubernetes.Interface, egressExcludedCIDRs []string) *NetworkPolicyProvisioner {
	return &NetworkPolicyProvisioner{client: client, egressExcludedCIDRs: egressExcludedCIDRs}
}

func (p *NetworkPolicyProvisioner) ProvisionAndVerify(ctx context.Context, job store.EnvironmentLifecycleJob) error {
	labels := map[string]string{ManagedByLabel: ManagedByValue, EnvironmentIDLabel: job.EnvironmentID}
	policies := []*networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "default-deny-ingress", Namespace: job.Namespace, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "default-deny-egress", Namespace: job.Namespace, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}}}, Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPointer(53), Protocol: protocolPointer(corev1.ProtocolUDP)}, {Port: intstrPointer(53), Protocol: protocolPointer(corev1.ProtocolTCP)}}},
					{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
					{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": observabilityNamespace()}}, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "opentelemetry-collector"}}}}, Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPointer(4317), Protocol: protocolPointer(corev1.ProtocolTCP)}, {Port: intstrPointer(4318), Protocol: protocolPointer(corev1.ProtocolTCP)}}},
					{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: mergeEgressExceptCIDRs(p.egressExcludedCIDRs)}}}},
				},
			},
		},
	}
	for _, desired := range policies {
		current, err := p.client.NetworkingV1().NetworkPolicies(job.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if _, err = p.client.NetworkingV1().NetworkPolicies(job.Namespace).Create(ctx, desired, metav1.CreateOptions{}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if !ownedByEnvironment(current.Labels, job.EnvironmentID) {
				return ErrResourceOwnership
			}
			candidate := current.DeepCopy()
			candidate.Spec = desired.Spec
			if _, err = p.client.NetworkingV1().NetworkPolicies(job.Namespace).Update(ctx, candidate, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
		verified, err := p.client.NetworkingV1().NetworkPolicies(job.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil || !ownedByEnvironment(verified.Labels, job.EnvironmentID) {
			return ErrPolicyVerificationFailed
		}
	}
	return nil
}

// SuffixHostnameProvider implements HostnameProvider for both preview and
// production environments using a single configured DNS suffix. Preview
// hostnames are derived from the environment namespace slug; production
// hostnames are derived from the app's hostname_label.
type SuffixHostnameProvider struct{ suffix string }

// NewSuffixHostnameProvider validates and returns a SuffixHostnameProvider.
// suffix is the operator-configured DNS suffix appended to all platform
// hostnames (e.g. "203-0-113-7.sslip.io" or "apps.example.com").
func NewSuffixHostnameProvider(suffix string) (*SuffixHostnameProvider, error) {
	suffix = strings.Trim(strings.ToLower(strings.TrimSpace(suffix)), ".")
	if suffix == "" || len(suffix) > 253 || strings.ContainsAny(suffix, "/:@") {
		return nil, errors.New("hostname suffix is invalid")
	}
	return &SuffixHostnameProvider{suffix: suffix}, nil
}

func (p *SuffixHostnameProvider) PreviewHostname(_ context.Context, job store.EnvironmentLifecycleJob) (string, error) {
	if !strings.HasPrefix(job.Namespace, "preview-") {
		return "", ErrHostnameProviderUnavailable
	}
	return strings.TrimPrefix(job.Namespace, "preview-") + "." + p.suffix, nil
}

// ProductionHostname returns the default production hostname for an app,
// assembled as hostnameLabel + "." + suffix.
func (p *SuffixHostnameProvider) ProductionHostname(hostnameLabel string) (string, error) {
	if hostnameLabel == "" {
		return "", errors.New("hostname_label is required")
	}
	return hostnameLabel + "." + p.suffix, nil
}

type LifecycleStore interface {
	CheckpointEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob, string) error
	BlockEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob, string, string, string) error
	CompleteEnvironmentProvision(context.Context, store.EnvironmentLifecycleJob, *string) error
	CompleteEnvironmentTeardown(context.Context, store.EnvironmentLifecycleJob) error
}

type LifecycleAdapter struct {
	client    kubernetes.Interface
	store     LifecycleStore
	policy    PolicyProvisioner
	hostnames HostnameProvider
	poll      time.Duration
}

func NewLifecycleAdapter(client kubernetes.Interface, dataStore LifecycleStore, policy PolicyProvisioner, hostnames HostnameProvider, poll time.Duration) (*LifecycleAdapter, error) {
	if client == nil || dataStore == nil || poll <= 0 {
		return nil, errors.New("invalid Kubernetes lifecycle adapter configuration")
	}
	return &LifecycleAdapter{client: client, store: dataStore, policy: policy, hostnames: hostnames, poll: poll}, nil
}

func (a *LifecycleAdapter) ExecuteEnvironmentLifecycle(ctx context.Context, job store.EnvironmentLifecycleJob) error {
	switch job.Kind {
	case "provision":
		return a.provision(ctx, job)
	case "teardown":
		return a.teardown(ctx, job)
	default:
		return fmt.Errorf("unknown environment lifecycle kind %q", job.Kind)
	}
}

func (a *LifecycleAdapter) provision(ctx context.Context, job store.EnvironmentLifecycleJob) error {
	labels := map[string]string{ManagedByLabel: ManagedByValue, EnvironmentIDLabel: job.EnvironmentID, PoliciesPendingLabel: "true"}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: job.Namespace, Labels: labels}}
	current, err := a.client.CoreV1().Namespaces().Get(ctx, job.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err = a.client.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if current.Labels[ManagedByLabel] != ManagedByValue || current.Labels[EnvironmentIDLabel] != job.EnvironmentID {
		return ErrResourceOwnership
	} else if current.Labels[PoliciesPendingLabel] != "true" {
		candidate := current.DeepCopy()
		candidate.Labels[PoliciesPendingLabel] = "true"
		if _, err = a.client.CoreV1().Namespaces().Update(ctx, candidate, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	if err := a.store.CheckpointEnvironmentLifecycle(ctx, job, "namespace_pending"); err != nil {
		return err
	}
	if err := a.applyPreviewBounds(ctx, job, labels); err != nil {
		return err
	}
	if err := a.store.CheckpointEnvironmentLifecycle(ctx, job, "resource_bounds_applied"); err != nil {
		return err
	}
	if a.policy == nil {
		return a.store.BlockEnvironmentLifecycle(ctx, job, "policy_capability_unavailable", "policy_provisioner_unavailable", ErrPolicyProvisionerUnavailable.Error())
	}
	if err := a.policy.ProvisionAndVerify(ctx, job); err != nil {
		return fmt.Errorf("%w: %v", ErrPolicyVerificationFailed, err)
	}
	if a.hostnames == nil {
		return a.store.BlockEnvironmentLifecycle(ctx, job, "hostname_capability_unavailable", "hostname_provider_unavailable", ErrHostnameProviderUnavailable.Error())
	}
	hostname, err := a.hostnames.PreviewHostname(ctx, job)
	if err != nil || hostname == "" {
		return fmt.Errorf("provide preview hostname: %w", err)
	}
	current, err = a.client.CoreV1().Namespaces().Get(ctx, job.Namespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	candidate := current.DeepCopy()
	delete(candidate.Labels, PoliciesPendingLabel)
	if _, err := a.client.CoreV1().Namespaces().Update(ctx, candidate, metav1.UpdateOptions{}); err != nil {
		return err
	}
	if err := a.store.CompleteEnvironmentProvision(ctx, job, &hostname); err != nil {
		current, getErr := a.client.CoreV1().Namespaces().Get(ctx, job.Namespace, metav1.GetOptions{})
		if getErr == nil {
			rollback := current.DeepCopy()
			rollback.Labels[PoliciesPendingLabel] = "true"
			_, _ = a.client.CoreV1().Namespaces().Update(ctx, rollback, metav1.UpdateOptions{})
		}
		return err
	}
	return nil
}

func (a *LifecycleAdapter) applyPreviewBounds(ctx context.Context, job store.EnvironmentLifecycleJob, labels map[string]string) error {
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "cyclopus-preview", Namespace: job.Namespace, Labels: labels}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
		corev1.ResourcePods:           resource.MustParse("1"),
		corev1.ResourceRequestsCPU:    *resource.NewMilliQuantity(int64(job.App.CPURequestMillicores), resource.DecimalSI),
		corev1.ResourceLimitsCPU:      *resource.NewMilliQuantity(int64(job.App.CPULimitMillicores), resource.DecimalSI),
		corev1.ResourceRequestsMemory: *resource.NewQuantity(int64(job.App.MemoryRequestMiB)*1024*1024, resource.BinarySI),
		corev1.ResourceLimitsMemory:   *resource.NewQuantity(int64(job.App.MemoryLimitMiB)*1024*1024, resource.BinarySI),
	}}}
	if current, err := a.client.CoreV1().ResourceQuotas(job.Namespace).Get(ctx, quota.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err = a.client.CoreV1().ResourceQuotas(job.Namespace).Create(ctx, quota, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if !ownedByEnvironment(current.Labels, job.EnvironmentID) {
			return ErrResourceOwnership
		}
		candidate := current.DeepCopy()
		candidate.Spec = quota.Spec
		if _, err = a.client.CoreV1().ResourceQuotas(job.Namespace).Update(ctx, candidate, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	limitRange := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "cyclopus-preview", Namespace: job.Namespace, Labels: labels}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer,
		DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(int64(job.App.CPURequestMillicores), resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(int64(job.App.MemoryRequestMiB)*1024*1024, resource.BinarySI)},
		Default:        corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(int64(job.App.CPULimitMillicores), resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(int64(job.App.MemoryLimitMiB)*1024*1024, resource.BinarySI)},
	}}}}
	if current, err := a.client.CoreV1().LimitRanges(job.Namespace).Get(ctx, limitRange.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		_, err = a.client.CoreV1().LimitRanges(job.Namespace).Create(ctx, limitRange, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	} else {
		if !ownedByEnvironment(current.Labels, job.EnvironmentID) {
			return ErrResourceOwnership
		}
		candidate := current.DeepCopy()
		candidate.Spec = limitRange.Spec
		_, err = a.client.CoreV1().LimitRanges(job.Namespace).Update(ctx, candidate, metav1.UpdateOptions{})
		return err
	}
}

func (a *LifecycleAdapter) teardown(ctx context.Context, job store.EnvironmentLifecycleJob) error {
	current, err := a.client.CoreV1().Namespaces().Get(ctx, job.Namespace, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if !ownedByEnvironment(current.Labels, job.EnvironmentID) {
			return ErrResourceOwnership
		}
		uid := current.UID
		policy := metav1.DeletePropagationForeground
		err = a.client.CoreV1().Namespaces().Delete(ctx, job.Namespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if err := a.store.CheckpointEnvironmentLifecycle(ctx, job, "namespace_delete_requested"); err != nil {
		return err
	}
	if err := wait.PollUntilContextCancel(ctx, a.poll, true, func(ctx context.Context) (bool, error) {
		_, err := a.client.CoreV1().Namespaces().Get(ctx, job.Namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		return err
	}
	return a.store.CompleteEnvironmentTeardown(ctx, job)
}

func ownedByEnvironment(labels map[string]string, environmentID string) bool {
	return labels[ManagedByLabel] == ManagedByValue && labels[EnvironmentIDLabel] == environmentID
}
