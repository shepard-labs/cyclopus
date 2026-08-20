package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/kubernetes"
	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/ocimetrics"
	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestRegistryCredentialRunnerStartupRecoversAndScans(t *testing.T) {
	operation := registryCredentialOperation("source_secret_written")
	dataStore := &registryCredentialRunnerStoreFake{orgIDs: []string{"org"}, operations: []store.RegistryCredentialOperation{operation}}
	processor := &registryCredentialOperationProcessorFake{result: RegistryCredentialValidated, called: make(chan struct{}, 1)}
	runner := NewRegistryCredentialRunner(RegistryCredentialConfig{ScanInterval: time.Hour, LeaseDuration: time.Minute, RequestTimeout: time.Second, ShutdownGrace: time.Second, Concurrency: 1, Owner: "worker-one"}, slog.Default(), dataStore, processor)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.called:
	case <-time.After(time.Second):
		t.Fatal("operation was not processed")
	}
	if !runner.Ready() || dataStore.operationRecoveries != 2 || dataStore.targetRecoveries != 2 || dataStore.claims != 1 {
		t.Fatalf("ready=%v operation recoveries=%d target recoveries=%d claims=%d", runner.Ready(), dataStore.operationRecoveries, dataStore.targetRecoveries, dataStore.claims)
	}
}

func TestRegistryCredentialRunnerExposesActiveAndZeroWorkMetrics(t *testing.T) {
	operation := registryCredentialOperation("source_secret_written")
	dataStore := &registryCredentialRunnerStoreFake{orgIDs: []string{"org"}, operations: []store.RegistryCredentialOperation{operation}}
	started := make(chan struct{})
	release := make(chan struct{})
	processor := registryCredentialOperationProcessorFunc(func(context.Context, store.RegistryCredentialOperation) (RegistryCredentialProcessResult, error) {
		close(started)
		<-release
		return RegistryCredentialValidated, nil
	})
	metrics := ocimetrics.New()
	runner := NewRegistryCredentialRunner(RegistryCredentialConfig{ScanInterval: time.Hour, LeaseDuration: time.Minute, RequestTimeout: time.Second, ShutdownGrace: time.Second, Concurrency: 1, Owner: "worker-one"}, slog.Default(), dataStore, processor, metrics)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("operation was not processed")
	}
	assertRegistryCredentialWorkMetrics(t, metrics, 1, 1)
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		if registryCredentialWorkMetrics(metrics, 0, 0) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("credential work metrics did not transition to zero")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertRegistryCredentialWorkMetrics(t *testing.T, metrics *ocimetrics.Metrics, active, backlog int) {
	t.Helper()
	if !registryCredentialWorkMetrics(metrics, active, backlog) {
		w := httptest.NewRecorder()
		metrics.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		t.Fatalf("metric exposition missing credential active=%d backlog=%d:\n%s", active, backlog, w.Body.String())
	}
}

func registryCredentialWorkMetrics(metrics *ocimetrics.Metrics, active, backlog int) bool {
	w := httptest.NewRecorder()
	metrics.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	text := w.Body.String()
	return strings.Contains(text, `cyclopus_oci_worker_leases{kind="credential"} `+strconv.Itoa(active)) &&
		strings.Contains(text, `cyclopus_oci_worker_backlog{kind="credential"} `+strconv.Itoa(backlog))
}

func TestRegistryCredentialRunnerRetriesAndFailsWithAllowlistedCodes(t *testing.T) {
	for name, result := range map[string]RegistryCredentialProcessResult{"retry": RegistryCredentialRetry, "failed": RegistryCredentialFailed} {
		t.Run(name, func(t *testing.T) {
			operation := registryCredentialOperation("source_secret_written")
			dataStore := &registryCredentialRunnerStoreFake{orgIDs: []string{"org"}, operations: []store.RegistryCredentialOperation{operation}, written: make(chan struct{}, 1)}
			runner := NewRegistryCredentialRunner(RegistryCredentialConfig{ScanInterval: time.Hour, LeaseDuration: time.Minute, RequestTimeout: time.Second, ShutdownGrace: time.Second, Concurrency: 1, Owner: "worker-one"}, slog.Default(), dataStore, &registryCredentialOperationProcessorFake{result: result})
			if err := runner.Startup(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-dataStore.written:
			case <-time.After(time.Second):
				t.Fatal("terminal write was not attempted")
			}
			if result == RegistryCredentialRetry && (dataStore.retried != 1 || dataStore.code != "postgres_retryable") {
				t.Fatalf("retry=%d code=%q", dataStore.retried, dataStore.code)
			}
			if result == RegistryCredentialFailed && (dataStore.failed != 1 || dataStore.code != "credential_validation_failed") {
				t.Fatalf("failed=%d code=%q", dataStore.failed, dataStore.code)
			}
		})
	}
}

