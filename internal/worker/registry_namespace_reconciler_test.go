package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/kubernetes"
	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/ocimetrics"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type namespaceStoreFake struct {
	mu                         sync.Mutex
	claims                     []store.NamespaceCredentialClaim
	cleanupClaims              []store.NamespaceCredentialCleanupClaim
	recovered                  int
	cleanupRecovered           int
	checkpoints                []string
	completed                  int
	rescheduled                []time.Duration
	failed                     []string
	cleanupCompleted           int
	cleanupRescheduled         []time.Duration
	cleanupFailed              []string
	cleanupCompleteErr         error
	checkpointErr, completeErr error
	orgIDs                     []string
	listCursors                []string
	claimOrder                 []string
}

func (f *namespaceStoreFake) RecoverExpiredNamespaceCredentialCleanupLeases(context.Context, int) (int64, error) {
	f.mu.Lock()
	f.cleanupRecovered++
	f.mu.Unlock()
	return 0, nil
}

func (f *namespaceStoreFake) RecoverExpiredNamespaceCredentialLeases(context.Context, int) (int64, error) {
	f.mu.Lock()
	f.recovered++
	f.mu.Unlock()
	return 0, nil
}
func (f *namespaceStoreFake) ListNamespaceCredentialOrgIDs(_ context.Context, afterOrgID string, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCursors = append(f.listCursors, afterOrgID)
	if f.orgIDs != nil {
		var orgIDs []string
		for _, orgID := range f.orgIDs {
			if orgID > afterOrgID && len(orgIDs) < limit {
				orgIDs = append(orgIDs, orgID)
			}
		}
		return orgIDs, nil
	}
	if len(f.claims) == 0 && len(f.cleanupClaims) == 0 {
		return nil, nil
	}
	return []string{"org"}, nil
}
func (f *namespaceStoreFake) ClaimNamespaceCredentialCleanups(_ context.Context, orgID, _ string, _ time.Duration, limit int) ([]store.NamespaceCredentialCleanupClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimOrder = append(f.claimOrder, "cleanup:"+orgID)
	var claims []store.NamespaceCredentialCleanupClaim
	for i := 0; i < len(f.cleanupClaims) && len(claims) < limit; {
		if f.cleanupClaims[i].OrgID != orgID {
			i++
			continue
		}
		claims = append(claims, f.cleanupClaims[i])
		f.cleanupClaims = append(f.cleanupClaims[:i], f.cleanupClaims[i+1:]...)
	}
	return claims, nil
}
func (f *namespaceStoreFake) ClaimNamespaceCredentials(_ context.Context, orgID string, _ string, _ time.Duration, limit int) ([]store.NamespaceCredentialClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimOrder = append(f.claimOrder, "reconcile:"+orgID)
	var claims []store.NamespaceCredentialClaim
	for i := 0; i < len(f.claims) && len(claims) < limit; {
		if f.claims[i].OrgID != orgID {
			i++
			continue
		}
		claims = append(claims, f.claims[i])
		f.claims = append(f.claims[:i], f.claims[i+1:]...)
	}
	return claims, nil
}
func (f *namespaceStoreFake) CompleteNamespaceCredentialCleanup(context.Context, store.NamespaceCredentialCleanupClaim, store.LeaseToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCompleted++
	return f.cleanupCompleteErr
}
func (f *namespaceStoreFake) CheckpointNamespaceCredential(_ context.Context, _ store.NamespaceCredentialClaim, _ store.LeaseToken, checkpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpoints = append(f.checkpoints, checkpoint)
	return f.checkpointErr
}
func (f *namespaceStoreFake) RescheduleNamespaceCredentialCleanup(_ context.Context, _ store.NamespaceCredentialCleanupClaim, _ store.LeaseToken, delay time.Duration) error {
	f.mu.Lock()
	f.cleanupRescheduled = append(f.cleanupRescheduled, delay)
	f.mu.Unlock()
	return nil
}
func (f *namespaceStoreFake) CompleteNamespaceCredential(context.Context, store.NamespaceCredentialClaim, store.LeaseToken) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed++
	return true, f.completeErr
}
func (f *namespaceStoreFake) FailNamespaceCredentialCleanup(_ context.Context, _ store.NamespaceCredentialCleanupClaim, _ store.LeaseToken, code string) error {
	f.mu.Lock()
	f.cleanupFailed = append(f.cleanupFailed, code)
	f.mu.Unlock()
	return nil
}
func (f *namespaceStoreFake) RescheduleNamespaceCredential(_ context.Context, _ store.NamespaceCredentialClaim, _ store.LeaseToken, delay time.Duration) error {
	f.mu.Lock()
	f.rescheduled = append(f.rescheduled, delay)
	f.mu.Unlock()
	return nil
}
func (f *namespaceStoreFake) FailNamespaceCredential(_ context.Context, _ store.NamespaceCredentialClaim, _ store.LeaseToken, code, message string) error {
	f.mu.Lock()
	f.failed = append(f.failed, code+":"+message)
	f.mu.Unlock()
	return nil
}

