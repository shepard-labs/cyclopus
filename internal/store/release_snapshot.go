package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/shepard-labs/cyclopus/internal/secretref"
)

var cronShapePattern = regexp.MustCompile(`^\S+( \S+){4}$`)
var immutableGHCRImagePattern = regexp.MustCompile(`^ghcr\.io/[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*@sha256:[0-9a-f]{64}$`)

const (
	ReleaseSnapshotVersion      = 4
	ReleaseSnapshotVersionV2    = 2 // HPA/none snapshots from before Phase 4 KEDA support
	ReleaseSnapshotVersionV3    = 3 // snapshots from before image pull secrets were persisted
	DesiredStateSnapshotVersion = 1
	EnvVarRevisionVersion       = 1
)

const (
	ReleaseIDAnnotationTemplate  = "${release_id}"
	ConfigHashAnnotationTemplate = "${config_hash}"
)

type ReleaseSnapshotEnvelope struct {
	Version        int                  `json:"version"`
	DesiredState   DesiredStateSnapshot `json:"desired_state"`
	EnvVarRevision *EnvVarRevision      `json:"env_var_revision,omitempty"`
}

type DesiredStateSnapshot struct {
	Version    int              `json:"version"`
	Secret     *SecretIntent    `json:"secret,omitempty"`
	Deployment DeploymentIntent `json:"deployment"`
	Service    ServiceIntent    `json:"service"`
	Ingress    IngressIntent    `json:"ingress"`
	Scaling    ScalingIntent    `json:"scaling"`
}

type SecretIntent struct {
	Version   int                 `json:"version"`
	Identity  ObjectIdentity      `json:"identity"`
	Immutable bool                `json:"immutable"`
	Entries   []SecretEntryIntent `json:"entries"`
}

type SecretEntryIntent struct {
	Key       string `json:"key"`
	SecretRef string `json:"secret_ref"`
}

type ObjectIdentity struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

type DeploymentIntent struct {
	Version                 int                 `json:"version"`
	Identity                ObjectIdentity      `json:"identity"`
	Selector                map[string]string   `json:"selector"`
	Replicas                int32               `json:"replicas"`
	Strategy                RollingUpdateIntent `json:"strategy"`
	PodLabels               map[string]string   `json:"pod_labels"`
	PodAnnotations          map[string]string   `json:"pod_annotations"`
	RuntimeClassName        string              `json:"runtime_class_name"`
	ProgressDeadlineSeconds int32               `json:"progress_deadline_seconds"`
	ImagePullSecrets        []string            `json:"image_pull_secrets,omitempty"`
	Container               ContainerIntent     `json:"container"`
}

type RollingUpdateIntent struct {
	Type           string `json:"type"`
	MaxUnavailable int32  `json:"max_unavailable"`
	MaxSurge       int32  `json:"max_surge"`
}

type ContainerIntent struct {
	Name           string          `json:"name"`
	Image          string          `json:"image"`
	Port           int32           `json:"port"`
	EnvFromSecrets []string        `json:"env_from_secrets"`
	Resources      ResourceIntent  `json:"resources"`
	ReadinessProbe ReadinessIntent `json:"readiness_probe"`
}

type ResourceIntent struct {
	CPURequestMillicores int64 `json:"cpu_request_millicores"`
	CPULimitMillicores   int64 `json:"cpu_limit_millicores"`
	MemoryRequestMiB     int64 `json:"memory_request_mib"`
	MemoryLimitMiB       int64 `json:"memory_limit_mib"`
}

type ReadinessIntent struct {
	Path string `json:"path"`
	Port int32  `json:"port"`
}

type ServiceIntent struct {
	Version    int               `json:"version"`
	Identity   ObjectIdentity    `json:"identity"`
	Selector   map[string]string `json:"selector"`
	Port       int32             `json:"port"`
	TargetPort int32             `json:"target_port"`
}

type IngressIntent struct {
	Version     int            `json:"version"`
	Identity    ObjectIdentity `json:"identity"`
	Hostname    string         `json:"hostname"`
	Path        string         `json:"path"`
	ServiceName string         `json:"service_name"`
	ServicePort int32          `json:"service_port"`
	TLS         TLSIntent      `json:"tls"`
}

