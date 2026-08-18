package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/config"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/worker"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestBuildRecoveryAdaptersUsesSuccessfullyConstructedAdapters(t *testing.T) {
	github := &fakeGitHubAdapter{}
	kubernetes := &fakeKubernetesAdapter{}
	githubCalls := 0
	kubernetesCalls := 0
	cfg := adapterTestConfig()

	adapters, err := buildRecoveryAdapters(cfg, nil, adapterConstructors{
		github: func(got config.GitHub) (worker.GitHubRecoveryAdapter, error) {
			githubCalls++
			if got.APIBaseURL != cfg.GitHub.APIBaseURL {
				t.Fatalf("GitHub config = %+v", got)
			}
			return github, nil
		},
		kubernetes: func(got config.Kubernetes) (worker.KubernetesRecoveryAdapter, error) {
			kubernetesCalls++
			if got.Kubeconfig != cfg.Kubernetes.Kubeconfig {
				t.Fatalf("Kubernetes config = %+v", got)
			}
			return kubernetes, nil
		},
	})
	if err != nil {
		t.Fatalf("buildRecoveryAdapters() error = %v", err)
	}
	if githubCalls != 1 || kubernetesCalls != 1 || adapters.GitHub != github || adapters.Kubernetes != kubernetes {
		t.Fatalf("constructed adapters = %+v, calls github:%d kubernetes:%d", adapters, githubCalls, kubernetesCalls)
	}
	if !adapters.Capabilities.GitHub || !adapters.Capabilities.Kubernetes {
		t.Fatalf("capabilities = %+v", adapters.Capabilities)
	}
}

