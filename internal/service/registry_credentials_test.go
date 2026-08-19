package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/store"
)

// fakeCredentialStore models the real reservation transaction: GetGHCRRegistry
// returns the committed registry state, reserve mutates it only on the first
// successful reservation, and attach publishes the provisioning/rotating
// state. Keyed operations index by idempotency key like the real unique index.
type fakeCredentialStore struct {
	mu           sync.Mutex
	registry     *store.Registry
	operations   map[string]*store.RegistryCredentialOperation
	fingerprints map[string][32]byte
	secretRef    string
	generation   store.RegistryCredentialGeneration
	calls        []string
	errs         map[string]error
	ensureErr    error
	attachCommit bool
}

func newFakeCredentialStore(registry *store.Registry) *fakeCredentialStore {
	return &fakeCredentialStore{registry: registry, operations: map[string]*store.RegistryCredentialOperation{}, fingerprints: map[string][32]byte{}, errs: map[string]error{}}
}

func (f *fakeCredentialStore) record(name string) {
	f.calls = append(f.calls, name)
}

func (f *fakeCredentialStore) GetGHCRRegistry(_ context.Context, orgID string) (store.Registry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("get_registry")
	if err := f.errs["get_registry"]; err != nil {
		return store.Registry{}, err
	}
	if f.registry == nil {
		return store.Registry{}, store.ErrNotFound
	}
	return *f.registry, nil
}

func (f *fakeCredentialStore) EnsureGHCRRegistry(_ context.Context, orgID string) (store.Registry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ensure_registry")
	if f.ensureErr != nil {
		return store.Registry{}, f.ensureErr
	}
	if f.registry == nil {
		f.registry = &store.Registry{ID: "registry-1", OrgID: orgID, Provider: "ghcr", RegistryHost: "ghcr.io", State: "absent"}
	}
	return *f.registry, nil
}

func (f *fakeCredentialStore) ReserveRegistryCredentialOperation(_ context.Context, p store.ReserveRegistryCredentialOperationParams) (store.RegistryCredentialOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("reserve")
	if err := f.errs["reserve"]; err != nil {
		return store.RegistryCredentialOperation{}, err
	}
	if existing := f.operations[p.IdempotencyKey]; existing != nil {
		if existing.Kind != p.Kind || stringValue(existing.TargetRepository) != p.TargetRepository || stringValue(existing.TargetReference) != p.TargetReference || f.fingerprints[p.IdempotencyKey] != p.CredentialFingerprint {
			return store.RegistryCredentialOperation{}, store.ErrConflict
		}
		return *existing, nil
	}
	if f.registry.PendingGeneration != nil || f.registry.OperationID != nil || f.registry.State == "creating" || f.registry.State == "rotating" || f.registry.State == "reconciling" {
		return store.RegistryCredentialOperation{}, store.ErrConflict
	}
	var repository, reference *string
	if p.TargetRepository != "" {
		repository = &p.TargetRepository
	}
	if p.TargetReference != "" {
		reference = &p.TargetReference
	}
	candidate := f.registry.ActiveGeneration + 1
	var previous *int64
	if f.registry.ActiveGeneration > 0 {
		previous = &f.registry.ActiveGeneration
	}
	if (p.Kind == "rotate") != (previous != nil) {
		return store.RegistryCredentialOperation{}, store.ErrFailedPrecondition
	}
	operation := &store.RegistryCredentialOperation{ID: "operation-1", OrgID: p.OrgID, RegistryID: p.RegistryID, Kind: p.Kind,
		Status: "awaiting_source", Checkpoint: "reserved", CandidateGeneration: candidate, PreviousGeneration: previous,
		TargetRepository: repository, TargetReference: reference, IdempotencyKey: p.IdempotencyKey}
	f.operations[p.IdempotencyKey] = operation
	f.fingerprints[p.IdempotencyKey] = p.CredentialFingerprint
	f.registry.PendingGeneration = &candidate
	f.registry.OperationID = &operation.ID
	return *operation, nil
}

