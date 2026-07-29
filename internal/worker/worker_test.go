package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/store"
)

func TestStartupBlocksDispatchRecoveryWithoutGitHubAdapterBeforeClaiming(t *testing.T) {
	fake := &fakeRecoveryStore{
		orgIDs:  []string{"org-one"},
		deploys: []store.ExpiredDeploy{{OrgID: "org-one", EnvironmentID: "environment-one"}},
	}
	runner := newTestRunner(fake, RecoveryAdapters{})

	err := runner.Startup(context.Background())
	assertBlockedRecovery(t, err, GitHubRecoveryCapability, "dispatch reconciliation")
	if fake.orchestrationClaims != 0 || fake.deployClaims != 0 {
		t.Fatalf("claims = orchestration:%d deploy:%d, want no claim mutations", fake.orchestrationClaims, fake.deployClaims)
	}
}

func TestStartupBlocksDeployRecoveryWithoutKubernetesAdapterBeforeClaiming(t *testing.T) {
	fake := &fakeRecoveryStore{
		orgIDs:  []string{"org-one"},
		deploys: []store.ExpiredDeploy{{OrgID: "org-one", EnvironmentID: "environment-one"}},
	}
	runner := newTestRunner(fake, RecoveryAdapters{
		Capabilities: RecoveryCapabilities{GitHub: true},
		GitHub:       &fakeGitHubRecoveryAdapter{},
	})

	err := runner.Startup(context.Background())
	assertBlockedRecovery(t, err, KubernetesRecoveryCapability, "expired deploy")
	if fake.orchestrationClaims != 0 || fake.deployClaims != 0 {
		t.Fatalf("claims = orchestration:%d deploy:%d, want preflight to prevent all claim mutations", fake.orchestrationClaims, fake.deployClaims)
	}
}

func TestStartupBlocksRecoveryWhenCapabilityFlagIsDisabled(t *testing.T) {
	fake := &fakeRecoveryStore{orgIDs: []string{"org-one"}}
	runner := newTestRunner(fake, RecoveryAdapters{GitHub: &fakeGitHubRecoveryAdapter{}})

	err := runner.Startup(context.Background())
	assertBlockedRecovery(t, err, GitHubRecoveryCapability, "dispatch reconciliation")
	if fake.orchestrationClaims != 0 {
		t.Fatalf("orchestration claims = %d, want 0", fake.orchestrationClaims)
	}
}

func TestStartupFreshSystemSucceedsWithoutAdapters(t *testing.T) {
	fake := &fakeRecoveryStore{requeued: 2}
	runner := newTestRunner(fake, RecoveryAdapters{})

	if err := runner.Startup(context.Background()); err != nil {
		t.Fatalf("Startup() error = %v", err)
	}
	if fake.orchestrationClaims != 0 || fake.deployClaims != 0 {
		t.Fatalf("claims = orchestration:%d deploy:%d, want 0", fake.orchestrationClaims, fake.deployClaims)
	}
}

func TestStartupUsesAvailableRecoveryAdapters(t *testing.T) {
	github := &fakeGitHubRecoveryAdapter{}
	kubernetes := &fakeKubernetesRecoveryAdapter{}
	fake := &fakeRecoveryStore{
		orgIDs:            []string{"org-one"},
		orchestrationJobs: []*store.OrchestrationJob{{ID: "job-one", OrgID: "org-one", Status: "dispatched"}},
		deploys:           []store.ExpiredDeploy{{OrgID: "org-one", EnvironmentID: "environment-one"}},
		deployLease:       store.DeployLease{OrgID: "org-one", EnvironmentID: "environment-one", ReleaseID: "release-one"},
	}
	runner := newTestRunner(fake, RecoveryAdapters{
		Capabilities: RecoveryCapabilities{GitHub: true, Kubernetes: true},
		GitHub:       github,
		Kubernetes:   kubernetes,
	})

	if err := runner.Startup(context.Background()); err != nil {
		t.Fatalf("Startup() error = %v", err)
	}
	if fake.orchestrationClaims != 2 {
		t.Fatalf("orchestration claims = %d, want job plus terminal not-found claim", fake.orchestrationClaims)
	}
	if fake.deployClaims != 1 || github.calls != 1 || kubernetes.calls != 1 {
		t.Fatalf("calls = deploy claims:%d github:%d kubernetes:%d, want 1 each", fake.deployClaims, github.calls, kubernetes.calls)
	}
	if fake.lastOwner != "worker-one" || fake.lastLeaseDuration != time.Minute {
		t.Fatalf("claim config = %q/%s", fake.lastOwner, fake.lastLeaseDuration)
	}
}

