package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/store"
	"github.com/cyclopus-dev/cyclopus/internal/webhook"
)

type RecoveryStore interface {
	DeleteExpiredIdempotencyRecords(context.Context, int) (int64, error)
	RequeueExpiredOrchestrationLeases(context.Context) (int64, error)
	ListOrchestrationOrgIDs(context.Context) ([]string, error)
	ListOrchestrationRecoveryOrgIDs(context.Context) ([]string, error)
	ClaimOrchestration(context.Context, string, string, time.Duration) (*store.OrchestrationJob, error)
	ClaimDispatchReconciliation(context.Context, string, string, time.Duration) (*store.OrchestrationJob, error)
	RescheduleOrchestration(context.Context, string, string, string, time.Duration) error
	RescheduleDispatchReconciliation(context.Context, string, string, string, time.Duration) error
	FailOrchestration(context.Context, store.FailOrchestrationParams) error
	ListExpiredDeploys(context.Context) ([]store.ExpiredDeploy, error)
	ClaimExpiredDeployLease(context.Context, string, string) (store.DeployLease, error)
	RescheduleDeployRecovery(context.Context, store.DeployLease, time.Duration) error
	QuarantineDeployRecovery(context.Context, store.DeployLease, string, string) error
	ListForwardDeployOrgIDs(context.Context, bool) ([]string, error)
	ClaimPendingDeploy(context.Context, string, []string) (store.DeployExecution, error)
	ClaimUnexpiredDeploying(context.Context, string, []string) (store.DeployExecution, error)
	RequeueExpiredLifecycleLeases(context.Context) (int64, error)
	ListLifecycleOrgIDs(context.Context) ([]string, error)
	ClaimEnvironmentLifecycleJob(context.Context, string, string, time.Duration) (store.EnvironmentLifecycleJob, error)
	RescheduleEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob, time.Duration, string, string) error
	ListIdlePreviewCandidates(context.Context, int) ([]store.IdlePreviewCandidate, error)
	RequestIdlePreviewTeardown(context.Context, store.IdlePreviewCandidate) (bool, error)
	ListWebhookOrgIDs(context.Context) ([]string, error)
	ClaimWebhookDelivery(context.Context, string, string, time.Duration) (store.WebhookDelivery, error)
}

type GitHubRecoveryAdapter interface {
	Dispatch(context.Context, *store.OrchestrationJob) error
	ReconcileDispatch(context.Context, *store.OrchestrationJob) error
}

type KubernetesRecoveryAdapter interface {
	RecoverExpiredDeploy(context.Context, store.DeployLease) (store.DeployRecoveryResult, error)
	ExecuteDeploy(context.Context, store.DeployExecution) (store.DeployRecoveryResult, error)
}

type KubernetesLifecycleAdapter interface {
	ExecuteEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob) error
}

type WebhookDeliveryAdapter interface {
	Deliver(context.Context, store.WebhookDelivery) (store.WebhookDeliveryOutcome, error)
}

type RecoveryAdapters struct {
	Capabilities               RecoveryCapabilities
	GitHub                     GitHubRecoveryAdapter
	GitHubErrorDisposition     func(error) (bool, time.Duration, string, bool)
	Kubernetes                 KubernetesRecoveryAdapter
	Lifecycle                  KubernetesLifecycleAdapter
	Webhook                    WebhookDeliveryAdapter
	KubernetesErrorDisposition func(error) (bool, time.Duration, string)
}

type RecoveryCapabilities struct {
	GitHub     bool
	Kubernetes bool
}

type RecoveryCapability string

const (
	GitHubRecoveryCapability     RecoveryCapability = "github"
	KubernetesRecoveryCapability RecoveryCapability = "kubernetes"
)

type BlockedRecoveryError struct {
	Capability RecoveryCapability
	Kind       string
}

func (e *BlockedRecoveryError) Error() string {
	return fmt.Sprintf("%s recovery is blocked: %s adapter is unavailable", e.Kind, e.Capability)
}

type Config struct {
	ScanInterval         time.Duration
	LeaseDuration        time.Duration
	MaxConcurrentDeploys int
	Owner                string
	RequestTimeout       time.Duration
}

