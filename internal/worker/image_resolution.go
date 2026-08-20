package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/ocimetrics"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type ImageResolutionStore interface {
	RecoverExpiredImageResolutionLeases(context.Context, int) (int64, error)
	ListImageResolutionOrgIDs(context.Context, int) ([]string, error)
	ClaimImageResolutions(context.Context, string, string, time.Duration, time.Duration, []string, int) ([]store.ImageResolutionClaim, error)
	RenewImageResolutionLease(context.Context, string, string, store.LeaseToken, time.Duration) (store.LeaseToken, error)
	RotateImageResolutionLease(context.Context, string, string, store.LeaseToken, time.Duration) (store.LeaseToken, error)
	RetryImageResolution(context.Context, store.RetryImageResolutionParams) (store.ImageResolutionWriteResult, error)
	CompleteImageResolution(context.Context, store.CompleteImageResolutionParams) (store.ImageResolutionWriteResult, error)
	FailImageResolution(context.Context, store.FailImageResolutionParams) (store.ImageResolutionWriteResult, error)
}

type ImageResolutionProcessor interface {
	Resolve(context.Context, store.ImageResolutionClaim) service.ImageResolutionOutcome
}

type ImageResolutionConfig struct {
	ScanInterval, LeaseDuration, PublicationDeadline, RequestTimeout, ShutdownGrace time.Duration
	Concurrency                                                                     int
	Owner                                                                           string
}

type ImageResolutionRunner struct {
	config    ImageResolutionConfig
	logger    *slog.Logger
	store     ImageResolutionStore
	processor ImageResolutionProcessor
	metrics   ocimetrics.Recorder
	now       func() time.Time
	ready     atomic.Bool
	sem       chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	scanMu    sync.Mutex
	inFlight  map[string]struct{}
	process   context.Context
	cancel    context.CancelFunc
}

func NewImageResolutionRunner(config ImageResolutionConfig, logger *slog.Logger, dataStore ImageResolutionStore, resolver oci.Resolver, credentials service.CredentialReader, recorders ...ocimetrics.Recorder) *ImageResolutionRunner {
	processor, _ := service.NewImageResolver(resolver, credentials)
	return newImageResolutionRunner(config, logger, dataStore, processor, recorders...)
}

func newImageResolutionRunner(config ImageResolutionConfig, logger *slog.Logger, dataStore ImageResolutionStore, processor ImageResolutionProcessor, recorders ...ocimetrics.Recorder) *ImageResolutionRunner {
	recorder := ocimetrics.Noop()
	if len(recorders) > 0 && recorders[0] != nil {
		recorder = recorders[0]
	}
	return &ImageResolutionRunner{config: config, logger: logger, store: dataStore, processor: processor,
		metrics: recorder, now: time.Now, sem: make(chan struct{}, config.Concurrency), inFlight: make(map[string]struct{})}
}

func (r *ImageResolutionRunner) Ready() bool { return r.ready.Load() }

func (r *ImageResolutionRunner) Startup(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	r.process, r.cancel = context.WithCancel(context.WithoutCancel(ctx))
	if _, err := r.store.RecoverExpiredImageResolutionLeases(ctx, 1000); err != nil {
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

func (r *ImageResolutionRunner) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.cancel == nil {
		return errors.New("image resolution worker Startup must run before Run")
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
				r.logger.Error("image resolution scan failed", "error", err)
			}
		}
	}
}

func (r *ImageResolutionRunner) validate() error {
	c := r.config
	if r.store == nil || r.processor == nil || r.logger == nil || c.Owner == "" || c.ScanInterval <= 0 ||
		c.LeaseDuration <= 0 || c.PublicationDeadline <= 0 || c.RequestTimeout <= 0 || c.RequestTimeout >= c.LeaseDuration ||
		c.ShutdownGrace <= 0 || c.Concurrency < 1 || c.Concurrency > 100 {
		return errors.New("invalid image resolution worker configuration")
	}
	return nil
}

func (r *ImageResolutionRunner) scan(ctx context.Context) error {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	backlog := int64(0)
	defer func() { r.metrics.Work("image_resolution", int64(len(r.sem)), backlog) }()
	if _, err := r.store.RecoverExpiredImageResolutionLeases(ctx, 1000); err != nil {
		return err
	}
	orgIDs, err := r.store.ListImageResolutionOrgIDs(ctx, 1000)
	if err != nil {
		return err
	}
	for _, orgID := range orgIDs {
		for {
			slots := cap(r.sem) - len(r.sem)
			if slots == 0 {
				return nil
			}
			excluded := r.inFlightEnvironmentIDs()
			claims, err := r.store.ClaimImageResolutions(ctx, orgID, r.config.Owner, r.config.LeaseDuration, r.config.PublicationDeadline, excluded, slots)
			if err != nil {
				return err
			}
			if len(claims) == 0 {
				break
			}
			backlog += int64(len(claims))
			for _, claim := range claims {
				r.markInFlight(claim.Environment.ID)
				r.sem <- struct{}{}
				r.wg.Add(1)
				go r.processClaim(r.process, claim)
			}
			if len(claims) < slots {
				break
			}
		}
	}
	return nil
}

