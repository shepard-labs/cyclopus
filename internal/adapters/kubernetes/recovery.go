package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/secretref"
	"github.com/cyclopus-dev/cyclopus/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	RecoveryReleaseAnnotation = "cyclopus.dev/recovery-release-id"
	RecoveryFenceAnnotation   = "cyclopus.dev/recovery-fence"
	RecoveryStartedAnnotation = "cyclopus.dev/recovery-started-at"
	ReleaseAnnotation         = "cyclopus.dev/release-id"
	ConfigHashAnnotation      = "cyclopus.dev/config-hash"
	EnvRevisionAnnotation     = "cyclopus.dev/env-revision-id"
	ManagedByLabel            = "app.kubernetes.io/managed-by"
	EnvironmentIDLabel        = "cyclopus.dev/environment-id"
	ManagedByValue            = "cyclopus"
)

var (
	ErrRecoveryStateInvalid = errors.New("kubernetes recovery state is invalid")
	ErrCompensationUnproven = errors.New("kubernetes compensation is not proven")
)

type Config struct {
	PollInterval    time.Duration
	ConflictRetries int
	APITimeout      time.Duration
}

type RecoveryState struct {
	Namespace                  string
	DeploymentName             string
	FailedDeploymentSnapshot   json.RawMessage
	PreviousDeploymentSnapshot json.RawMessage
	FailureReason              string
}

type RecoveryStore interface {
	RenewDeployLease(context.Context, store.DeployLease) (store.DeployLease, error)
	CheckpointDeployRecovery(context.Context, store.DeployLease, string) error
	AdoptRecoveredReleaseLive(context.Context, store.DeployLease) (store.FinalizeReleaseResult, error)
	FinalizeRecoveredReleaseFailed(context.Context, store.DeployLease, string) (store.FinalizeReleaseResult, error)
}

type RecoveryAdapter struct {
	config Config
	client kubernetes.Interface
	store  RecoveryStore
	now    func() time.Time
}

func NewRecoveryAdapter(config Config, client kubernetes.Interface, recoveryStore RecoveryStore) (*RecoveryAdapter, error) {
	if client == nil || recoveryStore == nil || config.PollInterval <= 0 || config.ConflictRetries <= 0 {
		return nil, errors.New("invalid Kubernetes recovery adapter configuration")
	}
	if config.APITimeout <= 0 {
		config.APITimeout = 5 * time.Second
	}
	return &RecoveryAdapter{config: config, client: client, store: recoveryStore, now: time.Now}, nil
}