type TLSIntent struct {
	Enabled    bool   `json:"enabled"`
	SecretName string `json:"secret_name"`
}

type ScalingIntent struct {
	Version int         `json:"version"`
	Kind    string      `json:"kind"`
	Name    string      `json:"name,omitempty"`
	HPA     *HPAIntent  `json:"hpa,omitempty"`
	KEDA    *KEDAIntent `json:"keda,omitempty"`
}

type HPAIntent struct {
	MinReplicas      int32 `json:"min_replicas"`
	MaxReplicas      int32 `json:"max_replicas"`
	TargetCPUPercent int32 `json:"target_cpu_percent"`
}

type KEDAIntent struct {
	Mode             string `json:"mode"`
	MinReplicas      int32  `json:"min_replicas"`
	MaxReplicas      int32  `json:"max_replicas"`
	TargetCPUPercent int32  `json:"target_cpu_percent"`
	// CronSchedules drives the keda.sh ScaledObject cron triggers (mode "cron").
	CronSchedules []KEDACronScheduleIntent `json:"cron_schedules,omitempty"`
	// HTTPTargetPendingRequests is the HTTP add-on concurrency target (mode "http").
	HTTPTargetPendingRequests int32 `json:"http_target_pending_requests,omitempty"`
}

type KEDACronScheduleIntent struct {
	Timezone        string `json:"timezone"`
	Start           string `json:"start"`
	End             string `json:"end"`
	DesiredReplicas int32  `json:"desired_replicas"`
}

type EnvVarRevision struct {
	Version   int                  `json:"version"`
	ID        string               `json:"id"`
	Revision  int64                `json:"revision"`
	Hash      string               `json:"hash"`
	Variables []ReleaseEnvironment `json:"variables"`
}

type ReleaseEnvironment struct {
	Key       string `json:"key"`
	SecretRef string `json:"secret_ref"`
	IsSecret  bool   `json:"is_secret"`
}

type ReleaseSnapshotContext struct {
	ImageVisibility string
	ImmutableImage  string
	ReleaseID       *string
}

// NormalizeReleaseSnapshotForContext prepares a snapshot for a new release
// without changing the historical snapshot it was decoded from.
func NormalizeReleaseSnapshotForContext(envelope ReleaseSnapshotEnvelope, context ReleaseSnapshotContext) (ReleaseSnapshotEnvelope, error) {
	normalized, err := upgradeLegacyReleaseSnapshot(envelope)
	if err != nil {
		return ReleaseSnapshotEnvelope{}, err
	}
	normalized = canonicalReleaseSnapshot(normalized)
	normalized.DesiredState.Deployment.Container.Image = context.ImmutableImage
	switch context.ImageVisibility {
	case "public":
		normalized.DesiredState.Deployment.ImagePullSecrets = []string{}
	case "private":
		normalized.DesiredState.Deployment.ImagePullSecrets = []string{"cyclopus-ghcr-pull"}
	default:
		return ReleaseSnapshotEnvelope{}, errors.New("release snapshot: image visibility must be public or private")
	}
	if context.ReleaseID != nil {
		normalized.DesiredState.Deployment.PodAnnotations["cyclopus.dev/release-id"] = *context.ReleaseID
	}
	if err := ValidateReleaseSnapshot(normalized); err != nil {
		return ReleaseSnapshotEnvelope{}, err
	}
	return normalized, nil
}

func EncodeReleaseSnapshot(envelope ReleaseSnapshotEnvelope) (json.RawMessage, string, error) {
	var err error
	envelope, err = upgradeLegacyReleaseSnapshot(envelope)
	if err != nil {
		return nil, "", err
	}
	canonical := canonicalReleaseSnapshot(envelope)
	if err := ValidateReleaseSnapshot(canonical); err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", fmt.Errorf("marshal release snapshot: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:]), nil
}

func DecodeReleaseSnapshot(encoded []byte) (ReleaseSnapshotEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var envelope ReleaseSnapshotEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return ReleaseSnapshotEnvelope{}, fmt.Errorf("decode release snapshot: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ReleaseSnapshotEnvelope{}, errors.New("decode release snapshot: multiple JSON values")
		}
		return ReleaseSnapshotEnvelope{}, fmt.Errorf("decode release snapshot trailing data: %w", err)
	}
	envelope, err := upgradeLegacyReleaseSnapshot(envelope)
	if err != nil {
		return ReleaseSnapshotEnvelope{}, err
	}
	envelope = canonicalReleaseSnapshot(envelope)
	if err := ValidateReleaseSnapshot(envelope); err != nil {
		return ReleaseSnapshotEnvelope{}, err
	}
	return envelope, nil
}

