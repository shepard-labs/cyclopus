package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/secretref"
	"github.com/cyclopus-dev/cyclopus/internal/store"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

const DeployFenceAnnotation = "cyclopus.dev/deploy-fence"

var (
	ErrKEDACRDMissing          = errors.New("Kubernetes KEDA CRDs are unavailable")
	ErrDesiredStateInvalid     = errors.New("Kubernetes desired state is invalid")
	ErrResourceOwnership       = errors.New("Kubernetes resource ownership is ambiguous")
	ErrNewerDeployFence        = errors.New("Kubernetes resource has a newer deploy fence")
	ErrImmutableSecretMismatch = errors.New("immutable Secret differs from release intent")
	ErrRolloutTerminal         = errors.New("deployment rollout terminally failed")
	ErrMutableSourceSecret     = errors.New("environment source Secret must be immutable")
)

var (
	scaledObjectGVR     = schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}
	httpScaledObjectGVR = schema.GroupVersionResource{Group: "http.keda.sh", Version: "v1alpha1", Resource: "httpscaledobjects"}
)

type ForwardStore interface {
	RecoveryStore
	FinalizeReleaseLive(context.Context, store.FinalizeReleaseParams) (store.FinalizeReleaseResult, error)
	FinalizeReleaseFailed(context.Context, store.FailReleaseParams) (store.FinalizeReleaseResult, error)
}

type DesiredObjects struct {
	Secret                 *corev1.Secret
	Service                *corev1.Service
	BridgeService          *corev1.Service // ExternalName bridge for keda_http interceptor routing
	Deployment             *appsv1.Deployment
	Ingress                *networkingv1.Ingress
	HPA                    *autoscalingv2.HorizontalPodAutoscaler
	ScaledObject           *unstructured.Unstructured
	HTTPScaledObject       *unstructured.Unstructured
	DeleteHPA              string
	DeleteScaledObject     string
	DeleteHTTPScaledObject string
	DeleteBridgeService    string // owned bridge service to remove on mode transitions
}

// KEDAHTTPOptions configures the HTTP add-on interceptor routing for keda_http apps.
// Traffic from the Ingress must pass through the interceptor proxy so scale-from-zero works.
type KEDAHTTPOptions struct {
	// InterceptorFQDN is the full cluster-internal FQDN of the interceptor proxy service,
	// e.g. "keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local".
	InterceptorFQDN string
	// InterceptorPort is the port the interceptor proxy listens on (default 8080).
	InterceptorPort int32
}

// defaultKEDAHTTPOptions returns sensible defaults matching the standard KEDA HTTP add-on v0.15.0 Helm chart.
func defaultKEDAHTTPOptions() KEDAHTTPOptions {
	return KEDAHTTPOptions{
		InterceptorFQDN: "keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local",
		InterceptorPort: 8080,
	}
}