type namespaceAdapterFake struct {
	mu            sync.Mutex
	reconciles    int
	deletes       int
	active, max   int
	err           error
	deleteErr     error
	block         <-chan struct{}
	started       chan struct{}
	deleteBlock   <-chan struct{}
	deleteStarted chan struct{}
	ignoreCancel  bool
}

func (f *namespaceAdapterFake) DeleteImagePullSecret(ctx context.Context, _ kubernetes.ImagePullSecretRequest) error {
	f.mu.Lock()
	f.deletes++
	err := f.deleteErr
	started := f.deleteStarted
	block := f.deleteBlock
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	return err
}

func (f *namespaceAdapterFake) ReadImagePullSecretIdentity(_ context.Context, orgID, _ string, generation int64, _ string, _ string) (kubernetes.ImagePullSecretIdentity, error) {
	name, _ := kubernetes.RegistryCredentialSecretName(orgID, generation)
	return kubernetes.ImagePullSecretIdentity{SourceSecretName: name, SourceUID: "source-uid", NamespaceUID: "namespace-uid"}, nil
}
func (f *namespaceAdapterFake) ReconcileImagePullSecret(ctx context.Context, _ kubernetes.ImagePullSecretRequest) error {
	f.mu.Lock()
	f.reconciles++
	f.active++
	if f.active > f.max {
		f.max = f.active
	}
	started := f.started
	block := f.block
	err := f.err
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		if f.ignoreCancel {
			<-block
		} else {
			select {
			case <-block:
			case <-ctx.Done():
			}
		}
	}
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return err
}

func namespaceConfig() RegistryNamespaceReconcilerConfig {
	return RegistryNamespaceReconcilerConfig{ScanInterval: time.Hour, LeaseDuration: time.Minute,
		RequestTimeout: time.Second, ShutdownGrace: time.Second, Concurrency: 2, Owner: "worker", InstanceNamespace: "instance"}
}

func namespaceClaim(environment string, epoch int64, checkpoint string) store.NamespaceCredentialClaim {
	return store.NamespaceCredentialClaim{OrgID: "org", EnvironmentID: environment, AppID: "app", Namespace: "namespace-" + environment,
		RegistryID: "registry", OperationID: "operation", SecretRef: "k8s-secret://instance/cyclopus-ghcr-e87cb45c05ad389d5890-g1#.dockerconfigjson",
		Generation: 1, Checkpoint: checkpoint, Attempts: int(epoch), Lease: store.LeaseToken{Owner: "worker", Epoch: epoch, ExpiresAt: time.Now().Add(time.Minute)}}
}

func namespaceOrgClaim(org, environment string, epoch int64) store.NamespaceCredentialClaim {
	claim := namespaceClaim(environment, epoch, "")
	claim.OrgID = org
	name, _ := kubernetes.RegistryCredentialSecretName(org, claim.Generation)
	claim.SecretRef = "k8s-secret://instance/" + name + "#.dockerconfigjson"
	return claim
}

func namespaceCleanupClaim(org, environment string, attempts int) store.NamespaceCredentialCleanupClaim {
	name, _ := kubernetes.RegistryCredentialSecretName(org, 1)
	return store.NamespaceCredentialCleanupClaim{OrgID: org, EnvironmentID: environment, AppID: "app", Namespace: "namespace-" + environment,
		RegistryID: "registry", SecretRef: "k8s-secret://instance/" + name + "#.dockerconfigjson",
		Generation: 1, Attempts: attempts, Lease: store.LeaseToken{Owner: "worker", Epoch: int64(attempts), ExpiresAt: time.Now().Add(time.Minute)}}
}