func (f *fakeCredentialStore) AttachRegistryCredentialSource(_ context.Context, p store.AttachRegistryCredentialSourceParams) (store.RegistryCredentialOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("attach")
	if err := f.errs["attach"]; err != nil {
		if f.attachCommit {
			f.commitAttach(p)
		}
		return store.RegistryCredentialOperation{}, err
	}
	return f.commitAttach(p)
}

func (f *fakeCredentialStore) commitAttach(p store.AttachRegistryCredentialSourceParams) (store.RegistryCredentialOperation, error) {
	operation := f.operations[p.IdempotencyKey]
	if operation == nil || operation.ID != p.OperationID {
		return store.RegistryCredentialOperation{}, store.ErrConflict
	}
	if operation.Status == "pending" {
		if f.secretRef != p.SecretRef {
			return store.RegistryCredentialOperation{}, store.ErrConflict
		}
		return *operation, nil
	}
	if operation.Status != "awaiting_source" {
		return store.RegistryCredentialOperation{}, store.ErrConflict
	}
	f.secretRef = p.SecretRef
	f.generation = store.RegistryCredentialGeneration{OrgID: p.OrgID, RegistryID: operation.RegistryID, Generation: operation.CandidateGeneration,
		SecretRef: p.SecretRef, State: "pending", OperationID: operation.ID}
	operation.Status = "pending"
	if operation.Kind == "import_create" {
		operation.Status = "awaiting_import"
	}
	operation.Checkpoint = "source_secret_written"
	state := "provisioning"
	if operation.PreviousGeneration != nil {
		state = "rotating"
	}
	f.registry.State = state
	f.registry.Checkpoint = "source_secret_written"
	f.registry.HasPendingCredential = true
	return *operation, nil
}

func (f *fakeCredentialStore) GetRegistryCredentialOperation(_ context.Context, _, id string) (store.RegistryCredentialOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("get_operation")
	if err := f.errs["get_operation"]; err != nil {
		return store.RegistryCredentialOperation{}, err
	}
	for _, operation := range f.operations {
		if operation.ID == id {
			return *operation, nil
		}
	}
	return store.RegistryCredentialOperation{}, store.ErrNotFound
}

func (f *fakeCredentialStore) GetRegistryCredentialGeneration(_ context.Context, _, _ string, _ int64) (store.RegistryCredentialGeneration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("get_generation")
	if err := f.errs["get_generation"]; err != nil {
		return store.RegistryCredentialGeneration{}, err
	}
	if f.generation.State == "" {
		for _, operation := range f.operations {
			return store.RegistryCredentialGeneration{OrgID: operation.OrgID, RegistryID: operation.RegistryID, Generation: operation.CandidateGeneration,
				State: "reserved", OperationID: operation.ID}, nil
		}
		return store.RegistryCredentialGeneration{}, store.ErrNotFound
	}
	return f.generation, nil
}

func (f *fakeCredentialStore) callSequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCredentialStore) setError(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[name] = err
}

type writeCall struct {
	orgID, registryID, operationID string
	generation                     int64
	username, pat                  string
}

type fakeSourceWriter struct {
	mu        sync.Mutex
	calls     []writeCall
	refs      []string
	err       error
	errOnce   bool
	deletes   int
	deleteErr error
}

func (f *fakeSourceWriter) DeleteCredential(_ context.Context, _, _, _ string, _ int64, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	return f.deleteErr
}

func (f *fakeSourceWriter) WriteCredential(_ context.Context, orgID, registryID, operationID string, generation int64, credential *oci.Credential) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, writeCall{orgID: orgID, registryID: registryID, operationID: operationID, generation: generation,
		username: string(credential.Username), pat: string(credential.PAT)})
	if f.err != nil && (!f.errOnce || len(f.calls) == 1) {
		return "", "", "", f.err
	}
	ref := fmt.Sprintf("k8s-secret://cyclopus-system/credential-%d#dockerconfigjson", len(f.refs)+1)
	f.refs = append(f.refs, ref)
	return ref, "uid", "1", nil
}