func BuildDesiredObjects(execution store.DeployExecution, envelope store.ReleaseSnapshotEnvelope, secretData map[string][]byte, httpOpts ...KEDAHTTPOptions) (DesiredObjects, error) {
	desired := envelope.DesiredState
	if execution.Namespace == "" || execution.ReleaseID == "" || execution.FailedRelease.ConfigHash == "" {
		return DesiredObjects{}, fmt.Errorf("%w: execution identity is incomplete", ErrDesiredStateInvalid)
	}
	d := desired.Deployment
	if d.Identity.Namespace != execution.Namespace || desired.Service.Identity.Namespace != execution.Namespace || desired.Ingress.Identity.Namespace != execution.Namespace {
		return DesiredObjects{}, fmt.Errorf("%w: snapshot namespace differs from execution namespace", ErrDesiredStateInvalid)
	}
	labels := managedLabels(d.Identity.Labels, execution.EnvironmentID)
	annotations := resolvedAnnotations(d.PodAnnotations, execution)
	replicas := d.Replicas
	deadline := d.ProgressDeadlineSeconds
	maxUnavailable := intstr.FromInt32(d.Strategy.MaxUnavailable)
	maxSurge := intstr.FromInt32(d.Strategy.MaxSurge)
	container := corev1.Container{
		Name: d.Container.Name, Image: d.Container.Image,
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: d.Container.Port}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(d.Container.Resources.CPURequestMillicores, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(d.Container.Resources.MemoryRequestMiB*1024*1024, resource.BinarySI)},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(d.Container.Resources.CPULimitMillicores, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(d.Container.Resources.MemoryLimitMiB*1024*1024, resource.BinarySI)},
		},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: d.Container.ReadinessProbe.Path, Port: intstr.FromInt32(d.Container.ReadinessProbe.Port)}}},
	}
	for _, name := range d.Container.EnvFromSecrets {
		container.EnvFrom = append(container.EnvFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}})
	}
	deployment := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: d.Identity.Name, Namespace: execution.Namespace, Labels: labels, Annotations: cloneStrings(annotations)}, Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, ProgressDeadlineSeconds: &deadline,
		Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &maxUnavailable, MaxSurge: &maxSurge}},
		Selector: &metav1.LabelSelector{MatchLabels: cloneStrings(d.Selector)},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: managedLabels(d.PodLabels, execution.EnvironmentID), Annotations: cloneStrings(annotations)}, Spec: corev1.PodSpec{RuntimeClassName: &d.RuntimeClassName, Containers: []corev1.Container{container}}},
	}}
	service := &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{Name: desired.Service.Identity.Name, Namespace: execution.Namespace, Labels: managedLabels(desired.Service.Identity.Labels, execution.EnvironmentID), Annotations: cloneStrings(annotations)}, Spec: corev1.ServiceSpec{Selector: cloneStrings(desired.Service.Selector), Ports: []corev1.ServicePort{{Name: "http", Port: desired.Service.Port, TargetPort: intstr.FromString("http")}}}}
	pathType := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{Name: desired.Ingress.Identity.Name, Namespace: execution.Namespace, Labels: managedLabels(desired.Ingress.Identity.Labels, execution.EnvironmentID), Annotations: cloneStrings(annotations)},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: desired.Ingress.Hostname,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
					Path: desired.Ingress.Path, PathType: &pathType,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: desired.Ingress.ServiceName, Port: networkingv1.ServiceBackendPort{Number: desired.Ingress.ServicePort}}},
				}}}},
			}},
			TLS: []networkingv1.IngressTLS{{Hosts: []string{desired.Ingress.Hostname}, SecretName: desired.Ingress.TLS.SecretName}},
		},
	}
	// Resolve keda_http interceptor options (H2: must route via interceptor for scale-from-zero).
	httpOptions := defaultKEDAHTTPOptions()
	if len(httpOpts) > 0 && httpOpts[0].InterceptorFQDN != "" {
		httpOptions = httpOpts[0]
	}
	bridgeSvcName := d.Identity.Name + "-keda-http-proxy"
	scalerName := desired.Scaling.Name
	if scalerName == "" {
		scalerName = d.Identity.Name
	}
	// H3: cron ScaledObjects are named "<scalerName>-cron" to avoid colliding
	// with the HTTP add-on's generated child ScaledObject. Deletes during
	// transitions must use the correct name for each kind.
	cronScalerName := scalerName + "-cron"
	objects := DesiredObjects{Service: service, Deployment: deployment, Ingress: ingress, DeleteHPA: d.Identity.Name, DeleteScaledObject: cronScalerName, DeleteHTTPScaledObject: scalerName, DeleteBridgeService: bridgeSvcName}
	if desired.Secret != nil {
		immutable := true
		secretAnnotations := map[string]string{}
		secretAnnotations[SecretContentHashAnnotation] = ComputeSecretDataHash(secretData)
		if envelope.EnvVarRevision != nil {
			secretAnnotations[EnvRevisionAnnotation] = envelope.EnvVarRevision.ID
		}
		objects.Secret = &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: desired.Secret.Identity.Name, Namespace: execution.Namespace, Labels: managedLabels(desired.Secret.Identity.Labels, execution.EnvironmentID), Annotations: secretAnnotations}, Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: cloneBytes(secretData)}
	}
	switch desired.Scaling.Kind {
	case "hpa":
		objects.HPA = buildHPA(desired, labels, annotations, execution, d.Identity.Name)
		objects.DeleteHPA = ""
	case "keda":
		switch desired.Scaling.KEDA.Mode {
		case "cron":
			objects.ScaledObject = buildKEDACronScaledObject(desired, labels, annotations, execution)
			objects.DeleteScaledObject = ""
			objects.DeleteHTTPScaledObject = scalerName
		case "http":
			objects.HTTPScaledObject = buildKEDAHTTPScaledObject(desired, labels, annotations, execution)
			objects.DeleteScaledObject = cronScalerName
			objects.DeleteHTTPScaledObject = ""
			// H2: Route ingress through the interceptor bridge so scale-from-zero works.
			bridge := buildKEDAHTTPBridgeService(d.Identity.Name, execution.Namespace, labels, annotations, execution.EnvironmentID, httpOptions)
			objects.BridgeService = bridge
			objects.DeleteBridgeService = ""
			// Repoint ingress at bridge service on interceptor port.
			ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name = bridge.Name
			ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number = httpOptions.InterceptorPort
		}
	}
	return objects, nil
}

// buildKEDAHTTPBridgeService creates an ExternalName Service that routes the
// app's Ingress to the HTTP add-on interceptor proxy (H2). The interceptor holds
// incoming requests and triggers scale-up when min_replicas==0.
func buildKEDAHTTPBridgeService(deploymentName, namespace string, labels, annotations map[string]string, environmentID string, opts KEDAHTTPOptions) *corev1.Service {
	bridgeName := deploymentName + "-keda-http-proxy"
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: bridgeName, Namespace: namespace, Labels: managedLabels(labels, environmentID), Annotations: cloneStrings(annotations)},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: opts.InterceptorFQDN,
			Ports:        []corev1.ServicePort{{Name: "http", Port: opts.InterceptorPort}},
		},
	}
}

func buildHPA(desired store.DesiredStateSnapshot, labels, annotations map[string]string, execution store.DeployExecution, deploymentName string) *autoscalingv2.HorizontalPodAutoscaler {
	minReplicas := desired.Scaling.HPA.MinReplicas
	return &autoscalingv2.HorizontalPodAutoscaler{TypeMeta: metav1.TypeMeta{APIVersion: "autoscaling/v2", Kind: "HorizontalPodAutoscaler"}, ObjectMeta: metav1.ObjectMeta{Name: desired.Scaling.Name, Namespace: execution.Namespace, Labels: labels, Annotations: cloneStrings(annotations)}, Spec: autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: deploymentName}, MinReplicas: &minReplicas, MaxReplicas: desired.Scaling.HPA.MaxReplicas, Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &desired.Scaling.HPA.TargetCPUPercent}}}}}}
}