func waitNamespaceWorker(t *testing.T, runner *RegistryNamespaceReconciler) {
	t.Helper()
	done := make(chan struct{})
	go func() { runner.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not finish")
	}
}

func TestRegistryNamespaceReconcilerSuccessCheckpointsAndVerifiesTwice(t *testing.T) {
	dataStore := &namespaceStoreFake{claims: []store.NamespaceCredentialClaim{namespaceClaim("one", 1, "")}}
	adapter := &namespaceAdapterFake{}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	adapter.mu.Lock()
	reconciles := adapter.reconciles
	adapter.mu.Unlock()
	if !runner.Ready() || dataStore.recovered != 1 || dataStore.completed != 1 || len(dataStore.checkpoints) != 2 ||
		dataStore.checkpoints[0] != "source_read" || dataStore.checkpoints[1] != "namespace_secret_written" || reconciles != 2 {
		t.Fatalf("unexpected success state: ready=%v recovered=%d checkpoints=%v completed=%d reconciles=%d", runner.Ready(), dataStore.recovered, dataStore.checkpoints, dataStore.completed, reconciles)
	}
}

func TestRegistryNamespaceReconcilerRecordsBoundedOutcomeAndWork(t *testing.T) {
	metrics := ocimetrics.New()
	dataStore := &namespaceStoreFake{claims: []store.NamespaceCredentialClaim{namespaceClaim("one", 1, "")}}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, &namespaceAdapterFake{}, metrics)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, runner)
	request, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, want := range []string{`cyclopus_oci_namespace_reconciliation_total{result="reconciled"} 1`, `cyclopus_oci_worker_leases{kind="namespace"} 0`} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestRegistryNamespaceReconcilerRetryAndAttemptTwelveFails(t *testing.T) {
	for _, test := range []struct {
		name                string
		epoch               int64
		wantRetry, wantFail int
	}{
		{name: "retry", epoch: 3, wantRetry: 1},
		{name: "attempt twelve", epoch: 12, wantFail: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore := &namespaceStoreFake{claims: []store.NamespaceCredentialClaim{namespaceClaim("one", test.epoch, "")}}
			adapter := &namespaceAdapterFake{err: errors.New("adapter")}
			runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
			runner.classify = func(error) (bool, oci.Code, bool) { return true, oci.CodeKubernetesUnavailable, true }
			if err := runner.Startup(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitNamespaceWorker(t, runner)
			dataStore.mu.Lock()
			defer dataStore.mu.Unlock()
			if len(dataStore.rescheduled) != test.wantRetry || len(dataStore.failed) != test.wantFail {
				t.Fatalf("rescheduled=%v failed=%v", dataStore.rescheduled, dataStore.failed)
			}
		})
	}
}

func TestRegistryNamespaceReconcilerPermanentOwnershipFailureUsesFixedCode(t *testing.T) {
	dataStore := &namespaceStoreFake{claims: []store.NamespaceCredentialClaim{namespaceClaim("one", 1, "")}}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, &namespaceAdapterFake{err: errors.New("sensitive raw error")})
	runner.classify = func(error) (bool, oci.Code, bool) { return false, oci.CodeKubernetesOwnershipConflict, true }
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	if len(dataStore.failed) != 1 || dataStore.failed[0] != "kubernetes_ownership_conflict:" {
		t.Fatalf("failure did not use fixed code without message: %v", dataStore.failed)
	}
}

func TestRegistryNamespaceReconcilerStaleLeaseSilentlyConverges(t *testing.T) {
	dataStore := &namespaceStoreFake{claims: []store.NamespaceCredentialClaim{namespaceClaim("one", 1, "")}, checkpointErr: store.ErrLeaseNotOwned}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, &namespaceAdapterFake{})
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	if dataStore.completed != 0 || len(dataStore.rescheduled) != 0 || len(dataStore.failed) != 0 {
		t.Fatalf("stale lease produced writes: completed=%d retry=%v failed=%v", dataStore.completed, dataStore.rescheduled, dataStore.failed)
	}
}