func (f *fakeSourceWriter) writes() []writeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]writeCall(nil), f.calls...)
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func readyGHCRRegistry() *store.Registry {
	return &store.Registry{ID: "registry-1", OrgID: "org-1", Provider: "ghcr", RegistryHost: "ghcr.io",
		State: "ready", ActiveGeneration: 1, HasActiveCredential: true}
}

func newTestRegistryCredentialService(persistence RegistryCredentialStore, source RegistryCredentialSourceWriter) *RegistryCredentialService {
	return NewRegistryCredentialService(persistence, source, []byte("0123456789abcdef0123456789abcdef"))
}

func TestRotateGhcrCredentialSequenceAndPendingStatus(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	registry, err := credentials.RotateGhcrCredential(t.Context(), ActorContext{UserID: "user", OrgID: "org-1", Operator: true},
		GhcrCredentialInput{Username: []byte("octocat"), PersonalAccessToken: []byte("pat-value")}, "rotate-1")
	if err != nil {
		t.Fatalf("rotate = %v", err)
	}
	if got, want := persistence.callSequence(), []string{"get_registry", "reserve", "attach", "get_registry"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	writes := source.writes()
	if len(writes) != 1 || writes[0].generation != 2 || writes[0].registryID != "registry-1" || writes[0].operationID != "operation-1" {
		t.Fatalf("writes = %+v", writes)
	}
	if registry.State != "rotating" || !registry.HasPendingCredential || registry.PendingGeneration == nil || *registry.PendingGeneration != 2 {
		t.Fatalf("status = %+v", registry)
	}
}

func TestRotateGhcrCredentialRequiresAdministrator(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	if _, err := credentials.RotateGhcrCredential(t.Context(), ActorContext{UserID: "user", OrgID: "org-1"},
		GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}, "rotate"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin error = %v", err)
	}
	if got := persistence.callSequence(); len(got) != 0 {
		t.Fatalf("store touched before authorization: %v", got)
	}
}

func TestRotateGhcrCredentialValidationBounds(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	credentials := newTestRegistryCredentialService(persistence, &fakeSourceWriter{})
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	for name, input := range map[string]GhcrCredentialInput{
		"empty username": {PersonalAccessToken: []byte("pat")},
		"empty pat":      {Username: []byte("octocat")},
		"control":        {Username: []byte("octo\ncat"), PersonalAccessToken: []byte("pat")},
		"invalid utf8":   {Username: []byte{0xff}, PersonalAccessToken: []byte("pat")},
		"oversized pat":  {Username: []byte("octocat"), PersonalAccessToken: make([]byte, 4097)},
	} {
		if _, err := credentials.RotateGhcrCredential(t.Context(), admin, input, "rotate"); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s error = %v, want invalid argument", name, err)
		}
	}
	if _, err := credentials.RotateGhcrCredential(t.Context(), admin, GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}, "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank idempotency key error = %v", err)
	}
	if got := persistence.callSequence(); len(got) != 0 {
		t.Fatalf("invalid input reached the store: %v", got)
	}
}

