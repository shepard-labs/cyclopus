package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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
				t.Fatal("readiness became true before startup recovery completed")
			}
			return nil
		},
		run: func(ctx context.Context) error {
			if !ready.Load() {
				t.Fatal("readiness remained false after startup recovery")
			}
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
			if !ready.Load() {
				t.Fatal("readiness remained false after successful startup recovery")
			}
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