func buildKEDACronScaledObject(desired store.DesiredStateSnapshot, labels, annotations map[string]string, execution store.DeployExecution) *unstructured.Unstructured {
	keda := desired.Scaling.KEDA
	triggers := make([]any, 0, len(keda.CronSchedules)+1)
	for index, schedule := range keda.CronSchedules {
		triggers = append(triggers, map[string]any{
			"type":     "cron",
			"metadata": map[string]any{"name": fmt.Sprintf("cron-%d", index), "timezone": schedule.Timezone, "start": schedule.Start, "end": schedule.End, "desiredReplicas": fmt.Sprintf("%d", schedule.DesiredReplicas)},
		})
	}
	triggers = append(triggers, map[string]any{
		"type":     "cpu",
		"metadata": map[string]any{"type": "Utilization", "value": fmt.Sprintf("%d", keda.TargetCPUPercent)},
	})
	// H3: cron ScaledObjects use a "-cron" suffix to avoid name collision with
	// the HTTP add-on's generated child ScaledObject on mode transitions.
	cronName := desired.Scaling.Name + "-cron"
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		"spec": map[string]any{
			"scaleTargetRef":  map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": desired.Deployment.Identity.Name},
			"minReplicaCount": int64(keda.MinReplicas),
			"maxReplicaCount": int64(keda.MaxReplicas),
			"triggers":        triggers,
		},
	}}
	object.SetName(cronName)
	object.SetNamespace(execution.Namespace)
	object.SetLabels(cloneStrings(labels))
	object.SetAnnotations(cloneStrings(annotations))
	return object
}

func buildKEDAHTTPScaledObject(desired store.DesiredStateSnapshot, labels, annotations map[string]string, execution store.DeployExecution) *unstructured.Unstructured {
	keda := desired.Scaling.KEDA
	// H1: scaleTargetRef must use "name" (not "deployment") per v0.15.0 CRD.
	// H1: scalingMetric.concurrency.targetValue must be int64, not string.
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "http.keda.sh/v1alpha1",
		"kind":       "HTTPScaledObject",
		"spec": map[string]any{
			"hosts":        []any{desired.Ingress.Hostname},
			"pathPrefixes": []any{desired.Ingress.Path},
			"scaleTargetRef": map[string]any{
				"name":       desired.Deployment.Identity.Name,
				"kind":       "Deployment",
				"apiVersion": "apps/v1",
				"service":    desired.Service.Identity.Name,
				"port":       int64(desired.Service.Port),
			},
			"replicas":      map[string]any{"min": int64(keda.MinReplicas), "max": int64(keda.MaxReplicas)},
			"scalingMetric": map[string]any{"concurrency": map[string]any{"targetValue": int64(keda.HTTPTargetPendingRequests)}},
		},
	}}
	object.SetName(desired.Scaling.Name)
	object.SetNamespace(execution.Namespace)
	object.SetLabels(cloneStrings(labels))
	object.SetAnnotations(cloneStrings(annotations))
	return object
}

func (a *RecoveryAdapter) ExecuteDeploy(ctx context.Context, execution store.DeployExecution) (store.DeployRecoveryResult, error) {
	result := store.DeployRecoveryResult{Lease: execution}
	bound := time.Now().Add(execution.Timeout)
	if execution.ExpiresAt.Before(bound) {
		bound = execution.ExpiresAt
	}
	ctx, cancel := context.WithDeadline(ctx, bound)
	defer cancel()
	envelope, err := store.DecodeReleaseSnapshot(execution.FailedRelease.ConfigSnapshot)
	if err != nil {
		return a.failWithoutMutation(ctx, execution, fmt.Errorf("%w: %v", ErrDesiredStateInvalid, err))
	}
	secretData, err := a.resolveSecretData(ctx, execution, envelope)
	if err != nil {
		if isForwardRetryable(err) {
			return result, err
		}
		return a.failWithoutMutation(ctx, execution, err)
	}
	objects, err := BuildDesiredObjects(execution, envelope, secretData)
	if err != nil {
		return a.failWithoutMutation(ctx, execution, err)
	}
	if err := a.preflightDesired(ctx, execution, objects); err != nil {
		if isForwardRetryable(err) {
			return result, err
		}
		return a.failWithoutMutation(ctx, execution, err)
	}
	execution, err = a.renew(ctx, execution)
	result.Lease = execution
	if err != nil {
		return result, err
	}
	if err := a.applyDesired(ctx, &execution, objects); err != nil {
		result.Lease = execution
		if isForwardRetryable(err) {
			return result, err
		}
		return a.compensateOrQuarantine(ctx, execution, err)
	}
	result.Lease = execution
	err = a.observeForward(ctx, &execution, objects.Deployment)
	result.Lease = execution
	if err == nil {
		_, err = a.store.(ForwardStore).FinalizeReleaseLive(ctx, store.FinalizeReleaseParams{
			Lease: execution, Hostname: execution.PlannedHostname, Audit: forwardAuditEvent(),
		})
		return result, err
	}
	if !errors.Is(err, ErrRolloutTerminal) {
		if isForwardRetryable(err) {
			return result, err
		}
		return a.compensateOrQuarantine(ctx, execution, err)
	}
	return a.compensateOrQuarantine(ctx, execution, err)
}