func TestRotateGhcrCredentialPreconditions(t *testing.T) {
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	input := GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}
	t.Run("absent", func(t *testing.T) {
		credentials := newTestRegistryCredentialService(newFakeCredentialStore(nil), &fakeSourceWriter{})
		if _, err := credentials.RotateGhcrCredential(t.Context(), admin, input, "rotate"); !errors.Is(err, ErrRegistryAuthRequired) {
			t.Fatalf("error = %v, want auth required", err)
		}
	})
	t.Run("in progress", func(t *testing.T) {
		persistence := newFakeCredentialStore(readyGHCRRegistry())
		source := &fakeSourceWriter{}
		if _, err := newTestRegistryCredentialService(persistence, source).RotateGhcrCredential(t.Context(), admin, input, "rotate"); err != nil {
			t.Fatal(err)
		}
		// A second rotation with a new key sees the in-flight candidate and is
		// rejected without a Secret write; org state remains one operation.
		if _, err := newTestRegistryCredentialService(persistence, source).RotateGhcrCredential(t.Context(), admin, input, "rotate-2"); !errors.Is(err, ErrRegistryCredentialInProgress) {
			t.Fatalf("error = %v, want in progress", err)
		}
		if len(source.writes()) != 1 {
			t.Fatalf("source writes = %d, want 1", len(source.writes()))
		}
	})
	t.Run("reserve conflict", func(t *testing.T) {
		persistence := func() *fakeCredentialStore {
			f := newFakeCredentialStore(readyGHCRRegistry())
			f.errs["reserve"] = store.ErrConflict
			return f
		}()
		credentials := newTestRegistryCredentialService(persistence, &fakeSourceWriter{})
		if _, err := credentials.RotateGhcrCredential(t.Context(), admin, input, "rotate"); !errors.Is(err, ErrInProgress) {
			t.Fatalf("error = %v, want in progress", err)
		}
	})
	t.Run("no active generation", func(t *testing.T) {
		registry := &store.Registry{ID: "registry-1", OrgID: "org-1", Provider: "ghcr", RegistryHost: "ghcr.io", State: "failed"}
		credentials := newTestRegistryCredentialService(newFakeCredentialStore(registry), &fakeSourceWriter{})
		if _, err := credentials.RotateGhcrCredential(t.Context(), admin, input, "rotate"); !errors.Is(err, ErrRegistryAuthRequired) {
			t.Fatalf("error = %v, want auth required", err)
		}
	})
}

func TestRotateGhcrCredentialReplayAfterTransientWriteFailure(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{err: errors.New("kubernetes unavailable"), errOnce: true}
	credentials := newTestRegistryCredentialService(persistence, source)
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	input := GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}
	if _, err := credentials.RotateGhcrCredential(t.Context(), admin, input, "rotate"); !errors.Is(err, ErrRegistryCredentialRetry) {
		t.Fatalf("first attempt error = %v, want retry", err)
	}
	// The durable reservation is replayed by a fresh attempt whose registry
	// snapshot still shows ready: reserve replays, the source write retries,
	// and attach completes exactly once more without a new generation.
	registry, err := newTestRegistryCredentialService(persistence, source).RotateGhcrCredential(t.Context(), admin, input, "rotate")
	if err != nil {
		t.Fatalf("retry = %v", err)
	}
	if got, want := persistence.callSequence(), []string{"get_registry", "reserve", "get_registry", "get_operation", "reserve", "attach", "get_registry"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if len(source.writes()) != 2 {
		t.Fatalf("source writes = %d, want 2", len(source.writes()))
	}
	if registry.State != "rotating" {
		t.Fatalf("status = %+v", registry)
	}
}

func TestRotateGhcrCredentialReplayAfterAttachUsesFreshSnapshot(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	input := GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}
	if _, err := credentials.RotateGhcrCredential(t.Context(), admin, input, "rotate"); err != nil {
		t.Fatal(err)
	}
	// A retry in a fresh process reads the updated rotating snapshot and
	// returns the same durable operation state without touching the Secret.
	if _, err := newTestRegistryCredentialService(persistence, source).RotateGhcrCredential(t.Context(), admin, input, "rotate"); err != nil {
		t.Fatalf("post-attach retry error = %v", err)
	}
	if len(source.writes()) != 1 {
		t.Fatalf("source writes = %d, want 1", len(source.writes()))
	}
}