func (a *RecoveryAdapter) RecoverExpiredDeploy(ctx context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	result := store.DeployRecoveryResult{Lease: lease}
	if lease.Timeout <= 0 {
		return result, fmt.Errorf("%w: positive persisted deploy timeout is required", ErrRecoveryStateInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, lease.Timeout)
	defer cancel()
	state, err := recoveryStateFromLease(lease)
	if err != nil {
		return result, err
	}
	failed, previous, err := validateRecoveryState(state)
	if err != nil {
		return result, err
	}

	lease, err = a.renew(ctx, lease)
	if err != nil {
		return result, err
	}
	result.Lease = lease
	if previous == nil {
		adopted, err := a.observeTerminalFailure(ctx, &lease, state, failed)
		result.Lease = lease
		if err != nil {
			return result, err
		}
		if adopted {
			return result, nil
		}
	} else {
		if err := a.restorePreviousSecretRevision(ctx, lease); err != nil {
			return result, err
		}
		lease, err = a.restore(ctx, lease, state, previous)
		result.Lease = lease
		if err != nil {
			return result, err
		}
		if err := a.store.CheckpointDeployRecovery(ctx, lease, "deployment_restored"); err != nil {
			return result, fmt.Errorf("checkpoint restored deployment: %w", err)
		}
		lease.RecoveryCheckpoint = "deployment_restored"
		result.Lease = lease
		if err := a.observeReady(ctx, &lease, state, previous); err != nil {
			result.Lease = lease
			return result, err
		}
		result.Lease = lease
	}

	lease, err = a.renew(ctx, lease)
	if err != nil {
		return result, err
	}
	result.Lease = lease
	if _, err := a.store.FinalizeRecoveredReleaseFailed(ctx, lease, state.FailureReason); err != nil {
		return result, fmt.Errorf("finalize compensated release failure: %w", err)
	}
	return result, nil
}

func RecoveryErrorDisposition(err error) (retryable bool, delay time.Duration, code string) {
	if err == nil {
		return false, 0, ""
	}
	if errors.Is(err, ErrRecoveryStateInvalid) {
		return false, 0, "recovery_state_invalid"
	}
	if errors.Is(err, ErrCompensationUnproven) {
		return false, 0, "compensation_unproven"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, store.ErrLeaseNotOwned) {
		return false, 0, ""
	}
	if errors.Is(err, context.DeadlineExceeded) || isTransient(err) {
		return true, 5 * time.Second, ""
	}
	return false, 0, "recovery_failed"
}

func recoveryStateFromLease(lease store.DeployLease) (RecoveryState, error) {
	failed, err := DecodeDeploymentSnapshot(lease.FailedRelease.ConfigSnapshot)
	if err != nil {
		return RecoveryState{}, fmt.Errorf("%w: failed snapshot: %v", ErrRecoveryStateInvalid, err)
	}
	state := RecoveryState{
		Namespace:                lease.Namespace,
		DeploymentName:           failed.Name,
		FailureReason:            "deployment did not complete before the deploy lease expired",
		FailedDeploymentSnapshot: lease.FailedRelease.ConfigSnapshot,
	}
	if lease.PreviousRelease != nil {
		state.PreviousDeploymentSnapshot = lease.PreviousRelease.ConfigSnapshot
	}
	return state, nil
}

func validateRecoveryState(state RecoveryState) (*appsv1.Deployment, *appsv1.Deployment, error) {
	if state.Namespace == "" || state.DeploymentName == "" || len(state.FailedDeploymentSnapshot) == 0 || state.FailureReason == "" {
		return nil, nil, fmt.Errorf("%w: namespace, deployment name, failed snapshot, and failure reason are required", ErrRecoveryStateInvalid)
	}
	failed, err := DecodeDeploymentSnapshot(state.FailedDeploymentSnapshot)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: failed snapshot: %v", ErrRecoveryStateInvalid, err)
	}
	if err := snapshotIdentityMatches(state, failed); err != nil {
		return nil, nil, err
	}
	if len(state.PreviousDeploymentSnapshot) == 0 {
		return failed, nil, nil
	}
	previous, err := DecodeDeploymentSnapshot(state.PreviousDeploymentSnapshot)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: previous snapshot: %v", ErrRecoveryStateInvalid, err)
	}
	if err := snapshotIdentityMatches(state, previous); err != nil {
		return nil, nil, err
	}
	return failed, previous, nil
}

func snapshotIdentityMatches(state RecoveryState, deployment *appsv1.Deployment) error {
	if deployment.Namespace != state.Namespace || deployment.Name != state.DeploymentName {
		return fmt.Errorf("%w: snapshot identity %s/%s does not match %s/%s", ErrRecoveryStateInvalid, deployment.Namespace, deployment.Name, state.Namespace, state.DeploymentName)
	}
	return nil
}

func (a *RecoveryAdapter) restore(ctx context.Context, lease store.DeployLease, state RecoveryState, previous *appsv1.Deployment) (store.DeployLease, error) {
	var renewed store.DeployLease
	backoff := retry.DefaultRetry
	backoff.Steps = a.config.ConflictRetries
	err := retry.OnError(backoff, isTransient, func() error {
		var err error
		renewed, err = a.renew(ctx, lease)
		if err != nil {
			return err
		}
		lease = renewed
		current, err := a.getDeployment(ctx, lease, state.Namespace, state.DeploymentName)
		if apierrors.IsNotFound(err) {
			if err := verifyOwnership(previous, lease.EnvironmentID); err != nil {
				return err
			}
			candidate := previous.DeepCopy()
			candidate.ResourceVersion = ""
			setRecoveryMetadata(candidate, lease, a.now())
			_, err = a.createDeployment(ctx, lease, state.Namespace, candidate)
			return err
		}
		if err != nil {
			return err
		}
		if err := verifyOwnership(current, lease.EnvironmentID); err != nil {
			return err
		}
		candidate := current.DeepCopy()
		if err := patchOwnedPodTemplate(candidate, previous, *lease.PreviousRelease); err != nil {
			return err
		}
		setRecoveryMetadata(candidate, lease, a.now())
		_, err = a.updateDeployment(ctx, lease, state.Namespace, candidate)
		return err
	})
	if err != nil {
		return lease, fmt.Errorf("restore deployment snapshot: %w", err)
	}
	return renewed, nil
}