// NormalizeReleaseSnapshotContext upgrades a readable historical snapshot and
// binds it to a new immutable release context. The historical row is unchanged.
func NormalizeReleaseSnapshotContext(encoded []byte, context ReleaseSnapshotContext) (json.RawMessage, string, error) {
	envelope, err := DecodeReleaseSnapshot(encoded)
	if err != nil {
		return nil, "", err
	}
	envelope, err = NormalizeReleaseSnapshotForContext(envelope, context)
	if err != nil {
		return nil, "", err
	}
	return EncodeReleaseSnapshot(envelope)
}

func buildContextualReleaseSnapshot(environment Environment, app App, imageRef, releaseID, visibility string) (json.RawMessage, string, error) {
	labels := map[string]string{"app.kubernetes.io/managed-by": "cyclopus", "app.kubernetes.io/name": app.Slug,
		"cyclopus.dev/environment-id": environment.ID}
	selector := map[string]string{"app.kubernetes.io/name": app.Slug, "cyclopus.dev/environment-id": environment.ID}
	replicas := int32(app.MinReplicas)
	scaling := ScalingIntent{Version: 1, Name: app.Slug}
	if environment.Kind == "preview" {
		replicas, scaling.Kind, scaling.Name = 1, "none", ""
	} else if app.ScalingMode == "hpa" {
		scaling.Kind = "hpa"
		scaling.HPA = &HPAIntent{MinReplicas: int32(app.MinReplicas), MaxReplicas: int32(app.MaxReplicas), TargetCPUPercent: int32(app.HPATargetCPUPercent)}
	} else {
		scaling.Kind = "keda"
		mode := strings.TrimPrefix(app.ScalingMode, "keda_")
		scaling.KEDA = &KEDAIntent{Mode: mode, MinReplicas: int32(app.MinReplicas), MaxReplicas: int32(app.MaxReplicas), TargetCPUPercent: int32(app.HPATargetCPUPercent)}
		if mode == "cron" {
			for _, schedule := range app.KEDACronSchedules {
				scaling.KEDA.CronSchedules = append(scaling.KEDA.CronSchedules, KEDACronScheduleIntent{Timezone: schedule.Timezone,
					Start: schedule.Start, End: schedule.End, DesiredReplicas: int32(schedule.DesiredReplicas)})
			}
		} else {
			scaling.KEDA.HTTPTargetPendingRequests = int32(app.KEDAHTTPTargetPendingRequests)
		}
	}
	identity := func(name string) ObjectIdentity {
		return ObjectIdentity{Name: name, Namespace: environment.Namespace, Labels: labels}
	}
	envelope := ReleaseSnapshotEnvelope{Version: ReleaseSnapshotVersion, DesiredState: DesiredStateSnapshot{Version: 1,
		Deployment: DeploymentIntent{Version: 1, Identity: identity(app.Slug), Selector: selector, Replicas: replicas,
			Strategy: RollingUpdateIntent{Type: "RollingUpdate", MaxSurge: 1}, PodLabels: labels,
			PodAnnotations: map[string]string{"cyclopus.dev/release-id": releaseID, "cyclopus.dev/config-hash": ConfigHashAnnotationTemplate,
				"cyclopus.dev/app-id": app.ID, "cyclopus.dev/org-id": app.OrgID}, RuntimeClassName: app.RuntimeClass,
			ProgressDeadlineSeconds: int32(app.DeployTimeoutSeconds), Container: ContainerIntent{Name: "app", Image: imageRef,
				Port: int32(app.ContainerPort), Resources: ResourceIntent{CPURequestMillicores: int64(app.CPURequestMillicores),
					CPULimitMillicores: int64(app.CPULimitMillicores), MemoryRequestMiB: int64(app.MemoryRequestMiB), MemoryLimitMiB: int64(app.MemoryLimitMiB)},
				ReadinessProbe: ReadinessIntent{Path: app.HealthCheckPath, Port: int32(app.ContainerPort)}}},
		Service: ServiceIntent{Version: 1, Identity: identity(app.Slug), Selector: selector, Port: 80, TargetPort: int32(app.ContainerPort)},
		Ingress: IngressIntent{Version: 1, Identity: identity(app.Slug), Hostname: *environment.PlannedHostname, Path: "/",
			ServiceName: app.Slug, ServicePort: 80, TLS: TLSIntent{Enabled: true, SecretName: app.Slug + "-tls"}}, Scaling: scaling}}
	encoded, _, err := EncodeReleaseSnapshot(envelope)
	if err != nil {
		return nil, "", err
	}
	return NormalizeReleaseSnapshotContext(encoded, ReleaseSnapshotContext{
		ImageVisibility: visibility,
		ImmutableImage:  imageRef,
		ReleaseID:       &releaseID,
	})
}

