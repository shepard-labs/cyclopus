package kubernetes

import (
	"errors"

	"github.com/cyclopus-dev/cyclopus/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
)

func EncodeDeploymentSnapshot(deployment *appsv1.Deployment) ([]byte, error) {
	if deployment == nil {
		return nil, errors.New("deployment snapshot: deployment is required")
	}
	copy := deployment.DeepCopy()
	clearServerMetadata(copy)
	if len(copy.Spec.Template.Spec.Containers) != 1 || copy.Spec.Replicas == nil {
		return nil, errors.New("deployment snapshot: one container and replicas are required")
	}
	container := copy.Spec.Template.Spec.Containers[0]
	port := int32(8080)
	if len(container.Ports) > 0 {
		port = container.Ports[0].ContainerPort
	}
	path := "/"
	if container.ReadinessProbe != nil && container.ReadinessProbe.HTTPGet != nil {
		path = container.ReadinessProbe.HTTPGet.Path
		if container.ReadinessProbe.HTTPGet.Port.IntVal > 0 {
			port = container.ReadinessProbe.HTTPGet.Port.IntVal
		}
	}
	labels := cloneStrings(copy.Labels)
	podLabels := cloneStrings(copy.Spec.Template.Labels)
	selector := map[string]string{}
	if copy.Spec.Selector != nil {
		selector = cloneStrings(copy.Spec.Selector.MatchLabels)
	}
	annotations := cloneStrings(copy.Spec.Template.Annotations)
	for _, key := range []string{ReleaseAnnotation, ConfigHashAnnotation, EnvRevisionAnnotation} {
		if value := copy.Annotations[key]; value != "" {
			annotations[key] = value
		}
	}
	if annotations[ReleaseAnnotation] == "" {
		annotations[ReleaseAnnotation] = store.ReleaseIDAnnotationTemplate
	}
	if annotations[ConfigHashAnnotation] == "" {
		annotations[ConfigHashAnnotation] = store.ConfigHashAnnotationTemplate
	}
	runtimeClass := "runc"
	if copy.Spec.Template.Spec.RuntimeClassName != nil {
		runtimeClass = *copy.Spec.Template.Spec.RuntimeClassName
	}
	deadline := int32(600)
	if copy.Spec.ProgressDeadlineSeconds != nil {
		deadline = *copy.Spec.ProgressDeadlineSeconds
	}
	resources := store.ResourceIntent{CPURequestMillicores: 100, CPULimitMillicores: 1000, MemoryRequestMiB: 128, MemoryLimitMiB: 512}
	if value := container.Resources.Requests.Cpu(); value != nil && !value.IsZero() {
		resources.CPURequestMillicores = value.MilliValue()
	}
	if value := container.Resources.Limits.Cpu(); value != nil && !value.IsZero() {
		resources.CPULimitMillicores = value.MilliValue()
	}
	if value := container.Resources.Requests.Memory(); value != nil && !value.IsZero() {
		resources.MemoryRequestMiB = value.Value() / (1024 * 1024)
	}
	if value := container.Resources.Limits.Memory(); value != nil && !value.IsZero() {
		resources.MemoryLimitMiB = value.Value() / (1024 * 1024)
	}
	var envFrom []string
	for _, source := range container.EnvFrom {
		if source.SecretRef != nil {
			envFrom = append(envFrom, source.SecretRef.Name)
		}
	}
	identity := store.ObjectIdentity{Name: copy.Name, Namespace: copy.Namespace, Labels: labels}
	scaling := store.ScalingIntent{Version: 1, Kind: "none"}
	if *copy.Spec.Replicas != 1 {
		scaling = store.ScalingIntent{Version: 1, Kind: "hpa", Name: copy.Name, HPA: &store.HPAIntent{MinReplicas: *copy.Spec.Replicas, MaxReplicas: *copy.Spec.Replicas, TargetCPUPercent: 70}}
	}
	envelope := store.ReleaseSnapshotEnvelope{Version: store.ReleaseSnapshotVersion, DesiredState: store.DesiredStateSnapshot{
		Version: store.DesiredStateSnapshotVersion,
		Deployment: store.DeploymentIntent{Version: 1, Identity: identity, Selector: selector, Replicas: *copy.Spec.Replicas,
			Strategy: store.RollingUpdateIntent{Type: "RollingUpdate", MaxUnavailable: 0, MaxSurge: 1}, PodLabels: podLabels,
			PodAnnotations: annotations, RuntimeClassName: runtimeClass, ProgressDeadlineSeconds: deadline,
			Container: store.ContainerIntent{Name: container.Name, Image: container.Image, Port: port, EnvFromSecrets: envFrom, Resources: resources, ReadinessProbe: store.ReadinessIntent{Path: path, Port: port}}},
		Service: store.ServiceIntent{Version: 1, Identity: identity, Selector: selector, Port: 80, TargetPort: port},
		Ingress: store.IngressIntent{Version: 1, Identity: identity, Hostname: copy.Name + ".example.test", Path: "/", ServiceName: copy.Name, ServicePort: 80, TLS: store.TLSIntent{Enabled: true, SecretName: copy.Name + "-tls"}},
		Scaling: scaling,
	}}
	encoded, _, err := store.EncodeReleaseSnapshot(envelope)
	return encoded, err
}