func isConflict(err error) bool {
	if apierrors.IsConflict(err) {
		return true
	}
	var status apierrors.APIStatus
	return errors.As(err, &status) && status.Status().Code == 409
}

func isTransient(err error) bool {
	return isConflict(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err)
}

func (a *RecoveryAdapter) observeReady(ctx context.Context, lease *store.DeployLease, state RecoveryState, expected *appsv1.Deployment) error {
	return a.observe(ctx, lease, state, func(deployment *appsv1.Deployment) (bool, error) {
		if deploymentFailed(deployment) {
			return false, fmt.Errorf("%w: restored deployment exceeded its progress deadline", ErrCompensationUnproven)
		}
		return deploymentReady(deployment, *lease, *lease.PreviousRelease), nil
	})
}

func (a *RecoveryAdapter) observeTerminalFailure(ctx context.Context, lease *store.DeployLease, state RecoveryState, failed *appsv1.Deployment) (bool, error) {
	adopted := false
	err := a.observe(ctx, lease, state, func(deployment *appsv1.Deployment) (bool, error) {
		if err := verifyOwnership(deployment, lease.EnvironmentID); err != nil {
			return false, err
		}
		if !releaseIdentityMatches(deployment, lease.FailedRelease) || !ownedTemplateMatches(deployment, failed) {
			return false, fmt.Errorf("%w: first deployment no longer matches its release snapshot", ErrCompensationUnproven)
		}
		if deploymentReady(deployment, *lease, lease.FailedRelease) {
			if err := a.store.CheckpointDeployRecovery(ctx, *lease, "late_ready_observed"); err != nil {
				return false, fmt.Errorf("checkpoint late-ready deployment: %w", err)
			}
			lease.RecoveryCheckpoint = "late_ready_observed"
			if _, err := a.store.AdoptRecoveredReleaseLive(ctx, *lease); err != nil {
				return false, fmt.Errorf("adopt late-ready release: %w", err)
			}
			adopted = true
			return true, nil
		}
		return deploymentFailed(deployment), nil
	})
	if err == nil && !adopted && lease.RecoveryCheckpoint != "first_deployment_missing" {
		if err := a.store.CheckpointDeployRecovery(ctx, *lease, "claimed"); err != nil {
			return false, fmt.Errorf("checkpoint terminal first deployment: %w", err)
		}
		lease.RecoveryCheckpoint = "claimed"
	}
	return adopted, err
}

func (a *RecoveryAdapter) observe(ctx context.Context, lease *store.DeployLease, state RecoveryState, condition func(*appsv1.Deployment) (bool, error)) error {
	err := wait.PollUntilContextCancel(ctx, a.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		renewed, err := a.renew(ctx, *lease)
		if err != nil {
			return false, err
		}
		*lease = renewed
		deployment, err := a.getDeployment(ctx, *lease, state.Namespace, state.DeploymentName)
		if err != nil {
			if apierrors.IsNotFound(err) && lease.PreviousRelease == nil {
				if err := a.store.CheckpointDeployRecovery(ctx, *lease, "first_deployment_missing"); err != nil {
					return false, fmt.Errorf("checkpoint missing first deployment: %w", err)
				}
				lease.RecoveryCheckpoint = "first_deployment_missing"
				return true, nil
			}
			return false, err
		}
		return condition(deployment)
	})
	if err != nil {
		return fmt.Errorf("observe deployment compensation: %w", err)
	}
	return nil
}

