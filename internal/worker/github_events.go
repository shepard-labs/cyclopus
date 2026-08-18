package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shepard-labs/cyclopus/internal/githubmetrics"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type GitHubEventStore interface {
	CountUnexpiredGitHubEventLeases(context.Context) (store.GitHubEventLeaseStatus, error)
	FailExpiredExhaustedGitHubEventLeases(context.Context, time.Duration, int) (store.GitHubEventLeaseStatus, error)
	ClaimGitHubWebhookDeliveries(context.Context, string, time.Duration, int) ([]store.GitHubWebhookDelivery, error)
	RenewGitHubWebhookDelivery(context.Context, string, store.LeaseToken, time.Duration) (store.LeaseToken, error)
	RotateGitHubWebhookDeliveryLease(context.Context, string, store.LeaseToken, time.Duration) (store.LeaseToken, error)
	RetryGitHubWebhookDelivery(context.Context, string, store.LeaseToken, time.Duration, time.Duration, string, string) error
	CompleteGitHubWebhookDelivery(context.Context, string, store.LeaseToken, string, time.Duration) error
	FailGitHubWebhookDelivery(context.Context, string, store.LeaseToken, string, string, time.Duration) error
	ClaimGitHubRoutingIntents(context.Context, string, time.Duration, int) ([]store.GitHubRoutingIntent, error)
	RenewGitHubRoutingIntent(context.Context, string, store.LeaseToken, time.Duration) (store.LeaseToken, error)
	RotateGitHubRoutingIntentLease(context.Context, string, store.LeaseToken, time.Duration) (store.LeaseToken, error)
	RetryGitHubRoutingIntent(context.Context, string, store.LeaseToken, time.Duration, string, string) error
	FailGitHubRoutingIntent(context.Context, string, store.LeaseToken, string, string) error
	RedactTerminalGitHubWebhookPayloads(context.Context, int) (int64, error)
}

type GitHubEventProcessor interface {
	ProcessDelivery(context.Context, store.GitHubWebhookDelivery) service.GitHubEventOutcome
	ProcessIntent(context.Context, store.GitHubRoutingIntent) service.GitHubEventOutcome
}

type GitHubEventConfig struct {
	ScanInterval, LeaseDuration, ProcessingTimeout, InitialRetry, MaxRetry time.Duration
	PayloadRetention, CleanupInterval, ShutdownGrace                       time.Duration
	DeliveryConcurrency, IntentConcurrency, MaxAttempts                    int
	CleanupBatch, MaxIntents                                               int
	Owner                                                                  string
}

type GitHubEventRunner struct {
	config          GitHubEventConfig
	logger          *slog.Logger
	store           GitHubEventStore
	processor       GitHubEventProcessor
	metrics         githubmetrics.Recorder
	jitter          func() float64
	now             func() time.Time
	ready           atomic.Bool
	writeOK         atomic.Bool
	delayedDelivery atomic.Int64
	delayedIntent   atomic.Int64
	wg              sync.WaitGroup
	writeWG         sync.WaitGroup
	delivery        chan struct{}
	intent          chan struct{}
	mu              sync.Mutex
	processCtx      context.Context
	processCancel   context.CancelFunc
	writeMu         sync.Mutex
	writeCtx        context.Context
	writeCancel     context.CancelFunc
	lastClean       time.Time
}

func NewGitHubEventRunner(config GitHubEventConfig, logger *slog.Logger, dataStore GitHubEventStore, processor GitHubEventProcessor, recorders ...githubmetrics.Recorder) *GitHubEventRunner {
	metrics := githubmetrics.Noop()
	if len(recorders) > 0 && recorders[0] != nil {
		metrics = recorders[0]
	}
	return &GitHubEventRunner{config: config, logger: logger, store: dataStore, processor: processor, jitter: rand.Float64, now: time.Now,
		metrics: metrics, delivery: make(chan struct{}, config.DeliveryConcurrency), intent: make(chan struct{}, config.IntentConcurrency)}
}

func (r *GitHubEventRunner) Ready() bool { return r.ready.Load() }