func upgradeLegacyReleaseSnapshot(envelope ReleaseSnapshotEnvelope) (ReleaseSnapshotEnvelope, error) {
	switch envelope.Version {
	case ReleaseSnapshotVersionV2:
		// v2 snapshots pre-date KEDA support. KEDA snapshots were never deployed
		// because the deployment path returned ErrKEDAUnsupported.
		if envelope.DesiredState.Scaling.Kind == "keda" {
			return ReleaseSnapshotEnvelope{}, errors.New("release snapshot: v2 KEDA snapshots require migration before deployment")
		}
		fallthrough
	case ReleaseSnapshotVersionV3:
		envelope.Version = ReleaseSnapshotVersion
		envelope.DesiredState.Deployment.ImagePullSecrets = []string{}
	}
	revision := envelope.EnvVarRevision
	if revision == nil || envelope.DesiredState.Secret != nil || len(envelope.DesiredState.Deployment.Container.EnvFromSecrets) != 0 {
		return envelope, nil
	}
	name := EnvDestinationSecretName(envelope.DesiredState.Deployment.Identity.Name, revision.ID)
	entries := make([]SecretEntryIntent, len(revision.Variables))
	for index, variable := range revision.Variables {
		entries[index] = SecretEntryIntent{Key: variable.Key, SecretRef: variable.SecretRef}
	}
	envelope.DesiredState.Secret = &SecretIntent{
		Version: 1,
		Identity: ObjectIdentity{
			Name:      name,
			Namespace: envelope.DesiredState.Deployment.Identity.Namespace,
			Labels:    cloneMap(envelope.DesiredState.Deployment.Identity.Labels),
		},
		Immutable: true,
		Entries:   entries,
	}
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{name}
	return envelope, nil
}

func ValidateReleaseSnapshot(envelope ReleaseSnapshotEnvelope) error {
	switch envelope.Version {
	case ReleaseSnapshotVersion:
		// v4 is the current version, full validation below.
	default:
		return fmt.Errorf("release snapshot: unsupported version %d", envelope.Version)
	}
	desired := envelope.DesiredState
	if desired.Version != DesiredStateSnapshotVersion {
		return fmt.Errorf("release snapshot desired state: unsupported version %d", desired.Version)
	}
	if err := validateDesiredState(desired); err != nil {
		return err
	}
	if revision := envelope.EnvVarRevision; revision != nil {
		if revision.Version != EnvVarRevisionVersion || revision.ID == "" || revision.Revision <= 0 {
			return errors.New("release snapshot: versioned env revision id and positive revision are required")
		}
		seen := make(map[string]struct{}, len(revision.Variables))
		for _, variable := range revision.Variables {
			parsed, err := secretref.Parse(variable.SecretRef)
			if variable.Key == "" || err != nil || !variable.IsSecret || parsed.Namespace != desired.Deployment.Identity.Namespace {
				return errors.New("release snapshot: env variables require key, secret ref, and secret=true")
			}
			if _, exists := seen[variable.Key]; exists {
				return errors.New("release snapshot: env variable keys must be unique")
			}
			seen[variable.Key] = struct{}{}
		}
		if desired.Deployment.PodAnnotations["cyclopus.dev/env-revision-id"] != revision.ID {
			return errors.New("release snapshot: pod env revision annotation must match env revision")
		}
		expectedHash := ComputeEnvVarRevisionHash(revision.Variables)
		if revision.Hash != expectedHash || desired.Deployment.PodAnnotations["cyclopus.dev/env-hash"] != expectedHash {
			return errors.New("release snapshot: pod env hash annotation must match canonical env revision hash")
		}
		if desired.Secret == nil {
			return errors.New("release snapshot: env revision requires immutable destination Secret intent")
		}
		secret := desired.Secret
		if secret.Version != 1 || !secret.Immutable {
			return errors.New("release snapshot: env destination Secret must be version 1 and immutable")
		}
		if err := validateIdentity(secret.Identity); err != nil {
			return fmt.Errorf("release snapshot secret: %w", err)
		}
		if secret.Identity.Namespace != desired.Deployment.Identity.Namespace || secret.Identity.Name != EnvDestinationSecretName(desired.Deployment.Identity.Name, revision.ID) {
			return errors.New("release snapshot: env destination Secret identity is not deterministic")
		}
		if len(desired.Deployment.Container.EnvFromSecrets) != 1 || desired.Deployment.Container.EnvFromSecrets[0] != secret.Identity.Name {
			return errors.New("release snapshot: Deployment must envFrom exactly the destination Secret")
		}
		if len(secret.Entries) != len(revision.Variables) {
			return errors.New("release snapshot: destination Secret entries must match env revision")
		}
		for index, variable := range revision.Variables {
			entry := secret.Entries[index]
			if entry.Key != variable.Key || entry.SecretRef != variable.SecretRef {
				return errors.New("release snapshot: destination Secret entries must canonically match env revision")
			}
		}
	} else if desired.Secret != nil {
		return errors.New("release snapshot: destination Secret requires an env revision")
	}
	return nil
}