func (a *RecoveryAdapter) restorePreviousSecretRevision(ctx context.Context, lease store.DeployLease) error {
	if lease.PreviousRelease == nil || lease.FailedRelease.Kind != "env_change" || lease.PreviousRelease.EnvVarRevisionID == nil {
		return nil
	}
	envelope, err := store.DecodeReleaseSnapshot(lease.PreviousRelease.ConfigSnapshot)
	if err != nil {
		return fmt.Errorf("decode previous env-change snapshot: %w", err)
	}
	if envelope.EnvVarRevision == nil || envelope.EnvVarRevision.ID != *lease.PreviousRelease.EnvVarRevisionID {
		return fmt.Errorf("%w: previous env-var revision is missing from release snapshot", ErrRecoveryStateInvalid)
	}
	seen := make(map[string]struct{})
	for _, variable := range envelope.EnvVarRevision.Variables {
		namespace, name, err := parseSecretRef(variable.SecretRef)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRecoveryStateInvalid, err)
		}
		if namespace != lease.Namespace {
			return fmt.Errorf("%w: secret namespace %q does not match environment namespace", ErrCompensationUnproven, namespace)
		}
		key := namespace + "/" + name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		opCtx, cancel := a.operationContext(ctx, lease)
		secret, err := a.client.CoreV1().Secrets(namespace).Get(opCtx, name, metav1.GetOptions{})
		cancel()
		if err != nil {
			return fmt.Errorf("verify previous immutable Secret %s: %w", key, err)
		}
		if secret.Immutable == nil || !*secret.Immutable || secret.Annotations[EnvRevisionAnnotation] != envelope.EnvVarRevision.ID ||
			secret.Annotations[ConfigHashAnnotation] != lease.PreviousRelease.ConfigHash {
			return fmt.Errorf("%w: previous Secret %s revision/hash is not verified", ErrCompensationUnproven, key)
		}
	}
	if err := a.store.CheckpointDeployRecovery(ctx, lease, "secret_revision_verified"); err != nil {
		return fmt.Errorf("checkpoint previous Secret revision: %w", err)
	}
	return nil
}

func parseSecretRef(ref string) (string, string, error) {
	parsed, err := secretref.Parse(ref)
	return parsed.Namespace, parsed.Name, err
}

func (a *RecoveryAdapter) renew(ctx context.Context, lease store.DeployLease) (store.DeployLease, error) {
	renewed, err := a.store.RenewDeployLease(ctx, lease)
	if err != nil {
		return store.DeployLease{}, fmt.Errorf("renew deploy recovery lease: %w", err)
	}
	return renewed, nil
}

func deploymentReady(deployment *appsv1.Deployment, lease store.DeployLease, expected store.ImmutableReleaseSnapshot) bool {
	if deployment.Generation == 0 || deployment.Status.ObservedGeneration < deployment.Generation || deployment.Spec.Replicas == nil {
		return false
	}
	desired := *deployment.Spec.Replicas
	if deployment.Status.UpdatedReplicas != desired || deployment.Status.ReadyReplicas != desired || deployment.Status.AvailableReplicas != desired || deployment.Status.UnavailableReplicas != 0 {
		return false
	}
	return releaseIdentityMatches(deployment, expected)
}

func releaseIdentityMatches(deployment *appsv1.Deployment, expected store.ImmutableReleaseSnapshot) bool {
	return deployment.Annotations[ReleaseAnnotation] == expected.ID && deployment.Annotations[ConfigHashAnnotation] == expected.ConfigHash
}

