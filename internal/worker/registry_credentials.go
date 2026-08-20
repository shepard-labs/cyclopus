package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/kubernetes"
	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/ocimetrics"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const maxRegistryCredentialAttempts = 12

type RegistryCredentialRunnerStore interface {
	RecoverExpiredRegistryCredentialLeases(context.Context, int) (int64, error)
	RecoverExpiredRegistryCredentialValidationTargets(context.Context, int) (int64, error)
	ListRegistryCredentialOperationOrgIDs(context.Context, int) ([]string, error)
	ClaimRegistryCredentialOperations(context.Context, string, string, time.Duration, int) ([]store.RegistryCredentialOperation, error)
	RetryRegistryCredentialOperation(context.Context, store.RegistryCredentialOperation, store.LeaseToken, time.Duration, string) error
	FailRegistryCredentialOperation(context.Context, store.RegistryCredentialOperation, store.LeaseToken, string) error
}

type RegistryCredentialOperationProcessor interface {
	Process(context.Context, store.RegistryCredentialOperation) (RegistryCredentialProcessResult, error)
}

type RegistryCredentialConfig struct {
	ScanInterval, LeaseDuration, RequestTimeout, ShutdownGrace time.Duration
	Concurrency                                                int
	Owner                                                      string
}

type RegistryCredentialRunner struct {
	config    RegistryCredentialConfig
	logger    *slog.Logger
	store     RegistryCredentialRunnerStore
	processor RegistryCredentialOperationProcessor
	metrics   ocimetrics.Recorder
	ready     atomic.Bool
	sem       chan struct{}
	wg        sync.WaitGroup
	scanMu    sync.Mutex
	process   context.Context
	cancel    context.CancelFunc
}

func NewRegistryCredentialRunner(config RegistryCredentialConfig, logger *slog.Logger, dataStore RegistryCredentialRunnerStore, processor RegistryCredentialOperationProcessor, recorders ...ocimetrics.Recorder) *RegistryCredentialRunner {
	recorder := ocimetrics.Noop()
	if len(recorders) > 0 && recorders[0] != nil {
		recorder = recorders[0]
	}
	return &RegistryCredentialRunner{config: config, logger: logger, store: dataStore, processor: processor, metrics: recorder, sem: make(chan struct{}, config.Concurrency)}
}

func (r *RegistryCredentialRunner) Ready() bool { return r.ready.Load() }

func (r *RegistryCredentialRunner) Startup(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	r.process, r.cancel = context.WithCancel(context.WithoutCancel(ctx))
	if err := r.recover(ctx); err != nil {
		r.cancel()
		return err
	}
	if err := r.scan(ctx); err != nil {
		r.cancel()
		r.wg.Wait()
		return err
	}
	r.ready.Store(true)
	return nil
}

func (r *RegistryCredentialRunner) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.cancel == nil {
		return errors.New("registry credential worker Startup must run before Run")
	}
	ticker := time.NewTicker(r.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.ready.Store(false)
			r.cancel()
			return r.awaitShutdown()
		case <-ticker.C:
			if err := r.scan(ctx); err != nil && ctx.Err() == nil {
				r.logger.Error("registry credential scan failed", "error", err)
			}
		}
	}
}

func (r *RegistryCredentialRunner) validate() error {
	c := r.config
	if r.store == nil || r.processor == nil || r.logger == nil || c.Owner == "" || c.ScanInterval <= 0 || c.LeaseDuration <= 0 || c.RequestTimeout <= 0 || c.RequestTimeout >= c.LeaseDuration || c.ShutdownGrace <= 0 || c.Concurrency < 1 || c.Concurrency > 100 {
		return errors.New("invalid registry credential worker configuration")
	}
	return nil
}

func (r *RegistryCredentialRunner) recover(ctx context.Context) error {
	if _, err := r.store.RecoverExpiredRegistryCredentialLeases(ctx, 1000); err != nil {
		return err
	}
	_, err := r.store.RecoverExpiredRegistryCredentialValidationTargets(ctx, 1000)
	return err
}