func TestRegistryCredentialFingerprintIsKeyedAndCredentialBound(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{}
	input := GhcrCredentialInput{Username: []byte("octocat"), PersonalAccessToken: []byte("pat-one")}
	first := newTestRegistryCredentialService(persistence, source).credentialFingerprint(input)
	second := newTestRegistryCredentialService(persistence, source).credentialFingerprint(input)
	if first != second {
		t.Fatal("same key and credential produced different fingerprints")
	}
	changedPAT := newTestRegistryCredentialService(persistence, source).credentialFingerprint(GhcrCredentialInput{Username: []byte("octocat"), PersonalAccessToken: []byte("pat-two")})
	if first == changedPAT {
		t.Fatal("different PAT produced the same fingerprint")
	}
	otherKey := NewRegistryCredentialService(persistence, source, []byte("abcdef0123456789abcdef0123456789")).credentialFingerprint(input)
	if first == otherKey {
		t.Fatal("different instance key produced the same fingerprint")
	}
}

func TestRotateGhcrCredentialReplayRejectsChangedCredential(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{err: errors.New("kubernetes unavailable"), errOnce: true}
	credentials := newTestRegistryCredentialService(persistence, source)
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	if _, err := credentials.RotateGhcrCredential(t.Context(), admin, GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("pat-one")}, "rotate"); !errors.Is(err, ErrRegistryCredentialRetry) {
		t.Fatalf("first attempt error = %v", err)
	}
	if _, err := credentials.RotateGhcrCredential(t.Context(), admin, GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("pat-two")}, "rotate"); !errors.Is(err, ErrInProgress) {
		t.Fatalf("changed credential replay error = %v, want conflict", err)
	}
}

func TestRotateGhcrCredentialRejectsPreAttachedOperationFromAnotherFlow(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{}
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	input := GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}
	if _, err := newTestRegistryCredentialService(persistence, source).RotateGhcrCredential(t.Context(), admin, input, "rotate"); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestRegistryCredentialService(persistence, source).RotateGhcrCredential(t.Context(), admin, input, "rotate-2"); !errors.Is(err, ErrRegistryCredentialInProgress) {
		t.Fatalf("second rotation error = %v, want in progress", err)
	}
	if len(source.writes()) != 1 {
		t.Fatalf("source writes = %d, want 1", len(source.writes()))
	}
}

func TestRotateGhcrCredentialAttachConflictIsPermanent(t *testing.T) {
	persistence := func() *fakeCredentialStore {
		f := newFakeCredentialStore(readyGHCRRegistry())
		f.errs["attach"] = store.ErrConflict
		return f
	}()
	source := &fakeSourceWriter{deleteErr: errors.New("cleanup ref must remain private")}
	credentials := newTestRegistryCredentialService(persistence, source)
	if _, err := credentials.RotateGhcrCredential(t.Context(), ActorContext{UserID: "user", OrgID: "org-1", Operator: true},
		GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}, "rotate"); !errors.Is(err, ErrConflict) {
		t.Fatalf("attach conflict error = %v", err)
	}
	if source.deletes != 1 {
		t.Fatalf("cleanup deletes = %d, want 1", source.deletes)
	}
}

func TestRotateGhcrCredentialAmbiguousAttachAfterCommitReturnsSuccess(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	persistence.errs["attach"] = errors.New("connection reset after commit")
	persistence.attachCommit = true
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	registry, err := credentials.RotateGhcrCredential(t.Context(), ActorContext{UserID: "user", OrgID: "org-1", Operator: true},
		GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}, "rotate")
	if err != nil || registry.State != "rotating" {
		t.Fatalf("ambiguous committed attach = %+v, %v", registry, err)
	}
	if source.deletes != 0 {
		t.Fatalf("committed source deletes = %d, want 0", source.deletes)
	}
}

func TestRotateGhcrCredentialUnknownAttachPreservesSource(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	persistence.errs["attach"] = errors.New("database unavailable")
	persistence.errs["get_operation"] = errors.New("database unavailable")
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	_, err := credentials.RotateGhcrCredential(t.Context(), ActorContext{UserID: "user", OrgID: "org-1", Operator: true},
		GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}, "rotate")
	if !errors.Is(err, ErrRegistryCredentialRetry) {
		t.Fatalf("unknown attach error = %v, want retry", err)
	}
	if source.deletes != 0 {
		t.Fatalf("unknown attach deletes = %d, want 0", source.deletes)
	}
}