type registryCredentialOperationProcessorFake struct {
	result RegistryCredentialProcessResult
	err    error
	called chan struct{}
}

type registryCredentialOperationProcessorFunc func(context.Context, store.RegistryCredentialOperation) (RegistryCredentialProcessResult, error)

func (f registryCredentialOperationProcessorFunc) Process(ctx context.Context, operation store.RegistryCredentialOperation) (RegistryCredentialProcessResult, error) {
	return f(ctx, operation)
}

func (f *registryCredentialOperationProcessorFake) Process(context.Context, store.RegistryCredentialOperation) (RegistryCredentialProcessResult, error) {
	if f.called != nil {
		f.called <- struct{}{}
	}
	return f.result, f.err
}

type registryCredentialRunnerStoreFake struct {
	orgIDs                                                         []string
	operations                                                     []store.RegistryCredentialOperation
	operationRecoveries, targetRecoveries, claims, retried, failed int
	code                                                           string
	written                                                        chan struct{}
}

func (f *registryCredentialRunnerStoreFake) RecoverExpiredRegistryCredentialLeases(context.Context, int) (int64, error) {
	f.operationRecoveries++
	return 0, nil
}
func (f *registryCredentialRunnerStoreFake) RecoverExpiredRegistryCredentialValidationTargets(context.Context, int) (int64, error) {
	f.targetRecoveries++
	return 0, nil
}
func (f *registryCredentialRunnerStoreFake) ListRegistryCredentialOperationOrgIDs(context.Context, int) ([]string, error) {
	return f.orgIDs, nil
}
func (f *registryCredentialRunnerStoreFake) ClaimRegistryCredentialOperations(context.Context, string, string, time.Duration, int) ([]store.RegistryCredentialOperation, error) {
	f.claims++
	values := f.operations
	f.operations = nil
	return values, nil
}
func (f *registryCredentialRunnerStoreFake) RetryRegistryCredentialOperation(context.Context, store.RegistryCredentialOperation, store.LeaseToken, time.Duration, string) error {
	f.retried++
	f.code = "postgres_retryable"
	if f.written != nil {
		f.written <- struct{}{}
	}
	return nil
}
func (f *registryCredentialRunnerStoreFake) FailRegistryCredentialOperation(_ context.Context, _ store.RegistryCredentialOperation, _ store.LeaseToken, code string) error {
	f.failed++
	f.code = code
	if f.written != nil {
		f.written <- struct{}{}
	}
	return nil
}

