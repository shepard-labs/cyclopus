package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
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
	runner := New(Config{ScanInterval: time.Millisecond, LeaseDuration: time.Second, MaxConcurrentDeploys: 4, RequestTimeout: time.Millisecond, Owner: "worker"},
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
	runner := New(Config{ScanInterval: time.Millisecond, LeaseDuration: time.Second, MaxConcurrentDeploys: 4, RequestTimeout: time.Millisecond, Owner: "worker"},
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
	return New(Config{ScanInterval: time.Second, LeaseDuration: time.Minute, MaxConcurrentDeploys: 4, RequestTimeout: time.Second, Owner: "worker-one"},
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

func (f *fakeKubernetesRecoveryAdapter) ExecuteDeploy(_ context.Context, lease store.DeployExecution) (store.DeployRecoveryResult, error) {
	f.calls++
	return store.DeployRecoveryResult{Lease: lease}, f.err
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
	resumeOrgIDs          []string
	pendingDeployOrgIDs   []string
	resumeDeploys         []store.DeployExecution
	pendingDeploys        []store.DeployExecution
	forwardClaims         int
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
func (*fakeRecoveryStore) RequeueExpiredLifecycleLeases(context.Context) (int64, error) {
	return 0, nil
}
func (*fakeRecoveryStore) ListLifecycleOrgIDs(context.Context) ([]string, error) { return nil, nil }
func (*fakeRecoveryStore) ClaimEnvironmentLifecycleJob(context.Context, string, string, time.Duration) (store.EnvironmentLifecycleJob, error) {
	return store.EnvironmentLifecycleJob{}, store.ErrNotFound
}
func (*fakeRecoveryStore) RescheduleEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob, time.Duration, string, string) error {
	return nil
}
func (*fakeRecoveryStore) ListIdlePreviewCandidates(context.Context, int) ([]store.IdlePreviewCandidate, error) {
	return nil, nil
}
func (*fakeRecoveryStore) RequestIdlePreviewTeardown(context.Context, store.IdlePreviewCandidate) (bool, error) {
	return false, nil
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

func (f *fakeRecoveryStore) ListForwardDeployOrgIDs(_ context.Context, resumeOnly bool) ([]string, error) {
	if resumeOnly {
		return f.resumeOrgIDs, nil
	}
	return f.pendingDeployOrgIDs, nil
}

func (f *fakeRecoveryStore) ClaimPendingDeploy(context.Context, string, []string) (store.DeployExecution, error) {
	f.forwardClaims++
	if len(f.pendingDeploys) == 0 {
		return store.DeployExecution{}, store.ErrNotFound
	}
	execution := f.pendingDeploys[0]
	f.pendingDeploys = f.pendingDeploys[1:]
	return execution, nil
}

func (f *fakeRecoveryStore) ClaimUnexpiredDeploying(context.Context, string, []string) (store.DeployExecution, error) {
	f.forwardClaims++
	if len(f.resumeDeploys) == 0 {
		return store.DeployExecution{}, store.ErrNotFound
	}
	execution := f.resumeDeploys[0]
	f.resumeDeploys = f.resumeDeploys[1:]
	return execution, nil
}

func TestStartupCrashResumeRunsUnexpiredDeployBeforeExpiredLeaseRecovery(t *testing.T) {
	order := []string{}
	kubernetes := &orderedKubernetesAdapter{order: &order}
	fake := &fakeRecoveryStore{
		resumeOrgIDs:  []string{"org-one"},
		resumeDeploys: []store.DeployExecution{{OrgID: "org-one", EnvironmentID: "active", ReleaseID: "release-active"}},
		deploys:       []store.ExpiredDeploy{{OrgID: "org-one", EnvironmentID: "expired"}},
		deployLease:   store.DeployLease{OrgID: "org-one", EnvironmentID: "expired", ReleaseID: "release-expired"},
	}
	runner := newTestRunner(fake, RecoveryAdapters{Capabilities: RecoveryCapabilities{Kubernetes: true}, Kubernetes: kubernetes})
	if err := runner.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "forward:active" || order[1] != "recovery:expired" {
		t.Fatalf("execution order = %#v", order)
	}
}

func TestStartupSweepStabilizesDeploysBeforeGitHubReconciliationAndNewDispatch(t *testing.T) {
	var orderMu sync.Mutex
	order := []string{}
	kubernetes := &blockingOrderKubernetes{record: func(value string) { orderMu.Lock(); order = append(order, value); orderMu.Unlock() }}
	github := &orderedGitHubAdapter{record: kubernetes.record}
	fake := &fakeRecoveryStore{
		resumeOrgIDs: []string{"org-one"}, resumeDeploys: []store.DeployExecution{{OrgID: "org-one", EnvironmentID: "active", ReleaseID: "active-release"}},
		deploys: []store.ExpiredDeploy{{OrgID: "org-one", EnvironmentID: "expired"}}, deployLease: store.DeployLease{OrgID: "org-one", EnvironmentID: "expired", ReleaseID: "expired-release"},
		orgIDs: []string{"org-one"}, orchestrationJobs: []*store.OrchestrationJob{{ID: "reconcile", OrgID: "org-one"}},
		queuedOrgIDs: []string{"org-one"}, queuedJobs: []*store.OrchestrationJob{{ID: "dispatch", OrgID: "org-one"}},
	}
	runner := newTestRunner(fake, RecoveryAdapters{Capabilities: RecoveryCapabilities{Kubernetes: true, GitHub: true}, Kubernetes: kubernetes, GitHub: github})
	if err := runner.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	want := []string{"forward:active", "recovery:expired", "reconcile:reconcile", "dispatch:dispatch"}
	if len(order) != len(want) {
		t.Fatalf("order=%#v", order)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("order=%#v want=%#v", order, want)
		}
	}
}

func TestDeployTasksSuppressDuplicateClaimsAndAllowUnrelatedScanProgressRace(t *testing.T) {
	deployStarted := make(chan struct{})
	releaseDeploy := make(chan struct{})
	kubernetes := &blockingDeployAdapter{started: deployStarted, release: releaseDeploy}
	claimStore := &concurrentForwardStore{execution: store.DeployExecution{OrgID: "org", EnvironmentID: "environment", ReleaseID: "release"}}
	runner := newTestRunner(claimStore, RecoveryAdapters{Capabilities: RecoveryCapabilities{Kubernetes: true}, Kubernetes: kubernetes})
	if err := runner.scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-deployStarted
	done := make(chan error, 1)
	go func() { done <- runner.scan(context.Background(), false) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated scan blocked on rollout observation")
	}
	if got := claimStore.successfulClaims.Load(); got != 1 {
		t.Fatalf("successful claims=%d", got)
	}
	close(releaseDeploy)
	runner.deployWG.Wait()
}

func TestPermanentForwardErrorCompletesTaskAndLaterOrganizationWorkProceeds(t *testing.T) {
	kubernetes := &sequenceDeployAdapter{errors: []error{errors.New("permanent"), nil}}
	fake := &fakeRecoveryStore{pendingDeployOrgIDs: []string{"org-one", "org-two"}, pendingDeploys: []store.DeployExecution{{OrgID: "org-one", EnvironmentID: "bad", ReleaseID: "one"}, {OrgID: "org-two", EnvironmentID: "good", ReleaseID: "two"}}}
	runner := newTestRunner(fake, RecoveryAdapters{Capabilities: RecoveryCapabilities{Kubernetes: true}, Kubernetes: kubernetes})
	if err := runner.scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	runner.deployWG.Wait()
	if kubernetes.calls.Load() != 2 {
		t.Fatalf("deploy calls=%d", kubernetes.calls.Load())
	}
}

func TestPeriodicDeployClaimsPendingReleaseAndExecutesWithoutPool(t *testing.T) {
	kubernetes := &orderedKubernetesAdapter{order: &[]string{}}
	fake := &fakeRecoveryStore{
		pendingDeployOrgIDs: []string{"org-one"},
		pendingDeploys:      []store.DeployExecution{{OrgID: "org-one", EnvironmentID: "pending", ReleaseID: "release"}},
	}
	runner := newTestRunner(fake, RecoveryAdapters{Capabilities: RecoveryCapabilities{Kubernetes: true}, Kubernetes: kubernetes})
	if err := runner.scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	runner.deployWG.Wait()
	if fake.forwardClaims != 2 || kubernetes.forwardCalls != 1 {
		t.Fatalf("claims=%d executions=%d", fake.forwardClaims, kubernetes.forwardCalls)
	}
}

type orderedKubernetesAdapter struct {
	order        *[]string
	forwardCalls int
}

type blockingOrderKubernetes struct{ record func(string) }

func (a *blockingOrderKubernetes) ExecuteDeploy(_ context.Context, execution store.DeployExecution) (store.DeployRecoveryResult, error) {
	a.record("forward:" + execution.EnvironmentID)
	return store.DeployRecoveryResult{Lease: execution}, nil
}
func (a *blockingOrderKubernetes) RecoverExpiredDeploy(_ context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	a.record("recovery:" + lease.EnvironmentID)
	return store.DeployRecoveryResult{Lease: lease}, nil
}

type orderedGitHubAdapter struct{ record func(string) }

func (a *orderedGitHubAdapter) Dispatch(_ context.Context, job *store.OrchestrationJob) error {
	a.record("dispatch:" + job.ID)
	return nil
}
func (a *orderedGitHubAdapter) ReconcileDispatch(_ context.Context, job *store.OrchestrationJob) error {
	a.record("reconcile:" + job.ID)
	return nil
}

type blockingDeployAdapter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingDeployAdapter) ExecuteDeploy(ctx context.Context, execution store.DeployExecution) (store.DeployRecoveryResult, error) {
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
		return store.DeployRecoveryResult{Lease: execution}, nil
	case <-ctx.Done():
		return store.DeployRecoveryResult{Lease: execution}, ctx.Err()
	}
}
func (*blockingDeployAdapter) RecoverExpiredDeploy(_ context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	return store.DeployRecoveryResult{Lease: lease}, nil
}

type sequenceDeployAdapter struct {
	calls  atomic.Int32
	mu     sync.Mutex
	errors []error
}

func (a *sequenceDeployAdapter) ExecuteDeploy(_ context.Context, execution store.DeployExecution) (store.DeployRecoveryResult, error) {
	a.calls.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if len(a.errors) > 0 {
		err = a.errors[0]
		a.errors = a.errors[1:]
	}
	return store.DeployRecoveryResult{Lease: execution}, err
}
func (*sequenceDeployAdapter) RecoverExpiredDeploy(_ context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	return store.DeployRecoveryResult{Lease: lease}, nil
}

type concurrentForwardStore struct {
	successfulClaims atomic.Int32
	claimed          atomic.Bool
	execution        store.DeployExecution
}

func (*concurrentForwardStore) DeleteExpiredIdempotencyRecords(context.Context, int) (int64, error) {
	return 0, nil
}
func (*concurrentForwardStore) RequeueExpiredOrchestrationLeases(context.Context) (int64, error) {
	return 0, nil
}
func (*concurrentForwardStore) RequeueExpiredLifecycleLeases(context.Context) (int64, error) {
	return 0, nil
}
func (*concurrentForwardStore) ListLifecycleOrgIDs(context.Context) ([]string, error) {
	return nil, nil
}
func (*concurrentForwardStore) ClaimEnvironmentLifecycleJob(context.Context, string, string, time.Duration) (store.EnvironmentLifecycleJob, error) {
	return store.EnvironmentLifecycleJob{}, store.ErrNotFound
}
func (*concurrentForwardStore) RescheduleEnvironmentLifecycle(context.Context, store.EnvironmentLifecycleJob, time.Duration, string, string) error {
	return nil
}
func (*concurrentForwardStore) ListIdlePreviewCandidates(context.Context, int) ([]store.IdlePreviewCandidate, error) {
	return nil, nil
}
func (*concurrentForwardStore) RequestIdlePreviewTeardown(context.Context, store.IdlePreviewCandidate) (bool, error) {
	return false, nil
}
func (*concurrentForwardStore) ListOrchestrationOrgIDs(context.Context) ([]string, error) {
	return nil, nil
}
func (*concurrentForwardStore) ListOrchestrationRecoveryOrgIDs(context.Context) ([]string, error) {
	return nil, nil
}
func (*concurrentForwardStore) ClaimOrchestration(context.Context, string, string, time.Duration) (*store.OrchestrationJob, error) {
	return nil, store.ErrNotFound
}
func (*concurrentForwardStore) ClaimDispatchReconciliation(context.Context, string, string, time.Duration) (*store.OrchestrationJob, error) {
	return nil, store.ErrNotFound
}
func (*concurrentForwardStore) RescheduleOrchestration(context.Context, string, string, string, time.Duration) error {
	return nil
}
func (*concurrentForwardStore) RescheduleDispatchReconciliation(context.Context, string, string, string, time.Duration) error {
	return nil
}
func (*concurrentForwardStore) FailOrchestration(context.Context, store.FailOrchestrationParams) error {
	return nil
}
func (*concurrentForwardStore) ListExpiredDeploys(context.Context) ([]store.ExpiredDeploy, error) {
	return nil, nil
}
func (*concurrentForwardStore) ClaimExpiredDeployLease(context.Context, string, string) (store.DeployLease, error) {
	return store.DeployLease{}, store.ErrNotFound
}
func (*concurrentForwardStore) RescheduleDeployRecovery(context.Context, store.DeployLease, time.Duration) error {
	return nil
}
func (*concurrentForwardStore) QuarantineDeployRecovery(context.Context, store.DeployLease, string, string) error {
	return nil
}
func (*concurrentForwardStore) ListForwardDeployOrgIDs(_ context.Context, resume bool) ([]string, error) {
	if resume {
		return nil, nil
	}
	return []string{"org"}, nil
}
func (s *concurrentForwardStore) ClaimPendingDeploy(_ context.Context, _ string, excluded []string) (store.DeployExecution, error) {
	for _, id := range excluded {
		if id == s.execution.EnvironmentID {
			return store.DeployExecution{}, store.ErrNotFound
		}
	}
	if !s.claimed.CompareAndSwap(false, true) {
		return store.DeployExecution{}, store.ErrNotFound
	}
	s.successfulClaims.Add(1)
	return s.execution, nil
}
func (*concurrentForwardStore) ClaimUnexpiredDeploying(context.Context, string, []string) (store.DeployExecution, error) {
	return store.DeployExecution{}, store.ErrNotFound
}

func (a *orderedKubernetesAdapter) ExecuteDeploy(_ context.Context, execution store.DeployExecution) (store.DeployRecoveryResult, error) {
	a.forwardCalls++
	*a.order = append(*a.order, "forward:"+execution.EnvironmentID)
	return store.DeployRecoveryResult{Lease: execution}, nil
}

func (a *orderedKubernetesAdapter) RecoverExpiredDeploy(_ context.Context, lease store.DeployLease) (store.DeployRecoveryResult, error) {
	*a.order = append(*a.order, "recovery:"+lease.EnvironmentID)
	return store.DeployRecoveryResult{Lease: lease}, nil
}
