package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/store"
)

var (
	// ErrRegistryCredentialInProgress reports an existing registry credential
	// operation that is still active; the caller must wait for it to finish.
	ErrRegistryCredentialInProgress = errors.New("service: registry credential operation in progress")
	// ErrRegistryCredentialRetry reports a transient write/attach failure after
	// the operation was reserved: the caller must retry the request with the
	// same idempotency key and input because raw credential material is never
	// persisted and cannot be recovered by the platform.
	ErrRegistryCredentialRetry = errors.New("service: registry credential source write failed; retry with the same idempotency key")
	// ErrRegistryCredentialExists rejects a supplied replacement credential when
	// the organization already has an active or in-progress GHCR credential;
	// replacement requires the explicit rotation RPC.
	ErrRegistryCredentialExists = errors.New("service: registry credential already configured")
)

const (
	maxRegistryCredentialUsernameBytes = 255
	maxRegistryCredentialPATBytes      = 4096
	registryCredentialOperationKind    = "github.repository.import"
)

// RegistryCredentialStore is the narrow persistence surface the per-org GHCR
// credential service needs. *store.Store satisfies it; tests inject fakes.
type RegistryCredentialStore interface {
	GetGHCRRegistry(ctx context.Context, orgID string) (store.Registry, error)
	EnsureGHCRRegistry(ctx context.Context, orgID string) (store.Registry, error)
	ReserveRegistryCredentialOperation(ctx context.Context, p store.ReserveRegistryCredentialOperationParams) (store.RegistryCredentialOperation, error)
	AttachRegistryCredentialSource(ctx context.Context, p store.AttachRegistryCredentialSourceParams) (store.RegistryCredentialOperation, error)
	GetRegistryCredentialOperation(ctx context.Context, orgID, id string) (store.RegistryCredentialOperation, error)
	GetRegistryCredentialGeneration(ctx context.Context, orgID, registryID string, generation int64) (store.RegistryCredentialGeneration, error)
}

type RegistryCredentialRecoveryStore interface {
	RescueAwaitingRegistryCredentialSources(ctx context.Context, olderThan time.Time, limit int) (int64, error)
	ListRegistryCredentialGarbageCollectionCandidates(ctx context.Context, olderThan time.Time, limit int) ([]store.RegistryCredentialGarbageCollectionCandidate, error)
}

// RegistryCredentialSourceWriter writes the immutable candidate source Secret
// and returns its managed reference. The Kubernetes adapter computes the
// deterministic Secret name from (orgID, generation), so a retry of the same
// candidate adopts the previously written exact-owned Secret.
type RegistryCredentialSourceWriter interface {
	WriteCredential(ctx context.Context, orgID, registryID, operationID string, generation int64, credential *oci.Credential) (secretRef string, uid string, resourceVersion string, err error)
	DeleteCredential(ctx context.Context, orgID, registryID, operationID string, generation int64, gcAuthorized bool) error
}

// GhcrCredentialInput carries raw credential material in mutable buffers;
// callers must treat them as short-lived and never copy them into requests,
// logs, audits, or persistence.
type GhcrCredentialInput struct {
	Username, PersonalAccessToken []byte
}

// registryCredentialPersister adapts a *store.Store to RegistryCredentialStore
// so a *Service can be constructed over any narrow store implementation
// (the GitHub discovery/integration paths still require *store.Store).
type registryCredentialPersister struct{ dataStore *store.Store }