func validateDesiredState(desired DesiredStateSnapshot) error {
	d := desired.Deployment
	if d.Version != 1 || desired.Service.Version != 1 || desired.Ingress.Version != 1 || desired.Scaling.Version != 1 {
		return errors.New("release snapshot desired state: unsupported object version")
	}
	if err := validateIdentity(d.Identity); err != nil {
		return fmt.Errorf("release snapshot deployment: %w", err)
	}
	if err := validateIdentity(desired.Service.Identity); err != nil {
		return fmt.Errorf("release snapshot service: %w", err)
	}
	if err := validateIdentity(desired.Ingress.Identity); err != nil {
		return fmt.Errorf("release snapshot ingress: %w", err)
	}
	if d.Identity.Namespace != desired.Service.Identity.Namespace || d.Identity.Namespace != desired.Ingress.Identity.Namespace {
		return errors.New("release snapshot: object namespaces must match")
	}
	if len(d.Selector) == 0 || !mapContains(d.PodLabels, d.Selector) || !mapsEqual(d.Selector, desired.Service.Selector) {
		return errors.New("release snapshot: deployment, pod, and service selectors must match")
	}
	if d.Replicas < 0 || d.Strategy.Type != "RollingUpdate" || d.Strategy.MaxUnavailable != 0 || d.Strategy.MaxSurge < 1 {
		return errors.New("release snapshot deployment: RollingUpdate with maxUnavailable=0 and positive maxSurge is required")
	}
	if d.RuntimeClassName != "gvisor" && d.RuntimeClassName != "runc" {
		return errors.New("release snapshot deployment: runtime class must be gvisor or runc")
	}
	if d.ProgressDeadlineSeconds <= 0 || d.Container.Name == "" || !immutableGHCRImagePattern.MatchString(d.Container.Image) || d.Container.Port < 1 || d.Container.Port > 65535 {
		return errors.New("release snapshot deployment: progress deadline, container, immutable GHCR image, and port are required")
	}
	if len(d.ImagePullSecrets) > 1 || len(d.ImagePullSecrets) == 1 && d.ImagePullSecrets[0] != "cyclopus-ghcr-pull" {
		return errors.New("release snapshot deployment: image pull secrets must be empty or exactly cyclopus-ghcr-pull")
	}
	r := d.Container.Resources
	if r.CPURequestMillicores <= 0 || r.CPULimitMillicores < r.CPURequestMillicores || r.MemoryRequestMiB <= 0 || r.MemoryLimitMiB < r.MemoryRequestMiB {
		return errors.New("release snapshot deployment: valid resource requests and limits are required")
	}
	if !strings.HasPrefix(d.Container.ReadinessProbe.Path, "/") || d.Container.ReadinessProbe.Port != d.Container.Port {
		return errors.New("release snapshot deployment: readiness path and container port are required")
	}
	if d.PodAnnotations["cyclopus.dev/release-id"] == "" || d.PodAnnotations["cyclopus.dev/config-hash"] == "" {
		return errors.New("release snapshot deployment: release and config hash annotations are required")
	}
	svc := desired.Service
	if svc.Port < 1 || svc.Port > 65535 || svc.TargetPort != d.Container.Port || svc.Identity.Name != d.Identity.Name {
		return errors.New("release snapshot service: identity and ports must target the deployment")
	}
	ing := desired.Ingress
	if strings.TrimSpace(ing.Hostname) == "" || ing.Path != "/" || ing.ServiceName != svc.Identity.Name || ing.ServicePort != svc.Port || !ing.TLS.Enabled || ing.TLS.SecretName == "" {
		return errors.New("release snapshot ingress: hostname, root path, service backend, and TLS are required")
	}
	scaling := desired.Scaling
	switch scaling.Kind {
	case "none":
		if scaling.Name != "" || scaling.HPA != nil || scaling.KEDA != nil || d.Replicas != 1 {
			return errors.New("release snapshot scaling: no-scaler identity requires exactly one replica")
		}
	case "hpa":
		if scaling.Name != d.Identity.Name || scaling.HPA == nil || scaling.KEDA != nil || scaling.HPA.MinReplicas < 1 || scaling.HPA.MaxReplicas < scaling.HPA.MinReplicas || scaling.HPA.TargetCPUPercent < 1 || scaling.HPA.TargetCPUPercent > 100 || d.Replicas != scaling.HPA.MinReplicas {
			return errors.New("release snapshot scaling: valid HPA intent is required")
		}
	case "keda":
		if scaling.Name != d.Identity.Name || scaling.KEDA == nil || scaling.HPA != nil || (scaling.KEDA.Mode != "cron" && scaling.KEDA.Mode != "http") || scaling.KEDA.MinReplicas < 0 || scaling.KEDA.MaxReplicas < 1 || scaling.KEDA.MaxReplicas < scaling.KEDA.MinReplicas || scaling.KEDA.TargetCPUPercent < 1 || scaling.KEDA.TargetCPUPercent > 100 || d.Replicas != scaling.KEDA.MinReplicas {
			return errors.New("release snapshot scaling: valid KEDA intent is required")
		}
		if err := validateKEDAModeIntent(scaling.KEDA); err != nil {
			return fmt.Errorf("release snapshot scaling: %w", err)
		}
	default:
		return errors.New("release snapshot scaling: kind must be none, hpa, or keda")
	}
	return nil
}