func TestRotateGhcrCredentialConcurrentOperationsAreIsolated(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{}
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	input := GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}
	results := make(chan error, 4)
	for index := 0; index < 4; index++ {
		go func(index int) {
			_, err := newTestRegistryCredentialService(persistence, source).RotateGhcrCredential(t.Context(), admin, input, fmt.Sprintf("rotate-%d", index))
			results <- err
		}(index)
	}
	var succeeded, rejected int
	for index := 0; index < 4; index++ {
		switch err := <-results; {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInProgress) || errors.Is(err, ErrRegistryCredentialInProgress) || errors.Is(err, ErrConflict):
			rejected++
		default:
			t.Fatalf("concurrent rotate error = %v", err)
		}
	}
	if succeeded != 1 || rejected != 3 {
		t.Fatalf("succeeded=%d rejected=%d, want 1/3", succeeded, rejected)
	}
}

func TestImportCredentialFirstCreateSequence(t *testing.T) {
	persistence := newFakeCredentialStore(nil)
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	binding, err := credentials.reserveImportCredential(t.Context(), admin, "acme/api", "0123456789abcdef0123456789abcdef01234567",
		GhcrCredentialInput{Username: []byte("octocat"), PersonalAccessToken: []byte("pat-value")}, true)
	if err != nil {
		t.Fatalf("first create = %v", err)
	}
	if got, want := persistence.callSequence(), []string{"get_registry", "ensure_registry", "reserve", "attach", "get_registry"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	writes := source.writes()
	if len(writes) != 1 || writes[0].generation != 1 {
		t.Fatalf("writes = %+v", writes)
	}
	if binding.registry.State != "provisioning" || binding.registry.OperationID == nil || *binding.registry.OperationID != "operation-1" {
		t.Fatalf("post-attach registry = %+v", binding.registry)
	}
	if binding.generation != 1 || binding.registry.ID != "registry-1" || binding.operation == nil || binding.operation.Kind != "import_create" {
		t.Fatalf("binding = %+v", binding)
	}
	if binding.operation.Status != "awaiting_import" {
		t.Fatalf("operation status = %q, want awaiting_import", binding.operation.Status)
	}
	if got := *binding.operation.TargetRepository; got != "acme/api" {
		t.Fatalf("target repository = %q", got)
	}
}

func TestImportCredentialFirstCreateReplayAfterAttach(t *testing.T) {
	persistence := newFakeCredentialStore(nil)
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	input := GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}
	first, err := credentials.reserveImportCredential(t.Context(), admin, "acme/api", "0123456789abcdef0123456789abcdef01234567", input, true)
	if err != nil {
		t.Fatal(err)
	}
	// A retry in a fresh process sees the provisioning registry snapshot and
	// replays the durable reservation by target: same operation and
	// candidate, no second Secret write.
	replay, err := newTestRegistryCredentialService(persistence, source).reserveImportCredential(t.Context(), admin, "acme/api", "0123456789abcdef0123456789abcdef01234567", input, true)
	if err != nil {
		t.Fatalf("replay = %v", err)
	}
	if replay.operation == nil || replay.operation.ID != first.operation.ID || replay.generation != first.generation {
		t.Fatalf("replay binding = %+v, want operation %s generation %d", replay, first.operation.ID, first.generation)
	}
	if len(source.writes()) != 1 {
		t.Fatalf("source writes = %d, want 1", len(source.writes()))
	}
}

func TestImportCredentialFirstCreateRejectsChangedTarget(t *testing.T) {
	persistence := newFakeCredentialStore(nil)
	credentials := newTestRegistryCredentialService(persistence, &fakeSourceWriter{})
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	input := GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}
	if _, err := credentials.reserveImportCredential(t.Context(), admin, "acme/api", "0123456789abcdef0123456789abcdef01234567", input, true); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestRegistryCredentialService(persistence, &fakeSourceWriter{}).reserveImportCredential(t.Context(), admin, "acme/other", "0123456789abcdef0123456789abcdef01234567", input, true); !errors.Is(err, ErrRegistryCredentialInProgress) {
		t.Fatalf("changed target error = %v, want in progress", err)
	}
}