func TestRegistryCredentialProcessorReadsPersistedSecret(t *testing.T) {
	credential := &oci.Credential{Username: []byte("octocat"), PAT: []byte("private-token")}
	source := &registryCredentialSourceFake{credential: credential}
	dataStore := &registryCredentialProcessorStoreFake{
		generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"},
		finish:     true,
	}
	processor, err := NewRegistryCredentialProcessor(dataStore, source, &registryCredentialResolverFake{}, "worker-one", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	operation := registryCredentialOperation("source_secret_written")
	if result, err := processor.Process(t.Context(), operation); result != RegistryCredentialValidated || err != nil {
		t.Fatalf("Process() = %q, %v", result, err)
	}
	if source.reads != 1 || source.lastRead == nil || source.lastRead.Username != nil || source.lastRead.PAT != nil {
		t.Fatalf("reads=%d credential=%+v", source.reads, source.lastRead)
	}
	if source.ref != dataStore.generation.SecretRef {
		t.Fatalf("read ref = %q, want persisted generation ref %q", source.ref, dataStore.generation.SecretRef)
	}
}

func TestRegistryCredentialProcessorRestartAfterLeaseReclaim(t *testing.T) {
	credential := &oci.Credential{Username: []byte("octocat"), PAT: []byte("private-token")}
	source := &registryCredentialSourceFake{credential: credential}
	dataStore := &registryCredentialProcessorStoreFake{
		generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"},
		finish:     true,
	}
	processor, err := NewRegistryCredentialProcessor(dataStore, source, &registryCredentialResolverFake{}, "worker-one", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a restart: a reclaimed lease presents the same operation again,
	// still at source_secret_written, and the worker must re-read the immutable
	// Secret and validate without any caller-supplied material.
	operation := registryCredentialOperation("source_secret_written")
	operation.Lease = &store.LeaseToken{Owner: "worker-two", Epoch: 3, ExpiresAt: time.Now().Add(time.Minute)}
	if result, err := processor.Process(t.Context(), operation); result != RegistryCredentialValidated || err != nil {
		t.Fatalf("restart Process() = %q, %v", result, err)
	}
	if source.reads != 1 {
		t.Fatalf("reads = %d, want exactly one Secret read from persisted ref", source.reads)
	}
}

func TestRegistryCredentialProcessorRejectsUnprocessableCheckpoints(t *testing.T) {
	source := &registryCredentialSourceFake{credential: &oci.Credential{Username: []byte("u"), PAT: []byte("p")}}
	dataStore := &registryCredentialProcessorStoreFake{generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"}}
	processor, err := NewRegistryCredentialProcessor(dataStore, source, &registryCredentialResolverFake{}, "worker-one", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, checkpoint := range []string{"reserved", "pending_recorded", "source_secret_write_started", "targets_validated", "reconciling_namespaces"} {
		operation := registryCredentialOperation(checkpoint)
		if result, err := processor.Process(t.Context(), operation); result != RegistryCredentialRetry || err == nil || source.reads != 0 {
			t.Fatalf("checkpoint %q Process() = %q, %v reads=%d", checkpoint, result, err, source.reads)
		}
	}
	operation := registryCredentialOperation("validation_complete")
	dataStore.finish = true
	if result, err := processor.Process(t.Context(), operation); result != RegistryCredentialValidated || err != nil || source.reads != 1 {
		t.Fatalf("validation_complete Process() = %q, %v reads=%d", result, err, source.reads)
	}
}

func TestRegistryCredentialProcessorReadFailureClassification(t *testing.T) {
	for name, test := range map[string]struct {
		readErr error
		result  RegistryCredentialProcessResult
	}{
		"transient API error":     {readErr: errors.New("connection refused"), result: RegistryCredentialRetry},
		"missing Secret":          {readErr: kubernetes.ErrRegistryCredentialNotFound, result: RegistryCredentialFailed},
		"malformed Secret":        {readErr: kubernetes.ErrRegistryCredentialInvalid, result: RegistryCredentialFailed},
		"wrapped malformed error": {readErr: errors.Join(errors.New("read"), kubernetes.ErrRegistryCredentialInvalid), result: RegistryCredentialFailed},
	} {
		t.Run(name, func(t *testing.T) {
			source := &registryCredentialSourceFake{readErr: test.readErr}
			dataStore := &registryCredentialProcessorStoreFake{generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"}}
			processor, err := NewRegistryCredentialProcessor(dataStore, source, &registryCredentialResolverFake{}, "worker-one", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			result, err := processor.Process(t.Context(), registryCredentialOperation("source_secret_written"))
			if result != test.result || err == nil {
				t.Fatalf("Process() = %q, %v; want %q with error", result, err, test.result)
			}
		})
	}
}

func TestRegistryCredentialProcessorValidationOutcomes(t *testing.T) {
	for name, test := range map[string]struct {
		err                        error
		attempts                   int
		result                     RegistryCredentialProcessResult
		finish                     bool
		completed, retried, failed int
	}{
		"validates exact target":       {result: RegistryCredentialValidated, finish: true, completed: 1},
		"retryable target":             {err: oci.NewRetryableError(oci.CodeRegistryUnavailable, oci.CodeRegistryUnavailable, 0), result: RegistryCredentialFailed, retried: 1},
		"twelfth attempt is permanent": {err: oci.NewRetryableError(oci.CodeRegistryUnavailable, oci.CodeRegistryUnavailable, 0), attempts: 12, result: RegistryCredentialFailed, failed: 1},
		"permanent target":             {err: oci.NewError(oci.CodeRegistryAuthFailed), result: RegistryCredentialFailed, failed: 1},
	} {
		t.Run(name, func(t *testing.T) {
			credential := &oci.Credential{Username: []byte("octocat"), PAT: []byte("private-token")}
			source := &registryCredentialSourceFake{credential: credential}
			target := store.RegistryCredentialValidationTarget{OperationID: "operation", OrgID: "org", RegistryID: "registry", Repository: "acme/api", Reference: "sha256:" + registryCredentialRepeat("a", 64), Attempts: test.attempts, Lease: &store.LeaseToken{Owner: "worker-one", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)}}
			dataStore := &registryCredentialProcessorStoreFake{generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"}, targets: []store.RegistryCredentialValidationTarget{target}, finish: test.finish}
			resolver := &registryCredentialResolverFake{err: test.err}
			processor, err := NewRegistryCredentialProcessor(dataStore, source, resolver, "worker-one", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			result, err := processor.Process(t.Context(), registryCredentialOperation("source_secret_written"))
			if result != test.result || dataStore.completed != test.completed || dataStore.retried != test.retried || dataStore.failed != test.failed {
				t.Fatalf("Process()=%q,%v store=%+v", result, err, dataStore)
			}
			if resolver.request.Host != oci.GHCRHost || resolver.request.Repository != target.Repository || resolver.request.Reference != target.Reference || resolver.request.Credential != source.lastRead || source.lastRead.Username != nil || source.lastRead.PAT != nil {
				t.Fatalf("request=%+v credential=%+v", resolver.request, credential)
			}
		})
	}
}

func TestRegistryCredentialProcessorPermanentFailureIgnoresStaleFinishError(t *testing.T) {
	credential := &oci.Credential{Username: []byte("octocat"), PAT: []byte("private-token")}
	source := &registryCredentialSourceFake{credential: credential}
	target := store.RegistryCredentialValidationTarget{OperationID: "operation", OrgID: "org", RegistryID: "registry", Repository: "acme/api", Reference: "sha256:" + registryCredentialRepeat("a", 64), Lease: &store.LeaseToken{Owner: "worker-one", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)}}
	for name, finishErr := range map[string]error{
		"precondition": store.ErrFailedPrecondition,
		"lease lost":   store.ErrLeaseNotOwned,
	} {
		t.Run(name, func(t *testing.T) {
			dataStore := &registryCredentialProcessorStoreFake{
				generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"},
				targets:    []store.RegistryCredentialValidationTarget{target},
				finishErr:  finishErr,
			}
			processor, err := NewRegistryCredentialProcessor(dataStore, source, &registryCredentialResolverFake{err: oci.NewError(oci.CodeRegistryAuthFailed)}, "worker-one", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			// Finish terminalized the operation after the permanent failure, so
			// its precondition/lease error must not be treated as retryable.
			if result, err := processor.Process(t.Context(), registryCredentialOperation("source_secret_written")); result != RegistryCredentialFailed || err == nil || dataStore.failed != 1 {
				t.Fatalf("Process() = %q, %v failed=%d", result, err, dataStore.failed)
			}
		})
	}
}

func TestRegistryCredentialProcessorRestartOnAlreadyFailedOperation(t *testing.T) {
	// A prior attempt failed a target and Finish terminalized the operation
	// (complete=false, no error). A restart must report Failed, never Retry.
	credential := &oci.Credential{Username: []byte("octocat"), PAT: []byte("private-token")}
	source := &registryCredentialSourceFake{credential: credential}
	dataStore := &registryCredentialProcessorStoreFake{
		generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"},
		finish:     false,
	}
	processor, err := NewRegistryCredentialProcessor(dataStore, source, &registryCredentialResolverFake{}, "worker-one", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := processor.Process(t.Context(), registryCredentialOperation("source_secret_written")); result != RegistryCredentialFailed || err == nil {
		t.Fatalf("Process() = %q, %v; want failed with error", result, err)
	}
}

func TestRegistryCredentialProcessorClaimError(t *testing.T) {
	credential := &oci.Credential{Username: []byte("octocat"), PAT: []byte("private-token")}
	source := &registryCredentialSourceFake{credential: credential}
	claimErr := errors.New("claim query failed")
	for name, dataStore := range map[string]*registryCredentialProcessorStoreFake{
		"claim store error": {generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"}, claimErr: claimErr},
		"nil target lease":  {generation: store.RegistryCredentialGeneration{SecretRef: "k8s-secret://cyclopus/cyclopus-ghcr-g1#.dockerconfigjson"}, targets: []store.RegistryCredentialValidationTarget{{OperationID: "operation", OrgID: "org", RegistryID: "registry", Repository: "acme/api", Reference: "sha256:" + registryCredentialRepeat("a", 64)}}},
	} {
		t.Run(name, func(t *testing.T) {
			processor, err := NewRegistryCredentialProcessor(dataStore, source, &registryCredentialResolverFake{}, "worker-one", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := processor.Process(t.Context(), registryCredentialOperation("source_secret_written")); result != RegistryCredentialRetry || err == nil {
				t.Fatalf("Process() = %q, %v; want retry with error", result, err)
			}
		})
	}
	operation := registryCredentialOperation("source_secret_written")
	operation.Lease = nil
	processor, _ := NewRegistryCredentialProcessor(&registryCredentialProcessorStoreFake{}, source, &registryCredentialResolverFake{}, "worker-one", time.Minute)
	if result, err := processor.Process(t.Context(), operation); result != RegistryCredentialRetry || !errors.Is(err, store.ErrLeaseNotOwned) {
		t.Fatalf("nil lease Process() = %q, %v", result, err)
	}
}

type registryCredentialSourceFake struct {
	credential *oci.Credential
	readErr    error
	reads      int
	lastRead   *oci.Credential
	ref        string
}

func (f *registryCredentialSourceFake) ReadCredential(_ context.Context, _, _ string, _ int64, secretRef string) (*oci.Credential, error) {
	f.reads++
	f.ref = secretRef
	if f.readErr != nil {
		return nil, f.readErr
	}
	f.lastRead = &oci.Credential{Username: append([]byte(nil), f.credential.Username...), PAT: append([]byte(nil), f.credential.PAT...)}
	return f.lastRead, nil
}

type registryCredentialResolverFake struct {
	request oci.PullValidationRequest
	err     error
}

func (*registryCredentialResolverFake) Resolve(context.Context, oci.ResolveRequest) (oci.ResolvedImage, error) {
	return oci.ResolvedImage{}, nil
}
func (f *registryCredentialResolverFake) ValidatePull(_ context.Context, request oci.PullValidationRequest) error {
	f.request = request
	return f.err
}

type registryCredentialProcessorStoreFake struct {
	generation                 store.RegistryCredentialGeneration
	targets                    []store.RegistryCredentialValidationTarget
	claimErr                   error
	finishErr                  error
	completed, retried, failed int
	finish                     bool
}

func (f *registryCredentialProcessorStoreFake) GetRegistryCredentialGeneration(context.Context, string, string, int64) (store.RegistryCredentialGeneration, error) {
	return f.generation, nil
}
func (f *registryCredentialProcessorStoreFake) ClaimRegistryCredentialValidationTargets(context.Context, store.RegistryCredentialOperation, string, time.Duration, int) ([]store.RegistryCredentialValidationTarget, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	return f.targets, nil
}
func (f *registryCredentialProcessorStoreFake) CompleteRegistryCredentialValidationTarget(context.Context, store.RegistryCredentialValidationTarget, store.LeaseToken) error {
	f.completed++
	return nil
}
func (f *registryCredentialProcessorStoreFake) RetryRegistryCredentialValidationTarget(context.Context, store.RegistryCredentialValidationTarget, store.LeaseToken, string, time.Duration) error {
	f.retried++
	return nil
}
func (f *registryCredentialProcessorStoreFake) FailRegistryCredentialValidationTarget(context.Context, store.RegistryCredentialValidationTarget, store.LeaseToken, string) error {
	f.failed++
	return nil
}
func (f *registryCredentialProcessorStoreFake) FinishRegistryCredentialValidation(context.Context, store.RegistryCredentialOperation, store.LeaseToken) (bool, error) {
	return f.finish, f.finishErr
}

func registryCredentialOperation(checkpoint string) store.RegistryCredentialOperation {
	return store.RegistryCredentialOperation{ID: "operation", OrgID: "org", RegistryID: "registry", CandidateGeneration: 1, Checkpoint: checkpoint, Lease: &store.LeaseToken{Owner: "worker-one", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)}}
}
func registryCredentialRepeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