func TestRunRejectsGitHubRequestTimeoutAtConsumerLeaseBoundaryBeforeStartup(t *testing.T) {
	keyring := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(keyring, []byte(`{"active_version":"k1","keys":{"k1":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{
		"--worker-lease-duration", "12s",
		"--github-event-worker-lease-duration", "14999999999ns",
		"--github-event-worker-processing-timeout", "1s",
		"--github-request-timeout", "10s",
	}, []string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://invalid.invalid/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		"CYCLOPUS_WEBHOOK_PAYLOAD_ENCRYPTION_KEY_FILE=" + keyring,
	})
	if err == nil || err.Error() != "github.request_timeout plus a 5s call pin safety margin must not exceed github_event_worker.lease_duration" {
		t.Fatalf("run() error = %v", err)
	}
}

func TestNewGitHubWebhookPayloadCodecUsesConfiguredKeyring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, []byte(`{"active_version":"k2","keys":{"k1":"MDEyMzQ1Njc4OWFiY2RlZg==","k2":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	codec, err := newGitHubWebhookPayloadCodec(config.Webhook{PayloadEncryptionKeyFile: path})
	if err != nil {
		t.Fatal(err)
	}
	context := store.GitHubWebhookPayloadContext{DeliveryID: "delivery", Event: "push", Action: "", OrgID: "org", PayloadDigest: "digest", InstallationID: 1}
	ciphertext, nonce, version, err := codec.Encrypt([]byte("payload"), context)
	if err != nil || version != "k2" {
		t.Fatalf("Encrypt() version=%q error=%v", version, err)
	}
	if plaintext, err := codec.Decrypt(ciphertext, nonce, version, context); err != nil || string(plaintext) != "payload" {
		t.Fatalf("Decrypt() = %q, %v", plaintext, err)
	}
}

func TestNewGitHubWebhookPayloadCodecRejectsUnsafeOrMalformedKeyring(t *testing.T) {
	for _, test := range []struct {
		name, contents string
		mode           os.FileMode
	}{
		{name: "group readable", contents: `{"active_version":"k1","keys":{"k1":"MDEyMzQ1Njc4OWFiY2RlZg=="}}`, mode: 0o640},
		{name: "unknown field", contents: `{"active_version":"k1","keys":{"k1":"MDEyMzQ1Njc4OWFiY2RlZg=="},"extra":true}`, mode: 0o600},
		{name: "duplicate field", contents: `{"active_version":"k1","active_version":"k2","keys":{"k1":"MDEyMzQ1Njc4OWFiY2RlZg=="}}`, mode: 0o600},
		{name: "duplicate version after trim", contents: `{"active_version":"k1","keys":{"k1":"MDEyMzQ1Njc4OWFiY2RlZg=="," k1 ":"MDEyMzQ1Njc4OWFiY2RlZg=="}}`, mode: 0o600},
		{name: "base64 whitespace", contents: `{"active_version":"k1","keys":{"k1":" MDEyMzQ1Njc4OWFiY2RlZg=="}}`, mode: 0o600},
		{name: "wrong key length", contents: `{"active_version":"k1","keys":{"k1":"c2hvcnQ="}}`, mode: 0o600},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keyring.json")
			if err := os.WriteFile(path, []byte(test.contents), test.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := newGitHubWebhookPayloadCodec(config.Webhook{PayloadEncryptionKeyFile: path}); err == nil {
				t.Fatal("newGitHubWebhookPayloadCodec() error = nil")
			}
		})
	}
}

func TestBuildRecoveryAdaptersSkipsGitHubWhenDisabled(t *testing.T) {
	cfg := adapterTestConfig()
	cfg.GitHub = config.GitHub{}
	githubCalled := false

	adapters, err := buildRecoveryAdapters(cfg, nil, adapterConstructors{
		github: func(config.GitHub) (worker.GitHubRecoveryAdapter, error) {
			githubCalled = true
			return &fakeGitHubAdapter{}, nil
		},
		kubernetes: func(config.Kubernetes) (worker.KubernetesRecoveryAdapter, error) {
			return &fakeKubernetesAdapter{}, nil
		},
	})
	if err != nil {
		t.Fatalf("buildRecoveryAdapters() error = %v", err)
	}
	if githubCalled || adapters.Capabilities.GitHub || adapters.GitHub != nil {
		t.Fatalf("GitHub adapter unexpectedly enabled: %+v", adapters)
	}
	if !adapters.Capabilities.Kubernetes || adapters.Kubernetes == nil {
		t.Fatalf("Kubernetes adapter not enabled: %+v", adapters)
	}
}

func TestBuildRecoveryAdaptersConstructionFailureReturnsNoReadyCapabilities(t *testing.T) {
	want := errors.New("cluster unavailable")
	adapters, err := buildRecoveryAdapters(adapterTestConfig(), nil, adapterConstructors{
		kubernetes: func(config.Kubernetes) (worker.KubernetesRecoveryAdapter, error) {
			return nil, want
		},
	})
	if !errors.Is(err, want) {
		t.Fatalf("buildRecoveryAdapters() error = %v, want %v", err, want)
	}
	if adapters.Capabilities.GitHub || adapters.Capabilities.Kubernetes || adapters.GitHub != nil || adapters.Kubernetes != nil {
		t.Fatalf("adapters = %+v, want no capabilities after construction failure", adapters)
	}
}

func TestBuildRecoveryAdaptersFailsWithoutWebhookCapableKubernetesAdapter(t *testing.T) {
	adapters, err := buildRecoveryAdapters(adapterTestConfig(), nil, adapterConstructors{
		kubernetes: func(config.Kubernetes) (worker.KubernetesRecoveryAdapter, error) {
			return &clientlessKubernetesAdapter{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "webhook delivery") {
		t.Fatalf("buildRecoveryAdapters() error = %v, want webhook capability failure", err)
	}
	if adapters.Webhook != nil {
		t.Fatal("webhook adapter constructed from a clientless Kubernetes adapter")
	}
}

func TestStartupWithDueWorkUsesConstructedAdapters(t *testing.T) {
	github := &countingGitHubAdapter{}
	kubernetes := &countingKubernetesAdapter{}
	adapters, err := buildRecoveryAdapters(adapterTestConfig(), nil, adapterConstructors{
		github: func(config.GitHub) (worker.GitHubRecoveryAdapter, error) { return github, nil },
		kubernetes: func(config.Kubernetes) (worker.KubernetesRecoveryAdapter, error) {
			return kubernetes, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveryStore := &startupRecoveryStore{
		orgIDs:  []string{"org"},
		jobs:    []*store.OrchestrationJob{{ID: "job", OrgID: "org", Status: "dispatched"}},
		deploys: []store.ExpiredDeploy{{OrgID: "org", EnvironmentID: "environment"}},
		lease:   store.DeployLease{OrgID: "org", EnvironmentID: "environment", ReleaseID: "release"},
	}
	runner := worker.New(worker.Config{ScanInterval: time.Second, LeaseDuration: time.Minute, MaxConcurrentDeploys: 4, RequestTimeout: time.Second, Owner: "worker"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), recoveryStore, adapters)

	if err := runner.Startup(context.Background()); err != nil {
		t.Fatalf("Startup() error = %v", err)
	}
	if github.calls != 1 || kubernetes.calls != 1 {
		t.Fatalf("adapter calls = github:%d kubernetes:%d, want 1 each", github.calls, kubernetes.calls)
	}
}

func adapterTestConfig() config.Config {
	return config.Config{
		GitHub: config.GitHub{
			APIBaseURL: "https://api.github.test/",
			UploadURL:  "https://uploads.github.test/", APIVersion: "2026-03-10",
			DispatchObservationInterval: time.Second, RunPollInterval: time.Second, RequestTimeout: time.Second,
		},
		Kubernetes: config.Kubernetes{Kubeconfig: "/kubeconfig", RecoveryPollInterval: time.Second, ConflictRetries: 3},
	}
}

type fakeGitHubAdapter struct{}

func (*fakeGitHubAdapter) Dispatch(context.Context, *store.OrchestrationJob) error { return nil }

func (*fakeGitHubAdapter) ReconcileDispatch(context.Context, *store.OrchestrationJob) error {
	return nil
}

type fakeKubernetesAdapter struct{}

func (*fakeKubernetesAdapter) KubernetesClient() kubernetes.Interface {
	return kubernetesfake.NewSimpleClientset()
}

type clientlessKubernetesAdapter struct{}

func (*clientlessKubernetesAdapter) ExecuteDeploy(_ context.Context, lease store.DeployExecution) (store.DeployRecoveryResult, error) {
	return store.DeployRecoveryResult{Lease: lease}, nil
}

func (*clientlessKubernetesAdapter) RecoverExpiredDeploy(_ context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	return store.DeployRecoveryResult{Lease: lease}, nil
}

func (*clientlessKubernetesAdapter) ExecuteEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob) error {
	return nil
}

func (*fakeKubernetesAdapter) ExecuteDeploy(_ context.Context, lease store.DeployExecution) (store.DeployRecoveryResult, error) {
	return store.DeployRecoveryResult{Lease: lease}, nil
}

func (*fakeKubernetesAdapter) RecoverExpiredDeploy(_ context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	return store.DeployRecoveryResult{Lease: lease}, nil
}
func (*fakeKubernetesAdapter) ExecuteEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob) error {
	return nil
}

type countingGitHubAdapter struct{ calls int }

func (a *countingGitHubAdapter) Dispatch(context.Context, *store.OrchestrationJob) error {
	a.calls++
	return nil
}

func (a *countingGitHubAdapter) ReconcileDispatch(context.Context, *store.OrchestrationJob) error {
	a.calls++
	return nil
}

type countingKubernetesAdapter struct{ calls int }

func (*countingKubernetesAdapter) KubernetesClient() kubernetes.Interface {
	return kubernetesfake.NewSimpleClientset()
}

func (a *countingKubernetesAdapter) ExecuteDeploy(_ context.Context, lease store.DeployExecution) (store.DeployRecoveryResult, error) {
	a.calls++
	return store.DeployRecoveryResult{Lease: lease}, nil
}

func (a *countingKubernetesAdapter) RecoverExpiredDeploy(_ context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	a.calls++
	return store.DeployRecoveryResult{Lease: lease}, nil
}
func (a *countingKubernetesAdapter) ExecuteEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob) error {
	a.calls++
	return nil
}

type startupRecoveryStore struct {
	orgIDs  []string
	jobs    []*store.OrchestrationJob
	deploys []store.ExpiredDeploy
	lease   store.DeployLease
}

func (*startupRecoveryStore) DeleteExpiredIdempotencyRecords(context.Context, int) (int64, error) {
	return 0, nil
}

func (*startupRecoveryStore) RequeueExpiredOrchestrationLeases(context.Context) (int64, error) {
	return 0, nil
}
func (*startupRecoveryStore) RequeueExpiredLifecycleLeases(context.Context) (int64, error) {
	return 0, nil
}
func (*startupRecoveryStore) ListLifecycleOrgIDs(context.Context) ([]string, error) { return nil, nil }
func (*startupRecoveryStore) ClaimEnvironmentLifecycleJob(context.Context, string, string, time.Duration) (store.EnvironmentLifecycleJob, error) {
	return store.EnvironmentLifecycleJob{}, store.ErrNotFound
}
func (*startupRecoveryStore) RescheduleEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob, time.Duration, string, string) error {
	return nil
}
func (*startupRecoveryStore) ListIdlePreviewCandidates(context.Context, int) ([]store.IdlePreviewCandidate, error) {
	return nil, nil
}
func (*startupRecoveryStore) RequestIdlePreviewTeardown(context.Context, store.IdlePreviewCandidate) (bool, error) {
	return false, nil
}

func (*startupRecoveryStore) ClaimOrchestration(context.Context, string, string, time.Duration) (*store.OrchestrationJob, error) {
	return nil, store.ErrNotFound
}

func (*startupRecoveryStore) RescheduleOrchestration(context.Context, string, string, string, time.Duration) error {
	return nil
}

func (*startupRecoveryStore) RescheduleDispatchReconciliation(context.Context, string, string, string, time.Duration) error {
	return nil
}

func (*startupRecoveryStore) FailOrchestration(context.Context, store.FailOrchestrationParams) error {
	return nil
}

func (s *startupRecoveryStore) ListOrchestrationRecoveryOrgIDs(context.Context) ([]string, error) {
	return s.orgIDs, nil
}

func (*startupRecoveryStore) ListOrchestrationOrgIDs(context.Context) ([]string, error) {
	return nil, nil
}
func (*startupRecoveryStore) ListWebhookOrgIDs(context.Context) ([]string, error) {
	return nil, nil
}
func (*startupRecoveryStore) ClaimWebhookDelivery(context.Context, string, string, time.Duration) (store.WebhookDelivery, error) {
	return store.WebhookDelivery{}, store.ErrNotFound
}
func (*startupRecoveryStore) ListReconciliationOrgIDs(context.Context) ([]string, error) {
	return nil, nil
}
func (*startupRecoveryStore) ClaimReconciliationRun(context.Context, string, string, time.Duration) (*store.ReconciliationRun, error) {
	return nil, store.ErrNotFound
}

func (s *startupRecoveryStore) ClaimDispatchReconciliation(context.Context, string, string, time.Duration) (*store.OrchestrationJob, error) {
	if len(s.jobs) == 0 {
		return nil, store.ErrNotFound
	}
	job := s.jobs[0]
	s.jobs = s.jobs[1:]
	return job, nil
}

func (s *startupRecoveryStore) ListExpiredDeploys(context.Context) ([]store.ExpiredDeploy, error) {
	return s.deploys, nil
}

func (s *startupRecoveryStore) ClaimExpiredDeployLease(context.Context, string, string) (store.DeployLease, error) {
	return s.lease, nil
}

func (*startupRecoveryStore) RescheduleDeployRecovery(context.Context, store.DeployLease, time.Duration) error {
	return nil
}

func (*startupRecoveryStore) QuarantineDeployRecovery(context.Context, store.DeployLease, string, string) error {
	return nil
}

func (*startupRecoveryStore) ListForwardDeployOrgIDs(context.Context, bool) ([]string, error) {
	return nil, nil
}

func (*startupRecoveryStore) ClaimPendingDeploy(context.Context, string, []string) (store.DeployExecution, error) {
	return store.DeployExecution{}, store.ErrNotFound
}

func (*startupRecoveryStore) ClaimUnexpiredDeploying(context.Context, string, []string) (store.DeployExecution, error) {
	return store.DeployExecution{}, store.ErrNotFound
}

func TestRunLifecycleReadinessAndGracefulCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ready atomic.Bool
	api := &fakeLifecycleServer{started: make(chan struct{}), stopped: make(chan struct{})}
	recovery := &fakeLifecycleWorker{
		startup: func(context.Context) error {
			if ready.Load() {
				return errors.New("readiness became true before startup recovery completed")
			}
			return nil
		},
		run: func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- runLifecycle(ctx, api, recovery, &ready) }()
	<-api.started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runLifecycle() error = %v", err)
	}
	if ready.Load() {
		t.Fatal("readiness remained true after lifecycle shutdown")
	}
	select {
	case <-api.stopped:
	default:
		t.Fatal("API did not finish graceful shutdown")
	}
}

func TestRunLifecycleWorkerErrorCancelsAPI(t *testing.T) {
	want := errors.New("worker failed")
	var ready atomic.Bool
	api := &fakeLifecycleServer{started: make(chan struct{}), stopped: make(chan struct{})}
	recovery := &fakeLifecycleWorker{
		startup: func(context.Context) error { return nil },
		run:     func(context.Context) error { return want },
	}

	err := runLifecycle(context.Background(), api, recovery, &ready)
	if !errors.Is(err, want) {
		t.Fatalf("runLifecycle() error = %v, want %v", err, want)
	}
	if ready.Load() {
		t.Fatal("readiness remained true after worker failure")
	}
	select {
	case <-api.stopped:
	default:
		t.Fatal("worker error did not cancel API")
	}
}

func TestRunLifecycleStartupFailureCancelsAPIAndStaysUnready(t *testing.T) {
	want := &worker.BlockedRecoveryError{
		Capability: worker.GitHubRecoveryCapability,
		Kind:       "dispatch reconciliation",
	}
	var ready atomic.Bool
	api := &fakeLifecycleServer{started: make(chan struct{}), stopped: make(chan struct{})}
	recovery := &fakeLifecycleWorker{startup: func(context.Context) error { return want }}

	err := runLifecycle(context.Background(), api, recovery, &ready)
	if !errors.Is(err, want) {
		t.Fatalf("runLifecycle() error = %v, want %v", err, want)
	}
	if ready.Load() {
		t.Fatal("readiness became true after startup failure")
	}
	select {
	case <-api.stopped:
	default:
		t.Fatal("startup failure did not cancel API")
	}
}

func TestRunLifecycleBlockedPeriodicRecoveryFailsProcessAndCancelsAPI(t *testing.T) {
	want := &worker.BlockedRecoveryError{
		Capability: worker.KubernetesRecoveryCapability,
		Kind:       "expired deploy",
	}
	var ready atomic.Bool
	api := &fakeLifecycleServer{started: make(chan struct{}), stopped: make(chan struct{})}
	recovery := &fakeLifecycleWorker{
		startup: func(context.Context) error { return nil },
		run: func(context.Context) error {
			return want
		},
	}

	err := runLifecycle(context.Background(), api, recovery, &ready)
	if !errors.Is(err, want) {
		t.Fatalf("runLifecycle() error = %v, want %v", err, want)
	}
	if ready.Load() {
		t.Fatal("readiness remained true after periodic recovery failure")
	}
	select {
	case <-api.stopped:
	default:
		t.Fatal("periodic recovery failure did not cancel API")
	}
}

func TestRunLifecycleStartsAllWorkersBeforeReadiness(t *testing.T) {
	var ready atomic.Bool
	var mu sync.Mutex
	order := []string{}
	record := func(value string) { mu.Lock(); order = append(order, value); mu.Unlock() }
	api := &fakeLifecycleServer{started: make(chan struct{}), stopped: make(chan struct{})}
	eventRunStarted := make(chan struct{}, 1)
	recovery := &fakeLifecycleWorker{startup: func(context.Context) error { record("recovery startup"); return nil }, run: func(ctx context.Context) error { record("recovery run"); <-ctx.Done(); return nil }}
	events := &fakeLifecycleWorker{startup: func(context.Context) error {
		record("event startup")
		return nil
	}, run: func(ctx context.Context) error {
		eventRunStarted <- struct{}{}
		record("event run")
		<-ctx.Done()
		return nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runLifecycle(ctx, api, recovery, &ready, events) }()
	for !ready.Load() {
		runtime.Gosched()
	}
	<-eventRunStarted
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 4 || order[0] != "recovery startup" || order[1] != "event startup" {
		t.Fatalf("lifecycle order = %v", order)
	}
	if ready.Load() {
		t.Fatal("readiness remained true after shutdown")
	}
}

func TestRunLifecycleStartupRollbackWaitsForStartedRunner(t *testing.T) {
	want := errors.New("event startup failed")
	var ready atomic.Bool
	api := &fakeLifecycleServer{started: make(chan struct{}), stopped: make(chan struct{})}
	runStarted := make(chan struct{})
	runCanceled := make(chan struct{})
	drain := make(chan struct{})
	recovery := &fakeLifecycleWorker{
		startup: func(context.Context) error { return nil },
		run: func(ctx context.Context) error {
			close(runStarted)
			<-ctx.Done()
			close(runCanceled)
			<-drain
			return nil
		},
	}
	events := &fakeLifecycleWorker{startup: func(context.Context) error {
		<-runStarted
		return want
	}}
	done := make(chan error, 1)
	go func() { done <- runLifecycle(t.Context(), api, recovery, &ready, events) }()
	<-runCanceled
	select {
	case err := <-done:
		t.Fatalf("runLifecycle returned before recovery drained: %v", err)
	default:
	}
	close(drain)
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("runLifecycle() error = %v, want %v", err, want)
	}
	if ready.Load() {
		t.Fatal("readiness became true during startup rollback")
	}
	select {
	case <-api.stopped:
	default:
		t.Fatal("startup rollback did not wait for API shutdown")
	}
}

func TestRunLifecycleAdditionalStartupFailurePreventsReadyAndCancelsAPI(t *testing.T) {
	want := errors.New("event database unavailable")
	var ready atomic.Bool
	api := &fakeLifecycleServer{started: make(chan struct{}), stopped: make(chan struct{})}
	recovery := &fakeLifecycleWorker{}
	events := &fakeLifecycleWorker{startup: func(context.Context) error { return want }}
	err := runLifecycle(t.Context(), api, recovery, &ready, events)
	if !errors.Is(err, want) || ready.Load() {
		t.Fatalf("runLifecycle() = %v ready=%t", err, ready.Load())
	}
	select {
	case <-api.stopped:
	default:
		t.Fatal("event startup failure did not stop API")
	}
}

func TestRunLifecycleOneRunnerErrorCancelsAPIAndSibling(t *testing.T) {
	want := errors.New("event scan unavailable")
	var ready atomic.Bool
	siblingStopped := make(chan struct{})
	api := &fakeLifecycleServer{started: make(chan struct{}), stopped: make(chan struct{})}
	recovery := &fakeLifecycleWorker{run: func(ctx context.Context) error { <-ctx.Done(); close(siblingStopped); return nil }}
	events := &fakeLifecycleWorker{run: func(context.Context) error { return want }}
	err := runLifecycle(t.Context(), api, recovery, &ready, events)
	if !errors.Is(err, want) || ready.Load() {
		t.Fatalf("runLifecycle() = %v ready=%t", err, ready.Load())
	}
	select {
	case <-api.stopped:
	default:
		t.Fatal("runner error did not stop API")
	}
	select {
	case <-siblingStopped:
	default:
		t.Fatal("runner error did not cancel sibling")
	}
}

type fakeLifecycleServer struct {
	started chan struct{}
	stopped chan struct{}
}

func (s *fakeLifecycleServer) Run(ctx context.Context) error {
	close(s.started)
	<-ctx.Done()
	close(s.stopped)
	return nil
}

type fakeLifecycleWorker struct {
	startup func(context.Context) error
	run     func(context.Context) error
}

func (w *fakeLifecycleWorker) Startup(ctx context.Context) error {
	if w.startup == nil {
		return nil
	}
	return w.startup(ctx)
}

func (w *fakeLifecycleWorker) Run(ctx context.Context) error {
	if w.run == nil {
		<-ctx.Done()
		return nil
	}
	return w.run(ctx)
}