func DecodeDeploymentSnapshot(encoded []byte) (*appsv1.Deployment, error) {
	envelope, err := store.DecodeReleaseSnapshot(encoded)
	if err != nil {
		return nil, err
	}
	d := envelope.DesiredState.Deployment
	replicas := d.Replicas
	deadline := d.ProgressDeadlineSeconds
	maxUnavailable := intstr.FromInt32(d.Strategy.MaxUnavailable)
	maxSurge := intstr.FromInt32(d.Strategy.MaxSurge)
	annotations := cloneStrings(d.PodAnnotations)
	container := corev1.Container{
		Name:  d.Container.Name,
		Image: d.Container.Image,
		Ports: []corev1.ContainerPort{{ContainerPort: d.Container.Port}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(d.Container.Resources.CPURequestMillicores, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(d.Container.Resources.MemoryRequestMiB*1024*1024, resource.BinarySI)},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(d.Container.Resources.CPULimitMillicores, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(d.Container.Resources.MemoryLimitMiB*1024*1024, resource.BinarySI)},
		},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: d.Container.ReadinessProbe.Path, Port: intstr.FromInt32(d.Container.ReadinessProbe.Port)}}},
	}
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: d.Identity.Name, Namespace: d.Identity.Namespace, Labels: cloneStrings(d.Identity.Labels), Annotations: cloneStrings(annotations)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, ProgressDeadlineSeconds: &deadline,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &maxUnavailable, MaxSurge: &maxSurge}},
			Selector: &metav1.LabelSelector{MatchLabels: cloneStrings(d.Selector)},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: cloneStrings(d.PodLabels), Annotations: annotations}, Spec: corev1.PodSpec{RuntimeClassName: &d.RuntimeClassName, Containers: []corev1.Container{container}}},
		},
	}
	for _, name := range d.Container.EnvFromSecrets {
		deployment.Spec.Template.Spec.Containers[0].EnvFrom = append(deployment.Spec.Template.Spec.Containers[0].EnvFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}})
	}
	return deployment, nil
}

func clearServerMetadata(deployment *appsv1.Deployment) {
	deployment.UID = ""
	deployment.ResourceVersion = ""
	deployment.Generation = 0
	deployment.CreationTimestamp = appsv1.Deployment{}.CreationTimestamp
	deployment.DeletionTimestamp = nil
	deployment.DeletionGracePeriodSeconds = nil
	deployment.ManagedFields = nil
	deployment.Status = appsv1.DeploymentStatus{}
}

func cloneStrings(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