func TestStartupQuarantinesKubernetesCompensationError(t *testing.T) {
	want := errors.New("compensation unresolved")
	latest := store.DeployLease{OrgID: "org-one", EnvironmentID: "environment-one", ReleaseID: "release-one", Fence: time.Unix(2, 0)}
	fake := &fakeRecoveryStore{
		deploys:     []store.ExpiredDeploy{{OrgID: "org-one", EnvironmentID: "environment-one"}},
		deployLease: store.DeployLease{OrgID: "org-one", EnvironmentID: "environment-one", ReleaseID: "release-one", Fence: time.Unix(1, 0)},
	}
	runner := newTestRunner(fake, RecoveryAdapters{
		Capabilities: RecoveryCapabilities{Kubernetes: true},
		Kubernetes:   &fakeKubernetesRecoveryAdapter{err: want, result: store.DeployRecoveryResult{Lease: latest}},
		KubernetesErrorDisposition: func(error) (bool, time.Duration, string) {
			return false, 0, "compensation_unproven"
		},
	})

	err := runner.Startup(context.Background())
	if err != nil || fake.rescheduleDeployCalls != 0 || fake.quarantineDeployCalls != 1 || !fake.quarantineLease.Fence.Equal(latest.Fence) {
		t.Fatalf("Startup() error = %v, reschedules = %d quarantines=%d", err, fake.rescheduleDeployCalls, fake.quarantineDeployCalls)
	}
}

func TestPeriodicRetryableKubernetesFailureIsRescheduledPerItem(t *testing.T) {
	want := errors.New("temporary cluster failure")
	latest := store.DeployLease{OrgID: "org-one", EnvironmentID: "environment-one", ReleaseID: "release-one", Fence: time.Unix(2, 0)}
	fake := &fakeRecoveryStore{
		deploys:     []store.ExpiredDeploy{{OrgID: "org-one", EnvironmentID: "environment-one"}},
		deployLease: store.DeployLease{OrgID: "org-one", EnvironmentID: "environment-one", ReleaseID: "release-one", Fence: time.Unix(1, 0)},
	}
	runner := newTestRunner(fake, RecoveryAdapters{
		Capabilities: RecoveryCapabilities{Kubernetes: true},
		Kubernetes:   &fakeKubernetesRecoveryAdapter{err: want, result: store.DeployRecoveryResult{Lease: latest}},
		KubernetesErrorDisposition: func(error) (bool, time.Duration, string) {
			return true, 3 * time.Second, ""
		},
	})

	if err := runner.scan(context.Background(), false); err != nil {
		t.Fatalf("scan() error = %v", err)
	}
	if fake.rescheduleDeployCalls != 1 || fake.rescheduleDelay != 3*time.Second || !fake.rescheduleLease.Fence.Equal(latest.Fence) {
		t.Fatalf("reschedule = %d/%s", fake.rescheduleDeployCalls, fake.rescheduleDelay)
	}
}

func TestPeriodicPermanentKubernetesFailureQuarantinesAndContinues(t *testing.T) {
	want := errors.New("invalid recovery state")
	fake := &fakeRecoveryStore{
		deploys: []store.ExpiredDeploy{
			{OrgID: "org-one", EnvironmentID: "environment-one"},
			{OrgID: "org-one", EnvironmentID: "environment-two"},
		},
		deployLease: store.DeployLease{OrgID: "org-one", EnvironmentID: "environment-one", ReleaseID: "release-one"},
	}
	runner := newTestRunner(fake, RecoveryAdapters{
		Capabilities: RecoveryCapabilities{Kubernetes: true},
		Kubernetes:   &fakeKubernetesRecoveryAdapter{errs: []error{want, nil}},
		KubernetesErrorDisposition: func(error) (bool, time.Duration, string) {
			return false, 0, "recovery_state_invalid"
		},
	})

	err := runner.scan(context.Background(), false)
	if err != nil || fake.deployClaims != 2 || fake.rescheduleDeployCalls != 0 || fake.quarantineDeployCalls != 1 || fake.quarantineCode != "recovery_state_invalid" {
		t.Fatalf("scan() error = %v, claims=%d reschedules=%d quarantines=%d/%s", err, fake.deployClaims, fake.rescheduleDeployCalls, fake.quarantineDeployCalls, fake.quarantineCode)
	}
}