func (p registryCredentialPersister) GetGHCRRegistry(ctx context.Context, orgID string) (store.Registry, error) {
	return p.dataStore.GetGHCRRegistry(ctx, orgID)
}
func (p registryCredentialPersister) EnsureGHCRRegistry(ctx context.Context, orgID string) (store.Registry, error) {
	return store.EnsureGHCRRegistry(ctx, p.dataStore, orgID)
}
func (p registryCredentialPersister) ReserveRegistryCredentialOperation(ctx context.Context, params store.ReserveRegistryCredentialOperationParams) (store.RegistryCredentialOperation, error) {
	return p.dataStore.ReserveRegistryCredentialOperation(ctx, params)
}
func (p registryCredentialPersister) AttachRegistryCredentialSource(ctx context.Context, params store.AttachRegistryCredentialSourceParams) (store.RegistryCredentialOperation, error) {
	return p.dataStore.AttachRegistryCredentialSource(ctx, params)
}
func (p registryCredentialPersister) GetRegistryCredentialOperation(ctx context.Context, orgID, id string) (store.RegistryCredentialOperation, error) {
	return p.dataStore.GetRegistryCredentialOperation(ctx, orgID, id)
}
func (p registryCredentialPersister) GetRegistryCredentialGeneration(ctx context.Context, orgID, registryID string, generation int64) (store.RegistryCredentialGeneration, error) {
	return p.dataStore.GetRegistryCredentialGeneration(ctx, orgID, registryID, generation)
}

// reservedRegistryCredential carries the durable candidate reservation an
// import commits into its own transaction: the app binding plus the
// environment's required credential generation. operation is nil when the
// import reuses an already-ready active credential.
type reservedRegistryCredential struct {
	registry   store.Registry
	generation int64
	ready      bool
	operation  *store.RegistryCredentialOperation
}

func reservedOperationID(reserved *reservedRegistryCredential) string {
	if reserved == nil || reserved.operation == nil {
		return ""
	}
	return reserved.operation.ID
}

func reservedGeneration(reserved *reservedRegistryCredential) int64 {
	if reserved == nil {
		return 0
	}
	return reserved.generation
}

// RegistryCredentialService owns the per-organization GHCR credential create
// and rotation flows: reservation with idempotency-key replay, raw-material
// validation, source Secret write, and attach. Validation, activation,
// namespace reconciliation, and garbage collection stay with the async saga
// worker from Task 6A/7; this service never activates a candidate.
type RegistryCredentialService struct {
	persistence    RegistryCredentialStore
	source         RegistryCredentialSourceWriter
	fingerprintKey []byte
}

// NewRegistryCredentialService builds the credential service over a narrow
// store implementation; used directly by focused tests.
func NewRegistryCredentialService(persistence RegistryCredentialStore, source RegistryCredentialSourceWriter, fingerprintKey []byte) *RegistryCredentialService {
	if persistence == nil || source == nil || len(fingerprintKey) < 32 {
		return nil
	}
	return &RegistryCredentialService{persistence: persistence, source: source, fingerprintKey: append([]byte(nil), fingerprintKey...)}
}

// NewRegistryCredentialServiceFromStore builds the credential service over the
// concrete store for production wiring and integration tests.
func NewRegistryCredentialServiceFromStore(dataStore *store.Store, source RegistryCredentialSourceWriter, fingerprintKey []byte) (*RegistryCredentialService, error) {
	if dataStore == nil {
		return nil, errors.New("registry credential service requires a store")
	}
	credentials := NewRegistryCredentialService(registryCredentialPersister{dataStore: dataStore}, source, fingerprintKey)
	if credentials == nil {
		return nil, errors.New("registry credential service requires a source writer and 32-byte fingerprint key")
	}
	return credentials, nil
}

// Configured reports whether the service has every dependency needed to
// perform credential mutations.
func (s *RegistryCredentialService) Configured() bool {
	return s != nil && s.persistence != nil && s.source != nil && len(s.fingerprintKey) >= 32
}

func (p registryCredentialPersister) RescueAwaitingRegistryCredentialSources(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	return p.dataStore.RescueAwaitingRegistryCredentialSources(ctx, olderThan, limit)
}

func (p registryCredentialPersister) ListRegistryCredentialGarbageCollectionCandidates(ctx context.Context, olderThan time.Time, limit int) ([]store.RegistryCredentialGarbageCollectionCandidate, error) {
	return p.dataStore.ListRegistryCredentialGarbageCollectionCandidates(ctx, olderThan, limit)
}

