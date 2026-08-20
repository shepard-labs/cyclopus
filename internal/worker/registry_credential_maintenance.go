package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
)

type RegistryCredentialMaintenanceStore interface {
	RescueAwaitingRegistryCredentialSources(context.Context, time.Time, int) (int64, error)
	ListRegistryCredentialGarbageCollectionCandidates(context.Context, time.Time, int) ([]store.RegistryCredentialGarbageCollectionCandidate, error)
	GarbageCollectRegistryCredential(context.Context, store.RegistryCredentialGarbageCollectionCandidate, time.Time, store.RegistryCredentialGarbageCollectionDelete) (bool, error)
}

type RegistryCredentialMaintenanceSource interface {
	DeleteCredential(context.Context, string, string, string, int64, bool) error
}

type RegistryCredentialMaintenanceConfig struct {
	ScanInterval, ShutdownGrace, RetiredRetention, SourceRescueTimeout time.Duration
}

type RegistryCredentialMaintenanceRunner struct {
	config RegistryCredentialMaintenanceConfig
	logger *slog.Logger
	store  RegistryCredentialMaintenanceStore
	source RegistryCredentialMaintenanceSource
}

func NewRegistryCredentialMaintenanceRunner(config RegistryCredentialMaintenanceConfig, logger *slog.Logger, dataStore RegistryCredentialMaintenanceStore, source RegistryCredentialMaintenanceSource) *RegistryCredentialMaintenanceRunner {
	return &RegistryCredentialMaintenanceRunner{config: config, logger: logger, store: dataStore, source: source}
}

func (r *RegistryCredentialMaintenanceRunner) Startup(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	scanCtx, cancel := context.WithTimeout(ctx, r.config.ShutdownGrace)
	defer cancel()
	return r.scan(scanCtx)
}

func (r *RegistryCredentialMaintenanceRunner) Run(ctx context.Context) error {
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
			scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.config.ShutdownGrace)
			err := r.scan(scanCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				r.logger.Error("registry credential maintenance failed", "error", err)
			}
		}
	}
}

func (r *RegistryCredentialMaintenanceRunner) validate() error {
	if r.logger == nil || r.store == nil || r.source == nil || r.config.ScanInterval <= 0 || r.config.ShutdownGrace <= 0 || r.config.RetiredRetention <= 0 || r.config.SourceRescueTimeout <= 0 {
		return errors.New("invalid registry credential maintenance configuration")
	}
	return nil
}

func (r *RegistryCredentialMaintenanceRunner) scan(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	now := time.Now()
	if _, err := r.store.RescueAwaitingRegistryCredentialSources(ctx, now.Add(-r.config.SourceRescueTimeout), 100); err != nil {
		return err
	}
	cutoff := now.Add(-r.config.RetiredRetention)
	candidates, err := r.store.ListRegistryCredentialGarbageCollectionCandidates(ctx, cutoff, 100)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		_, err = r.store.GarbageCollectRegistryCredential(ctx, candidate, cutoff, func(deleteCtx context.Context, exact store.RegistryCredentialGarbageCollectionCandidate) error {
			return r.source.DeleteCredential(deleteCtx, exact.OrgID, exact.RegistryID, exact.OperationID, exact.Generation, true)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