func TestRegistryNamespaceReconcilerReclaimedWrittenSecretReverifies(t *testing.T) {
	dataStore := &namespaceStoreFake{claims: []store.NamespaceCredentialClaim{namespaceClaim("one", 2, "namespace_secret_written")}}
	adapter := &namespaceAdapterFake{}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	adapter.mu.Lock()
	reconciles := adapter.reconciles
	adapter.mu.Unlock()
	if len(dataStore.checkpoints) != 1 || dataStore.checkpoints[0] != "namespace_secret_written" || reconciles != 2 || dataStore.completed != 1 {
		t.Fatalf("reclaim was not reverified: checkpoints=%v reconciles=%d completed=%d", dataStore.checkpoints, reconciles, dataStore.completed)
	}
}

func TestRegistryNamespaceReconcilerBoundsConcurrencyAndEnvironmentInFlight(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 4)
	dataStore := &namespaceStoreFake{claims: []store.NamespaceCredentialClaim{namespaceClaim("one", 1, ""), namespaceClaim("two", 1, ""), namespaceClaim("one", 2, "")}}
	adapter := &namespaceAdapterFake{block: block, started: started}
	config := namespaceConfig()
	config.Concurrency = 2
	runner := NewRegistryNamespaceReconciler(config, slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("expected concurrent reconciliation")
		}
	}
	adapter.mu.Lock()
	max := adapter.max
	adapter.mu.Unlock()
	if max != 2 {
		t.Fatalf("max concurrency=%d", max)
	}
	close(block)
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	if dataStore.completed != 2 {
		t.Fatalf("duplicate environment ran concurrently: completed=%d", dataStore.completed)
	}
}

func TestRegistryNamespaceReconcilerRotatesOrganizationsAcrossScans(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		name := "reconciliation"
		if cleanup {
			name = "cleanup"
		}
		t.Run(name, func(t *testing.T) {
			firstBlock := make(chan struct{})
			firstStarted := make(chan struct{}, 1)
			dataStore := &namespaceStoreFake{orgIDs: []string{"saturated", "waiting"}}
			adapter := &namespaceAdapterFake{}
			if cleanup {
				dataStore.cleanupClaims = []store.NamespaceCredentialCleanupClaim{
					namespaceCleanupClaim("saturated", "one", 1),
					namespaceCleanupClaim("saturated", "two", 1),
					namespaceCleanupClaim("saturated", "three", 1),
					namespaceCleanupClaim("waiting", "waiting", 1),
				}
				adapter.deleteBlock, adapter.deleteStarted = firstBlock, firstStarted
			} else {
				dataStore.claims = []store.NamespaceCredentialClaim{
					namespaceOrgClaim("saturated", "one", 1),
					namespaceOrgClaim("saturated", "two", 1),
					namespaceOrgClaim("saturated", "three", 1),
					namespaceOrgClaim("waiting", "waiting", 1),
				}
				adapter.block, adapter.started = firstBlock, firstStarted
			}
			config := namespaceConfig()
			config.Concurrency = 1
			runner := NewRegistryNamespaceReconciler(config, slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
			if err := runner.Startup(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-firstStarted:
			case <-time.After(time.Second):
				t.Fatal("saturated organization did not start")
			}
			close(firstBlock)
			waitNamespaceWorker(t, runner)

			secondBlock := make(chan struct{})
			secondStarted := make(chan struct{}, 1)
			adapter.mu.Lock()
			if cleanup {
				adapter.deleteBlock, adapter.deleteStarted = secondBlock, secondStarted
			} else {
				adapter.block, adapter.started = secondBlock, secondStarted
			}
			adapter.mu.Unlock()
			if err := runner.scan(t.Context(), false); err != nil {
				t.Fatal(err)
			}
			select {
			case <-secondStarted:
			case <-time.After(time.Second):
				t.Fatal("waiting organization did not start on second scan")
			}
			dataStore.mu.Lock()
			remainingWaiting := 0
			if cleanup {
				for _, claim := range dataStore.cleanupClaims {
					if claim.OrgID == "waiting" {
						remainingWaiting++
					}
				}
			} else {
				for _, claim := range dataStore.claims {
					if claim.OrgID == "waiting" {
						remainingWaiting++
					}
				}
			}
			dataStore.mu.Unlock()
			if remainingWaiting != 0 {
				t.Fatalf("waiting organization remained starved after cursor rotation: %d", remainingWaiting)
			}
			close(secondBlock)
			waitNamespaceWorker(t, runner)
		})
	}
}