func (r *ImageResolutionRunner) processClaim(root context.Context, claim store.ImageResolutionClaim) {
	started := time.Now()
	defer func() {
		<-r.sem
		r.clearInFlight(claim.Environment.ID)
		r.metrics.Work("image_resolution", int64(len(r.sem)), 0)
		r.wg.Done()
	}()
	ctx, cancel := context.WithTimeout(root, r.config.RequestTimeout)
	lost := &atomic.Bool{}
	done, stopped := make(chan struct{}), make(chan struct{})
	go r.renew(ctx, cancel, done, stopped, lost, claim)
	outcome := r.processor.Resolve(ctx, claim)
	close(done)
	<-stopped
	cancel()
	if lost.Load() || outcome.Disposition == service.ImageResolutionLeaseLost || root.Err() != nil {
		if root.Err() == nil {
			r.metrics.ImageResolution("lease_lost", time.Since(started))
		}
		return
	}
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(root), r.config.RequestTimeout)
	defer writeCancel()
	lease, err := r.store.RotateImageResolutionLease(writeCtx, claim.Job.OrgID, claim.Job.ID, claim.Lease, r.config.LeaseDuration)
	if err != nil {
		if errors.Is(err, store.ErrLeaseNotOwned) {
			r.metrics.ImageResolution("lease_lost", time.Since(started))
		} else {
			r.metrics.ImageResolution("failed", time.Since(started))
		}
		return
	}
	claim.Lease = lease
	var result store.ImageResolutionWriteResult
	metricResult := "failed"
	switch outcome.Disposition {
	case service.ImageResolutionComplete:
		metricResult = "complete"
		result, err = r.store.CompleteImageResolution(writeCtx, service.ImageResolutionCompleteParams(claim, outcome.Image))
	case service.ImageResolutionRetry:
		remaining := claim.Deadline.Sub(r.now())
		delay := oci.RetryDelay(claim.Job.OCI.Attempts, outcome.RetryAfter, remaining, rand.Reader)
		if delay <= 0 || !r.now().Add(delay).Before(claim.Deadline) {
			result, err = r.store.FailImageResolution(writeCtx, store.FailImageResolutionParams{Claim: claim, Lease: lease, Code: string(oci.CodeImageNotPublished)})
		} else {
			metricResult = "retry"
			result, err = r.store.RetryImageResolution(writeCtx, store.RetryImageResolutionParams{Claim: claim, Lease: lease, Delay: delay, Code: service.SanitizeImageResolutionCode(outcome.Code)})
		}
	case service.ImageResolutionSuperseded:
		metricResult = "superseded"
		result, err = r.store.RetryImageResolution(writeCtx, store.RetryImageResolutionParams{Claim: claim, Lease: lease, Code: string(oci.CodeRegistryUnavailable)})
	case service.ImageResolutionFail:
		result, err = r.store.FailImageResolution(writeCtx, store.FailImageResolutionParams{Claim: claim, Lease: lease, Code: service.SanitizeImageResolutionCode(outcome.Code)})
	default:
		result, err = r.store.FailImageResolution(writeCtx, store.FailImageResolutionParams{Claim: claim, Lease: lease, Code: string(oci.CodeRegistryResponseInvalid)})
	}
	_ = result
	if errors.Is(err, store.ErrLeaseNotOwned) {
		metricResult = "lease_lost"
	} else if err != nil {
		metricResult = "failed"
	}
	r.metrics.ImageResolution(metricResult, time.Since(started))
	if err != nil && !errors.Is(err, store.ErrLeaseNotOwned) {
		r.logger.Warn("image resolution final write failed", "job_id", claim.Job.ID, "error", err)
	}
}

func (r *ImageResolutionRunner) renew(ctx context.Context, cancel context.CancelFunc, done <-chan struct{}, stopped chan<- struct{}, lost *atomic.Bool, claim store.ImageResolutionClaim) {
	defer close(stopped)
	ticker := time.NewTicker(r.config.LeaseDuration / 3)
	defer ticker.Stop()
	token := claim.Lease
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			renewed, err := r.store.RenewImageResolutionLease(ctx, claim.Job.OrgID, claim.Job.ID, token, r.config.LeaseDuration)
			if err != nil {
				lost.Store(true)
				cancel()
				return
			}
			token = renewed
		}
	}
}

func (r *ImageResolutionRunner) markInFlight(environmentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight[environmentID] = struct{}{}
}

func (r *ImageResolutionRunner) inFlightEnvironmentIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.inFlight))
	for id := range r.inFlight {
		ids = append(ids, id)
	}
	return ids
}

func (r *ImageResolutionRunner) clearInFlight(environmentID string) {
	r.mu.Lock()
	delete(r.inFlight, environmentID)
	r.mu.Unlock()
}

func (r *ImageResolutionRunner) awaitShutdown() error {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	timer := time.NewTimer(r.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		r.logger.Warn("image resolution worker shutdown", "result", "grace_exhausted")
		return nil
	}
}
