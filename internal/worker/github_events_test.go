package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestGitHubEventStartupScansBeforeReadinessAndReportsActiveLeases(t *testing.T) {
	var logs bytes.Buffer
	fake := &fakeGitHubEventStore{leaseStatus: store.GitHubEventLeaseStatus{Deliveries: 2, Intents: 3}}
	runner := newGitHubEventTestRunner(fake, &fakeGitHubEventProcessor{}, slog.New(slog.NewJSONHandler(&logs, nil)))

	if err := runner.Startup(t.Context()); err != nil {
		t.Fatalf("Startup() error = %v", err)
	}
	if !runner.Ready() || fake.countCalls != 1 || fake.cleanupCalls != 1 || fake.deliveryClaimCalls != 1 || fake.intentClaimCalls != 1 {
		t.Fatalf("startup state = ready:%t counts:%d cleanup:%d delivery claims:%d intent claims:%d", runner.Ready(), fake.countCalls, fake.cleanupCalls, fake.deliveryClaimCalls, fake.intentClaimCalls)
	}
	text := logs.String()
	for _, want := range []string{`"result":"delayed"`, `"deliveries":2`, `"intents":3`, `"kind":"delivery"`, `"kind":"intent"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("startup logs %q do not contain %q", text, want)
		}
	}
	runner.wg.Wait()
}

func TestGitHubEventStartupFailureNeverBecomesReady(t *testing.T) {
	want := errors.New("database unavailable")
	for _, test := range []struct {
		name string
		fake *fakeGitHubEventStore
	}{
		{name: "lease inspection", fake: &fakeGitHubEventStore{countErr: want}},
		{name: "startup cleanup", fake: &fakeGitHubEventStore{cleanupErr: want}},
		{name: "exhausted lease sweep", fake: &fakeGitHubEventStore{sweepErr: want}},
		{name: "delivery claim", fake: &fakeGitHubEventStore{deliveryClaimErr: want}},
		{name: "intent claim", fake: &fakeGitHubEventStore{intentClaimErr: want}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newGitHubEventTestRunner(test.fake, &fakeGitHubEventProcessor{}, discardLogger())
			err := runner.Startup(t.Context())
			if !errors.Is(err, want) || runner.Ready() {
				t.Fatalf("Startup() = %v, ready=%t, want database error and not ready", err, runner.Ready())
			}
		})
	}
}

func TestGitHubEventScanLaunchesPartialClaimsBeforeReturningError(t *testing.T) {
	want := errors.New("claim unavailable")
	for _, test := range []struct {
		name string
		fake *fakeGitHubEventStore
	}{
		{name: "delivery claimed before intent error", fake: &fakeGitHubEventStore{deliveries: []store.GitHubWebhookDelivery{leasedDelivery("delivery", 1)}, intentClaimErr: want}},
		{name: "intent claimed after delivery error", fake: &fakeGitHubEventStore{deliveryClaimErr: want, intents: []store.GitHubRoutingIntent{leasedIntent("intent", 1)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			processed := make(chan struct{}, 1)
			processor := &fakeGitHubEventProcessor{
				delivery: func(context.Context, store.GitHubWebhookDelivery) service.GitHubEventOutcome {
					processed <- struct{}{}
					return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
				},
				intent: func(context.Context, store.GitHubRoutingIntent) service.GitHubEventOutcome {
					processed <- struct{}{}
					return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
				},
			}
			runner := newGitHubEventTestRunner(test.fake, processor, discardLogger())
			enableTestWrites(runner)
			if err := runner.scan(t.Context(), context.Background(), false); !errors.Is(err, want) {
				t.Fatalf("scan() error = %v, want %v", err, want)
			}
			runner.wg.Wait()
			select {
			case <-processed:
			default:
				t.Fatal("successfully claimed work was not processed")
			}
		})
	}
}

func TestGitHubEventStartupIntentClaimFailureProcessesClaimedDelivery(t *testing.T) {
	want := errors.New("intent claim unavailable")
	processed := make(chan struct{}, 1)
	processor := &fakeGitHubEventProcessor{delivery: func(context.Context, store.GitHubWebhookDelivery) service.GitHubEventOutcome {
		processed <- struct{}{}
		return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
	}}
	fake := &fakeGitHubEventStore{deliveries: []store.GitHubWebhookDelivery{leasedDelivery("delivery", 1)}, intentClaimErr: want}
	runner := newGitHubEventTestRunner(fake, processor, discardLogger())
	if err := runner.Startup(t.Context()); !errors.Is(err, want) {
		t.Fatalf("Startup() error = %v, want %v", err, want)
	}
	select {
	case <-processed:
	default:
		t.Fatal("claimed delivery was not processed before startup returned")
	}
	if fake.deliveryCompleteCalls != 1 || runner.Ready() {
		t.Fatalf("failed startup writes=%d ready=%t", fake.terminalWrites(), runner.Ready())
	}
}

func TestGitHubEventStartupDeliveryClaimFailureProcessesClaimedIntent(t *testing.T) {
	want := errors.New("delivery claim unavailable")
	processed := make(chan struct{}, 1)
	processor := &fakeGitHubEventProcessor{intent: func(context.Context, store.GitHubRoutingIntent) service.GitHubEventOutcome {
		processed <- struct{}{}
		return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
	}}
	fake := &fakeGitHubEventStore{deliveryClaimErr: want, intents: []store.GitHubRoutingIntent{leasedIntent("intent", 1)}}
	runner := newGitHubEventTestRunner(fake, processor, discardLogger())
	if err := runner.Startup(t.Context()); !errors.Is(err, want) {
		t.Fatalf("Startup() error = %v, want %v", err, want)
	}
	select {
	case <-processed:
	default:
		t.Fatal("claimed intent was not processed before startup returned")
	}
	if fake.intentRetryCalls != 0 || fake.intentFailCalls != 0 || runner.Ready() {
		t.Fatalf("failed startup intent writes=%d/%d ready=%t", fake.intentRetryCalls, fake.intentFailCalls, runner.Ready())
	}
}

func TestGitHubEventRunnerRejectsInvalidConfiguration(t *testing.T) {
	valid := gitHubEventTestConfig()
	for _, test := range []struct {
		name   string
		mutate func(*GitHubEventConfig)
	}{
		{name: "missing owner", mutate: func(c *GitHubEventConfig) { c.Owner = "" }},
		{name: "processing equals lease", mutate: func(c *GitHubEventConfig) { c.ProcessingTimeout = c.LeaseDuration }},
		{name: "reversed retry", mutate: func(c *GitHubEventConfig) { c.InitialRetry = c.MaxRetry + time.Second }},
		{name: "retry maximum", mutate: func(c *GitHubEventConfig) { c.MaxRetry = 15*time.Minute + time.Nanosecond }},
		{name: "attempt mismatch", mutate: func(c *GitHubEventConfig) { c.MaxAttempts = 13 }},
		{name: "delivery concurrency", mutate: func(c *GitHubEventConfig) { c.DeliveryConcurrency = 101 }},
		{name: "intent concurrency", mutate: func(c *GitHubEventConfig) { c.IntentConcurrency = 0 }},
		{name: "negative retention", mutate: func(c *GitHubEventConfig) { c.PayloadRetention = -1 }},
		{name: "retention over maximum", mutate: func(c *GitHubEventConfig) { c.PayloadRetention = 30*24*time.Hour + time.Nanosecond }},
		{name: "cleanup batch", mutate: func(c *GitHubEventConfig) { c.CleanupBatch = 1001 }},
		{name: "max intents", mutate: func(c *GitHubEventConfig) { c.MaxIntents = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			runner := NewGitHubEventRunner(cfg, discardLogger(), &fakeGitHubEventStore{}, &fakeGitHubEventProcessor{})
			if err := runner.Startup(t.Context()); err == nil {
				t.Fatal("Startup() error = nil, want invalid configuration")
			}
		})
	}
}

func TestGitHubEventScanBoundsIndependentConcurrencyAndDoesNotOverclaim(t *testing.T) {
	deliveryStarted := make(chan string, 4)
	intentStarted := make(chan string, 4)
	release := make(chan struct{})
	processor := &fakeGitHubEventProcessor{
		delivery: func(ctx context.Context, d store.GitHubWebhookDelivery) service.GitHubEventOutcome {
			deliveryStarted <- d.ID
			select {
			case <-release:
			case <-ctx.Done():
			}
			return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
		},
		intent: func(ctx context.Context, i store.GitHubRoutingIntent) service.GitHubEventOutcome {
			intentStarted <- i.ID
			select {
			case <-release:
			case <-ctx.Done():
			}
			return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
		},
	}
	fake := &fakeGitHubEventStore{
		deliveries: []store.GitHubWebhookDelivery{leasedDelivery("d1", 1), leasedDelivery("d2", 1), leasedDelivery("d3", 1)},
		intents:    []store.GitHubRoutingIntent{leasedIntent("i1", 1), leasedIntent("i2", 1), leasedIntent("i3", 1)},
	}
	cfg := gitHubEventTestConfig()
	cfg.DeliveryConcurrency, cfg.IntentConcurrency = 2, 1
	runner := NewGitHubEventRunner(cfg, discardLogger(), fake, processor)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		<-deliveryStarted
	}
	<-intentStarted
	if fake.deliveryLimits[0] != 2 || fake.intentLimits[0] != 1 || fake.deliveryClaimed != 2 || fake.intentClaimed != 1 {
		t.Fatalf("claims = delivery limits:%v claimed:%d intent limits:%v claimed:%d", fake.deliveryLimits, fake.deliveryClaimed, fake.intentLimits, fake.intentClaimed)
	}
	if err := runner.scan(t.Context(), runner.processCtx, false); err != nil {
		t.Fatal(err)
	}
	if fake.deliveryClaimCalls != 1 || fake.intentClaimCalls != 1 {
		t.Fatalf("full pools were reclaimed: delivery=%d intent=%d", fake.deliveryClaimCalls, fake.intentClaimCalls)
	}
	close(release)
	runner.wg.Wait()
	if fake.deliveryCompleteCalls != 2 || fake.intentRetryCalls != 0 || fake.intentFailCalls != 0 {
		t.Fatalf("independent completion = deliveries:%d intent retry/fail:%d/%d", fake.deliveryCompleteCalls, fake.intentRetryCalls, fake.intentFailCalls)
	}
}

func TestGitHubEventDeliveryOutcomeMappingAndRetryDelay(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    service.GitHubEventOutcome
		wantMethod string
		wantCode   string
	}{
		{name: "complete", outcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}, wantMethod: "complete", wantCode: "processed"},
		{name: "permission ignored", outcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "permission_revoked"}, wantMethod: "complete", wantCode: "ignored"},
		{name: "superseded ignored", outcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "superseded"}, wantMethod: "complete", wantCode: "ignored"},
		{name: "permanent failure", outcome: service.GitHubEventOutcome{Disposition: service.GitHubEventFail, Code: "invalid_event"}, wantMethod: "fail", wantCode: "invalid_event"},
		{name: "retry", outcome: service.GitHubEventOutcome{Disposition: service.GitHubEventRetry, Code: "github_unavailable"}, wantMethod: "retry", wantCode: "github_unavailable"},
		{name: "unknown", outcome: service.GitHubEventOutcome{Code: "secret-wrapped-error"}, wantMethod: "fail", wantCode: "internal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeGitHubEventStore{}
			runner := newGitHubEventTestRunner(fake, &fakeGitHubEventProcessor{deliveryOutcome: test.outcome}, discardLogger())
			enableTestWrites(runner)
			runDeliveryDirect(runner, t.Context(), leasedDelivery("delivery", 2))
			if fake.lastMethod != test.wantMethod || fake.lastCode != test.wantCode {
				t.Fatalf("terminal write = %s/%s, want %s/%s", fake.lastMethod, fake.lastCode, test.wantMethod, test.wantCode)
			}
		})
	}

	runner := newGitHubEventTestRunner(&fakeGitHubEventStore{}, &fakeGitHubEventProcessor{}, discardLogger())
	runner.jitter = func() float64 { return 0 }
	if got := runner.retryDelay(1, 0); got != runner.config.InitialRetry/2 {
		t.Fatalf("first retry with minimum jitter = %s", got)
	}
	runner.jitter = func() float64 { return 1 }
	if got := runner.retryDelay(2, 0); got != 3*runner.config.InitialRetry {
		t.Fatalf("second retry with maximum jitter = %s", got)
	}
	if got := runner.retryDelay(12, 30*time.Minute); got != runner.config.MaxRetry {
		t.Fatalf("RetryAfter/max bound = %s, want %s", got, runner.config.MaxRetry)
	}
}

func TestGitHubEventRetryAtAttemptTwelveUsesTerminalizingStoreOperation(t *testing.T) {
	fake := &fakeGitHubEventStore{}
	runner := newGitHubEventTestRunner(fake, &fakeGitHubEventProcessor{deliveryOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventRetry, Code: "github_unavailable"}}, discardLogger())
	enableTestWrites(runner)
	runDeliveryDirect(runner, t.Context(), leasedDelivery("delivery", 12))
	if fake.deliveryRetryCalls != 1 || fake.deliveryCompleteCalls != 0 || fake.deliveryFailCalls != 0 {
		t.Fatalf("attempt 12 writes = retry:%d complete:%d fail:%d", fake.deliveryRetryCalls, fake.deliveryCompleteCalls, fake.deliveryFailCalls)
	}
	if fake.lastRetention != runner.config.PayloadRetention {
		t.Fatalf("retry retention = %s, want %s", fake.lastRetention, runner.config.PayloadRetention)
	}
}

func TestGitHubEventLeaseRenewalLossCancelsProcessorAndSuppressesWrite(t *testing.T) {
	for _, test := range []struct {
		name     string
		renewErr error
		delivery bool
	}{
		{name: "delivery lease lost", renewErr: store.ErrLeaseNotOwned, delivery: true},
		{name: "delivery database error", renewErr: errors.New("database unavailable"), delivery: true},
		{name: "intent lease lost", renewErr: store.ErrLeaseNotOwned},
		{name: "intent database error", renewErr: errors.New("database unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			cancelled := make(chan struct{})
			processor := &fakeGitHubEventProcessor{
				delivery: func(ctx context.Context, _ store.GitHubWebhookDelivery) service.GitHubEventOutcome {
					<-ctx.Done()
					close(cancelled)
					return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
				},
				intent: func(ctx context.Context, _ store.GitHubRoutingIntent) service.GitHubEventOutcome {
					<-ctx.Done()
					close(cancelled)
					return service.GitHubEventOutcome{Disposition: service.GitHubEventRetry, Code: "github_unavailable"}
				},
			}
			fake := &fakeGitHubEventStore{renewErr: test.renewErr}
			cfg := gitHubEventTestConfig()
			cfg.LeaseDuration, cfg.ProcessingTimeout = 30*time.Millisecond, 20*time.Millisecond
			runner := NewGitHubEventRunner(cfg, discardLogger(), fake, processor)
			enableTestWrites(runner)
			if test.delivery {
				runDeliveryDirect(runner, t.Context(), leasedDelivery("delivery", 1))
			} else {
				runIntentDirect(runner, t.Context(), leasedIntent("intent", 1))
			}
			select {
			case <-cancelled:
			default:
				t.Fatal("renewal failure did not cancel processor")
			}
			if fake.terminalWrites() != 0 {
				t.Fatalf("renewal failure terminal writes = %d", fake.terminalWrites())
			}
			if fake.renewCalls == 0 {
				t.Fatal("lease was not renewed at the configured fraction")
			}
		})
	}
}

func TestGitHubEventIntentAtomicSuccessWinsRenewalLeaseRace(t *testing.T) {
	var logs bytes.Buffer
	renewStarted := make(chan struct{})
	committed := make(chan struct{})
	renewReturned := make(chan struct{})
	fake := &fakeGitHubEventStore{intentRenew: func(context.Context) error {
		close(renewStarted)
		<-committed
		close(renewReturned)
		return store.ErrLeaseNotOwned
	}}
	processor := &fakeGitHubEventProcessor{intent: func(context.Context, store.GitHubRoutingIntent) service.GitHubEventOutcome {
		<-renewStarted
		close(committed)
		<-renewReturned
		return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
	}}
	cfg := gitHubEventTestConfig()
	cfg.LeaseDuration, cfg.ProcessingTimeout = 30*time.Millisecond, time.Second
	runner := NewGitHubEventRunner(cfg, slog.New(slog.NewJSONHandler(&logs, nil)), fake, processor)
	enableTestWrites(runner)
	runIntentDirect(runner, t.Context(), leasedIntent("intent", 1))
	if text := logs.String(); !strings.Contains(text, `"result":"processed"`) || strings.Contains(text, `"result":"lease_lost"`) {
		t.Fatalf("intent result log = %q", text)
	}
	if fake.intentRetryCalls != 0 || fake.intentFailCalls != 0 {
		t.Fatalf("atomic success wrote retry/fail = %d/%d", fake.intentRetryCalls, fake.intentFailCalls)
	}
}

func TestGitHubEventFinalDeliveryRotationFencesTerminalWrite(t *testing.T) {
	original := store.LeaseToken{Owner: "worker-one", Epoch: 1, ExpiresAt: time.Unix(100, 0)}
	rotated := store.LeaseToken{Owner: "worker-one", Epoch: 2, ExpiresAt: time.Unix(200, 0)}
	processorStarted, releaseProcessor := make(chan struct{}), make(chan struct{})
	writeStarted, releaseWrite := make(chan struct{}), make(chan struct{})
	fake := &fakeGitHubEventStore{rotatedToken: rotated}
	fake.deliveryWrite = func(context.Context) error {
		close(writeStarted)
		<-releaseWrite
		return nil
	}
	processor := &fakeGitHubEventProcessor{delivery: func(context.Context, store.GitHubWebhookDelivery) service.GitHubEventOutcome {
		close(processorStarted)
		<-releaseProcessor
		return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
	}}
	runner := newGitHubEventTestRunner(fake, processor, discardLogger())
	enableTestWrites(runner)
	delivery := leasedDelivery("delivery", 1)
	delivery.Lease = &original
	done := make(chan struct{})
	go func() { runDeliveryDirect(runner, t.Context(), delivery); close(done) }()
	<-processorStarted
	close(releaseProcessor)
	<-writeStarted
	if fake.rotateCalls != 1 || fake.lastRotateToken != original || fake.lastWriteToken != rotated {
		t.Fatalf("final rotation/write tokens = calls:%d rotate:%+v write:%+v", fake.rotateCalls, fake.lastRotateToken, fake.lastWriteToken)
	}
	select {
	case <-done:
		t.Fatal("delivery returned before delayed terminal write")
	default:
	}
	close(releaseWrite)
	<-done
	if fake.deliveryCompleteCalls != 1 {
		t.Fatalf("delivery completion calls = %d", fake.deliveryCompleteCalls)
	}
}

func TestGitHubEventFinalDeliveryRotationFailureSuppressesTerminalWrite(t *testing.T) {
	secret := "raw database error secret"
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "lease not owned", err: store.ErrLeaseNotOwned},
		{name: "database error", err: errors.New(secret)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			fake := &fakeGitHubEventStore{rotateErr: test.err}
			runner := newGitHubEventTestRunner(fake, &fakeGitHubEventProcessor{deliveryOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventRetry, Code: "github_unavailable"}}, slog.New(slog.NewJSONHandler(&logs, nil)))
			enableTestWrites(runner)
			runDeliveryDirect(runner, t.Context(), leasedDelivery("delivery", 1))
			if fake.rotateCalls != 1 || fake.terminalWrites() != 0 {
				t.Fatalf("final rotation calls/writes = %d/%d", fake.rotateCalls, fake.terminalWrites())
			}
			text := logs.String()
			if !strings.Contains(text, `"result":"lease_lost"`) || strings.Contains(text, secret) || strings.Contains(text, "raw database error") {
				t.Fatalf("final rotation log = %q", text)
			}
		})
	}
}

func TestGitHubEventFinalIntentRotationUsesFencedToken(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition service.GitHubEventDisposition
		wantMethod  string
	}{
		{name: "retry", disposition: service.GitHubEventRetry, wantMethod: "intent-retry"},
		{name: "fail", disposition: service.GitHubEventFail, wantMethod: "intent-fail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := store.LeaseToken{Owner: "worker-one", Epoch: 3, ExpiresAt: time.Unix(100, 0)}
			rotated := store.LeaseToken{Owner: "worker-one", Epoch: 4, ExpiresAt: time.Unix(200, 0)}
			fake := &fakeGitHubEventStore{rotatedToken: rotated}
			runner := newGitHubEventTestRunner(fake, &fakeGitHubEventProcessor{intentOutcome: service.GitHubEventOutcome{Disposition: test.disposition, Code: map[service.GitHubEventDisposition]string{service.GitHubEventRetry: "github_unavailable", service.GitHubEventFail: "invalid_snapshot"}[test.disposition]}}, discardLogger())
			enableTestWrites(runner)
			intent := leasedIntent("intent", 1)
			intent.Lease = &original
			runIntentDirect(runner, t.Context(), intent)
			if fake.rotateCalls != 1 || fake.lastRotateToken != original || fake.lastWriteToken != rotated || fake.lastMethod != test.wantMethod {
				t.Fatalf("final intent rotation/write = calls:%d rotate:%+v write:%+v method:%q", fake.rotateCalls, fake.lastRotateToken, fake.lastWriteToken, fake.lastMethod)
			}
		})
	}
}

func TestGitHubEventAtomicIntentCompleteSkipsFinalRenewalAndPostwrite(t *testing.T) {
	fake := &fakeGitHubEventStore{}
	runner := newGitHubEventTestRunner(fake, &fakeGitHubEventProcessor{intentOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}}, discardLogger())
	enableTestWrites(runner)
	runIntentDirect(runner, t.Context(), leasedIntent("intent", 1))
	if fake.rotateCalls != 0 || fake.terminalWrites() != 0 {
		t.Fatalf("atomic complete rotations/writes = %d/%d", fake.rotateCalls, fake.terminalWrites())
	}
}

func TestGitHubEventShutdownWaitsForCanceledFinalWrites(t *testing.T) {
	for _, test := range []struct {
		name       string
		blockRenew bool
	}{
		{name: "final rotation", blockRenew: true},
		{name: "terminal write"},
	} {
		t.Run(test.name, func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			fake := &fakeGitHubEventStore{deliveries: []store.GitHubWebhookDelivery{leasedDelivery("delivery", 1)}}
			block := func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				close(stopped)
				return ctx.Err()
			}
			if test.blockRenew {
				fake.deliveryRotate = block
			} else {
				fake.deliveryWrite = block
			}
			cfg := gitHubEventTestConfig()
			cfg.ShutdownGrace = 5 * time.Millisecond
			runner := NewGitHubEventRunner(cfg, discardLogger(), fake, &fakeGitHubEventProcessor{deliveryOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}})
			ctx, cancel := context.WithCancel(t.Context())
			if err := runner.Startup(ctx); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			<-started
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			select {
			case <-stopped:
			default:
				t.Fatal("Run returned before the final write stopped")
			}
			if fake.committedWrites != 0 {
				t.Fatalf("late committed writes = %d", fake.committedWrites)
			}
		})
	}
}

func TestGitHubEventIntentOutcomeMappingAssumesAtomicSuccess(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    service.GitHubEventOutcome
		wantMethod string
	}{
		{name: "atomic success has no separate completion", outcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}},
		{name: "retry", outcome: service.GitHubEventOutcome{Disposition: service.GitHubEventRetry, Code: "github_unavailable"}, wantMethod: "intent-retry"},
		{name: "permanent", outcome: service.GitHubEventOutcome{Disposition: service.GitHubEventFail, Code: "invalid_snapshot"}, wantMethod: "intent-fail"},
		{name: "unknown", outcome: service.GitHubEventOutcome{Code: "raw error"}, wantMethod: "intent-fail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeGitHubEventStore{}
			runner := newGitHubEventTestRunner(fake, &fakeGitHubEventProcessor{intentOutcome: test.outcome}, discardLogger())
			enableTestWrites(runner)
			runIntentDirect(runner, t.Context(), leasedIntent("intent", 1))
			if fake.lastMethod != test.wantMethod {
				t.Fatalf("intent write = %q, want %q", fake.lastMethod, test.wantMethod)
			}
		})
	}
}

func TestGitHubEventCleanupBatchIntervalAndError(t *testing.T) {
	fake := &fakeGitHubEventStore{cleanupResult: 2}
	runner := newGitHubEventTestRunner(fake, &fakeGitHubEventProcessor{}, discardLogger())
	now := time.Unix(100, 0)
	runner.now = func() time.Time { return now }
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fake.cleanupCalls != 1 || fake.cleanupBatch != runner.config.CleanupBatch {
		t.Fatalf("cleanup = calls:%d batch:%d", fake.cleanupCalls, fake.cleanupBatch)
	}
	if fake.sweepCalls != 1 || fake.sweepBatch != runner.config.CleanupBatch || fake.sweepRetention != runner.config.PayloadRetention {
		t.Fatalf("startup sweep = calls:%d batch:%d retention:%s", fake.sweepCalls, fake.sweepBatch, fake.sweepRetention)
	}
	if err := runner.scan(t.Context(), runner.processCtx, false); err != nil {
		t.Fatal(err)
	}
	if fake.cleanupCalls != 1 || fake.sweepCalls != 2 {
		t.Fatalf("before interval cleanup/sweep = %d/%d", fake.cleanupCalls, fake.sweepCalls)
	}
	now = now.Add(runner.config.CleanupInterval)
	if err := runner.scan(t.Context(), runner.processCtx, false); err != nil {
		t.Fatal(err)
	}
	if fake.cleanupCalls != 2 {
		t.Fatalf("cleanup after interval = %d", fake.cleanupCalls)
	}
	fake.cleanupErr = errors.New("cleanup unavailable")
	runner.lastClean = time.Time{}
	if err := runner.scan(t.Context(), runner.processCtx, false); !errors.Is(err, fake.cleanupErr) {
		t.Fatalf("cleanup error = %v", err)
	}
	fake.cleanupErr = nil
	fake.sweepErr = errors.New("sweep unavailable")
	if err := runner.scan(t.Context(), runner.processCtx, false); !errors.Is(err, fake.sweepErr) {
		t.Fatalf("sweep error = %v", err)
	}
}

func TestGitHubEventPeriodicClaimErrorDoesNotCancelActiveSibling(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	processor := &fakeGitHubEventProcessor{delivery: func(ctx context.Context, _ store.GitHubWebhookDelivery) service.GitHubEventOutcome {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
	}}
	fake := &fakeGitHubEventStore{deliveries: []store.GitHubWebhookDelivery{leasedDelivery("delivery", 1)}}
	cfg := gitHubEventTestConfig()
	cfg.ScanInterval = time.Millisecond
	runner := NewGitHubEventRunner(cfg, discardLogger(), fake, processor)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-started
	want := errors.New("intent claim unavailable")
	fake.intentClaimErr = want
	fake.intentClaimAttempt = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	select {
	case <-fake.intentClaimAttempt:
	case <-time.After(time.Second):
		t.Fatal("periodic intent claim was not attempted")
	}
	select {
	case <-cancelled:
		t.Fatal("claim error canceled active delivery")
	default:
	}
	if !runner.Ready() {
		t.Fatal("claim error made periodic worker unready")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestGitHubEventShutdownGracefulAndForced(t *testing.T) {
	t.Run("graceful processing terminalizes", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		fake := &fakeGitHubEventStore{deliveries: []store.GitHubWebhookDelivery{leasedDelivery("delivery", 1)}}
		processor := &fakeGitHubEventProcessor{delivery: func(context.Context, store.GitHubWebhookDelivery) service.GitHubEventOutcome {
			close(started)
			<-release
			return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
		}}
		runner := newGitHubEventTestRunner(fake, processor, discardLogger())
		ctx, cancel := context.WithCancel(t.Context())
		if err := runner.Startup(ctx); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		<-started
		close(release)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if fake.deliveryCompleteCalls != 1 || runner.Ready() {
			t.Fatalf("graceful completion=%d ready=%t", fake.deliveryCompleteCalls, runner.Ready())
		}
	})

	t.Run("forced timeout leaves lease and suppresses late write", func(t *testing.T) {
		started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
		fake := &fakeGitHubEventStore{deliveries: []store.GitHubWebhookDelivery{leasedDelivery("delivery", 1)}}
		processor := &fakeGitHubEventProcessor{delivery: func(context.Context, store.GitHubWebhookDelivery) service.GitHubEventOutcome {
			close(started)
			<-release
			close(returned)
			return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}
		}}
		cfg := gitHubEventTestConfig()
		cfg.ShutdownGrace = 5 * time.Millisecond
		runner := NewGitHubEventRunner(cfg, discardLogger(), fake, processor)
		ctx, cancel := context.WithCancel(t.Context())
		if err := runner.Startup(ctx); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		<-started
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if fake.terminalWrites() != 0 || fake.releaseCalls != 0 {
			t.Fatalf("forced shutdown writes=%d releases=%d", fake.terminalWrites(), fake.releaseCalls)
		}
		close(release)
		<-returned
		runner.wg.Wait()
		if fake.terminalWrites() != 0 {
			t.Fatalf("late processor terminal writes=%d", fake.terminalWrites())
		}
	})

	t.Run("forced timeout cancels blocked terminal write", func(t *testing.T) {
		writeStarted, writeCancelled := make(chan struct{}), make(chan struct{})
		fake := &fakeGitHubEventStore{deliveries: []store.GitHubWebhookDelivery{leasedDelivery("delivery", 1)}}
		fake.deliveryWrite = func(ctx context.Context) error {
			close(writeStarted)
			<-ctx.Done()
			close(writeCancelled)
			return ctx.Err()
		}
		processor := &fakeGitHubEventProcessor{deliveryOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed"}}
		cfg := gitHubEventTestConfig()
		cfg.ShutdownGrace = 5 * time.Millisecond
		runner := NewGitHubEventRunner(cfg, discardLogger(), fake, processor)
		ctx, cancel := context.WithCancel(t.Context())
		if err := runner.Startup(ctx); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		<-writeStarted
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		select {
		case <-writeCancelled:
		default:
			t.Fatal("Run returned before the terminal write stopped")
		}
	})
}

func TestGitHubEventCancellationStopsNewClaims(t *testing.T) {
	fake := &fakeGitHubEventStore{}
	cfg := gitHubEventTestConfig()
	cfg.ScanInterval = time.Millisecond
	runner := NewGitHubEventRunner(cfg, discardLogger(), fake, &fakeGitHubEventProcessor{})
	ctx, cancel := context.WithCancel(t.Context())
	if err := runner.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	deliveryClaims, intentClaims := fake.deliveryClaimCalls, fake.intentClaimCalls
	time.Sleep(3 * time.Millisecond)
	if fake.deliveryClaimCalls != deliveryClaims || fake.intentClaimCalls != intentClaims {
		t.Fatalf("claims continued after cancellation: %d/%d to %d/%d", deliveryClaims, intentClaims, fake.deliveryClaimCalls, fake.intentClaimCalls)
	}
}

func TestGitHubEventStructuredLogsExcludeSensitiveValues(t *testing.T) {
	var logs bytes.Buffer
	secret := "payload-secret-signature-token"
	fake := &fakeGitHubEventStore{deliveryWriteErr: errors.New("wrapped " + secret)}
	processor := &fakeGitHubEventProcessor{deliveryOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed", IntentCount: 2}}
	runner := newGitHubEventTestRunner(fake, processor, slog.New(slog.NewJSONHandler(&logs, nil)))
	enableTestWrites(runner)
	delivery := leasedDelivery("internal-id", 1)
	delivery.DeliveryID, delivery.Event, delivery.Action, delivery.Payload = "public-delivery", "push", "created", []byte(secret)
	runDeliveryDirect(runner, t.Context(), delivery)
	text := logs.String()
	for _, want := range []string{`"delivery_id":"public-delivery"`, `"event":"push"`, `"action":"created"`, `"result":"database_unavailable"`, `"attempt":1`, `"fanout":2`} {
		if !strings.Contains(text, want) {
			t.Fatalf("log %q does not contain fixed metadata %q", text, want)
		}
	}
	for _, forbidden := range []string{secret, "wrapped", "signature", "token", "internal-id"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("log exposed %q: %s", forbidden, text)
		}
	}
}

func gitHubEventTestConfig() GitHubEventConfig {
	return GitHubEventConfig{ScanInterval: time.Hour, LeaseDuration: time.Minute, ProcessingTimeout: time.Second, InitialRetry: 5 * time.Second, MaxRetry: 15 * time.Minute, PayloadRetention: time.Hour, CleanupInterval: time.Minute, ShutdownGrace: time.Second, DeliveryConcurrency: 4, IntentConcurrency: 4, MaxAttempts: 12, CleanupBatch: 100, MaxIntents: 1000, Owner: "worker-one"}
}

func newGitHubEventTestRunner(dataStore GitHubEventStore, processor GitHubEventProcessor, logger *slog.Logger) *GitHubEventRunner {
	return NewGitHubEventRunner(gitHubEventTestConfig(), logger, dataStore, processor)
}

func enableTestWrites(runner *GitHubEventRunner) {
	runner.writeCtx, runner.writeCancel = context.WithCancel(context.Background())
	runner.writeOK.Store(true)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func leasedDelivery(id string, attempts int) store.GitHubWebhookDelivery {
	return store.GitHubWebhookDelivery{ID: id, DeliveryID: id + "-public", Event: "push", Attempts: attempts, Lease: &store.LeaseToken{Owner: "worker-one", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)}}
}

func leasedIntent(id string, attempts int) store.GitHubRoutingIntent {
	return store.GitHubRoutingIntent{ID: id, AppID: "app", Ref: "refs/heads/main", EventKind: "push", Attempts: attempts, Lease: &store.LeaseToken{Owner: "worker-one", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)}}
}

func runDeliveryDirect(runner *GitHubEventRunner, ctx context.Context, delivery store.GitHubWebhookDelivery) {
	runner.delivery <- struct{}{}
	runner.wg.Add(1)
	runner.processDelivery(ctx, delivery)
}

func runIntentDirect(runner *GitHubEventRunner, ctx context.Context, intent store.GitHubRoutingIntent) {
	runner.intent <- struct{}{}
	runner.wg.Add(1)
	runner.processIntent(ctx, intent)
}

type fakeGitHubEventProcessor struct {
	delivery        func(context.Context, store.GitHubWebhookDelivery) service.GitHubEventOutcome
	intent          func(context.Context, store.GitHubRoutingIntent) service.GitHubEventOutcome
	deliveryOutcome service.GitHubEventOutcome
	intentOutcome   service.GitHubEventOutcome
}

func (f *fakeGitHubEventProcessor) ProcessDelivery(ctx context.Context, delivery store.GitHubWebhookDelivery) service.GitHubEventOutcome {
	if f.delivery != nil {
		return f.delivery(ctx, delivery)
	}
	return f.deliveryOutcome
}
func (f *fakeGitHubEventProcessor) ProcessIntent(ctx context.Context, intent store.GitHubRoutingIntent) service.GitHubEventOutcome {
	if f.intent != nil {
		return f.intent(ctx, intent)
	}
	return f.intentOutcome
}

type fakeGitHubEventStore struct {
	mu                                                                                                      sync.Mutex
	leaseStatus                                                                                             store.GitHubEventLeaseStatus
	countErr, cleanupErr, sweepErr, deliveryClaimErr, intentClaimErr, renewErr, rotateErr, deliveryWriteErr error
	deliveries                                                                                              []store.GitHubWebhookDelivery
	intents                                                                                                 []store.GitHubRoutingIntent
	countCalls, cleanupCalls, cleanupBatch, sweepCalls, sweepBatch                                          int
	deliveryClaimCalls, intentClaimCalls                                                                    int
	deliveryClaimed, intentClaimed, renewCalls, rotateCalls                                                 int
	deliveryLimits, intentLimits                                                                            []int
	deliveryRetryCalls, deliveryCompleteCalls, deliveryFailCalls                                            int
	intentRetryCalls, intentFailCalls, releaseCalls                                                         int
	cleanupResult                                                                                           int64
	sweepResult                                                                                             store.GitHubEventLeaseStatus
	lastMethod, lastCode                                                                                    string
	lastRetention, sweepRetention                                                                           time.Duration
	deliveryWrite                                                                                           func(context.Context) error
	deliveryRenew, intentRenew, deliveryRotate, intentRotate                                                func(context.Context) error
	intentClaimAttempt                                                                                      chan struct{}
	renewedToken, rotatedToken, lastRenewToken, lastRotateToken, lastWriteToken                             store.LeaseToken
	committedWrites                                                                                         int
}

func (f *fakeGitHubEventStore) CountUnexpiredGitHubEventLeases(context.Context) (store.GitHubEventLeaseStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countCalls++
	return f.leaseStatus, f.countErr
}
func (f *fakeGitHubEventStore) FailExpiredExhaustedGitHubEventLeases(_ context.Context, retention time.Duration, limit int) (store.GitHubEventLeaseStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweepCalls++
	f.sweepRetention, f.sweepBatch = retention, limit
	return f.sweepResult, f.sweepErr
}
func (f *fakeGitHubEventStore) ClaimGitHubWebhookDeliveries(_ context.Context, _ string, _ time.Duration, limit int) ([]store.GitHubWebhookDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveryClaimCalls++
	f.deliveryLimits = append(f.deliveryLimits, limit)
	if f.deliveryClaimErr != nil {
		return nil, f.deliveryClaimErr
	}
	n := min(limit, len(f.deliveries))
	out := append([]store.GitHubWebhookDelivery(nil), f.deliveries[:n]...)
	f.deliveries = f.deliveries[n:]
	f.deliveryClaimed += n
	return out, nil
}
func (f *fakeGitHubEventStore) RenewGitHubWebhookDelivery(ctx context.Context, _ string, token store.LeaseToken, _ time.Duration) (store.LeaseToken, error) {
	f.mu.Lock()
	f.renewCalls++
	f.lastRenewToken = token
	renew, err := f.deliveryRenew, f.renewErr
	result := f.renewedToken
	f.mu.Unlock()
	if renew != nil {
		err = renew(ctx)
	}
	return result, err
}
func (f *fakeGitHubEventStore) RotateGitHubWebhookDeliveryLease(ctx context.Context, _ string, token store.LeaseToken, _ time.Duration) (store.LeaseToken, error) {
	f.mu.Lock()
	f.rotateCalls++
	f.lastRotateToken = token
	rotate, err := f.deliveryRotate, f.rotateErr
	result := f.rotatedToken
	f.mu.Unlock()
	if rotate != nil {
		err = rotate(ctx)
	}
	return result, err
}
func (f *fakeGitHubEventStore) RetryGitHubWebhookDelivery(ctx context.Context, _ string, token store.LeaseToken, _ time.Duration, retention time.Duration, code, _ string) error {
	f.mu.Lock()
	f.deliveryRetryCalls++
	f.lastMethod, f.lastCode = "retry", code
	f.lastRetention = retention
	f.lastWriteToken = token
	write, err := f.deliveryWrite, f.deliveryWriteErr
	f.mu.Unlock()
	if write != nil {
		return write(ctx)
	}
	if err == nil {
		f.mu.Lock()
		f.committedWrites++
		f.mu.Unlock()
	}
	return err
}
func (f *fakeGitHubEventStore) CompleteGitHubWebhookDelivery(ctx context.Context, _ string, token store.LeaseToken, code string, _ time.Duration) error {
	f.mu.Lock()
	f.deliveryCompleteCalls++
	f.lastMethod, f.lastCode = "complete", code
	f.lastWriteToken = token
	write, err := f.deliveryWrite, f.deliveryWriteErr
	f.mu.Unlock()
	if write != nil {
		return write(ctx)
	}
	if err == nil {
		f.mu.Lock()
		f.committedWrites++
		f.mu.Unlock()
	}
	return err
}
func (f *fakeGitHubEventStore) FailGitHubWebhookDelivery(_ context.Context, _ string, token store.LeaseToken, code, _ string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveryFailCalls++
	f.lastMethod, f.lastCode = "fail", code
	f.lastWriteToken = token
	if f.deliveryWriteErr == nil {
		f.committedWrites++
	}
	return f.deliveryWriteErr
}
func (f *fakeGitHubEventStore) ClaimGitHubRoutingIntents(_ context.Context, _ string, _ time.Duration, limit int) ([]store.GitHubRoutingIntent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intentClaimCalls++
	f.intentLimits = append(f.intentLimits, limit)
	if f.intentClaimAttempt != nil {
		select {
		case f.intentClaimAttempt <- struct{}{}:
		default:
		}
	}
	if f.intentClaimErr != nil {
		return nil, f.intentClaimErr
	}
	n := min(limit, len(f.intents))
	out := append([]store.GitHubRoutingIntent(nil), f.intents[:n]...)
	f.intents = f.intents[n:]
	f.intentClaimed += n
	return out, nil
}
func (f *fakeGitHubEventStore) RenewGitHubRoutingIntent(ctx context.Context, _ string, token store.LeaseToken, _ time.Duration) (store.LeaseToken, error) {
	f.mu.Lock()
	f.renewCalls++
	f.lastRenewToken = token
	renew, err := f.intentRenew, f.renewErr
	result := f.renewedToken
	f.mu.Unlock()
	if renew != nil {
		err = renew(ctx)
	}
	return result, err
}
func (f *fakeGitHubEventStore) RotateGitHubRoutingIntentLease(ctx context.Context, _ string, token store.LeaseToken, _ time.Duration) (store.LeaseToken, error) {
	f.mu.Lock()
	f.rotateCalls++
	f.lastRotateToken = token
	rotate, err := f.intentRotate, f.rotateErr
	result := f.rotatedToken
	f.mu.Unlock()
	if rotate != nil {
		err = rotate(ctx)
	}
	return result, err
}
func (f *fakeGitHubEventStore) RetryGitHubRoutingIntent(_ context.Context, _ string, token store.LeaseToken, _ time.Duration, code, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intentRetryCalls++
	f.lastMethod, f.lastCode = "intent-retry", code
	f.lastWriteToken = token
	if f.deliveryWriteErr == nil {
		f.committedWrites++
	}
	return f.deliveryWriteErr
}
func (f *fakeGitHubEventStore) FailGitHubRoutingIntent(_ context.Context, _ string, token store.LeaseToken, code, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intentFailCalls++
	f.lastMethod, f.lastCode = "intent-fail", code
	f.lastWriteToken = token
	if f.deliveryWriteErr == nil {
		f.committedWrites++
	}
	return f.deliveryWriteErr
}
func (f *fakeGitHubEventStore) RedactTerminalGitHubWebhookPayloads(_ context.Context, batch int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCalls++
	f.cleanupBatch = batch
	return f.cleanupResult, f.cleanupErr
}
func (f *fakeGitHubEventStore) terminalWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deliveryRetryCalls + f.deliveryCompleteCalls + f.deliveryFailCalls + f.intentRetryCalls + f.intentFailCalls
}