func (a *RecoveryAdapter) failWithoutMutation(ctx context.Context, execution store.DeployExecution, cause error) (store.DeployRecoveryResult, error) {
	result := store.DeployRecoveryResult{Lease: execution}
	_, err := a.store.(ForwardStore).FinalizeReleaseFailed(ctx, store.FailReleaseParams{
		Lease: execution, FailureReason: cause.Error(), Audit: forwardAuditEvent(),
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func (a *RecoveryAdapter) resolveSecretData(ctx context.Context, execution store.DeployExecution, envelope store.ReleaseSnapshotEnvelope) (map[string][]byte, error) {
	if envelope.DesiredState.Secret == nil {
		return nil, nil
	}
	data := make(map[string][]byte, len(envelope.DesiredState.Secret.Entries))
	for _, entry := range envelope.DesiredState.Secret.Entries {
		parsed, err := secretref.Parse(entry.SecretRef)
		if err != nil || parsed.Namespace != execution.Namespace {
			return nil, fmt.Errorf("%w: invalid source Secret reference", ErrDesiredStateInvalid)
		}
		opCtx, cancel := a.operationContext(ctx, execution)
		secret, err := a.client.CoreV1().Secrets(parsed.Namespace).Get(opCtx, parsed.Name, metav1.GetOptions{})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("read source Secret %s/%s: %w", parsed.Namespace, parsed.Name, err)
		}
		if secret.Immutable == nil || !*secret.Immutable {
			return nil, fmt.Errorf("%w: source Secret %s/%s", ErrMutableSourceSecret, parsed.Namespace, parsed.Name)
		}
		value, ok := secret.Data[parsed.Key]
		if !ok {
			return nil, fmt.Errorf("%w: source Secret key is missing", ErrDesiredStateInvalid)
		}
		data[entry.Key] = append([]byte(nil), value...)
	}
	return data, nil
}

func (a *RecoveryAdapter) preflightDesired(ctx context.Context, execution store.DeployExecution, objects DesiredObjects) error {
	if objects.Secret != nil {
		current, err := a.client.CoreV1().Secrets(objects.Secret.Namespace).Get(ctx, objects.Secret.Name, metav1.GetOptions{})
		if err == nil {
			if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
				return err
			}
			if current.Immutable == nil || !*current.Immutable || !reflect.DeepEqual(current.Data, objects.Secret.Data) ||
				current.Annotations[EnvRevisionAnnotation] != objects.Secret.Annotations[EnvRevisionAnnotation] ||
				current.Annotations[SecretContentHashAnnotation] != ComputeSecretDataHash(current.Data) {
				return ErrImmutableSecretMismatch
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	for _, check := range []func() error{
		func() error {
			current, err := a.client.CoreV1().Services(objects.Service.Namespace).Get(ctx, objects.Service.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			return verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence)
		},
		func() error {
			current, err := a.client.AppsV1().Deployments(objects.Deployment.Namespace).Get(ctx, objects.Deployment.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
				return err
			}
			if !reflect.DeepEqual(current.Spec.Selector, objects.Deployment.Spec.Selector) {
				return fmt.Errorf("%w: Deployment selector drift is immutable", ErrDesiredStateInvalid)
			}
			return nil
		},
		func() error {
			current, err := a.client.NetworkingV1().Ingresses(objects.Ingress.Namespace).Get(ctx, objects.Ingress.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			return verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence)
		},
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return a.preflightScaling(ctx, execution, objects)
}

func (a *RecoveryAdapter) preflightScaling(ctx context.Context, execution store.DeployExecution, objects DesiredObjects) error {
	if objects.HPA != nil || objects.DeleteHPA != "" {
		name := objects.DeleteHPA
		if objects.HPA != nil {
			name = objects.HPA.Name
		}
		current, err := a.client.AutoscalingV2().HorizontalPodAutoscalers(execution.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	for _, scaler := range []struct {
		gvr     schema.GroupVersionResource
		desired *unstructured.Unstructured
		deleted string
	}{
		{gvr: scaledObjectGVR, desired: objects.ScaledObject, deleted: objects.DeleteScaledObject},
		{gvr: httpScaledObjectGVR, desired: objects.HTTPScaledObject, deleted: objects.DeleteHTTPScaledObject},
	} {
		if scaler.desired == nil && scaler.deleted == "" {
			continue
		}
		// A delete-only path means the object should be absent. If the dynamic client
		// or CRD is unavailable, the object cannot exist — preflight passes.
		if scaler.desired == nil {
			if a.dynamic == nil || !a.dynamicHasKind(ctx, scaler.gvr) {
				continue
			}
		} else {
			if a.dynamic == nil {
				return fmt.Errorf("%w: dynamic Kubernetes client is not configured", ErrKEDACRDMissing)
			}
			if !a.dynamicHasKind(ctx, scaler.gvr) {
				return ErrKEDACRDMissing
			}
		}
		name := scaler.deleted
		if scaler.desired != nil {
			name = scaler.desired.GetName()
		}
		current, err := a.dynamic.Resource(scaler.gvr).Namespace(execution.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if err := verifyObject(current.GetLabels(), current.GetAnnotations(), execution.EnvironmentID, execution.Fence); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return classifyDynamicError(err)
		}
	}
	return nil
}

func (a *RecoveryAdapter) applyDesired(ctx context.Context, execution *store.DeployExecution, objects DesiredObjects) error {
	return a.applyDesiredWithIdentity(ctx, execution, objects, execution.ReleaseID, execution.FailedRelease.ConfigHash)
}

func (a *RecoveryAdapter) applyDesiredWithIdentity(ctx context.Context, execution *store.DeployExecution, objects DesiredObjects, releaseID, configHash string) error {
	setDesiredIdentity(objects, releaseID, configHash)
	if err := a.preflightScaling(ctx, *execution, objects); err != nil {
		return err
	}
	if objects.Secret != nil {
		if err := a.applySecret(ctx, execution, objects.Secret, releaseID, configHash); err != nil {
			return err
		}
	}
	if err := a.applyService(ctx, execution, objects.Service); err != nil {
		return err
	}
	if err := a.applyDeployment(ctx, execution, objects.Deployment); err != nil {
		return err
	}
	if err := a.applyIngress(ctx, execution, objects.Ingress); err != nil {
		return err
	}
	// Preflight proves ownership before any scaler mutation. Delete the old
	// controller before applying the replacement so two controllers never
	// target the Deployment concurrently (§3.4).
	if objects.DeleteHPA != "" {
		if err := a.deleteOwnedHPA(ctx, execution, objects.DeleteHPA); err != nil {
			return err
		}
	}
	if objects.DeleteScaledObject != "" {
		if err := a.deleteOwnedScaledObject(ctx, execution, scaledObjectGVR, objects.DeleteScaledObject); err != nil {
			return err
		}
	}
	if objects.DeleteHTTPScaledObject != "" {
		if err := a.deleteOwnedScaledObject(ctx, execution, httpScaledObjectGVR, objects.DeleteHTTPScaledObject); err != nil {
			return err
		}
	}
	if objects.HPA != nil {
		if err := a.applyHPA(ctx, execution, objects.HPA); err != nil {
			return err
		}
	}
	if objects.ScaledObject != nil {
		if err := a.applyScaledObject(ctx, execution, scaledObjectGVR, objects.ScaledObject); err != nil {
			return err
		}
	}
	if objects.HTTPScaledObject != nil {
		if err := a.applyScaledObject(ctx, execution, httpScaledObjectGVR, objects.HTTPScaledObject); err != nil {
			return err
		}
	}
	// H2: apply or delete the interceptor bridge Service for keda_http mode.
	if objects.DeleteBridgeService != "" {
		if err := a.deleteOwnedService(ctx, execution, objects.DeleteBridgeService); err != nil {
			return err
		}
	}
	if objects.BridgeService != nil {
		if err := a.applyService(ctx, execution, objects.BridgeService); err != nil {
			return err
		}
	}
	return nil
}

func setDesiredIdentity(objects DesiredObjects, releaseID, configHash string) {
	if objects.Service != nil {
		setMetadataIdentity(&objects.Service.ObjectMeta, releaseID, configHash)
	}
	if objects.Deployment != nil {
		setMetadataIdentity(&objects.Deployment.ObjectMeta, releaseID, configHash)
	}
	if objects.Ingress != nil {
		setMetadataIdentity(&objects.Ingress.ObjectMeta, releaseID, configHash)
	}
	if objects.HPA != nil {
		setMetadataIdentity(&objects.HPA.ObjectMeta, releaseID, configHash)
	}
	if objects.ScaledObject != nil {
		setUnstructuredIdentity(objects.ScaledObject, releaseID, configHash)
	}
	if objects.HTTPScaledObject != nil {
		setUnstructuredIdentity(objects.HTTPScaledObject, releaseID, configHash)
	}
	if objects.BridgeService != nil {
		setMetadataIdentity(&objects.BridgeService.ObjectMeta, releaseID, configHash)
	}
	if objects.Deployment != nil {
		if objects.Deployment.Spec.Template.Annotations == nil {
			objects.Deployment.Spec.Template.Annotations = map[string]string{}
		}
		objects.Deployment.Spec.Template.Annotations[ReleaseAnnotation] = releaseID
		objects.Deployment.Spec.Template.Annotations[ConfigHashAnnotation] = configHash
	}
}

func setUnstructuredIdentity(object *unstructured.Unstructured, releaseID, configHash string) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[ReleaseAnnotation] = releaseID
	annotations[ConfigHashAnnotation] = configHash
	object.SetAnnotations(annotations)
}

func setMetadataIdentity(metadata *metav1.ObjectMeta, releaseID, configHash string) {
	if metadata.Annotations == nil {
		metadata.Annotations = map[string]string{}
	}
	metadata.Annotations[ReleaseAnnotation] = releaseID
	metadata.Annotations[ConfigHashAnnotation] = configHash
}

func (a *RecoveryAdapter) applySecret(ctx context.Context, execution *store.DeployExecution, desired *corev1.Secret, identity ...string) error {
	return a.retryMutation(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		stampFence(&desired.ObjectMeta, lease)
		current, err := a.client.CoreV1().Secrets(desired.Namespace).Get(opCtx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = a.client.CoreV1().Secrets(desired.Namespace).Create(opCtx, desired, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		if current.Immutable == nil || !*current.Immutable || !reflect.DeepEqual(current.Data, desired.Data) ||
			current.Annotations[EnvRevisionAnnotation] != desired.Annotations[EnvRevisionAnnotation] ||
			current.Annotations[SecretContentHashAnnotation] != desired.Annotations[SecretContentHashAnnotation] ||
			current.Annotations[SecretContentHashAnnotation] != ComputeSecretDataHash(current.Data) {
			return ErrImmutableSecretMismatch
		}
		return nil
	})
}

func (a *RecoveryAdapter) applyService(ctx context.Context, execution *store.DeployExecution, desired *corev1.Service) error {
	return a.retryMutation(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		stampFence(&desired.ObjectMeta, lease)
		current, err := a.client.CoreV1().Services(desired.Namespace).Get(opCtx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = a.client.CoreV1().Services(desired.Namespace).Create(opCtx, desired, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		candidate := current.DeepCopy()
		mergeManagedMetadata(&candidate.ObjectMeta, desired.ObjectMeta)
		candidate.Spec.Selector = cloneStrings(desired.Spec.Selector)
		ports := append([]corev1.ServicePort(nil), desired.Spec.Ports...)
		for index := range ports {
			for _, existing := range current.Spec.Ports {
				if existing.Name == ports[index].Name && existing.Port == ports[index].Port {
					ports[index].NodePort = existing.NodePort
					ports[index].AppProtocol = existing.AppProtocol
					break
				}
			}
		}
		candidate.Spec.Ports = ports
		_, err = a.client.CoreV1().Services(desired.Namespace).Update(opCtx, candidate, metav1.UpdateOptions{})
		return err
	})
}

func (a *RecoveryAdapter) applyDeployment(ctx context.Context, execution *store.DeployExecution, desired *appsv1.Deployment) error {
	return a.retryMutation(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		stampFence(&desired.ObjectMeta, lease)
		stampFenceAnnotation(desired.Spec.Template.Annotations, lease)
		current, err := a.client.AppsV1().Deployments(desired.Namespace).Get(opCtx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = a.client.AppsV1().Deployments(desired.Namespace).Create(opCtx, desired, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		if !reflect.DeepEqual(current.Spec.Selector, desired.Spec.Selector) {
			return fmt.Errorf("%w: Deployment selector drift is immutable", ErrDesiredStateInvalid)
		}
		candidate := current.DeepCopy()
		mergeManagedMetadata(&candidate.ObjectMeta, desired.ObjectMeta)
		candidate.Spec.Replicas = desired.Spec.Replicas
		candidate.Spec.Paused = false
		candidate.Spec.Strategy = desired.Spec.Strategy
		candidate.Spec.ProgressDeadlineSeconds = desired.Spec.ProgressDeadlineSeconds
		candidate.Spec.Template.Labels = desired.Spec.Template.Labels
		candidate.Spec.Template.Annotations = desired.Spec.Template.Annotations
		candidate.Spec.Template.Spec.RuntimeClassName = desired.Spec.Template.Spec.RuntimeClassName
		candidate.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
		_, err = a.client.AppsV1().Deployments(desired.Namespace).Update(opCtx, candidate, metav1.UpdateOptions{})
		return err
	})
}

func (a *RecoveryAdapter) compensateOrQuarantine(ctx context.Context, execution store.DeployExecution, cause error) (store.DeployRecoveryResult, error) {
	result, err := a.compensateForward(ctx, execution, cause.Error())
	if err == nil {
		return result, nil
	}
	lease := result.Lease
	if lease.ReleaseID == "" {
		lease = execution
	}
	if quarantineErr := a.store.(ForwardStore).QuarantineDeployRecovery(ctx, lease, "forward_permanent_after_mutation", cause.Error()); quarantineErr != nil {
		return result, fmt.Errorf("quarantine permanent forward failure: %w", quarantineErr)
	}
	return result, nil
}

func isForwardRetryable(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, store.ErrLeaseNotOwned) || errors.Is(err, ErrKEDACRDMissing) || isTransient(err)
}

func (a *RecoveryAdapter) applyIngress(ctx context.Context, execution *store.DeployExecution, desired *networkingv1.Ingress) error {
	return a.retryMutation(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		stampFence(&desired.ObjectMeta, lease)
		current, err := a.client.NetworkingV1().Ingresses(desired.Namespace).Get(opCtx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = a.client.NetworkingV1().Ingresses(desired.Namespace).Create(opCtx, desired, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		candidate := current.DeepCopy()
		mergeManagedMetadata(&candidate.ObjectMeta, desired.ObjectMeta)
		candidate.Spec = desired.Spec
		_, err = a.client.NetworkingV1().Ingresses(desired.Namespace).Update(opCtx, candidate, metav1.UpdateOptions{})
		return err
	})
}

func (a *RecoveryAdapter) applyHPA(ctx context.Context, execution *store.DeployExecution, desired *autoscalingv2.HorizontalPodAutoscaler) error {
	return a.retryMutation(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		stampFence(&desired.ObjectMeta, lease)
		current, err := a.client.AutoscalingV2().HorizontalPodAutoscalers(desired.Namespace).Get(opCtx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = a.client.AutoscalingV2().HorizontalPodAutoscalers(desired.Namespace).Create(opCtx, desired, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		candidate := current.DeepCopy()
		mergeManagedMetadata(&candidate.ObjectMeta, desired.ObjectMeta)
		candidate.Spec = desired.Spec
		_, err = a.client.AutoscalingV2().HorizontalPodAutoscalers(desired.Namespace).Update(opCtx, candidate, metav1.UpdateOptions{})
		return err
	})
}

func (a *RecoveryAdapter) deleteOwnedService(ctx context.Context, execution *store.DeployExecution, name string) error {
	return a.withMutationLease(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		current, err := a.client.CoreV1().Services(execution.Namespace).Get(opCtx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		return a.client.CoreV1().Services(execution.Namespace).Delete(opCtx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.UID}})
	})
}

func (a *RecoveryAdapter) deleteOwnedHPA(ctx context.Context, execution *store.DeployExecution, name string) error {
	return a.withMutationLease(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		current, err := a.client.AutoscalingV2().HorizontalPodAutoscalers(execution.Namespace).Get(opCtx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := verifyObject(current.Labels, current.Annotations, execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		return a.client.AutoscalingV2().HorizontalPodAutoscalers(execution.Namespace).Delete(opCtx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.UID}})
	})
}

func (a *RecoveryAdapter) applyScaledObject(ctx context.Context, execution *store.DeployExecution, gvr schema.GroupVersionResource, desired *unstructured.Unstructured) error {
	if a.dynamic == nil {
		return fmt.Errorf("%w: dynamic Kubernetes client is not configured", ErrKEDACRDMissing)
	}
	return a.retryMutation(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		if !a.dynamicHasKind(opCtx, gvr) {
			return ErrKEDACRDMissing
		}
		current, err := a.dynamic.Resource(gvr).Namespace(desired.GetNamespace()).Get(opCtx, desired.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			stampUnstructuredFence(desired, lease)
			_, err = a.dynamic.Resource(gvr).Namespace(desired.GetNamespace()).Create(opCtx, desired, metav1.CreateOptions{})
			return classifyDynamicError(err)
		}
		if err != nil {
			return classifyDynamicError(err)
		}
		if err := verifyObject(current.GetLabels(), current.GetAnnotations(), execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		candidate := current.DeepCopy()
		mergeUnstructuredMetadata(candidate, desired)
		candidate.Object["spec"] = desired.Object["spec"]
		_, err = a.dynamic.Resource(gvr).Namespace(desired.GetNamespace()).Update(opCtx, candidate, metav1.UpdateOptions{})
		return classifyDynamicError(err)
	})
}

func (a *RecoveryAdapter) deleteOwnedScaledObject(ctx context.Context, execution *store.DeployExecution, gvr schema.GroupVersionResource, name string) error {
	// If no dynamic client or CRD absent, the object cannot exist — deletion is complete.
	if a.dynamic == nil {
		return nil
	}
	return a.withMutationLease(ctx, execution, func(opCtx context.Context, lease store.DeployLease) error {
		if !a.dynamicHasKind(opCtx, gvr) {
			return nil
		}
		current, err := a.dynamic.Resource(gvr).Namespace(execution.Namespace).Get(opCtx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return classifyDynamicError(err)
		}
		if err := verifyObject(current.GetLabels(), current.GetAnnotations(), execution.EnvironmentID, execution.Fence); err != nil {
			return err
		}
		uid := current.GetUID()
		return classifyDynamicError(a.dynamic.Resource(gvr).Namespace(execution.Namespace).Delete(opCtx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}))
	})
}

func (a *RecoveryAdapter) dynamicHasKind(ctx context.Context, gvr schema.GroupVersionResource) bool {
	_, err := a.dynamic.Resource(gvr).Namespace("default").List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil || !isDynamicKindUnavailable(err)
}

func classifyDynamicError(err error) error {
	if err == nil {
		return nil
	}
	if isDynamicKindUnavailable(err) {
		return ErrKEDACRDMissing
	}
	return err
}

func isDynamicKindUnavailable(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}

func stampUnstructuredFence(object *unstructured.Unstructured, lease store.DeployLease) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[DeployFenceAnnotation] = lease.Fence.UTC().Format(time.RFC3339Nano)
	object.SetAnnotations(annotations)
}

func mergeUnstructuredMetadata(candidate, desired *unstructured.Unstructured) {
	labels := candidate.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for key := range labels {
		if isCyclopusManagedMetadataKey(key) {
			delete(labels, key)
		}
	}
	for key, value := range desired.GetLabels() {
		labels[key] = value
	}
	candidate.SetLabels(labels)
	annotations := candidate.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for key := range annotations {
		if isCyclopusManagedMetadataKey(key) {
			delete(annotations, key)
		}
	}
	for key, value := range desired.GetAnnotations() {
		annotations[key] = value
	}
	candidate.SetAnnotations(annotations)
}

func (a *RecoveryAdapter) retryMutation(ctx context.Context, execution *store.DeployExecution, mutation func(context.Context, store.DeployLease) error) error {
	backoff := retry.DefaultRetry
	backoff.Steps = a.config.ConflictRetries
	return retry.OnError(backoff, isConflict, func() error { return a.withMutationLease(ctx, execution, mutation) })
}

func (a *RecoveryAdapter) withMutationLease(ctx context.Context, execution *store.DeployExecution, mutation func(context.Context, store.DeployLease) error) error {
	renewed, err := a.renew(ctx, *execution)
	if err != nil {
		return err
	}
	*execution = renewed
	opCtx, cancel := a.operationContext(ctx, renewed)
	defer cancel()
	return mutation(opCtx, renewed)
}

func (a *RecoveryAdapter) observeForward(ctx context.Context, execution *store.DeployExecution, expected *appsv1.Deployment) error {
	return wait.PollUntilContextCancel(ctx, a.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		renewed, err := a.renew(ctx, *execution)
		if err != nil {
			return false, err
		}
		*execution = renewed
		current, err := a.getDeployment(ctx, renewed, expected.Namespace, expected.Name)
		if err != nil {
			return false, err
		}
		if err := verifyOwnership(current, execution.EnvironmentID); err != nil {
			return false, ErrResourceOwnership
		}
		if deploymentFailed(current) {
			return false, fmt.Errorf("%w: ProgressDeadlineExceeded", ErrRolloutTerminal)
		}
		return deploymentReady(current, renewed, execution.FailedRelease), nil
	})
}

func (a *RecoveryAdapter) compensateForward(ctx context.Context, execution store.DeployExecution, reason string) (store.DeployRecoveryResult, error) {
	result := store.DeployRecoveryResult{Lease: execution}
	if execution.PreviousRelease == nil {
		_, err := a.store.(ForwardStore).FinalizeReleaseFailed(ctx, store.FailReleaseParams{
			Lease: execution, FailureReason: reason, Audit: forwardAuditEvent(),
		})
		return result, err
	}
	execution, _, err := a.restorePreviousDesiredState(ctx, execution, reason)
	result.Lease = execution
	if err != nil {
		result.Lease = execution
		return result, err
	}
	result.Lease = execution
	_, err = a.store.(ForwardStore).FinalizeReleaseFailed(ctx, store.FailReleaseParams{
		Lease: execution, FailureReason: reason, Audit: forwardAuditEvent(),
	})
	return result, err
}

func (a *RecoveryAdapter) restorePreviousDesiredState(ctx context.Context, lease store.DeployLease, reason string) (store.DeployLease, *appsv1.Deployment, error) {
	if lease.PreviousRelease == nil {
		return lease, nil, fmt.Errorf("%w: previous release is required", ErrRecoveryStateInvalid)
	}
	previousEnvelope, err := store.DecodeReleaseSnapshot(lease.PreviousRelease.ConfigSnapshot)
	if err != nil {
		return lease, nil, fmt.Errorf("decode previous desired state: %w", err)
	}
	if err := a.restorePreviousSecretRevision(ctx, lease); err != nil {
		return lease, nil, err
	}
	previousIdentity := lease
	previousIdentity.ReleaseID = lease.PreviousRelease.ID
	previousIdentity.FailedRelease = *lease.PreviousRelease
	objects, err := BuildDesiredObjects(previousIdentity, previousEnvelope, nil)
	if err != nil {
		return lease, nil, err
	}
	objects.Secret = nil
	if err := a.applyDesiredWithIdentity(ctx, &lease, objects, lease.PreviousRelease.ID, lease.PreviousRelease.ConfigHash); err != nil {
		if !isTransient(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, store.ErrLeaseNotOwned) {
			return lease, objects.Deployment, fmt.Errorf("%w: restore previous desired state: %v", ErrCompensationUnproven, err)
		}
		return lease, objects.Deployment, fmt.Errorf("restore previous desired state: %w", err)
	}
	if err := a.store.CheckpointDeployRecovery(ctx, lease, "desired_state_restored"); err != nil {
		return lease, objects.Deployment, fmt.Errorf("checkpoint restored desired state: %w", err)
	}
	lease.RecoveryCheckpoint = "desired_state_restored"
	state := RecoveryState{Namespace: lease.Namespace, DeploymentName: objects.Deployment.Name, FailureReason: reason}
	if err := a.observeReady(ctx, &lease, state, objects.Deployment); err != nil {
		return lease, objects.Deployment, err
	}
	return lease, objects.Deployment, nil
}

func forwardAuditEvent() *store.AuditEvent {
	return &store.AuditEvent{ID: uuid.Must(uuid.NewV7()).String()}
}

func managedLabels(source map[string]string, environmentID string) map[string]string {
	labels := cloneStrings(source)
	labels[ManagedByLabel] = ManagedByValue
	labels[EnvironmentIDLabel] = environmentID
	return labels
}

func resolvedAnnotations(source map[string]string, execution store.DeployExecution) map[string]string {
	annotations := cloneStrings(source)
	annotations[ReleaseAnnotation] = execution.ReleaseID
	annotations[ConfigHashAnnotation] = execution.FailedRelease.ConfigHash
	annotations[DeployFenceAnnotation] = execution.Fence.UTC().Format(time.RFC3339Nano)
	return annotations
}

func verifyObject(labels, annotations map[string]string, environmentID string, fence time.Time) error {
	if labels[ManagedByLabel] != ManagedByValue || labels[EnvironmentIDLabel] != environmentID {
		return ErrResourceOwnership
	}
	if raw := annotations[DeployFenceAnnotation]; raw != "" {
		existing, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return ErrResourceOwnership
		}
		if existing.After(fence) {
			return ErrNewerDeployFence
		}
	}
	return nil
}

func mergeManagedMetadata(candidate *metav1.ObjectMeta, desired metav1.ObjectMeta) {
	if candidate.Labels == nil {
		candidate.Labels = map[string]string{}
	}
	for key := range candidate.Labels {
		if isCyclopusManagedMetadataKey(key) {
			delete(candidate.Labels, key)
		}
	}
	for key, value := range desired.Labels {
		candidate.Labels[key] = value
	}
	if candidate.Annotations == nil {
		candidate.Annotations = map[string]string{}
	}
	for key := range candidate.Annotations {
		if isCyclopusManagedMetadataKey(key) {
			delete(candidate.Annotations, key)
		}
	}
	for key, value := range desired.Annotations {
		candidate.Annotations[key] = value
	}
}

func isCyclopusManagedMetadataKey(key string) bool {
	return strings.HasPrefix(key, "cyclopus.dev/") || key == ManagedByLabel || key == "app.kubernetes.io/name"
}

func stampFence(metadata *metav1.ObjectMeta, lease store.DeployLease) {
	if metadata.Annotations == nil {
		metadata.Annotations = map[string]string{}
	}
	stampFenceAnnotation(metadata.Annotations, lease)
}

func stampFenceAnnotation(annotations map[string]string, lease store.DeployLease) {
	annotations[DeployFenceAnnotation] = lease.Fence.UTC().Format(time.RFC3339Nano)
}

func cloneBytes(source map[string][]byte) map[string][]byte {
	copy := make(map[string][]byte, len(source))
	for key, value := range source {
		copy[key] = append([]byte(nil), value...)
	}
	return copy
}

func ComputeSecretDataHash(data map[string][]byte) string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		hash.Write([]byte(key))
		hash.Write([]byte{0})
		hash.Write(data[key])
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