func (r *GitHubEventRunner) Startup(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	root := context.WithoutCancel(ctx)
	processCtx, processCancel := context.WithCancel(root)
	writeCtx, writeCancel := context.WithCancel(root)
	r.mu.Lock()
	r.processCtx = processCtx
	r.processCancel = processCancel
	r.mu.Unlock()
	r.writeMu.Lock()
	r.writeCtx = writeCtx
	r.writeCancel = writeCancel
	r.writeOK.Store(true)
	r.writeMu.Unlock()
	err := r.scan(ctx, processCtx, true)
	if err != nil {
		r.wg.Wait()
		processCancel()
		r.stopWrites()
		r.writeWG.Wait()
		return err
	}
	r.ready.Store(true)
	return nil
}

func (r *GitHubEventRunner) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	processCtx := r.processCtx
	processCancel := r.processCancel
	r.mu.Unlock()
	if processCancel == nil {
		return errors.New("GitHub event worker Startup must run before Run")
	}
	ticker := time.NewTicker(r.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.ready.Store(false)
			processCancel()
			return r.awaitShutdown()
		case <-ticker.C:
			if err := r.scan(ctx, processCtx, false); err != nil && ctx.Err() == nil {
				r.logger.Error("github event scan failed", "error", err)
			}
		}
	}
}

func (r *GitHubEventRunner) validate() error {
	c := r.config
	if r.store == nil || r.processor == nil || r.logger == nil || r.now == nil || c.Owner == "" || c.ScanInterval <= 0 || c.LeaseDuration <= 0 || c.ProcessingTimeout <= 0 || c.ProcessingTimeout >= c.LeaseDuration || c.InitialRetry <= 0 || c.InitialRetry > c.MaxRetry || c.MaxRetry > 15*time.Minute || c.PayloadRetention < 0 || c.PayloadRetention > 30*24*time.Hour || c.CleanupInterval <= 0 || c.ShutdownGrace <= 0 || c.DeliveryConcurrency < 1 || c.DeliveryConcurrency > 100 || c.IntentConcurrency < 1 || c.IntentConcurrency > 100 || c.MaxAttempts != 12 || c.CleanupBatch < 1 || c.CleanupBatch > 1000 || c.MaxIntents < 1 || c.MaxIntents > 10000 {
		return errors.New("invalid GitHub event worker configuration")
	}
	return nil
}

type githubEventWork struct {
	deliveries []store.GitHubWebhookDelivery
	intents    []store.GitHubRoutingIntent
}

func (r *GitHubEventRunner) claimScan(claimCtx context.Context, startup bool) (githubEventWork, error) {
	var work githubEventWork
	if startup || r.lastClean.IsZero() || r.now().Sub(r.lastClean) >= r.config.CleanupInterval {
		redacted, err := r.store.RedactTerminalGitHubWebhookPayloads(claimCtx, r.config.CleanupBatch)
		if err != nil {
			return work, fmt.Errorf("redact terminal GitHub webhook payloads: %w", err)
		}
		r.lastClean = r.now()
		if redacted > 0 {
			r.logger.Info("github event payload cleanup", "result", "redacted", "count", redacted)
		}
	}
	exhausted, err := r.store.FailExpiredExhaustedGitHubEventLeases(claimCtx, r.config.PayloadRetention, r.config.CleanupBatch)
	if err != nil {
		return work, fmt.Errorf("fail expired exhausted GitHub event leases: %w", err)
	}
	if exhausted.Deliveries > 0 || exhausted.Intents > 0 {
		r.logger.Info("github event exhausted lease sweep", "deliveries", exhausted.Deliveries, "intents", exhausted.Intents)
	}
	deliverySlots := cap(r.delivery) - len(r.delivery)
	var claimErr error
	if deliverySlots > 0 {
		deliveries, err := r.store.ClaimGitHubWebhookDeliveries(claimCtx, r.config.Owner, r.config.LeaseDuration, deliverySlots)
		if err != nil {
			claimErr = errors.Join(claimErr, fmt.Errorf("claim GitHub webhook deliveries: %w", err))
		} else {
			work.deliveries = deliveries
			if startup {
				r.logger.Info("github event startup scan", "kind", "delivery", "result", "claimed", "count", len(deliveries))
			}
		}
	}
	intentSlots := cap(r.intent) - len(r.intent)
	if intentSlots > 0 {
		intents, err := r.store.ClaimGitHubRoutingIntents(claimCtx, r.config.Owner, r.config.LeaseDuration, intentSlots)
		if err != nil {
			claimErr = errors.Join(claimErr, fmt.Errorf("claim GitHub routing intents: %w", err))
		} else {
			work.intents = intents
			if startup {
				r.logger.Info("github event startup scan", "kind", "intent", "result", "claimed", "count", len(intents))
			}
		}
	}
	return work, claimErr
}