func (r *RegistryCredentialRunner) scan(ctx context.Context) error {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	claimed := int64(0)
	defer func() { r.metrics.Work("credential", int64(len(r.sem)), claimed) }()
	if err := r.recover(ctx); err != nil {
		return err
	}
	orgIDs, err := r.store.ListRegistryCredentialOperationOrgIDs(ctx, 1000)
	if err != nil {
		return err
	}
	for _, orgID := range orgIDs {
		for {
			slots := cap(r.sem) - len(r.sem)
			if slots == 0 {
				return nil
			}
			operations, err := r.store.ClaimRegistryCredentialOperations(ctx, orgID, r.config.Owner, r.config.LeaseDuration, slots)
			if err != nil {
				return err
			}
			claimed = min(claimed+int64(len(operations)), int64(cap(r.sem)))
			for _, operation := range operations {
				r.sem <- struct{}{}
				r.wg.Add(1)
				go r.processOperation(r.process, operation)
			}
			if len(operations) < slots {
				break
			}
		}
	}
	return nil
}

func (r *RegistryCredentialRunner) processOperation(root context.Context, operation store.RegistryCredentialOperation) {
	defer func() {
		<-r.sem
		r.metrics.Work("credential", int64(len(r.sem)), 0)
		r.wg.Done()
	}()
	started := time.Now()
	ctx, cancel := context.WithTimeout(root, r.config.RequestTimeout)
	result, processErr := r.processor.Process(ctx, operation)
	cancel()
	if root.Err() != nil || operation.Lease == nil {
		return
	}
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(root), r.config.RequestTimeout)
	defer writeCancel()
	var err error
	switch result {
	case RegistryCredentialValidated:
		r.metrics.CredentialSaga("validated", time.Since(started))
		return
	case RegistryCredentialRetry:
		err = r.store.RetryRegistryCredentialOperation(writeCtx, operation, *operation.Lease, time.Second, "postgres_retryable")
	case RegistryCredentialFailed:
		err = r.store.FailRegistryCredentialOperation(writeCtx, operation, *operation.Lease, "credential_validation_failed")
	default:
		err = r.store.FailRegistryCredentialOperation(writeCtx, operation, *operation.Lease, "credential_validation_failed")
		result = RegistryCredentialFailed
	}
	r.metrics.CredentialSaga(string(result), time.Since(started))
	if err != nil && !errors.Is(err, store.ErrLeaseNotOwned) {
		r.logger.Warn("registry credential final write failed", "operation_id", operation.ID, "error", err, "process_error", processErr)
	}
}

func (r *RegistryCredentialRunner) awaitShutdown() error {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	timer := time.NewTimer(r.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		r.logger.Warn("registry credential worker shutdown", "result", "grace_exhausted")
		return nil
	}
}

// RegistryCredentialSource reads the immutable source Secret referenced by a
// persisted generation. Source writes happen at attach time before the worker
// leases the operation; the worker never writes or checkpoints source Secrets.
type RegistryCredentialSource interface {
	ReadCredential(context.Context, string, string, int64, string) (*oci.Credential, error)
}

type RegistryCredentialProcessorStore interface {
	GetRegistryCredentialGeneration(context.Context, string, string, int64) (store.RegistryCredentialGeneration, error)
	ClaimRegistryCredentialValidationTargets(context.Context, store.RegistryCredentialOperation, string, time.Duration, int) ([]store.RegistryCredentialValidationTarget, error)
	CompleteRegistryCredentialValidationTarget(context.Context, store.RegistryCredentialValidationTarget, store.LeaseToken) error
	RetryRegistryCredentialValidationTarget(context.Context, store.RegistryCredentialValidationTarget, store.LeaseToken, string, time.Duration) error
	FailRegistryCredentialValidationTarget(context.Context, store.RegistryCredentialValidationTarget, store.LeaseToken, string) error
	FinishRegistryCredentialValidation(context.Context, store.RegistryCredentialOperation, store.LeaseToken) (bool, error)
}

type RegistryCredentialProcessResult string

const (
	RegistryCredentialValidated RegistryCredentialProcessResult = "validated"
	RegistryCredentialRetry     RegistryCredentialProcessResult = "retry"
	RegistryCredentialFailed    RegistryCredentialProcessResult = "failed"
)

type RegistryCredentialProcessor struct {
	store       RegistryCredentialProcessorStore
	source      RegistryCredentialSource
	resolver    oci.Resolver
	owner       string
	targetLease time.Duration
}