func validateKEDAModeIntent(keda *KEDAIntent) error {
	switch keda.Mode {
	case "cron":
		if len(keda.CronSchedules) == 0 || len(keda.CronSchedules) > 16 || keda.HTTPTargetPendingRequests != 0 {
			return errors.New("keda cron mode requires 1-16 cron schedules and no HTTP target")
		}
		seen := make(map[string]struct{}, len(keda.CronSchedules))
		for _, schedule := range keda.CronSchedules {
			if strings.TrimSpace(schedule.Timezone) == "" || len(schedule.Timezone) > 64 ||
				!cronShapePattern.MatchString(schedule.Start) || !cronShapePattern.MatchString(schedule.End) ||
				schedule.DesiredReplicas < 0 {
				return errors.New("keda cron schedules require a timezone, 5-field cron start/end, and non-negative replicas")
			}
			key := schedule.Timezone + "\x00" + schedule.Start + "\x00" + schedule.End
			if _, exists := seen[key]; exists {
				return errors.New("keda cron schedule windows must be unique")
			}
			seen[key] = struct{}{}
		}
	case "http":
		if keda.HTTPTargetPendingRequests < 1 || len(keda.CronSchedules) != 0 {
			return errors.New("keda http mode requires a positive pending-requests target and no cron schedules")
		}
	}
	return nil
}

func validateIdentity(identity ObjectIdentity) error {
	if strings.TrimSpace(identity.Name) == "" || strings.TrimSpace(identity.Namespace) == "" {
		return errors.New("name and namespace are required")
	}
	if identity.Labels["app.kubernetes.io/managed-by"] != "cyclopus" || identity.Labels["cyclopus.dev/environment-id"] == "" {
		return errors.New("ownership labels are required")
	}
	return nil
}