func TestImportCredentialReadyReuseRejectsSuppliedReplacement(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	binding, err := credentials.reserveImportCredential(t.Context(), admin, "acme/api", "0123456789abcdef0123456789abcdef01234567", GhcrCredentialInput{}, false)
	if err != nil {
		t.Fatalf("reuse = %v", err)
	}
	if binding.operation != nil || binding.generation != 1 || binding.registry.ID != "registry-1" {
		t.Fatalf("reuse binding = %+v", binding)
	}
	if got := persistence.callSequence(); fmt.Sprint(got) != "[get_registry]" {
		t.Fatalf("calls = %v, want only get_registry", got)
	}
	if _, err := credentials.reserveImportCredential(t.Context(), admin, "acme/api", "0123456789abcdef0123456789abcdef01234567",
		GhcrCredentialInput{Username: []byte("other"), PersonalAccessToken: []byte("pat")}, true); !errors.Is(err, ErrRegistryCredentialExists) {
		t.Fatalf("supplied replacement error = %v, want exists", err)
	}
	if len(source.writes()) != 0 {
		t.Fatalf("replacement reached the Secret writer")
	}
}

func TestImportCredentialRequiresInputWhenUnconfigured(t *testing.T) {
	persistence := newFakeCredentialStore(nil)
	credentials := newTestRegistryCredentialService(persistence, &fakeSourceWriter{})
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	if _, err := credentials.reserveImportCredential(t.Context(), admin, "acme/api", "0123456789abcdef0123456789abcdef01234567", GhcrCredentialInput{}, false); !errors.Is(err, ErrRegistryAuthRequired) {
		t.Fatalf("missing input error = %v, want auth required", err)
	}
	if got := persistence.callSequence(); fmt.Sprint(got) != "[get_registry]" {
		t.Fatalf("calls = %v", got)
	}
}

func TestImportCredentialInProgressRegistry(t *testing.T) {
	persistence := newFakeCredentialStore(readyGHCRRegistry())
	source := &fakeSourceWriter{}
	admin := ActorContext{UserID: "user", OrgID: "org-1", Operator: true}
	input := GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}
	if _, err := newTestRegistryCredentialService(persistence, source).RotateGhcrCredential(t.Context(), admin, input, "rotate"); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestRegistryCredentialService(persistence, source).reserveImportCredential(t.Context(), admin, "acme/api", "0123456789abcdef0123456789abcdef01234567", input, true); !errors.Is(err, ErrRegistryCredentialInProgress) {
		t.Fatalf("error = %v, want in progress", err)
	}
	if len(source.writes()) != 1 {
		t.Fatalf("source writes = %d, want 1", len(source.writes()))
	}
}

func TestImportCredentialFailedRegistryAllowsNewCandidate(t *testing.T) {
	registry := &store.Registry{ID: "registry-1", OrgID: "org-1", Provider: "ghcr", RegistryHost: "ghcr.io", State: "failed"}
	persistence := newFakeCredentialStore(registry)
	source := &fakeSourceWriter{}
	credentials := newTestRegistryCredentialService(persistence, source)
	binding, err := credentials.reserveImportCredential(t.Context(), ActorContext{UserID: "user", OrgID: "org-1", Operator: true},
		"acme/api", "0123456789abcdef0123456789abcdef01234567", GhcrCredentialInput{Username: []byte("u"), PersonalAccessToken: []byte("p")}, true)
	if err != nil {
		t.Fatalf("failed registry recreate = %v", err)
	}
	if binding.operation == nil || binding.generation != 1 {
		t.Fatalf("binding = %+v", binding)
	}
	if len(source.writes()) != 1 {
		t.Fatalf("source writes = %d, want 1", len(source.writes()))
	}
}