func deploymentFailed(deployment *appsv1.Deployment) bool {
	if deployment.Status.ObservedGeneration < deployment.Generation {
		return false
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == "False" && condition.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

func verifyOwnership(deployment *appsv1.Deployment, environmentID string) error {
	if deployment.Labels[ManagedByLabel] != ManagedByValue || deployment.Labels[EnvironmentIDLabel] != environmentID {
		return fmt.Errorf("%w: deployment ownership labels do not match environment", ErrCompensationUnproven)
	}
	return nil
}

func patchOwnedPodTemplate(candidate, expected *appsv1.Deployment, release store.ImmutableReleaseSnapshot) error {
	for i := range expected.Spec.Template.Spec.Containers {
		wanted := expected.Spec.Template.Spec.Containers[i]
		found := false
		for j := range candidate.Spec.Template.Spec.Containers {
			if candidate.Spec.Template.Spec.Containers[j].Name == wanted.Name {
				candidate.Spec.Template.Spec.Containers[j].Image = wanted.Image
				candidate.Spec.Template.Spec.Containers[j].Env = append([]corev1.EnvVar(nil), wanted.Env...)
				candidate.Spec.Template.Spec.Containers[j].EnvFrom = append([]corev1.EnvFromSource(nil), wanted.EnvFrom...)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: owned container %q is missing", ErrCompensationUnproven, wanted.Name)
		}
	}
	if candidate.Spec.Template.Annotations == nil {
		candidate.Spec.Template.Annotations = map[string]string{}
	}
	candidate.Spec.Template.Annotations[ReleaseAnnotation] = release.ID
	candidate.Spec.Template.Annotations[ConfigHashAnnotation] = release.ConfigHash
	if candidate.Annotations == nil {
		candidate.Annotations = map[string]string{}
	}
	candidate.Annotations[ReleaseAnnotation] = release.ID
	candidate.Annotations[ConfigHashAnnotation] = release.ConfigHash
	return nil
}

func ownedTemplateMatches(actual, expected *appsv1.Deployment) bool {
	for _, wanted := range expected.Spec.Template.Spec.Containers {
		matched := false
		for _, current := range actual.Spec.Template.Spec.Containers {
			if current.Name == wanted.Name && current.Image == wanted.Image && reflect.DeepEqual(current.Env, wanted.Env) && reflect.DeepEqual(current.EnvFrom, wanted.EnvFrom) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func setRecoveryMetadata(deployment *appsv1.Deployment, lease store.DeployLease, now time.Time) {
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}
	deployment.Annotations[RecoveryReleaseAnnotation] = lease.ReleaseID
	deployment.Annotations[RecoveryFenceAnnotation] = lease.Fence.UTC().Format(time.RFC3339Nano)
	deployment.Annotations[RecoveryStartedAnnotation] = now.UTC().Format(time.RFC3339Nano)
}

func (a *RecoveryAdapter) operationContext(ctx context.Context, lease store.DeployLease) (context.Context, context.CancelFunc) {
	timeout := a.config.APITimeout
	remaining := time.Until(lease.ExpiresAt)
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	if timeout >= remaining {
		timeout = remaining / 2
	}
	return context.WithTimeout(ctx, timeout)
}

func (a *RecoveryAdapter) getDeployment(ctx context.Context, lease store.DeployLease, namespace, name string) (*appsv1.Deployment, error) {
	var deployment *appsv1.Deployment
	err := retry.OnError(retry.DefaultBackoff, isTransient, func() error {
		opCtx, cancel := a.operationContext(ctx, lease)
		defer cancel()
		var err error
		deployment, err = a.client.AppsV1().Deployments(namespace).Get(opCtx, name, metav1.GetOptions{})
		return err
	})
	return deployment, err
}

func (a *RecoveryAdapter) updateDeployment(ctx context.Context, lease store.DeployLease, namespace string, deployment *appsv1.Deployment) (*appsv1.Deployment, error) {
	opCtx, cancel := a.operationContext(ctx, lease)
	defer cancel()
	return a.client.AppsV1().Deployments(namespace).Update(opCtx, deployment, metav1.UpdateOptions{})
}

func (a *RecoveryAdapter) createDeployment(ctx context.Context, lease store.DeployLease, namespace string, deployment *appsv1.Deployment) (*appsv1.Deployment, error) {
	opCtx, cancel := a.operationContext(ctx, lease)
	defer cancel()
	return a.client.AppsV1().Deployments(namespace).Create(opCtx, deployment, metav1.CreateOptions{})
}