func canonicalReleaseSnapshot(envelope ReleaseSnapshotEnvelope) ReleaseSnapshotEnvelope {
	copy := envelope
	copy.DesiredState.Deployment.Identity.Labels = cloneMap(envelope.DesiredState.Deployment.Identity.Labels)
	if envelope.DesiredState.Secret != nil {
		secret := *envelope.DesiredState.Secret
		secret.Identity.Labels = cloneMap(envelope.DesiredState.Secret.Identity.Labels)
		secret.Entries = append([]SecretEntryIntent(nil), envelope.DesiredState.Secret.Entries...)
		sort.Slice(secret.Entries, func(i, j int) bool { return secret.Entries[i].Key < secret.Entries[j].Key })
		copy.DesiredState.Secret = &secret
	}
	copy.DesiredState.Deployment.Selector = cloneMap(envelope.DesiredState.Deployment.Selector)
	copy.DesiredState.Deployment.PodLabels = cloneMap(envelope.DesiredState.Deployment.PodLabels)
	copy.DesiredState.Deployment.PodAnnotations = cloneMap(envelope.DesiredState.Deployment.PodAnnotations)
	copy.DesiredState.Deployment.ImagePullSecrets = append([]string{}, envelope.DesiredState.Deployment.ImagePullSecrets...)
	copy.DesiredState.Service.Identity.Labels = cloneMap(envelope.DesiredState.Service.Identity.Labels)
	copy.DesiredState.Service.Selector = cloneMap(envelope.DesiredState.Service.Selector)
	copy.DesiredState.Ingress.Identity.Labels = cloneMap(envelope.DesiredState.Ingress.Identity.Labels)
	if envelope.DesiredState.Scaling.KEDA != nil {
		keda := *envelope.DesiredState.Scaling.KEDA
		keda.CronSchedules = append([]KEDACronScheduleIntent(nil), envelope.DesiredState.Scaling.KEDA.CronSchedules...)
		sort.Slice(keda.CronSchedules, func(i, j int) bool {
			left, right := keda.CronSchedules[i], keda.CronSchedules[j]
			if left.Timezone != right.Timezone {
				return left.Timezone < right.Timezone
			}
			if left.Start != right.Start {
				return left.Start < right.Start
			}
			if left.End != right.End {
				return left.End < right.End
			}
			return left.DesiredReplicas < right.DesiredReplicas
		})
		if keda.Mode == "http" && keda.HTTPTargetPendingRequests == 0 {
			keda.HTTPTargetPendingRequests = defaultKEDAHTTPTargetPendingRequests
		}
		copy.DesiredState.Scaling.KEDA = &keda
	}
	if envelope.EnvVarRevision != nil {
		revision := *envelope.EnvVarRevision
		revision.Variables = append([]ReleaseEnvironment(nil), revision.Variables...)
		sort.Slice(revision.Variables, func(i, j int) bool { return revision.Variables[i].Key < revision.Variables[j].Key })
		copy.EnvVarRevision = &revision
	}
	return copy
}

func EnvDestinationSecretName(deploymentName, revisionID string) string {
	base := strings.ToLower(strings.TrimSpace(deploymentName))
	base = strings.Map(func(r rune) rune {
		if unicode.IsLower(r) || unicode.IsDigit(r) || r == '-' {
			return r
		}
		return '-'
	}, base)
	base = strings.Trim(base, "-")
	if base == "" {
		base = "app"
	}
	sum := sha256.Sum256([]byte(revisionID))
	suffix := hex.EncodeToString(sum[:8])
	maxBase := 253 - len("-env-") - len(suffix)
	if len(base) > maxBase {
		base = strings.TrimRight(base[:maxBase], "-")
	}
	return base + "-env-" + suffix
}

func ComputeEnvVarRevisionHash(variables []ReleaseEnvironment) string {
	canonical := append([]ReleaseEnvironment(nil), variables...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Key < canonical[j].Key })
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return map[string]string{}
	}
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func mapContains(container, subset map[string]string) bool {
	for key, value := range subset {
		if container[key] != value {
			return false
		}
	}
	return true
}