func TestRegistryNamespaceReconcilerKeysetPaginationWrapsBeyondFirstThousand(t *testing.T) {
	orgIDs := make([]string, 1002)
	for index := range orgIDs {
		orgIDs[index] = fmt.Sprintf("org-%04d", index)
	}
	dataStore := &namespaceStoreFake{orgIDs: orgIDs}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, &namespaceAdapterFake{})
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runner.scan(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := runner.scan(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	want := []string{"", "org-0999", "org-1001", ""}
	if !slices.Equal(dataStore.listCursors, want) {
		t.Fatalf("discovery cursors=%v, want %v", dataStore.listCursors, want)
	}
}

func TestRegistryNamespaceReconcilerSameEnvironmentIDAcrossOrganizationsRunsConcurrently(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 2)
	dataStore := &namespaceStoreFake{
		orgIDs: []string{"org-one", "org-two"},
		claims: []store.NamespaceCredentialClaim{
			namespaceOrgClaim("org-one", "shared-environment", 1),
			namespaceOrgClaim("org-two", "shared-environment", 1),
		},
	}
	adapter := &namespaceAdapterFake{block: block, started: started}
	config := namespaceConfig()
	config.Concurrency = 2
	runner := NewRegistryNamespaceReconciler(config, slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("same environment ID in separate organizations did not run concurrently")
		}
	}
	adapter.mu.Lock()
	max := adapter.max
	adapter.mu.Unlock()
	if max != 2 {
		t.Fatalf("max concurrency=%d, want 2", max)
	}
	close(block)
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	completed := dataStore.completed
	remaining := len(dataStore.claims)
	dataStore.mu.Unlock()
	if completed != 2 || remaining != 0 {
		t.Fatalf("cross-organization claims stalled: completed=%d remaining=%d", completed, remaining)
	}
}

func TestRegistryNamespaceReconcilerShutdownGrace(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	dataStore := &namespaceStoreFake{claims: []store.NamespaceCredentialClaim{namespaceClaim("one", 1, "")}}
	config := namespaceConfig()
	config.ShutdownGrace = 10 * time.Millisecond
	runner := NewRegistryNamespaceReconciler(config, slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, &namespaceAdapterFake{block: block, started: started, ignoreCancel: true})
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	startedAt := time.Now()
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.Ready() || time.Since(startedAt) > 250*time.Millisecond {
		t.Fatalf("shutdown did not honor grace: ready=%v elapsed=%s", runner.Ready(), time.Since(startedAt))
	}
	close(block)
	waitNamespaceWorker(t, runner)
}

func TestRegistryNamespaceCleanupOwnedSuccess(t *testing.T) {
	dataStore := &namespaceStoreFake{cleanupClaims: []store.NamespaceCredentialCleanupClaim{namespaceCleanupClaim("org", "one", 1)}}
	adapter := &namespaceAdapterFake{}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	adapter.mu.Lock()
	deletes := adapter.deletes
	adapter.mu.Unlock()
	if dataStore.cleanupRecovered != 1 || dataStore.cleanupCompleted != 1 || deletes != 1 {
		t.Fatalf("cleanup did not complete: recovered=%d completed=%d deletes=%d", dataStore.cleanupRecovered, dataStore.cleanupCompleted, deletes)
	}
}

func TestRegistryNamespaceCleanupPermanentOwnershipCollision(t *testing.T) {
	dataStore := &namespaceStoreFake{cleanupClaims: []store.NamespaceCredentialCleanupClaim{namespaceCleanupClaim("org", "one", 1)}}
	adapter := &namespaceAdapterFake{deleteErr: errors.New("unowned destination")}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
	runner.classify = func(error) (bool, oci.Code, bool) { return false, oci.CodeKubernetesOwnershipConflict, true }
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	defer dataStore.mu.Unlock()
	if len(dataStore.cleanupFailed) != 1 || dataStore.cleanupFailed[0] != "kubernetes_ownership_conflict" || dataStore.cleanupCompleted != 0 {
		t.Fatalf("unexpected permanent cleanup result: failed=%v completed=%d", dataStore.cleanupFailed, dataStore.cleanupCompleted)
	}
}