type Runner struct {
	config    Config
	logger    *slog.Logger
	store     RecoveryStore
	adapters  RecoveryAdapters
	deployMu  sync.Mutex
	inFlight  map[string]struct{}
	deployWG  sync.WaitGroup
	deploySem chan struct{}
}

func New(config Config, logger *slog.Logger, recoveryStore RecoveryStore, adapters RecoveryAdapters) *Runner {
	return &Runner{config: config, logger: logger, store: recoveryStore, adapters: adapters,
		inFlight: make(map[string]struct{}), deploySem: make(chan struct{}, config.MaxConcurrentDeploys)}
}

func (r *Runner) Startup(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	return r.scan(ctx, true)
}

func (r *Runner) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	ticker := time.NewTicker(r.config.ScanInterval)
	defer ticker.Stop()
	defer r.deployWG.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.scan(ctx, false); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func (r *Runner) validate() error {
	if r.store == nil || r.logger == nil || r.config.ScanInterval <= 0 || r.config.LeaseDuration <= 0 || r.config.MaxConcurrentDeploys <= 0 || r.config.RequestTimeout <= 0 || r.config.RequestTimeout >= r.config.LeaseDuration || r.config.Owner == "" {
		return errors.New("invalid recovery worker configuration")
	}
	if r.config.LeaseDuration <= webhook.RequestTimeout {
		return fmt.Errorf("webhook lease duration %s must exceed the %s delivery timeout", r.config.LeaseDuration, webhook.RequestTimeout)
	}
	return nil
}

