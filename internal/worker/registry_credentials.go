package worker

import (
	"context"
	"errors"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/kubernetes"
	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const maxRegistryCredentialAttempts = 12

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