func NewRegistryCredentialProcessor(dataStore RegistryCredentialProcessorStore, source RegistryCredentialSource, resolver oci.Resolver, owner string, targetLease time.Duration) (*RegistryCredentialProcessor, error) {
	if dataStore == nil || source == nil || resolver == nil || owner == "" || targetLease <= 0 {
		return nil, errors.New("invalid registry credential processor configuration")
	}
	return &RegistryCredentialProcessor{store: dataStore, source: source, resolver: resolver, owner: owner, targetLease: targetLease}, nil
}

// Process runs only against leased operations whose source Secret was already
// durably attached: source_secret_written while validation targets remain, or
// validation_complete once every target resolved and downstream reconciliation
// stages still run. awaiting_source operations never reach the worker.
// Credential material is always reconstructed from the persisted generation
// SecretRef, never retained in the database or accepted from the caller.
func (p *RegistryCredentialProcessor) Process(ctx context.Context, operation store.RegistryCredentialOperation) (RegistryCredentialProcessResult, error) {
	if operation.Lease == nil {
		return RegistryCredentialRetry, store.ErrLeaseNotOwned
	}
	if operation.Checkpoint != "source_secret_written" && operation.Checkpoint != "validation_complete" {
		return RegistryCredentialRetry, errors.New("registry credential operation checkpoint is not processable by the validation worker")
	}
	generation, err := p.store.GetRegistryCredentialGeneration(ctx, operation.OrgID, operation.RegistryID, operation.CandidateGeneration)
	if err != nil {
		return RegistryCredentialRetry, err
	}
	credential, err := p.source.ReadCredential(ctx, operation.OrgID, operation.RegistryID, operation.CandidateGeneration, generation.SecretRef)
	if err != nil {
		if errors.Is(err, kubernetes.ErrRegistryCredentialInvalid) || errors.Is(err, kubernetes.ErrRegistryCredentialNotFound) {
			return RegistryCredentialFailed, errors.New("source registry credential Secret is malformed or missing")
		}
		return RegistryCredentialRetry, err
	}
	defer credential.Cleanup()
	targets, err := p.store.ClaimRegistryCredentialValidationTargets(ctx, operation, p.owner, p.targetLease, 1000)
	if err != nil {
		return RegistryCredentialRetry, err
	}
	permanent := false
	for _, target := range targets {
		if target.Lease == nil {
			return RegistryCredentialRetry, store.ErrLeaseNotOwned
		}
		err = p.resolver.ValidatePull(ctx, oci.PullValidationRequest{Host: oci.GHCRHost, Repository: target.Repository, Reference: target.Reference, Credential: credential})
		if err == nil {
			err = p.store.CompleteRegistryCredentialValidationTarget(ctx, target, *target.Lease)
		} else {
			disposition := oci.Classify(err, time.Now())
			if disposition.Retryable && target.Attempts < maxRegistryCredentialAttempts {
				delay := disposition.RetryAfter
				if delay <= 0 {
					delay = time.Second
				}
				err = p.store.RetryRegistryCredentialValidationTarget(ctx, target, *target.Lease, disposition.Code, delay)
			} else {
				err = p.store.FailRegistryCredentialValidationTarget(ctx, target, *target.Lease, disposition.Code)
				permanent = true
			}
		}
		if err != nil {
			return RegistryCredentialRetry, err
		}
	}
	complete, err := p.store.FinishRegistryCredentialValidation(ctx, operation, *operation.Lease)
	if err != nil {
		// A permanent failure observed in this call already terminalized the
		// operation through Finish; the failed target remains durable evidence,
		// so a lease-loss or stale-checkpoint error here is not retryable.
		if permanent {
			return RegistryCredentialFailed, errors.New("registry credential validation failed permanently")
		}
		return RegistryCredentialRetry, err
	}
	if complete {
		return RegistryCredentialValidated, nil
	}
	if permanent {
		// Finish terminalized the operation because a target was failed.
		return RegistryCredentialFailed, errors.New("registry credential validation failed permanently")
	}
	// Finish returned incomplete without error and no target was failed in this
	// call. Either targets legitimately remain pending, or a restart is
	// re-processing an operation that a prior attempt already terminalized;
	// the latter must not spin in Retry.
	return RegistryCredentialFailed, errors.New("registry credential validation did not complete without a recorded failure")
}