func (r *Runner) scan(ctx context.Context, startup bool) error {
	deleted, err := r.store.DeleteExpiredIdempotencyRecords(ctx, 1000)
	if err != nil {
		return fmt.Errorf("clean expired idempotency records: %w", err)
	}
	if deleted > 0 {
		r.logger.Info("deleted expired idempotency records", "count", deleted)
	}
	requeued, err := r.store.RequeueExpiredOrchestrationLeases(ctx)
	if err != nil {
		return fmt.Errorf("recover orchestration leases: %w", err)
	}
	if requeued > 0 {
		r.logger.Info("requeued expired orchestration leases", "count", requeued)
	}
	if _, err := r.store.RequeueExpiredLifecycleLeases(ctx); err != nil {
		return fmt.Errorf("recover environment lifecycle leases: %w", err)
	}
	idleCandidates, err := r.store.ListIdlePreviewCandidates(ctx, 100)
	if err != nil {
		return fmt.Errorf("scan idle preview environments: %w", err)
	}
	for _, candidate := range idleCandidates {
		if _, err := r.store.RequestIdlePreviewTeardown(ctx, candidate); err != nil {
			return fmt.Errorf("request idle preview teardown %s/%s: %w", candidate.OrgID, candidate.EnvironmentID, err)
		}
	}
	lifecycleOrgIDs, err := r.store.ListLifecycleOrgIDs(ctx)
	if err != nil {
		return fmt.Errorf("scan environment lifecycle: %w", err)
	}
	webhookOrgIDs, err := r.store.ListWebhookOrgIDs(ctx)
	if err != nil {
		return fmt.Errorf("scan webhook deliveries: %w", err)
	}

	queuedOrgIDs, err := r.store.ListOrchestrationOrgIDs(ctx)
	if err != nil {
		return fmt.Errorf("scan queued orchestration: %w", err)
	}
	recoveryOrgIDs, err := r.store.ListOrchestrationRecoveryOrgIDs(ctx)
	if err != nil {
		return fmt.Errorf("scan orchestration recovery: %w", err)
	}
	orgIDs := mergeOrgIDs(queuedOrgIDs, recoveryOrgIDs)
	resumeDeployOrgIDs, err := r.store.ListForwardDeployOrgIDs(ctx, true)
	if err != nil {
		return fmt.Errorf("scan resumable deploys: %w", err)
	}
	pendingDeployOrgIDs, err := r.store.ListForwardDeployOrgIDs(ctx, false)
	if err != nil {
		return fmt.Errorf("scan pending deploys: %w", err)
	}
	deploys, err := r.store.ListExpiredDeploys(ctx)
	if err != nil {
		return fmt.Errorf("scan expired deploys: %w", err)
	}
	if len(orgIDs) > 0 && (!r.adapters.Capabilities.GitHub || r.adapters.GitHub == nil) {
		return &BlockedRecoveryError{Capability: GitHubRecoveryCapability, Kind: "dispatch reconciliation"}
	}
	if (len(resumeDeployOrgIDs) > 0 || len(pendingDeployOrgIDs) > 0 || len(deploys) > 0) && (!r.adapters.Capabilities.Kubernetes || r.adapters.Kubernetes == nil) {
		kind := "forward deploy"
		if len(resumeDeployOrgIDs) == 0 && len(pendingDeployOrgIDs) == 0 {
			kind = "expired deploy"
		}
		return &BlockedRecoveryError{Capability: KubernetesRecoveryCapability, Kind: kind}
	}
	if len(lifecycleOrgIDs) > 0 && (!r.adapters.Capabilities.Kubernetes || r.adapters.Lifecycle == nil) {
		return &BlockedRecoveryError{Capability: KubernetesRecoveryCapability, Kind: "environment lifecycle"}
	}
	for _, orgID := range lifecycleOrgIDs {
		for {
			job, err := r.store.ClaimEnvironmentLifecycleJob(ctx, orgID, r.config.Owner, r.config.LeaseDuration)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				return fmt.Errorf("claim environment lifecycle for organization %s: %w", orgID, err)
			}
			requestCtx, cancel := context.WithTimeout(ctx, r.config.RequestTimeout)
			err = r.adapters.Lifecycle.ExecuteEnvironmentLifecycle(requestCtx, job)
			cancel()
			if err == nil {
				continue
			}
			if errors.Is(err, store.ErrConflict) {
				if rescheduleErr := r.store.RescheduleEnvironmentLifecycle(ctx, job, time.Second, "active_work", err.Error()); rescheduleErr != nil {
					return rescheduleErr
				}
				continue
			}
			if rescheduleErr := r.store.RescheduleEnvironmentLifecycle(ctx, job, retryDelay(job.Attempt, 0, r.config.LeaseDuration), "lifecycle_retry", err.Error()); rescheduleErr != nil {
				return fmt.Errorf("reschedule environment lifecycle %s: %w", job.ID, rescheduleErr)
			}
		}
	}
	for _, orgID := range webhookOrgIDs {
		if r.adapters.Webhook == nil {
			return errors.New("webhook delivery adapter is unavailable")
		}
		for {
			delivery, err := r.store.ClaimWebhookDelivery(ctx, orgID, r.config.Owner, r.config.LeaseDuration)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				return fmt.Errorf("claim webhook delivery for organization %s: %w", orgID, err)
			}
			requestCtx, cancel := context.WithTimeout(ctx, webhook.RequestTimeout)
			outcome, err := r.adapters.Webhook.Deliver(requestCtx, delivery)
			cancel()
			if err != nil && !errors.Is(err, store.ErrLeaseNotOwned) {
				r.logger.Warn("webhook delivery error", "org_id", delivery.OrgID, "webhook_id", delivery.WebhookID,
					"delivery_id", delivery.ID, "event", delivery.Event, "error", err)
				continue
			}
			if outcome.Abandoned {
				r.logger.Warn("webhook.delivery_failed", "org_id", delivery.OrgID, "webhook_id", delivery.WebhookID,
					"delivery_id", delivery.ID, "event", delivery.Event, "reason", outcome.Reason)
			}
			if outcome.EndpointDisabled {
				r.logger.Warn("webhook.disabled", "org_id", delivery.OrgID, "webhook_id", delivery.WebhookID)
			}
		}
	}

	if startup {
		for {
			claimed, err := r.scheduleForwardDeploys(ctx, resumeDeployOrgIDs, true, true)
			if err != nil {
				return err
			}
			r.deployWG.Wait()
			if claimed == 0 {
				break
			}
		}
	} else if _, err := r.scheduleForwardDeploys(ctx, resumeDeployOrgIDs, true, false); err != nil {
		return err
	}

	for _, deploy := range deploys {
		if r.environmentInFlight(deploy.EnvironmentID) {
			continue
		}
		lease, err := r.store.ClaimExpiredDeployLease(ctx, deploy.OrgID, deploy.EnvironmentID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("claim expired deploy %s/%s: %w", deploy.OrgID, deploy.EnvironmentID, err)
		}
		result, err := r.adapters.Kubernetes.RecoverExpiredDeploy(ctx, lease)
		if err != nil {
			lease = result.Lease
			if r.adapters.KubernetesErrorDisposition == nil {
				return fmt.Errorf("Kubernetes deploy recovery %s/%s returned error without classifier: %w", lease.OrgID, lease.EnvironmentID, err)
			}
			retryable, delay, code := r.adapters.KubernetesErrorDisposition(err)
			if !retryable {
				if code == "" {
					r.logger.Warn("Kubernetes deploy recovery ownership lost", "org_id", lease.OrgID,
						"environment_id", lease.EnvironmentID, "startup", startup, "error", err)
					continue
				}
				if quarantineErr := r.store.QuarantineDeployRecovery(ctx, lease, code, err.Error()); quarantineErr != nil {
					return fmt.Errorf("quarantine Kubernetes deploy recovery %s/%s: %w", lease.OrgID, lease.EnvironmentID, quarantineErr)
				}
				r.logger.Error("Kubernetes deploy recovery quarantined", "org_id", lease.OrgID,
					"environment_id", lease.EnvironmentID, "code", code, "startup", startup, "error", err)
				continue
			}
			if delay <= 0 {
				delay = time.Second
			}
			if rescheduleErr := r.store.RescheduleDeployRecovery(ctx, lease, delay); rescheduleErr != nil {
				return fmt.Errorf("reschedule Kubernetes deploy recovery %s/%s: %w", lease.OrgID, lease.EnvironmentID, rescheduleErr)
			}
			r.logger.Error("Kubernetes deploy recovery failed", "org_id", lease.OrgID,
				"environment_id", lease.EnvironmentID, "retry_after", delay, "error", err)
		}
	}

	for _, orgID := range recoveryOrgIDs {
		for {
			job, err := r.store.ClaimDispatchReconciliation(ctx, orgID, r.config.Owner, r.config.LeaseDuration)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				return fmt.Errorf("claim orchestration recovery for organization %s: %w", orgID, err)
			}
			requestCtx, cancel := context.WithTimeout(ctx, r.config.RequestTimeout)
			err = r.adapters.GitHub.ReconcileDispatch(requestCtx, job)
			cancel()
			if err != nil {
				if handleErr := r.handleGitHubJobError(ctx, job, true, err); handleErr != nil {
					return handleErr
				}
			}
		}
	}

	for _, orgID := range queuedOrgIDs {
		for {
			job, err := r.store.ClaimOrchestration(ctx, orgID, r.config.Owner, r.config.LeaseDuration)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				return fmt.Errorf("claim queued orchestration for organization %s: %w", orgID, err)
			}
			requestCtx, cancel := context.WithTimeout(ctx, r.config.RequestTimeout)
			err = r.adapters.GitHub.Dispatch(requestCtx, job)
			cancel()
			if err != nil {
				if handleErr := r.handleGitHubJobError(ctx, job, false, err); handleErr != nil {
					return handleErr
				}
			}
		}
	}

	if _, err := r.scheduleForwardDeploys(ctx, pendingDeployOrgIDs, false, startup); err != nil {
		return err
	}
	return nil
}