func TestRegistryNamespaceCleanupRetryAndAttemptExhaustion(t *testing.T) {
	for _, test := range []struct {
		name                   string
		attempts               int
		wantRetry, wantFailure int
	}{
		{name: "retry", attempts: 3, wantRetry: 1},
		{name: "exhausted", attempts: 12, wantFailure: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore := &namespaceStoreFake{cleanupClaims: []store.NamespaceCredentialCleanupClaim{namespaceCleanupClaim("org", "one", test.attempts)}}
			runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, &namespaceAdapterFake{deleteErr: errors.New("unavailable")})
			runner.classify = func(error) (bool, oci.Code, bool) { return true, oci.CodeKubernetesUnavailable, true }
			if err := runner.Startup(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitNamespaceWorker(t, runner)
			dataStore.mu.Lock()
			defer dataStore.mu.Unlock()
			if len(dataStore.cleanupRescheduled) != test.wantRetry || len(dataStore.cleanupFailed) != test.wantFailure {
				t.Fatalf("retry=%v failed=%v", dataStore.cleanupRescheduled, dataStore.cleanupFailed)
			}
		})
	}
}

func TestRegistryNamespaceCleanupPublicPrivateOrTeardownStaleCASConverges(t *testing.T) {
	for _, stale := range []string{"public_to_private", "teardown"} {
		t.Run(stale, func(t *testing.T) {
			dataStore := &namespaceStoreFake{cleanupClaims: []store.NamespaceCredentialCleanupClaim{namespaceCleanupClaim("org", "one", 1)}, cleanupCompleteErr: store.ErrLeaseNotOwned}
			runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, &namespaceAdapterFake{})
			if err := runner.Startup(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitNamespaceWorker(t, runner)
			dataStore.mu.Lock()
			defer dataStore.mu.Unlock()
			if len(dataStore.cleanupRescheduled) != 0 || len(dataStore.cleanupFailed) != 0 {
				t.Fatalf("stale cleanup CAS did not converge: retry=%v failed=%v", dataStore.cleanupRescheduled, dataStore.cleanupFailed)
			}
		})
	}
}

func TestRegistryNamespaceCleanupCrashReclaimRepeatsOwnershipSafeDelete(t *testing.T) {
	adapter := &namespaceAdapterFake{}
	firstStore := &namespaceStoreFake{cleanupClaims: []store.NamespaceCredentialCleanupClaim{namespaceCleanupClaim("org", "one", 1)}, cleanupCompleteErr: store.ErrLeaseNotOwned}
	first := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), firstStore, adapter)
	if err := first.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, first)
	secondStore := &namespaceStoreFake{cleanupClaims: []store.NamespaceCredentialCleanupClaim{namespaceCleanupClaim("org", "one", 2)}}
	second := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), secondStore, adapter)
	if err := second.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, second)
	adapter.mu.Lock()
	deletes := adapter.deletes
	adapter.mu.Unlock()
	secondStore.mu.Lock()
	completed := secondStore.cleanupCompleted
	secondStore.mu.Unlock()
	if deletes != 2 || completed != 1 {
		t.Fatalf("reclaim did not repeat safe deletion: deletes=%d completed=%d", deletes, completed)
	}
}

func TestRegistryNamespaceCleanupDoesNotClaimAcrossOrganizations(t *testing.T) {
	dataStore := &namespaceStoreFake{cleanupClaims: []store.NamespaceCredentialCleanupClaim{namespaceCleanupClaim("other-org", "one", 1)}, claims: []store.NamespaceCredentialClaim{namespaceClaim("reconcile", 1, "")}}
	adapter := &namespaceAdapterFake{}
	runner := NewRegistryNamespaceReconciler(namespaceConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), dataStore, adapter)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitNamespaceWorker(t, runner)
	dataStore.mu.Lock()
	remaining := len(dataStore.cleanupClaims)
	completed := dataStore.cleanupCompleted
	dataStore.mu.Unlock()
	if remaining != 1 || completed != 0 {
		t.Fatalf("cross-org cleanup was claimed: remaining=%d completed=%d", remaining, completed)
	}
}