func (r *GitHubEventRunner) scan(claimCtx, processCtx context.Context, startup bool) error {
	work, err := r.claimScan(claimCtx, startup)
	r.launch(processCtx, work)
	status, countErr := r.store.CountUnexpiredGitHubEventLeases(claimCtx)
	if countErr != nil {
		return errors.Join(err, fmt.Errorf("inspect unexpired GitHub event leases: %w", countErr))
	}
	activeDeliveries, activeIntents := int64(len(r.delivery)), int64(len(r.intent))
	delayedDeliveries := max(status.Deliveries-activeDeliveries, 0)
	delayedIntents := max(status.Intents-activeIntents, 0)
	r.delayedDelivery.Store(delayedDeliveries)
	r.delayedIntent.Store(delayedIntents)
	r.updateLeaseMetrics()
	if startup && (delayedDeliveries > 0 || delayedIntents > 0) {
		r.logger.Info("github event startup delayed by active leases", "result", "delayed", "deliveries", delayedDeliveries, "intents", delayedIntents)
	}
	return err
}

func (r *GitHubEventRunner) launch(processCtx context.Context, work githubEventWork) {
	for _, delivery := range work.deliveries {
		r.delivery <- struct{}{}
		r.wg.Add(1)
		go r.processDelivery(processCtx, delivery)
	}
	for _, intent := range work.intents {
		r.intent <- struct{}{}
		r.wg.Add(1)
		go r.processIntent(processCtx, intent)
	}
	r.updateLeaseMetrics()
}

func (r *GitHubEventRunner) processDelivery(root context.Context, delivery store.GitHubWebhookDelivery) {
	defer func() { <-r.delivery; r.updateLeaseMetrics(); r.wg.Done() }()
	started := r.now()
	ctx, cancel := context.WithTimeout(root, r.config.ProcessingTimeout)
	defer cancel()
	lost := &atomic.Bool{}
	done := make(chan struct{})
	renewedDone := make(chan struct{})
	go r.renewDelivery(ctx, cancel, done, renewedDone, lost, delivery)
	outcome := r.processor.ProcessDelivery(ctx, delivery)
	close(done)
	<-renewedDone
	if lost.Load() {
		r.metrics.Delivery(delivery.Event, delivery.Action, "lease_lost", r.now().Sub(started), outcome.IntentCount)
		r.logDelivery(delivery, "lease_lost", outcome.IntentCount, started)
		return
	}
	writeCtx, writeCancel, ok := r.beginWrite()
	if !ok {
		r.metrics.Delivery(delivery.Event, delivery.Action, "failed", r.now().Sub(started), outcome.IntentCount)
		r.logDelivery(delivery, "shutdown_expired", outcome.IntentCount, started)
		return
	}
	defer r.endWrite(writeCancel)
	lease, err := r.store.RotateGitHubWebhookDeliveryLease(writeCtx, delivery.ID, *delivery.Lease, r.config.LeaseDuration)
	if err != nil {
		result, logResult := "failed", "database_unavailable"
		if errors.Is(err, store.ErrLeaseNotOwned) {
			result, logResult = "lease_lost", "lease_lost"
		}
		r.metrics.Delivery(delivery.Event, delivery.Action, result, r.now().Sub(started), outcome.IntentCount)
		r.logDelivery(delivery, logResult, outcome.IntentCount, started)
		return
	}
	disposition := outcome.Disposition
	switch outcome.Disposition {
	case service.GitHubEventComplete:
		result := outcome.Code
		if result == "permission_revoked" || result == "superseded" {
			result = "ignored"
		}
		err = r.store.CompleteGitHubWebhookDelivery(writeCtx, delivery.ID, lease, result, r.config.PayloadRetention)
	case service.GitHubEventFail:
		err = r.store.FailGitHubWebhookDelivery(writeCtx, delivery.ID, lease, outcome.Code, outcome.Code, r.config.PayloadRetention)
	case service.GitHubEventRetry:
		err = r.store.RetryGitHubWebhookDelivery(writeCtx, delivery.ID, lease, r.retryDelay(delivery.Attempts, outcome.RetryAfter), r.config.PayloadRetention, outcome.Code, outcome.Code)
	default:
		err = r.store.FailGitHubWebhookDelivery(writeCtx, delivery.ID, lease, "internal", "internal", r.config.PayloadRetention)
	}
	result := outcome.Code
	if errors.Is(err, store.ErrLeaseNotOwned) {
		result = "lease_lost"
	} else if err != nil {
		result = "database_unavailable"
	}
	r.metrics.Delivery(delivery.Event, delivery.Action, deliveryMetricResult(disposition, result), r.now().Sub(started), outcome.IntentCount)
	r.logDelivery(delivery, result, outcome.IntentCount, started)
}