func (r *Runner) scheduleForwardDeploys(ctx context.Context, orgIDs []string, resume, startup bool) (int, error) {
	claimed := 0
	for _, orgID := range orgIDs {
		for {
			select {
			case r.deploySem <- struct{}{}:
			default:
				return claimed, nil
			}
			execution, err := r.claimAndMarkForwardDeploy(ctx, orgID, resume)
			if errors.Is(err, store.ErrNotFound) {
				<-r.deploySem
				break
			}
			if err != nil {
				<-r.deploySem
				return claimed, fmt.Errorf("claim forward deploy for organization %s: %w", orgID, err)
			}
			claimed++
			r.deployWG.Add(1)
			go func(execution store.DeployExecution) {
				defer r.deployWG.Done()
				defer func() { <-r.deploySem; r.clearEnvironmentInFlight(execution.EnvironmentID) }()
				result, err := r.adapters.Kubernetes.ExecuteDeploy(ctx, execution)
				if err != nil {
					r.logger.Warn("Kubernetes forward deploy remains durable", "org_id", execution.OrgID, "environment_id", execution.EnvironmentID, "release_id", execution.ReleaseID, "fence", result.Lease.Fence, "resumed", resume, "startup", startup, "error", err)
				}
			}(execution)
		}
	}
	return claimed, nil
}

