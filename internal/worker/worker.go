package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/store"
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
}

type GitHubRecoveryAdapter interface {
	Dispatch(context.Context, *store.OrchestrationJob) error
	ReconcileDispatch(context.Context, *store.OrchestrationJob) error
}

type KubernetesRecoveryAdapter interface {
	RecoverExpiredDeploy(context.Context, store.DeployLease) (store.DeployRecoveryResult, error)
}

type RecoveryAdapters struct {
	Capabilities               RecoveryCapabilities
	GitHub                     GitHubRecoveryAdapter
	GitHubErrorDisposition     func(error) (bool, time.Duration, string, bool)
	Kubernetes                 KubernetesRecoveryAdapter
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
	ScanInterval   time.Duration
	LeaseDuration  time.Duration
	Owner          string
	RequestTimeout time.Duration
}

type Runner struct {
	config   Config
	logger   *slog.Logger
	store    RecoveryStore
	adapters RecoveryAdapters
}

func New(config Config, logger *slog.Logger, recoveryStore RecoveryStore, adapters RecoveryAdapters) *Runner {
	return &Runner{config: config, logger: logger, store: recoveryStore, adapters: adapters}
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
	if r.store == nil || r.logger == nil || r.config.ScanInterval <= 0 || r.config.LeaseDuration <= 0 || r.config.RequestTimeout <= 0 || r.config.RequestTimeout >= r.config.LeaseDuration || r.config.Owner == "" {
		return errors.New("invalid recovery worker configuration")
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

	queuedOrgIDs, err := r.store.ListOrchestrationOrgIDs(ctx)
	if err != nil {
		return fmt.Errorf("scan queued orchestration: %w", err)
	}
	recoveryOrgIDs, err := r.store.ListOrchestrationRecoveryOrgIDs(ctx)
	if err != nil {
		return fmt.Errorf("scan orchestration recovery: %w", err)
	}
	orgIDs := mergeOrgIDs(queuedOrgIDs, recoveryOrgIDs)
	deploys, err := r.store.ListExpiredDeploys(ctx)
	if err != nil {
		return fmt.Errorf("scan expired deploys: %w", err)
	}
	if len(orgIDs) > 0 && (!r.adapters.Capabilities.GitHub || r.adapters.GitHub == nil) {
		return &BlockedRecoveryError{Capability: GitHubRecoveryCapability, Kind: "dispatch reconciliation"}
	}
	if len(deploys) > 0 && (!r.adapters.Capabilities.Kubernetes || r.adapters.Kubernetes == nil) {
		return &BlockedRecoveryError{Capability: KubernetesRecoveryCapability, Kind: "expired deploy"}
	}

	for _, orgID := range orgIDs {
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

	for _, deploy := range deploys {
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
	return nil
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