func TestStartupDispatchesQueuedProductionWork(t *testing.T) {
	github := &fakeGitHubRecoveryAdapter{}
	fake := &fakeRecoveryStore{
		queuedOrgIDs: []string{"org-one"},
		queuedJobs: []*store.OrchestrationJob{{
			ID: "job-one", OrgID: "org-one", Status: "leased", GitRef: "refs/heads/main",
		}},
	}
	runner := newTestRunner(fake, RecoveryAdapters{
		Capabilities: RecoveryCapabilities{GitHub: true}, GitHub: github,
	})

	if err := runner.Startup(context.Background()); err != nil {
		t.Fatalf("Startup() error = %v", err)
	}
	if github.dispatchCalls != 1 || github.reconcileCalls != 0 {
		t.Fatalf("GitHub calls = dispatch:%d reconcile:%d", github.dispatchCalls, github.reconcileCalls)
	}
	if fake.queuedClaims != 2 {
		t.Fatalf("queued claims = %d, want job plus not-found claim", fake.queuedClaims)
	}
}

func TestRunReturnsBlockedRecoveryDiscoveredPeriodicallyWithoutClaiming(t *testing.T) {
	fake := &fakeRecoveryStore{orgIDs: []string{"org-one"}}
	runner := New(Config{ScanInterval: time.Millisecond, LeaseDuration: time.Second, RequestTimeout: time.Millisecond, Owner: "worker"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), fake, RecoveryAdapters{})

	err := runner.Run(context.Background())
	assertBlockedRecovery(t, err, GitHubRecoveryCapability, "dispatch reconciliation")
	if fake.orchestrationClaims != 0 {
		t.Fatalf("orchestration claims = %d, want 0", fake.orchestrationClaims)
	}
}

func TestRunReturnsScanError(t *testing.T) {
	want := errors.New("database unavailable")
	fake := &fakeRecoveryStore{requeueErr: want}
	runner := New(Config{ScanInterval: time.Millisecond, LeaseDuration: time.Second, RequestTimeout: time.Millisecond, Owner: "worker"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), fake, RecoveryAdapters{})

	err := runner.Run(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want %v", err, want)
	}
}

func newTestRunner(recoveryStore RecoveryStore, adapters RecoveryAdapters) *Runner {
	if adapters.GitHub != nil && adapters.GitHubErrorDisposition == nil {
		adapters.GitHubErrorDisposition = func(error) (bool, time.Duration, string, bool) { return false, 0, "test_failure", true }
	}
	return New(Config{ScanInterval: time.Second, LeaseDuration: time.Minute, RequestTimeout: time.Second, Owner: "worker-one"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), recoveryStore, adapters)
}

func assertBlockedRecovery(t *testing.T, err error, capability RecoveryCapability, kind string) {
	t.Helper()
	var blocked *BlockedRecoveryError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want *BlockedRecoveryError", err)
	}
	if blocked.Capability != capability || blocked.Kind != kind {
		t.Fatalf("blocked error = %#v, want capability %q kind %q", blocked, capability, kind)
	}
}

type fakeGitHubRecoveryAdapter struct {
	calls          int
	dispatchCalls  int
	reconcileCalls int
	err            error
}

func (f *fakeGitHubRecoveryAdapter) ErrorDisposition(error) (bool, time.Duration, string, bool) {
	return false, 0, "", false
}

func (f *fakeGitHubRecoveryAdapter) Dispatch(context.Context, *store.OrchestrationJob) error {
	f.calls++
	f.dispatchCalls++
	return f.err
}