func (r *Runner) claimAndMarkForwardDeploy(ctx context.Context, orgID string, resume bool) (store.DeployExecution, error) {
	r.deployMu.Lock()
	defer r.deployMu.Unlock()
	excluded := make([]string, 0, len(r.inFlight))
	for environmentID := range r.inFlight {
		excluded = append(excluded, environmentID)
	}
	var execution store.DeployExecution
	var err error
	if resume {
		execution, err = r.store.ClaimUnexpiredDeploying(ctx, orgID, excluded)
	} else {
		execution, err = r.store.ClaimPendingDeploy(ctx, orgID, excluded)
	}
	if err == nil {
		r.inFlight[execution.EnvironmentID] = struct{}{}
	}
	return execution, err
}
func (r *Runner) clearEnvironmentInFlight(environmentID string) {
	r.deployMu.Lock()
	delete(r.inFlight, environmentID)
	r.deployMu.Unlock()
}
func (r *Runner) environmentInFlight(environmentID string) bool {
	r.deployMu.Lock()
	defer r.deployMu.Unlock()
	_, ok := r.inFlight[environmentID]
	return ok
}
func mergeOrgIDs(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var merged []string
	for _, group := range groups {
		for _, orgID := range group {
			if _, exists := seen[orgID]; exists {
				continue
			}
			seen[orgID] = struct{}{}
			merged = append(merged, orgID)
		}
	}
	return merged
}

func (r *Runner) handleGitHubJobError(ctx context.Context, job *store.OrchestrationJob, reconciling bool, err error) error {
	var dispatchStarted interface{ DispatchStarted() bool }
	if errors.As(err, &dispatchStarted) && dispatchStarted.DispatchStarted() {
		reconciling = true
	}
	if r.adapters.GitHubErrorDisposition == nil {
		return fmt.Errorf("GitHub orchestration %s/%s returned error without classifier: %w", job.OrgID, job.ID, err)
	}
	retryable, retryAfter, code, classified := r.adapters.GitHubErrorDisposition(err)
	if !classified {
		return fmt.Errorf("GitHub orchestration %s/%s returned unclassified error: %w", job.OrgID, job.ID, err)
	}
	if retryable {
		delay := retryDelay(job.Attempt, retryAfter, r.config.LeaseDuration)
		var storeErr error
		if reconciling || job.Status == "dispatch_reconciling" || job.Status == "dispatched" {
			storeErr = r.store.RescheduleDispatchReconciliation(ctx, job.OrgID, job.ID, r.config.Owner, delay)
		} else {
			storeErr = r.store.RescheduleOrchestration(ctx, job.OrgID, job.ID, r.config.Owner, delay)
		}
		if storeErr != nil {
			return fmt.Errorf("reschedule GitHub orchestration %s/%s: %w", job.OrgID, job.ID, storeErr)
		}
		r.logger.Warn("GitHub orchestration rescheduled", "org_id", job.OrgID, "job_id", job.ID, "delay", delay, "error", err)
		return nil
	}
	if code == "" {
		code = "github_job_failed"
	}
	if storeErr := r.store.FailOrchestration(ctx, store.FailOrchestrationParams{
		OrgID: job.OrgID, JobID: job.ID, Owner: r.config.Owner, Code: code, Message: err.Error(),
	}); storeErr != nil {
		return fmt.Errorf("terminalize GitHub orchestration %s/%s: %w", job.OrgID, job.ID, storeErr)
	}
	r.logger.Error("GitHub orchestration terminalized", "org_id", job.OrgID, "job_id", job.ID, "code", code, "error", err)
	return nil
}

func retryDelay(attempt int, retryAfter, leaseDuration time.Duration) time.Duration {
	delay := time.Second
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	maxDelay := leaseDuration / 2
	if maxDelay > 30*time.Second {
		maxDelay = 30 * time.Second
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}