func (r *GitHubEventRunner) processIntent(root context.Context, intent store.GitHubRoutingIntent) {
	defer func() { <-r.intent; r.updateLeaseMetrics(); r.wg.Done() }()
	started := r.now()
	ctx, cancel := context.WithTimeout(root, r.config.ProcessingTimeout)
	defer cancel()
	lost := &atomic.Bool{}
	done := make(chan struct{})
	renewedDone := make(chan struct{})
	go r.renewIntent(ctx, cancel, done, renewedDone, lost, intent)
	outcome := r.processor.ProcessIntent(ctx, intent)
	close(done)
	<-renewedDone
	if lost.Load() && outcome.Disposition != service.GitHubEventComplete {
		r.metrics.Intent(intent.EventKind, "lease_lost", r.now().Sub(started))
		r.logIntent(intent, "lease_lost", started)
		return
	}
	if outcome.Disposition == service.GitHubEventComplete {
		// ExecuteGitHubRoutingIntent atomically applies the side effect and terminalizes the intent.
		r.metrics.Intent(intent.EventKind, intentMetricResult(outcome.Disposition, outcome.Code), r.now().Sub(started))
		r.logIntent(intent, outcome.Code, started)
		return
	}
	writeCtx, writeCancel, ok := r.beginWrite()
	if !ok {
		r.metrics.Intent(intent.EventKind, "failed", r.now().Sub(started))
		r.logIntent(intent, "shutdown_expired", started)
		return
	}
	defer r.endWrite(writeCancel)
	lease, err := r.store.RotateGitHubRoutingIntentLease(writeCtx, intent.ID, *intent.Lease, r.config.LeaseDuration)
	if err != nil {
		result, logResult := "failed", "database_unavailable"
		if errors.Is(err, store.ErrLeaseNotOwned) {
			result, logResult = "lease_lost", "lease_lost"
		}
		r.metrics.Intent(intent.EventKind, result, r.now().Sub(started))
		r.logIntent(intent, logResult, started)
		return
	}
	disposition := outcome.Disposition
	switch outcome.Disposition {
	case service.GitHubEventFail:
		err = r.store.FailGitHubRoutingIntent(writeCtx, intent.ID, lease, outcome.Code, outcome.Code)
	case service.GitHubEventRetry:
		err = r.store.RetryGitHubRoutingIntent(writeCtx, intent.ID, lease, r.retryDelay(intent.Attempts, outcome.RetryAfter), outcome.Code, outcome.Code)
	default:
		err = r.store.FailGitHubRoutingIntent(writeCtx, intent.ID, lease, "internal", "internal")
	}
	result := outcome.Code
	if errors.Is(err, store.ErrLeaseNotOwned) {
		result = "lease_lost"
	} else if err != nil {
		result = "database_unavailable"
	}
	r.metrics.Intent(intent.EventKind, intentMetricResult(disposition, result), r.now().Sub(started))
	r.logIntent(intent, result, started)
}

func (r *GitHubEventRunner) renewDelivery(ctx context.Context, cancel context.CancelFunc, done <-chan struct{}, stopped chan<- struct{}, lost *atomic.Bool, delivery store.GitHubWebhookDelivery) {
	defer close(stopped)
	ticker := time.NewTicker(r.config.LeaseDuration / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if _, err := r.store.RenewGitHubWebhookDelivery(ctx, delivery.ID, *delivery.Lease, r.config.LeaseDuration); err != nil {
				lost.Store(true)
				cancel()
				return
			}
		}
	}
}