func (s *RegistryCredentialService) RecoveryCandidates(ctx context.Context, olderThan time.Time, limit int) (int64, []store.RegistryCredentialGarbageCollectionCandidate, error) {
	recovery, ok := s.persistence.(RegistryCredentialRecoveryStore)
	if !ok {
		return 0, nil, store.ErrFailedPrecondition
	}
	rescued, err := recovery.RescueAwaitingRegistryCredentialSources(ctx, olderThan, limit)
	if err != nil {
		return 0, nil, err
	}
	candidates, err := recovery.ListRegistryCredentialGarbageCollectionCandidates(ctx, olderThan, limit)
	return rescued, candidates, err
}

func (s *RegistryCredentialService) credentialFingerprint(input GhcrCredentialInput) [32]byte {
	mac := hmac.New(sha256.New, s.fingerprintKey)
	_, _ = mac.Write([]byte("cyclopus.registry-credential.v1\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(input.Username)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(input.Username)
	binary.BigEndian.PutUint64(length[:], uint64(len(input.PersonalAccessToken)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(input.PersonalAccessToken)
	var fingerprint [32]byte
	copy(fingerprint[:], mac.Sum(nil))
	return fingerprint
}

func (s *Service) registryCredentials() *RegistryCredentialService {
	if s == nil {
		return nil
	}
	return s.registryCredentialService
}

// validRegistryCredentialField requires valid UTF-8 within bounds and rejects
// ASCII control bytes and DEL, matching the Kubernetes adapter's material
// contract so a candidate never fails Secret writing for malformed input.
func validRegistryCredentialField(value []byte, max int) bool {
	if len(value) == 0 || len(value) > max || !utf8.Valid(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validRegistryCredentialInput(input GhcrCredentialInput) bool {
	return validRegistryCredentialField(input.Username, maxRegistryCredentialUsernameBytes) &&
		!bytes.Contains(input.Username, []byte{':'}) &&
		validRegistryCredentialField(input.PersonalAccessToken, maxRegistryCredentialPATBytes)
}

// reserve persists the operation/candidate reservation. A replayed idempotency
// key with identical kind/target returns the original reservation without a
// new generation; a different kind/target on the same key conflicts. The
// supplied secret bytes are never part of the reservation or its comparison.
func (s *RegistryCredentialService) reserve(ctx context.Context, actor ActorContext, registryID, kind, key, repository, reference string, fingerprint [32]byte) (store.RegistryCredentialOperation, error) {
	operation, err := s.persistence.ReserveRegistryCredentialOperation(ctx, store.ReserveRegistryCredentialOperationParams{
		OrgID: actor.OrgID, RegistryID: registryID, Kind: kind, IdempotencyKey: key,
		TargetRepository: repository, TargetReference: reference, CredentialFingerprint: fingerprint,
		ActorUserID: actor.UserID, AuditID: newID()})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			return operation, ErrInProgress
		case errors.Is(err, store.ErrFailedPrecondition):
			return operation, registryCredentialFailureError(operation)
		case errors.Is(err, store.ErrInvalidArgument):
			return operation, ErrInvalidArgument
		}
		return operation, err
	}
	return operation, nil
}

func registryCredentialFailureError(operation store.RegistryCredentialOperation) error {
	if operation.FailureCode != nil && *operation.FailureCode == "registry_auth_failed" {
		return ErrRegistryAuthRequired
	}
	return store.ErrFailedPrecondition
}

// attachSource constructs the exact candidate credential, writes the immutable
// source Secret (deterministic name, adopt-on-retry), then attaches its
// reference. Raw material is cleared on every path. A transient failure leaves
// the operation in awaiting_source so the same idempotency-key request can
// retry; it never terminalizes the operation and never persists raw bytes.
func (s *RegistryCredentialService) attachSource(ctx context.Context, operation store.RegistryCredentialOperation, input GhcrCredentialInput) (store.RegistryCredentialOperation, error) {
	credential := &oci.Credential{Username: append([]byte(nil), input.Username...), PAT: append([]byte(nil), input.PersonalAccessToken...),
		OrgID: operation.OrgID, RegistryID: operation.RegistryID, Generation: operation.CandidateGeneration}
	defer credential.Cleanup()
	secretRef, _, _, err := s.source.WriteCredential(ctx, operation.OrgID, operation.RegistryID, operation.ID, operation.CandidateGeneration, credential)
	if err != nil {
		return operation, fmt.Errorf("%w: %v", ErrRegistryCredentialRetry, err)
	}
	attached, err := s.persistence.AttachRegistryCredentialSource(ctx, store.AttachRegistryCredentialSourceParams{
		OrgID: operation.OrgID, OperationID: operation.ID, IdempotencyKey: operation.IdempotencyKey, SecretRef: secretRef})
	if err != nil {
		current, operationErr := s.persistence.GetRegistryCredentialOperation(ctx, operation.OrgID, operation.ID)
		generation, generationErr := s.persistence.GetRegistryCredentialGeneration(ctx, operation.OrgID, operation.RegistryID, operation.CandidateGeneration)
		registry, registryErr := s.persistence.GetGHCRRegistry(ctx, operation.OrgID)
		if operationErr == nil && generationErr == nil && registryErr == nil {
			attachedStatus := "pending"
			if operation.Kind == "import_create" {
				attachedStatus = "awaiting_import"
			}
			committed := current.Status == attachedStatus && current.Checkpoint == "source_secret_written" &&
				generation.State == "pending" && generation.SecretRef == secretRef &&
				registry.OperationID != nil && *registry.OperationID == operation.ID &&
				registry.PendingGeneration != nil && *registry.PendingGeneration == operation.CandidateGeneration &&
				registry.HasPendingCredential && registry.Checkpoint == "source_secret_written"
			if committed {
				return current, nil
			}
			preCommit := current.Status == "awaiting_source" && current.Checkpoint == "reserved" &&
				generation.State == "reserved" && generation.SecretRef == "" &&
				registry.OperationID != nil && *registry.OperationID == operation.ID &&
				registry.PendingGeneration != nil && *registry.PendingGeneration == operation.CandidateGeneration &&
				!registry.HasPendingCredential
			if preCommit {
				cleanupCtx := context.WithoutCancel(ctx)
				_ = s.source.DeleteCredential(cleanupCtx, operation.OrgID, operation.RegistryID, operation.ID, operation.CandidateGeneration, true)
				if errors.Is(err, store.ErrConflict) {
					return operation, ErrConflict
				}
			}
		}
		return operation, fmt.Errorf("%w: %v", ErrRegistryCredentialRetry, err)
	}
	return attached, nil
}

// execute runs reserve → write → attach and returns the non-secret registry
// status. Replay of the same idempotency key after a crash re-executes the
// remaining steps without a second Secret or generation.
func (s *RegistryCredentialService) execute(ctx context.Context, actor ActorContext, registryID, kind, key, repository, reference string, input GhcrCredentialInput) (store.Registry, error) {
	operation, err := s.reserve(ctx, actor, registryID, kind, key, repository, reference, s.credentialFingerprint(input))
	if err != nil {
		return store.Registry{}, err
	}
	if operation.Status == "awaiting_source" {
		if operation, err = s.attachSource(ctx, operation, input); err != nil {
			return store.Registry{}, err
		}
	}
	return s.persistence.GetGHCRRegistry(ctx, actor.OrgID)
} // RotateGhcrCredential starts a candidate rotation for the organization's
// existing ready GHCR credential. It requires an active generation, rejects a
// concurrent active operation, writes the new source Secret, and returns the
// pending non-secret status; the saga worker validates and activates.
func (s *RegistryCredentialService) RotateGhcrCredential(ctx context.Context, actor ActorContext, input GhcrCredentialInput, key string) (store.Registry, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.Registry{}, err
	}
	if !s.Configured() {
		return store.Registry{}, ErrRegistryAuthRequired
	}
	if !validRegistryCredentialInput(input) || !validIdempotencyKey(key) {
		return store.Registry{}, ErrInvalidArgument
	}
	registry, err := s.persistence.GetGHCRRegistry(ctx, actor.OrgID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Registry{}, ErrRegistryAuthRequired
	}
	if err != nil {
		return store.Registry{}, mapError(err)
	}
	// An in-flight operation rejects a new rotation except the same-key retry
	// of a rotation that crashed after reserving but before attaching: raw
	// material is never persisted, so the caller must re-present it.
	if registry.OperationID != nil && registry.PendingGeneration != nil {
		existing, getErr := s.persistence.GetRegistryCredentialOperation(ctx, actor.OrgID, *registry.OperationID)
		if getErr != nil {
			return store.Registry{}, mapError(getErr)
		}
		if existing.Kind != "rotate" || existing.IdempotencyKey != key {
			return store.Registry{}, ErrRegistryCredentialInProgress
		}
		fingerprint := s.credentialFingerprint(input)
		if _, reserveErr := s.reserve(ctx, actor, registry.ID, "rotate", key, "", "", fingerprint); reserveErr != nil {
			return store.Registry{}, reserveErr
		}
		switch existing.Status {
		case "awaiting_source":
			if _, err := s.attachSource(ctx, existing, input); err != nil {
				return store.Registry{}, err
			}
		case "pending", "leased":
			return registry, nil
		case "failed", "cancelled":
			return store.Registry{}, registryCredentialFailureError(existing)
		default:
			return store.Registry{}, ErrRegistryCredentialInProgress
		}
		return s.persistence.GetGHCRRegistry(ctx, actor.OrgID)
	}
	if registry.State != "ready" || registry.ActiveGeneration < 1 || !registry.HasActiveCredential {
		return store.Registry{}, ErrRegistryAuthRequired
	}
	fingerprint := s.credentialFingerprint(input)
	operation, err := s.reserve(ctx, actor, registry.ID, "rotate", key, "", "", fingerprint)
	if err != nil {
		return store.Registry{}, err
	}
	if operation.Status == "awaiting_source" {
		if operation, err = s.attachSource(ctx, operation, input); err != nil {
			return store.Registry{}, err
		}
	}
	return s.persistence.GetGHCRRegistry(ctx, actor.OrgID)
}

// RotateGhcrCredential delegates to the credential service. Rotation targets
// are snapshotted from the org's live private images inside the reservation.
func (s *Service) RotateGhcrCredential(ctx context.Context, actor ActorContext, input GhcrCredentialInput, key string) (store.Registry, error) {
	credentials := s.registryCredentials()
	if credentials == nil {
		return store.Registry{}, ErrRegistryAuthRequired
	}
	return credentials.RotateGhcrCredential(ctx, actor, input, key)
}

// reserveImportCredential reserves (and, on first create, writes/attaches) the
// per-org GHCR credential for a private import. It runs after the import's
// GitHub calls establish the initial commit SHA and before the import store
// transaction so the app/environment rows can atomically reference the
// candidate. When a ready credential already exists, the import reuses it and
// any supplied replacement input is rejected in favor of explicit rotation.
func (s *RegistryCredentialService) reserveImportCredential(ctx context.Context, actor ActorContext, repository, reference string, input GhcrCredentialInput, supplied bool) (reservedRegistryCredential, error) {
	return s.reserveImportCredentialWithKey(ctx, actor, repository, reference, repository+":"+reference, input, supplied)
}

func (s *RegistryCredentialService) reserveImportCredentialWithKey(ctx context.Context, actor ActorContext, repository, reference, importKey string, input GhcrCredentialInput, supplied bool) (reservedRegistryCredential, error) {
	if !s.Configured() {
		return reservedRegistryCredential{}, ErrRegistryAuthRequired
	}
	registry, err := s.persistence.GetGHCRRegistry(ctx, actor.OrgID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return reservedRegistryCredential{}, mapError(err)
	}
	// Ready active credential: reuse. Supplied replacement material is
	// rejected so a PAT never silently disappears into a no-op.
	if err == nil && registry.State == "ready" && registry.ActiveGeneration > 0 && registry.HasActiveCredential && registry.OperationID == nil && registry.PendingGeneration == nil {
		if supplied {
			return reservedRegistryCredential{}, ErrRegistryCredentialExists
		}
		return reservedRegistryCredential{registry: registry, generation: registry.ActiveGeneration, ready: true}, nil
	}
	// A live reservation by a prior import attempt replays only for the exact
	// same repository/reference; any other target conflicts rather than
	// silently discarding the supplied material.
	if err == nil && registry.OperationID != nil && registry.PendingGeneration != nil {
		existing, getErr := s.persistence.GetRegistryCredentialOperation(ctx, actor.OrgID, *registry.OperationID)
		if getErr != nil {
			return reservedRegistryCredential{}, mapError(getErr)
		}
		sameTarget := existing.TargetRepository != nil && *existing.TargetRepository == repository &&
			existing.TargetReference != nil && *existing.TargetReference == reference
		if existing.Kind != "import_create" || existing.IdempotencyKey != registryCredentialImportKey(importKey) || !sameTarget ||
			existing.Status != "awaiting_source" && existing.Status != "awaiting_import" && existing.Status != "pending" && existing.Status != "leased" {
			return reservedRegistryCredential{}, ErrRegistryCredentialInProgress
		}
		if !supplied || !validRegistryCredentialInput(input) {
			return reservedRegistryCredential{}, ErrRegistryAuthRequired
		}
		fingerprint := s.credentialFingerprint(input)
		if _, reserveErr := s.reserve(ctx, actor, registry.ID, "import_create", registryCredentialImportKey(importKey), repository, reference, fingerprint); reserveErr != nil {
			return reservedRegistryCredential{}, reserveErr
		}
		if existing.Status == "awaiting_source" {
			if existing, getErr = s.attachSource(ctx, existing, input); getErr != nil {
				return reservedRegistryCredential{}, getErr
			}
		}
		return reservedRegistryCredential{registry: registry, generation: existing.CandidateGeneration, operation: &existing}, nil
	}
	// First create (absent, never-configured, or terminally failed) requires
	// raw input that passes the same bounds as the rotation RPC.
	if !supplied {
		return reservedRegistryCredential{}, ErrRegistryAuthRequired
	}
	if !validRegistryCredentialInput(input) {
		return reservedRegistryCredential{}, ErrInvalidArgument
	}
	if errors.Is(err, store.ErrNotFound) {
		if registry, err = s.persistence.EnsureGHCRRegistry(ctx, actor.OrgID); err != nil {
			return reservedRegistryCredential{}, mapError(err)
		}
		if registry.State == "ready" && registry.ActiveGeneration > 0 {
			// Ensure raced with a concurrent successful create: the supplied
			// replacement cannot be silently discarded.
			return reservedRegistryCredential{}, ErrRegistryCredentialExists
		}
		if registry.OperationID != nil && registry.PendingGeneration != nil {
			return reservedRegistryCredential{}, ErrRegistryCredentialInProgress
		}
	}
	fingerprint := s.credentialFingerprint(input)
	operation, err := s.reserve(ctx, actor, registry.ID, "import_create", registryCredentialImportKey(importKey), repository, reference, fingerprint)
	if err != nil {
		return reservedRegistryCredential{}, err
	}
	if operation.Status == "awaiting_source" {
		if operation, err = s.attachSource(ctx, operation, input); err != nil {
			return reservedRegistryCredential{}, err
		}
	}
	registry, err = s.persistence.GetGHCRRegistry(ctx, actor.OrgID)
	if err != nil {
		return reservedRegistryCredential{}, mapError(err)
	}
	return reservedRegistryCredential{registry: registry, generation: operation.CandidateGeneration, operation: &operation}, nil
}

// registryCredentialImportKey derives the deterministic operation key shared
// by every import of the same org/registry candidate. First-credential imports
// are deterministic against the same immutable GitHub target (repository at
// its head SHA), so a second import of the identical candidate replays the
// reservation instead of stacking operations; a different target or a
// completed key conflicts with the durable record rather than discarding a
// second PAT, and the rescue sweep bounds abandoned reservations.
func registryCredentialImportKey(importKey string) string {
	return "ghcr.import." + importKey
}

// validIdempotencyKey mirrors the store's request-key bounds without
// normalizing caller input.
func validIdempotencyKey(key string) bool {
	return len(key) > 0 && len(key) <= 255 && key == strings.TrimSpace(key) && utf8.ValidString(key)
}