func (f *fakeGitHubRecoveryAdapter) ReconcileDispatch(context.Context, *store.OrchestrationJob) error {
	f.calls++
	f.reconcileCalls++
	return f.err
}

type fakeKubernetesRecoveryAdapter struct {
	calls  int
	err    error
	errs   []error
	result store.DeployRecoveryResult
}

func (f *fakeKubernetesRecoveryAdapter) RecoverExpiredDeploy(_ context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	f.calls++
	result := f.result
	if result.Lease.OrgID == "" {
		result.Lease = lease
	}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return result, err
	}
	return result, f.err
}

type fakeRecoveryStore struct {
	idempotencyDeleted    int64
	idempotencyCleanupErr error
	requeued              int64
	requeueErr            error
	orgIDs                []string
	queuedOrgIDs          []string
	queuedJobs            []*store.OrchestrationJob
	queuedClaims          int
	orchestrationJobs     []*store.OrchestrationJob
	orchestrationClaims   int
	deploys               []store.ExpiredDeploy
	deployLease           store.DeployLease
	deployClaims          int
	lastOwner             string
	lastLeaseDuration     time.Duration
	rescheduleDeployCalls int
	rescheduleDelay       time.Duration
	rescheduleLease       store.DeployLease
	quarantineDeployCalls int
	quarantineCode        string
	quarantineLease       store.DeployLease
}

func (f *fakeRecoveryStore) DeleteExpiredIdempotencyRecords(context.Context, int) (int64, error) {
	return f.idempotencyDeleted, f.idempotencyCleanupErr
}

func (f *fakeRecoveryStore) ClaimOrchestration(context.Context, string, string, time.Duration) (*store.OrchestrationJob, error) {
	f.queuedClaims++
	if len(f.queuedJobs) == 0 {
		return nil, store.ErrNotFound
	}
	job := f.queuedJobs[0]
	f.queuedJobs = f.queuedJobs[1:]
	return job, nil
}

func (f *fakeRecoveryStore) RescheduleOrchestration(context.Context, string, string, string, time.Duration) error {
	return nil
}

func (f *fakeRecoveryStore) RescheduleDispatchReconciliation(context.Context, string, string, string, time.Duration) error {
	return nil
}

func (f *fakeRecoveryStore) FailOrchestration(context.Context, store.FailOrchestrationParams) error {
	return nil
}

func (f *fakeRecoveryStore) RequeueExpiredOrchestrationLeases(context.Context) (int64, error) {
	return f.requeued, f.requeueErr
}

func (f *fakeRecoveryStore) ListOrchestrationRecoveryOrgIDs(context.Context) ([]string, error) {
	return f.orgIDs, nil
}

func (f *fakeRecoveryStore) ListOrchestrationOrgIDs(context.Context) ([]string, error) {
	return f.queuedOrgIDs, nil
}

func (f *fakeRecoveryStore) ClaimDispatchReconciliation(_ context.Context, _ string, owner string, leaseDuration time.Duration) (*store.OrchestrationJob, error) {
	f.orchestrationClaims++
	f.lastOwner = owner
	f.lastLeaseDuration = leaseDuration
	if len(f.orchestrationJobs) == 0 {
		return nil, store.ErrNotFound
	}
	job := f.orchestrationJobs[0]
	f.orchestrationJobs = f.orchestrationJobs[1:]
	return job, nil
}

func (f *fakeRecoveryStore) ListExpiredDeploys(context.Context) ([]store.ExpiredDeploy, error) {
	return f.deploys, nil
}

func (f *fakeRecoveryStore) ClaimExpiredDeployLease(context.Context, string, string) (store.DeployLease, error) {
	f.deployClaims++
	return f.deployLease, nil
}

func (f *fakeRecoveryStore) RescheduleDeployRecovery(_ context.Context, lease store.DeployLease, delay time.Duration) error {
	f.rescheduleDeployCalls++
	f.rescheduleDelay = delay
	f.rescheduleLease = lease
	return nil
}

func (f *fakeRecoveryStore) QuarantineDeployRecovery(_ context.Context, lease store.DeployLease, code, _ string) error {
	f.quarantineDeployCalls++
	f.quarantineCode = code
	f.quarantineLease = lease
	return nil
}