func (r *GitHubEventRunner) renewIntent(ctx context.Context, cancel context.CancelFunc, done <-chan struct{}, stopped chan<- struct{}, lost *atomic.Bool, intent store.GitHubRoutingIntent) {
	defer close(stopped)
	ticker := time.NewTicker(r.config.LeaseDuration / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if _, err := r.store.RenewGitHubRoutingIntent(ctx, intent.ID, *intent.Lease, r.config.LeaseDuration); err != nil {
				lost.Store(true)
				cancel()
				return
			}
		}
	}
}

func (r *GitHubEventRunner) retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	power := math.Pow(2, float64(max(attempt-1, 0)))
	delay := time.Duration(float64(r.config.InitialRetry) * power)
	if delay > r.config.MaxRetry {
		delay = r.config.MaxRetry
	}
	delay = time.Duration(float64(delay) * (0.5 + r.jitter()))
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > r.config.MaxRetry {
		return r.config.MaxRetry
	}
	return delay
}

func (r *GitHubEventRunner) awaitShutdown() error {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	timer := time.NewTimer(r.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		r.stopWrites()
		r.writeWG.Wait()
		return nil
	case <-timer.C:
		r.stopWrites()
		r.writeWG.Wait()
		r.logger.Warn("github event worker shutdown", "result", "grace_exhausted")
		return nil
	}
}

func (r *GitHubEventRunner) beginWrite() (context.Context, context.CancelFunc, bool) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if !r.writeOK.Load() || r.writeCtx == nil {
		return nil, nil, false
	}
	r.writeWG.Add(1)
	ctx, cancel := context.WithTimeout(r.writeCtx, r.config.ProcessingTimeout)
	return ctx, cancel, true
}

func (r *GitHubEventRunner) endWrite(cancel context.CancelFunc) {
	cancel()
	r.writeWG.Done()
}

func (r *GitHubEventRunner) stopWrites() {
	r.writeMu.Lock()
	r.writeOK.Store(false)
	if r.writeCancel != nil {
		r.writeCancel()
	}
	r.writeMu.Unlock()
}

func (r *GitHubEventRunner) logDelivery(d store.GitHubWebhookDelivery, result string, fanout int, started time.Time) {
	attrs := []any{"delivery_id", d.DeliveryID, "event", d.Event, "action", d.Action, "result", result, "attempt", d.Attempts, "latency_ms", r.now().Sub(started).Milliseconds(), "fanout", fanout}
	if d.InstallationID != nil {
		attrs = append(attrs, "installation_id", *d.InstallationID)
	}
	if d.RepositoryID != nil {
		attrs = append(attrs, "repository_id", *d.RepositoryID)
	}
	r.logger.Info("github webhook delivery processed", attrs...)
}

func (r *GitHubEventRunner) logIntent(i store.GitHubRoutingIntent, result string, started time.Time) {
	r.logger.Info("github routing intent processed", "intent_id", i.ID, "app_id", i.AppID, "ref", i.Ref, "event", i.EventKind, "result", result, "attempt", i.Attempts, "latency_ms", r.now().Sub(started).Milliseconds())
}

func deliveryMetricResult(disposition service.GitHubEventDisposition, result string) string {
	if result == "lease_lost" {
		return "lease_lost"
	}
	switch disposition {
	case service.GitHubEventComplete:
		return "processed"
	case service.GitHubEventRetry:
		return "retry"
	default:
		return "failed"
	}
}

func intentMetricResult(disposition service.GitHubEventDisposition, result string) string {
	if result == "lease_lost" {
		return "lease_lost"
	}
	if result == "superseded" {
		return "superseded"
	}
	switch disposition {
	case service.GitHubEventComplete:
		return "processed"
	case service.GitHubEventRetry:
		return "retry"
	default:
		return "failed"
	}
}

func (r *GitHubEventRunner) updateLeaseMetrics() {
	r.metrics.Leases(int64(len(r.delivery)), r.delayedDelivery.Load(), int64(len(r.intent)), r.delayedIntent.Load())
}
