package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/secretref"
)

const maxRegistryCredentialAttempts = 12

type ReserveRegistryCredentialOperationParams struct {
	OrgID, RegistryID, Kind, IdempotencyKey string
	TargetRepository, TargetReference       string
	CredentialFingerprint                   [32]byte
	ActorUserID, AuditID                    string
}

type AttachRegistryCredentialSourceParams struct {
	OrgID, OperationID, IdempotencyKey, SecretRef string
}

type RegistryCredentialGeneration struct {
	OrgID, RegistryID, SecretRef, State, OperationID string
	Generation                                       int64
	CreatedAt                                        time.Time
	ValidatedAt, ActivatedAt, RetiredAt              *time.Time
	GarbageCollectedAt                               *time.Time
}

type RegistryCredentialOperation struct {
	ID, OrgID, RegistryID, Kind, Status, Checkpoint, IdempotencyKey string
	CandidateGeneration                                             int64
	PreviousGeneration                                              *int64
	TargetRepository, TargetReference, FailureCode, FailureMessage  *string
	Lease                                                           *LeaseToken
	Attempts                                                        int
	NextAttemptAt, CreatedAt, UpdatedAt                             time.Time
	TerminalAt                                                      *time.Time
}

type RegistryCredentialValidationTarget struct {
	OperationID, OrgID, RegistryID, Repository, Reference, Status string
	Attempts                                                      int
	NextAttemptAt                                                 time.Time
	FailureCode                                                   *string
	ValidatedAt                                                   *time.Time
	Lease, OperationLease                                         *LeaseToken
}

type RegistryCredentialMaterial struct {
	OrgID, RegistryID, OperationID, SecretRef string
	Generation                                int64
}

type RegistryCredentialGarbageCollectionCandidate struct {
	OrgID, RegistryID, OperationID, SecretRef string
	Generation                                int64
}

type RegistryCredentialGarbageCollectionDelete func(context.Context, RegistryCredentialGarbageCollectionCandidate) error

type NamespaceCredentialClaim struct {
	OrgID, EnvironmentID, AppID, Namespace string
	RegistryID, OperationID, SecretRef     string
	Checkpoint                             string
	Generation                             int64
	Attempts                               int
	Lease                                  LeaseToken
}

type NamespaceCredentialCleanupClaim struct {
	OrgID, EnvironmentID, AppID, Namespace string
	RegistryID, SecretRef                  string
	Generation                             int64
	Attempts                               int
	Lease                                  LeaseToken
}

type NamespaceCredentialReconciliation struct {
	OrgID, EnvironmentID, AppID, Namespace string
	RegistryID                             *string
	RequiredGeneration, ReadyGeneration    *int64
	State, Checkpoint                      string
	FailureCode, FailureMessage            *string
	Attempts                               int
	NextAttemptAt                          time.Time
	Lease                                  *LeaseToken
}

const (
	registryDueCandidateQuery = `SELECT id FROM registries
		WHERE state IN ('creating','rotating','reconciling','deleting') AND lease_owner IS NULL
		AND attempts<12 AND next_attempt_at<=clock_timestamp()
		ORDER BY next_attempt_at,updated_at,id LIMIT $1`
	registryExpiredCandidateQuery = `SELECT id FROM registries
		WHERE lease_owner IS NOT NULL AND attempts<12 AND lease_expires_at<=clock_timestamp()
		ORDER BY lease_expires_at,id LIMIT $1`
	ociDueCandidateQuery = `SELECT id FROM orchestration_jobs
		WHERE status='resolving_image' AND oci_resolution_state IN ('pending','polling')
		AND oci_lease_owner IS NULL AND oci_attempts<20 AND oci_next_attempt_at<=clock_timestamp()
		ORDER BY oci_next_attempt_at,created_at,id LIMIT $1`
	ociExpiredCandidateQuery = `SELECT id FROM orchestration_jobs
		WHERE oci_lease_owner IS NOT NULL AND oci_attempts<20 AND oci_lease_expires_at<=clock_timestamp()
		ORDER BY oci_lease_expires_at,id LIMIT $1`
	registryOperationDueCandidateQuery = `SELECT id FROM registry_credential_operations
		WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp()
		ORDER BY next_attempt_at,created_at,id LIMIT $1`
	registryOperationExpiredCandidateQuery = `SELECT id FROM registry_credential_operations
		WHERE status='leased' AND attempts<12 AND lease_expires_at<=clock_timestamp()
		ORDER BY lease_expires_at,id LIMIT $1`
	registryValidationTargetDueCandidateQuery = `SELECT operation_id,repository FROM registry_credential_validation_targets
		WHERE status='pending' AND lease_owner IS NULL AND attempts<12 AND next_attempt_at<=clock_timestamp()
		ORDER BY next_attempt_at,operation_id,repository LIMIT $1`
	namespaceCredentialDueCandidateQuery = `SELECT id FROM environments
		WHERE registry_credential_state='pending' AND registry_credential_lease_owner IS NULL
		AND registry_credential_checkpoint IS DISTINCT FROM 'namespace_secret_written'
		AND lifecycle_status<>'teardown_requested' AND torn_down_at IS NULL AND registry_credential_attempts<12
		AND registry_credential_next_attempt_at<=clock_timestamp()
		ORDER BY registry_credential_next_attempt_at,created_at,id LIMIT $1`
	namespaceCredentialExpiredCandidateQuery = `SELECT id FROM environments
		WHERE registry_credential_lease_owner IS NOT NULL AND registry_credential_attempts<12
		AND registry_credential_lease_expires_at<=clock_timestamp()
		ORDER BY registry_credential_lease_expires_at,id LIMIT $1`
	registryGenerationGCCandidateQuery = `SELECT registry_id,generation FROM registry_credential_generations
		WHERE state IN ('retired','garbage_collecting') AND retired_at<=$1
		ORDER BY retired_at,registry_id,generation LIMIT $2`
)

const registryCredentialOperationColumns = `id::text,org_id::text,registry_id::text,kind,status,checkpoint,
	candidate_generation,previous_generation,target_repository,target_reference,idempotency_key,lease_owner,lease_epoch,lease_expires_at,
	attempts,CASE WHEN next_attempt_at='infinity'::timestamptz THEN NULL ELSE next_attempt_at END,failure_code,failure_message,created_at,updated_at,terminal_at`
const registryCredentialOperationSelect = `SELECT ` + registryCredentialOperationColumns + ` FROM registry_credential_operations`

// registryCredentialOperationQualifiedColumns lists the same projections as
// registryCredentialOperationColumns qualified for the claim UPDATE ... FROM
// candidates form where bare names are ambiguous.
const registryCredentialOperationQualifiedColumns = `operation.id::text,operation.org_id::text,operation.registry_id::text,operation.kind,operation.status,operation.checkpoint,
	operation.candidate_generation,operation.previous_generation,operation.target_repository,operation.target_reference,operation.idempotency_key,operation.lease_owner,operation.lease_epoch,operation.lease_expires_at,
	operation.attempts,CASE WHEN operation.next_attempt_at='infinity'::timestamptz THEN NULL ELSE operation.next_attempt_at END,operation.failure_code,operation.failure_message,operation.created_at,operation.updated_at,operation.terminal_at`

func scanRegistryCredentialOperation(row rowScanner, value *RegistryCredentialOperation) error {
	var owner *string
	var epoch int64
	var expiresAt *time.Time
	var nextAttemptAt *time.Time
	if err := row.Scan(&value.ID, &value.OrgID, &value.RegistryID, &value.Kind, &value.Status, &value.Checkpoint,
		&value.CandidateGeneration, &value.PreviousGeneration, &value.TargetRepository, &value.TargetReference, &value.IdempotencyKey,
		&owner, &epoch, &expiresAt, &value.Attempts, &nextAttemptAt, &value.FailureCode, &value.FailureMessage,
		&value.CreatedAt, &value.UpdatedAt, &value.TerminalAt); err != nil {
		return err
	}
	// validation_complete pins next_attempt_at to infinity, surfaced as zero.
	if nextAttemptAt != nil {
		value.NextAttemptAt = *nextAttemptAt
	}
	if owner != nil && expiresAt != nil {
		value.Lease = &LeaseToken{Owner: *owner, Epoch: epoch, ExpiresAt: *expiresAt}
	}
	return nil
}

func (s *Store) GetRegistryCredentialOperation(ctx context.Context, orgID, id string) (RegistryCredentialOperation, error) {
	var value RegistryCredentialOperation
	err := scanRegistryCredentialOperation(s.db.QueryRow(ctx, registryCredentialOperationSelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &value)
	return value, resourceQueryError("get registry credential operation", err)
}

func (s *Store) ReserveRegistryCredentialOperation(ctx context.Context, p ReserveRegistryCredentialOperationParams) (RegistryCredentialOperation, error) {
	if p.OrgID == "" || p.RegistryID == "" || strings.TrimSpace(p.IdempotencyKey) == "" ||
		p.ActorUserID == "" || p.AuditID == "" || p.CredentialFingerprint == [32]byte{} ||
		(p.Kind != "create" && p.Kind != "import_create" && p.Kind != "rotate") ||
		(p.Kind != "rotate" && (!oci.ValidRepository(p.TargetRepository, 1) || !validValidationReference(p.TargetReference))) ||
		(p.Kind == "rotate" && (p.TargetRepository != "" || p.TargetReference != "")) {
		return RegistryCredentialOperation{}, ErrInvalidArgument
	}
	var result RegistryCredentialOperation
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.OrgID+":"+p.RegistryID); err != nil {
			return err
		}
		if err := scanRegistryCredentialOperation(tx.QueryRow(ctx, registryCredentialOperationSelect+` WHERE org_id=$1 AND idempotency_key=$2`, p.OrgID, p.IdempotencyKey), &result); err == nil {
			if result.RegistryID != p.RegistryID || result.Kind != p.Kind || stringPointerValue(result.TargetRepository) != p.TargetRepository ||
				stringPointerValue(result.TargetReference) != p.TargetReference {
				return ErrConflict
			}
			var matches bool
			if err := tx.QueryRow(ctx, `SELECT COALESCE(credential_fingerprint=$3,false) FROM registry_credential_operations WHERE org_id=$1 AND id=$2`, p.OrgID, result.ID, p.CredentialFingerprint[:]).Scan(&matches); err != nil {
				return err
			}
			if !matches {
				return ErrConflict
			}
			if result.Status == "failed" || result.Status == "cancelled" {
				return ErrFailedPrecondition
			}
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var state string
		var active int64
		var pending *int64
		if err := tx.QueryRow(ctx, `SELECT state,active_generation,pending_generation FROM registries WHERE org_id=$1 AND id=$2 AND provider='ghcr' AND registry_host='ghcr.io' FOR UPDATE`, p.OrgID, p.RegistryID).Scan(&state, &active, &pending); err != nil {
			return err
		}
		if pending != nil || state == "creating" || state == "rotating" || state == "reconciling" {
			return ErrConflict
		}
		candidate := active + 1
		if err := tx.QueryRow(ctx, `SELECT GREATEST($3,COALESCE(max(generation),0)+1) FROM registry_credential_generations WHERE org_id=$1 AND registry_id=$2`, p.OrgID, p.RegistryID, candidate).Scan(&candidate); err != nil {
			return err
		}
		if candidate < 1 {
			candidate = 1
		}
		var previous *int64
		if active > 0 {
			previous = &active
		}
		if (p.Kind == "rotate") != (previous != nil) {
			return ErrFailedPrecondition
		}
		var operationID string
		if err := tx.QueryRow(ctx, `SELECT uuidv7()::text`).Scan(&operationID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO registry_credential_generations(org_id,registry_id,generation,state,operation_id) VALUES($1,$2,$3,'reserved',$4)`, p.OrgID, p.RegistryID, candidate, operationID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,candidate_generation,previous_generation,target_repository,target_reference,idempotency_key,credential_fingerprint,actor_user_id) VALUES($1,$2,$3,$4,'awaiting_source','reserved',$5,$6,NULLIF($7,''),NULLIF($8,''),$9,$10,$11)`, operationID, p.OrgID, p.RegistryID, p.Kind, candidate, previous, p.TargetRepository, p.TargetReference, p.IdempotencyKey, p.CredentialFingerprint[:], p.ActorUserID); err != nil {
			return err
		}
		action := "registry.credential.create_requested"
		if p.Kind == "rotate" {
			action = "registry.credential.rotation_requested"
		}
		after, err := json.Marshal(struct {
			OperationID string `json:"operation_id"`
			Kind        string `json:"kind"`
			Generation  int64  `json:"generation"`
			Status      string `json:"status"`
		}{operationID, p.Kind, candidate, "awaiting_source"})
		if err != nil {
			return err
		}
		if err := insertAuditEvent(ctx, tx, p.OrgID, &AuditEvent{ID: p.AuditID, ActorUserID: &p.ActorUserID}, action, "registry", p.RegistryID, nil, after); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `WITH known AS (
			SELECT app.image_repository repository,substring(job.immutable_image_ref from '@(sha256:[0-9a-f]{64})$') reference,job.updated_at observed_at
			FROM apps app JOIN environments environment ON environment.org_id=app.org_id AND environment.app_id=app.id AND environment.torn_down_at IS NULL
			JOIN orchestration_jobs job ON job.org_id=environment.org_id AND job.environment_id=environment.id
			WHERE app.org_id=$1 AND app.registry_id=$2 AND app.image_visibility='private' AND app.deleted_at IS NULL
			  AND job.status NOT IN ('failed','blocked','cancelled','superseded') AND job.oci_resolution_state='resolved'
			  AND job.resolved_registry_id=$2 AND job.immutable_image_ref IS NOT NULL
			UNION ALL
			SELECT app.image_repository,substring(release.image_ref from '@(sha256:[0-9a-f]{64})$'),release.created_at
			FROM apps app JOIN environments environment ON environment.org_id=app.org_id AND environment.app_id=app.id
			JOIN releases release ON release.org_id=environment.org_id AND release.environment_id=environment.id
			WHERE app.org_id=$1 AND app.registry_id=$2 AND app.image_visibility='private' AND app.deleted_at IS NULL
			  AND environment.torn_down_at IS NULL AND release.status='live' AND release.registry_credential_registry_id=$2
		), latest AS (SELECT DISTINCT ON(repository) repository,reference FROM known WHERE reference IS NOT NULL ORDER BY repository,observed_at DESC),
		 targets AS (SELECT repository,reference FROM latest UNION SELECT $4,$5 WHERE $4<>'' AND $5<>'')
		 INSERT INTO registry_credential_validation_targets(operation_id,org_id,registry_id,repository,reference)
		 SELECT $3,$1,$2,repository,reference FROM targets`, p.OrgID, p.RegistryID, operationID, p.TargetRepository, p.TargetReference); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE registries SET state=CASE WHEN active_generation>0 THEN 'ready' ELSE 'absent' END,checkpoint='reserved',pending_generation=$3,pending_credential_secret_ref=NULL,operation_id=$4,failure_code=NULL,failure_message=NULL,attempts=0,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, p.OrgID, p.RegistryID, candidate, operationID); err != nil {
			return err
		}
		return scanRegistryCredentialOperation(tx.QueryRow(ctx, registryCredentialOperationSelect+` WHERE org_id=$1 AND id=$2`, p.OrgID, operationID), &result)
	})
	return result, resourceQueryError("reserve registry credential operation", err)
}

func (s *Store) AttachRegistryCredentialSource(ctx context.Context, p AttachRegistryCredentialSourceParams) (RegistryCredentialOperation, error) {
	if p.OrgID == "" || p.OperationID == "" || strings.TrimSpace(p.IdempotencyKey) == "" {
		return RegistryCredentialOperation{}, ErrInvalidArgument
	}
	if _, err := secretref.Parse(p.SecretRef); err != nil {
		return RegistryCredentialOperation{}, ErrInvalidArgument
	}
	var result RegistryCredentialOperation
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanRegistryCredentialOperation(tx.QueryRow(ctx, registryCredentialOperationSelect+` WHERE org_id=$1 AND id=$2 FOR UPDATE`, p.OrgID, p.OperationID), &result); err != nil {
			return err
		}
		if result.IdempotencyKey != p.IdempotencyKey {
			return ErrConflict
		}
		var existing *string
		var generationState string
		if err := tx.QueryRow(ctx, `SELECT secret_ref,state FROM registry_credential_generations WHERE org_id=$1 AND registry_id=$2 AND generation=$3 FOR UPDATE`, p.OrgID, result.RegistryID, result.CandidateGeneration).Scan(&existing, &generationState); err != nil {
			return err
		}
		attachedStatus := "pending"
		if result.Kind == "import_create" {
			attachedStatus = "awaiting_import"
		}
		if existing != nil {
			if *existing != p.SecretRef || generationState != "pending" || result.Checkpoint != "source_secret_written" || result.Status != attachedStatus {
				return ErrConflict
			}
			return nil
		}
		if result.Status != "awaiting_source" || result.Checkpoint != "reserved" || generationState != "reserved" {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE registry_credential_generations SET secret_ref=$4,state='pending' WHERE org_id=$1 AND registry_id=$2 AND generation=$3`, p.OrgID, result.RegistryID, result.CandidateGeneration, p.SecretRef); err != nil {
			return err
		}
		state := "provisioning"
		if result.PreviousGeneration != nil {
			state = "rotating"
		}
		if _, err := tx.Exec(ctx, `UPDATE registries SET state=$3,checkpoint='source_secret_written',pending_credential_secret_ref=$4,updated_at=clock_timestamp() WHERE org_id=$1 AND operation_id=$2`, p.OrgID, result.ID, state, p.SecretRef); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE registry_credential_operations SET status=$3,checkpoint='source_secret_written',next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, p.OrgID, result.ID, attachedStatus); err != nil {
			return err
		}
		return scanRegistryCredentialOperation(tx.QueryRow(ctx, registryCredentialOperationSelect+` WHERE org_id=$1 AND id=$2`, p.OrgID, result.ID), &result)
	})
	return result, resourceQueryError("attach registry credential source", err)
}

func validValidationReference(value string) bool {
	if len(value) == 40 {
		for _, c := range value {
			if c < '0' || c > '9' && c < 'a' || c > 'f' {
				return false
			}
		}
		return true
	}
	_, _, err := oci.ParseDigest(value)
	return err == nil
}

const registryCredentialGenerationSelect = `SELECT org_id::text,registry_id::text,generation,COALESCE(secret_ref,''),state,operation_id::text,
	created_at,validated_at,activated_at,retired_at,garbage_collected_at FROM registry_credential_generations`

func scanRegistryCredentialGeneration(row rowScanner, value *RegistryCredentialGeneration) error {
	return row.Scan(&value.OrgID, &value.RegistryID, &value.Generation, &value.SecretRef, &value.State, &value.OperationID,
		&value.CreatedAt, &value.ValidatedAt, &value.ActivatedAt, &value.RetiredAt, &value.GarbageCollectedAt)
}

func (s *Store) GetRegistryCredentialGeneration(ctx context.Context, orgID, registryID string, generation int64) (RegistryCredentialGeneration, error) {
	var value RegistryCredentialGeneration
	err := scanRegistryCredentialGeneration(s.db.QueryRow(ctx, registryCredentialGenerationSelect+` WHERE org_id=$1 AND registry_id=$2 AND generation=$3`, orgID, registryID, generation), &value)
	return value, resourceQueryError("get registry credential generation", err)
}

const registryCredentialValidationTargetColumns = `operation_id::text,org_id::text,registry_id::text,repository,reference,status,
	attempts,next_attempt_at,failure_code,validated_at,lease_owner,lease_epoch,lease_expires_at`
const registryCredentialValidationTargetSelect = `SELECT ` + registryCredentialValidationTargetColumns + ` FROM registry_credential_validation_targets`

// registryCredentialValidationTargetQualifiedColumns prefixes every column for
// the claim UPDATE ... FROM candidates form where bare names are ambiguous.
func registryCredentialValidationTargetQualifiedColumns() string {
	return "target." + strings.ReplaceAll(registryCredentialValidationTargetColumns, ",", ",target.")
}

func scanRegistryCredentialValidationTarget(row rowScanner, value *RegistryCredentialValidationTarget) error {
	var owner *string
	var epoch int64
	var expires *time.Time
	if err := row.Scan(&value.OperationID, &value.OrgID, &value.RegistryID, &value.Repository, &value.Reference, &value.Status,
		&value.Attempts, &value.NextAttemptAt, &value.FailureCode, &value.ValidatedAt, &owner, &epoch, &expires); err != nil {
		return err
	}
	if owner != nil && expires != nil {
		value.Lease = &LeaseToken{Owner: *owner, Epoch: epoch, ExpiresAt: *expires}
	}
	return nil
}

func (s *Store) GetRegistryCredentialValidationTarget(ctx context.Context, orgID, operationID, repository, reference string) (RegistryCredentialValidationTarget, error) {
	if reference != "legacy" && !validValidationReference(reference) {
		return RegistryCredentialValidationTarget{}, ErrInvalidArgument
	}
	var value RegistryCredentialValidationTarget
	err := scanRegistryCredentialValidationTarget(s.db.QueryRow(ctx, registryCredentialValidationTargetSelect+` WHERE org_id=$1 AND operation_id=$2 AND repository=$3 AND reference=$4`, orgID, operationID, repository, reference), &value)
	return value, resourceQueryError("get registry credential validation target", err)
}

func (s *Store) RecoverExpiredRegistryCredentialLeases(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, ErrInvalidArgument
	}
	var count int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+registryCredentialOperationColumns+` FROM registry_credential_operations WHERE status='leased' AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at,id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		var operations []RegistryCredentialOperation
		for rows.Next() {
			var operation RegistryCredentialOperation
			if err := scanRegistryCredentialOperation(rows, &operation); err != nil {
				rows.Close()
				return err
			}
			operations = append(operations, operation)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, operation := range operations {
			if operation.Attempts >= maxRegistryCredentialAttempts {
				if err := terminalizeRegistryCredentialOperation(ctx, tx, operation.OrgID, operation.ID, "credential_operation_exhausted"); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `UPDATE registry_credential_operations SET status='pending',lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, operation.OrgID, operation.ID); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) RescueAwaitingRegistryCredentialSources(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	if olderThan.IsZero() || limit < 1 {
		return 0, ErrInvalidArgument
	}
	var count int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT org_id::text,id::text FROM registry_credential_operations WHERE status IN ('awaiting_source','awaiting_import') AND updated_at<=$1 ORDER BY updated_at,id LIMIT $2 FOR UPDATE SKIP LOCKED`, olderThan, limit)
		if err != nil {
			return err
		}
		var values [][2]string
		for rows.Next() {
			var value [2]string
			if err := rows.Scan(&value[0], &value[1]); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, value := range values {
			if err := terminalizeRegistryCredentialOperation(ctx, tx, value[0], value[1], "credential_operation_exhausted"); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) ListRegistryCredentialGarbageCollectionCandidates(ctx context.Context, olderThan time.Time, limit int) ([]RegistryCredentialGarbageCollectionCandidate, error) {
	if olderThan.IsZero() || limit < 1 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `SELECT generation.org_id::text,generation.registry_id::text,generation.operation_id::text,
		generation.generation,COALESCE(generation.secret_ref,'') FROM registry_credential_generations generation
		JOIN registry_credential_operations operation ON operation.org_id=generation.org_id AND operation.id=generation.operation_id
		WHERE generation.state='failed' AND operation.terminal_at<=$1
		AND NOT EXISTS (SELECT 1 FROM registries registry WHERE registry.org_id=generation.org_id AND registry.id=generation.registry_id
			AND (registry.active_generation=generation.generation OR registry.pending_generation=generation.generation OR registry.previous_generation=generation.generation))
		AND NOT EXISTS (SELECT 1 FROM apps app JOIN environments environment ON environment.org_id=app.org_id AND environment.app_id=app.id
			WHERE app.org_id=generation.org_id AND app.registry_id=generation.registry_id AND environment.torn_down_at IS NULL
			AND (environment.required_registry_credential_generation=generation.generation OR environment.ready_registry_credential_generation=generation.generation))
		AND NOT EXISTS (SELECT 1 FROM environments environment WHERE environment.org_id=generation.org_id
			AND environment.registry_credential_cleanup_registry_id=generation.registry_id
			AND environment.registry_credential_cleanup_generation=generation.generation
			AND environment.registry_credential_cleanup_state IN ('pending','failed'))
		AND NOT EXISTS (SELECT 1 FROM releases release WHERE release.org_id=generation.org_id AND release.registry_credential_registry_id=generation.registry_id
			AND release.registry_credential_generation=generation.generation)
		ORDER BY operation.terminal_at,generation.registry_id,generation.generation LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []RegistryCredentialGarbageCollectionCandidate
	for rows.Next() {
		var value RegistryCredentialGarbageCollectionCandidate
		if err := rows.Scan(&value.OrgID, &value.RegistryID, &value.OperationID, &value.Generation, &value.SecretRef); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// GarbageCollectRegistryCredential revalidates and deletes one failed source
// while the generation row is locked. The generation lock also conflicts with
// the foreign-key key-share locks taken by current reference-creating paths.
func (s *Store) GarbageCollectRegistryCredential(ctx context.Context, candidate RegistryCredentialGarbageCollectionCandidate, olderThan time.Time, deleteSource RegistryCredentialGarbageCollectionDelete) (bool, error) {
	if candidate.OrgID == "" || candidate.RegistryID == "" || candidate.OperationID == "" || candidate.Generation < 1 || candidate.SecretRef == "" || olderThan.IsZero() || deleteSource == nil {
		return false, ErrInvalidArgument
	}
	collected := false
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM releases WHERE org_id=$1 AND registry_credential_registry_id=$2
			AND registry_credential_generation=$3 FOR UPDATE`, candidate.OrgID, candidate.RegistryID, candidate.Generation)
		if err != nil {
			return err
		}
		rows.Close()
		var current RegistryCredentialGarbageCollectionCandidate
		var eligible bool
		err = tx.QueryRow(ctx, `SELECT generation.org_id::text,generation.registry_id::text,generation.operation_id::text,
			generation.generation,COALESCE(generation.secret_ref,''),
			generation.state='failed' AND operation.terminal_at<=$6
			AND NOT EXISTS (SELECT 1 FROM registries registry_ref WHERE registry_ref.org_id=generation.org_id AND registry_ref.id=generation.registry_id
				AND (registry_ref.active_generation=generation.generation OR registry_ref.pending_generation=generation.generation OR registry_ref.previous_generation=generation.generation))
			AND NOT EXISTS (SELECT 1 FROM apps app JOIN environments environment ON environment.org_id=app.org_id AND environment.app_id=app.id
				WHERE app.org_id=generation.org_id AND app.registry_id=generation.registry_id AND environment.torn_down_at IS NULL
				AND (environment.required_registry_credential_generation=generation.generation OR environment.ready_registry_credential_generation=generation.generation))
			AND NOT EXISTS (SELECT 1 FROM environments environment WHERE environment.org_id=generation.org_id
				AND environment.registry_credential_cleanup_registry_id=generation.registry_id
				AND environment.registry_credential_cleanup_generation=generation.generation
				AND environment.registry_credential_cleanup_state IN ('pending','failed'))
				AND NOT EXISTS (SELECT 1 FROM releases release WHERE release.org_id=generation.org_id AND release.registry_credential_registry_id=generation.registry_id
					AND release.registry_credential_generation=generation.generation)
			FROM registry_credential_generations generation
			JOIN registry_credential_operations operation ON operation.org_id=generation.org_id AND operation.registry_id=generation.registry_id AND operation.id=generation.operation_id
			JOIN registries registry ON registry.org_id=generation.org_id AND registry.id=generation.registry_id
			WHERE generation.org_id=$1 AND generation.registry_id=$2 AND generation.generation=$3 AND generation.operation_id=$4 AND generation.secret_ref=$5
			FOR UPDATE OF generation,operation,registry`, candidate.OrgID, candidate.RegistryID, candidate.Generation, candidate.OperationID, candidate.SecretRef, olderThan).
			Scan(&current.OrgID, &current.RegistryID, &current.OperationID, &current.Generation, &current.SecretRef, &eligible)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		if err := deleteSource(ctx, current); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE registry_credential_generations SET state='garbage_collected',garbage_collected_at=clock_timestamp()
			WHERE org_id=$1 AND registry_id=$2 AND generation=$3 AND operation_id=$4 AND state='failed'`, current.OrgID, current.RegistryID, current.Generation, current.OperationID); err != nil {
			return err
		}
		collected = true
		return nil
	})
	return collected, err
}

func (s *Store) ListRegistryCredentialOperationOrgIDs(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT org_id::text FROM registry_credential_operations WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp() ORDER BY org_id::text LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) ClaimRegistryCredentialOperations(ctx context.Context, orgID, owner string, leaseDuration time.Duration, limit int) ([]RegistryCredentialOperation, error) {
	if orgID == "" || strings.TrimSpace(owner) == "" || leaseDuration <= 0 || limit < 1 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS (SELECT id FROM registry_credential_operations WHERE org_id=$1 AND status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,created_at,id LIMIT $4 FOR UPDATE SKIP LOCKED) UPDATE registry_credential_operations operation SET status='leased',lease_owner=$2,lease_epoch=lease_epoch+1,lease_expires_at=clock_timestamp()+$3::interval,attempts=attempts+1,updated_at=clock_timestamp() FROM candidates WHERE operation.id=candidates.id RETURNING `+registryCredentialOperationQualifiedColumns, orgID, owner, leaseDuration, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []RegistryCredentialOperation
	for rows.Next() {
		var value RegistryCredentialOperation
		if err := scanRegistryCredentialOperation(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// CheckpointRegistryCredentialSource exists only for migrations between the
// legacy write-checkpoint flow and the reserve/attach flow. source_secret_written
// is recorded by AttachRegistryCredentialSource inside its own transaction;
// workers never checkpoint source writes, and source_secret_write_started is
// not part of the reserve/attach state machine.
func (s *Store) CheckpointRegistryCredentialSource(ctx context.Context, operation RegistryCredentialOperation, lease LeaseToken, checkpoint string) error {
	if checkpoint != "source_secret_written" {
		return ErrInvalidArgument
	}
	tag, err := s.db.Exec(ctx, `UPDATE registry_credential_operations SET checkpoint=$5,updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND lease_epoch=$4 AND lease_expires_at>clock_timestamp()`, operation.OrgID, operation.ID, lease.Owner, lease.Epoch, checkpoint)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrLeaseNotOwned
	}
	if err == nil {
		_, err = s.db.Exec(ctx, `UPDATE registries SET checkpoint=$3,updated_at=clock_timestamp() WHERE org_id=$1 AND operation_id=$2`, operation.OrgID, operation.ID, checkpoint)
	}
	return err
}

func (s *Store) RetryRegistryCredentialOperation(ctx context.Context, operation RegistryCredentialOperation, lease LeaseToken, delay time.Duration, code string) error {
	if delay < 0 {
		return ErrInvalidArgument
	}
	tag, err := s.db.Exec(ctx, `UPDATE registry_credential_operations SET status='pending',lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=clock_timestamp()+$5::interval,failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND lease_epoch=$4 AND lease_expires_at>clock_timestamp()`, operation.OrgID, operation.ID, lease.Owner, lease.Epoch, delay)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrLeaseNotOwned
	}
	return err
}

func (s *Store) FailRegistryCredentialOperation(ctx context.Context, operation RegistryCredentialOperation, lease LeaseToken, code string) error {
	if strings.TrimSpace(code) == "" {
		return ErrInvalidArgument
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT status='leased' AND lease_owner=$3 AND lease_epoch=$4 AND lease_expires_at>clock_timestamp() FROM registry_credential_operations WHERE org_id=$1 AND id=$2 FOR UPDATE`, operation.OrgID, operation.ID, lease.Owner, lease.Epoch).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrLeaseNotOwned
		}
		return terminalizeRegistryCredentialOperation(ctx, tx, operation.OrgID, operation.ID, code)
	})
}

func (s *Store) ClaimRegistryCredentialValidationTargets(ctx context.Context, operation RegistryCredentialOperation, owner string, leaseDuration time.Duration, limit int) ([]RegistryCredentialValidationTarget, error) {
	if operation.Lease == nil || owner == "" || owner != operation.Lease.Owner || leaseDuration <= 0 || limit < 1 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `WITH operation AS (SELECT id FROM registry_credential_operations WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND lease_epoch=$4 AND lease_expires_at>clock_timestamp()), candidates AS (SELECT target.repository,target.reference FROM registry_credential_validation_targets target,operation WHERE target.org_id=$1 AND target.operation_id=$2 AND target.status='pending' AND (target.lease_owner IS NULL OR target.lease_expires_at<=clock_timestamp()) AND target.attempts<12 AND target.next_attempt_at<=clock_timestamp() ORDER BY target.next_attempt_at,target.repository,target.reference LIMIT $7 FOR UPDATE OF target SKIP LOCKED) UPDATE registry_credential_validation_targets target SET lease_owner=$5,lease_epoch=lease_epoch+1,lease_expires_at=clock_timestamp()+$6::interval,attempts=attempts+1 FROM candidates WHERE target.operation_id=$2 AND target.repository=candidates.repository AND target.reference=candidates.reference RETURNING `+registryCredentialValidationTargetQualifiedColumns(), operation.OrgID, operation.ID, operation.Lease.Owner, operation.Lease.Epoch, owner, leaseDuration, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []RegistryCredentialValidationTarget
	for rows.Next() {
		var value RegistryCredentialValidationTarget
		if err := scanRegistryCredentialValidationTarget(rows, &value); err != nil {
			return nil, err
		}
		operationLease := *operation.Lease
		value.OperationLease = &operationLease
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) RecoverExpiredRegistryCredentialValidationTargets(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, ErrInvalidArgument
	}
	var count int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT target.org_id::text,target.operation_id::text,target.repository,target.reference,target.attempts FROM registry_credential_validation_targets target JOIN registry_credential_operations operation ON operation.id=target.operation_id AND operation.org_id=target.org_id WHERE target.status='pending' AND target.lease_owner IS NOT NULL AND target.lease_expires_at<=clock_timestamp() ORDER BY target.lease_expires_at,target.operation_id,target.repository,target.reference LIMIT $1 FOR UPDATE OF target,operation SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		var values []struct {
			orgID, operationID, repository, reference string
			attempts                                  int
		}
		for rows.Next() {
			var value struct {
				orgID, operationID, repository, reference string
				attempts                                  int
			}
			if err := rows.Scan(&value.orgID, &value.operationID, &value.repository, &value.reference, &value.attempts); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, value := range values {
			if value.attempts >= maxRegistryCredentialAttempts {
				if err := terminalizeRegistryCredentialOperation(ctx, tx, value.orgID, value.operationID, "credential_operation_exhausted"); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `UPDATE registry_credential_validation_targets SET lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=clock_timestamp() WHERE org_id=$1 AND operation_id=$2 AND repository=$3 AND reference=$4 AND status='pending' AND lease_expires_at<=clock_timestamp()`, value.orgID, value.operationID, value.repository, value.reference); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) CompleteRegistryCredentialValidationTarget(ctx context.Context, target RegistryCredentialValidationTarget, lease LeaseToken) error {
	return s.finishRegistryCredentialValidationTarget(ctx, target, lease, "validated", "", 0)
}
func (s *Store) RetryRegistryCredentialValidationTarget(ctx context.Context, target RegistryCredentialValidationTarget, lease LeaseToken, code string, delay time.Duration) error {
	if delay < 0 {
		return ErrInvalidArgument
	}
	return s.finishRegistryCredentialValidationTarget(ctx, target, lease, "pending", code, delay)
}
func (s *Store) FailRegistryCredentialValidationTarget(ctx context.Context, target RegistryCredentialValidationTarget, lease LeaseToken, code string) error {
	return s.finishRegistryCredentialValidationTarget(ctx, target, lease, "failed", code, 0)
}

func (s *Store) finishRegistryCredentialValidationTarget(ctx context.Context, target RegistryCredentialValidationTarget, lease LeaseToken, status, code string, delay time.Duration) error {
	validated := status == "validated"
	failed := status == "failed"
	if !validated && !failed && status != "pending" {
		return ErrInvalidArgument
	}
	if target.OperationLease == nil {
		return ErrInvalidArgument
	}
	// The schema invariant (status='failed' OR failure_code IS NULL) permits
	// persisted failure codes only on terminally failed targets; retries keep
	// the transient code in the worker log line, not the row.
	var failure any = nil
	if failed {
		failure = code
	}
	tag, err := s.db.Exec(ctx, `UPDATE registry_credential_validation_targets target SET status=$9,lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=clock_timestamp()+$10::interval,failure_code=$11,validated_at=CASE WHEN $12 THEN clock_timestamp() END WHERE target.org_id=$1 AND target.operation_id=$2 AND target.repository=$3 AND target.reference=$4 AND target.lease_owner=$5 AND target.lease_epoch=$6 AND target.lease_expires_at>clock_timestamp() AND EXISTS (SELECT 1 FROM registry_credential_operations operation WHERE operation.org_id=target.org_id AND operation.id=target.operation_id AND operation.status='leased' AND operation.lease_owner=$7 AND operation.lease_epoch=$8 AND operation.lease_expires_at>clock_timestamp())`, target.OrgID, target.OperationID, target.Repository, target.Reference, lease.Owner, lease.Epoch, target.OperationLease.Owner, target.OperationLease.Epoch, status, delay, failure, validated)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrLeaseNotOwned
	}
	return err
}

func (s *Store) FinishRegistryCredentialValidation(ctx context.Context, operation RegistryCredentialOperation, lease LeaseToken) (bool, error) {
	var complete bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var owned bool
		var currentCheckpoint string
		if err := tx.QueryRow(ctx, `SELECT status='leased' AND lease_owner=$3 AND lease_epoch=$4 AND lease_expires_at>clock_timestamp(),checkpoint FROM registry_credential_operations WHERE org_id=$1 AND id=$2 FOR UPDATE`, operation.OrgID, operation.ID, lease.Owner, lease.Epoch).Scan(&owned, &currentCheckpoint); err != nil {
			return err
		}
		if !owned {
			return ErrLeaseNotOwned
		}
		if currentCheckpoint != "source_secret_written" && currentCheckpoint != "targets_validated" {
			return ErrFailedPrecondition
		}
		var pending, failed int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='pending'),count(*) FILTER(WHERE status='failed') FROM registry_credential_validation_targets WHERE org_id=$1 AND operation_id=$2`, operation.OrgID, operation.ID).Scan(&pending, &failed); err != nil {
			return err
		}
		if failed > 0 {
			if err := terminalizeRegistryCredentialOperation(ctx, tx, operation.OrgID, operation.ID, "credential_validation_failed"); err != nil {
				return err
			}
			return nil
		}
		if pending > 0 {
			return ErrFailedPrecondition
		}
		tag, err := tx.Exec(ctx, `UPDATE registry_credential_operations SET status='pending',checkpoint='validation_complete',lease_owner=NULL,lease_expires_at=NULL,next_attempt_at='infinity',updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND lease_epoch=$4 AND lease_expires_at>clock_timestamp()`, operation.OrgID, operation.ID, lease.Owner, lease.Epoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseNotOwned
		}
		if _, err = tx.Exec(ctx, `UPDATE registry_credential_generations SET validated_at=clock_timestamp() WHERE org_id=$1 AND operation_id=$2 AND state='pending'`, operation.OrgID, operation.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE registry_credential_operations SET checkpoint='namespaces_reconciling',updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND status='pending' AND checkpoint='validation_complete'`, operation.OrgID, operation.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE registries SET checkpoint='namespaces_reconciling',state='reconciling',updated_at=clock_timestamp() WHERE org_id=$1 AND operation_id=$2`, operation.OrgID, operation.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE environments environment SET required_registry_credential_generation=$3,
			ready_registry_credential_generation=NULL,registry_credential_state='pending',registry_credential_checkpoint=NULL,
			registry_credential_error_code=NULL,registry_credential_error_message=NULL,registry_credential_attempts=0,
			registry_credential_next_attempt_at=clock_timestamp(),registry_credential_lease_owner=NULL,
			registry_credential_lease_expires_at=NULL
			FROM apps app WHERE environment.org_id=$1 AND app.org_id=environment.org_id AND app.id=environment.app_id
			AND app.registry_id=$2 AND app.image_visibility='private' AND app.deleted_at IS NULL
			AND environment.torn_down_at IS NULL AND environment.lifecycle_status<>'teardown_requested'`,
			operation.OrgID, operation.RegistryID, operation.CandidateGeneration); err != nil {
			return err
		}
		_, err = activateRegistryCredentialGenerationIfReady(ctx, tx, operation.OrgID, operation.RegistryID, operation.ID, operation.CandidateGeneration, "")
		complete = err == nil
		return err
	})
	return complete, err
}

func terminalizeRegistryCredentialOperation(ctx context.Context, tx pgx.Tx, orgID, operationID, code string) error {
	var registryID string
	var candidate int64
	var previous *int64
	if err := tx.QueryRow(ctx, `UPDATE registry_credential_operations SET status='failed',lease_owner=NULL,lease_expires_at=NULL,failure_code=$3,failure_message=NULL,terminal_at=clock_timestamp(),updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND status NOT IN ('succeeded','failed','cancelled') RETURNING registry_id::text,candidate_generation,previous_generation`, orgID, operationID, code).Scan(&registryID, &candidate, &previous); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE registry_credential_generations SET state='failed' WHERE org_id=$1 AND registry_id=$2 AND generation=$3 AND state IN ('reserved','pending')`, orgID, registryID, candidate); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE registry_credential_validation_targets SET status=CASE WHEN status='failed' THEN 'failed' ELSE 'skipped' END,lease_owner=NULL,lease_expires_at=NULL,failure_code=CASE WHEN status='failed' THEN failure_code ELSE NULL END WHERE org_id=$1 AND operation_id=$2 AND status IN ('pending','failed')`, orgID, operationID); err != nil {
		return err
	}
	if previous != nil {
		_, err := tx.Exec(ctx, `UPDATE registries SET state='ready',checkpoint='activated',pending_generation=NULL,pending_credential_secret_ref=NULL,operation_id=NULL,failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND operation_id=$3`, orgID, registryID, operationID)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE registries SET state='failed',checkpoint=NULL,pending_generation=NULL,pending_credential_secret_ref=NULL,operation_id=NULL,failure_code=$4,failure_message=NULL,updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2 AND operation_id=$3`, orgID, registryID, operationID, code)
	return err
}

const namespaceCredentialReconciliationSelect = `SELECT environment.org_id::text,app.registry_id::text,environment.id::text,
	environment.app_id::text,environment.namespace,environment.required_registry_credential_generation,
	environment.ready_registry_credential_generation,environment.registry_credential_state,
	COALESCE(environment.registry_credential_checkpoint,''),environment.registry_credential_error_code,
	environment.registry_credential_error_message,environment.registry_credential_attempts,
	environment.registry_credential_next_attempt_at,environment.registry_credential_lease_owner,
	environment.registry_credential_lease_epoch,environment.registry_credential_lease_expires_at
	FROM environments environment JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id`

func scanNamespaceCredentialReconciliation(row rowScanner, value *NamespaceCredentialReconciliation) error {
	var owner *string
	var epoch int64
	var expiresAt *time.Time
	if err := row.Scan(&value.OrgID, &value.RegistryID, &value.EnvironmentID, &value.AppID, &value.Namespace,
		&value.RequiredGeneration, &value.ReadyGeneration, &value.State, &value.Checkpoint, &value.FailureCode,
		&value.FailureMessage, &value.Attempts, &value.NextAttemptAt, &owner, &epoch, &expiresAt); err != nil {
		return err
	}
	if owner != nil && expiresAt != nil {
		value.Lease = &LeaseToken{Owner: *owner, Epoch: epoch, ExpiresAt: *expiresAt}
	}
	return nil
}

func (s *Store) GetNamespaceCredentialReconciliation(ctx context.Context, orgID, environmentID string) (NamespaceCredentialReconciliation, error) {
	var value NamespaceCredentialReconciliation
	err := scanNamespaceCredentialReconciliation(s.db.QueryRow(ctx, namespaceCredentialReconciliationSelect+` WHERE environment.org_id=$1 AND environment.id=$2`, orgID, environmentID), &value)
	return value, resourceQueryError("get namespace credential reconciliation", err)
}

func (s *Store) RecoverExpiredNamespaceCredentialLeases(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, ErrInvalidArgument
	}
	var recovered int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT environment.org_id::text,environment.id::text,
			environment.registry_credential_attempts
			FROM environments environment JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
			WHERE environment.registry_credential_state='pending' AND environment.registry_credential_lease_owner IS NOT NULL
			AND environment.registry_credential_lease_expires_at<=clock_timestamp()
			ORDER BY environment.registry_credential_lease_expires_at,environment.id LIMIT $1
			FOR UPDATE OF environment SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		type expiredNamespaceCredential struct {
			orgID, environmentID string
			attempts             int
		}
		var values []expiredNamespaceCredential
		for rows.Next() {
			var value expiredNamespaceCredential
			if err := rows.Scan(&value.orgID, &value.environmentID, &value.attempts); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, value := range values {
			if value.attempts >= maxRegistryCredentialAttempts {
				if err := failExpiredNamespaceCredential(ctx, tx, value.orgID, value.environmentID, "kubernetes_unavailable"); err != nil {
					return err
				}
			} else if tag, err := tx.Exec(ctx, `UPDATE environments SET registry_credential_lease_owner=NULL,
				registry_credential_lease_expires_at=NULL,registry_credential_next_attempt_at=clock_timestamp()
				WHERE org_id=$1 AND id=$2 AND registry_credential_state='pending'`, value.orgID, value.environmentID); err != nil {
				return err
			} else if tag.RowsAffected() == 0 {
				return ErrFailedPrecondition
			}
			recovered++
		}
		return nil
	})
	return recovered, err
}

func (s *Store) RecoverExpiredNamespaceCredentialCleanupLeases(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, ErrInvalidArgument
	}
	var recovered int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT environment.org_id::text,environment.id::text,environment.app_id::text,
			environment.registry_credential_cleanup_attempts
			FROM environments environment JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
			WHERE environment.registry_credential_cleanup_state='pending'
			AND environment.registry_credential_cleanup_lease_owner IS NOT NULL
			AND environment.registry_credential_cleanup_lease_expires_at<=clock_timestamp()
			ORDER BY environment.registry_credential_cleanup_lease_expires_at,environment.id LIMIT $1
			FOR UPDATE OF environment,app SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		type expiredCleanup struct {
			orgID, environmentID, appID string
			attempts                    int
		}
		var values []expiredCleanup
		for rows.Next() {
			var value expiredCleanup
			if err := rows.Scan(&value.orgID, &value.environmentID, &value.appID, &value.attempts); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, value := range values {
			if value.attempts >= maxRegistryCredentialAttempts {
				if _, err := tx.Exec(ctx, `UPDATE environments SET registry_credential_cleanup_state='failed',
					registry_credential_cleanup_lease_owner=NULL,registry_credential_cleanup_lease_expires_at=NULL
					WHERE org_id=$1 AND id=$2 AND app_id=$3 AND registry_credential_cleanup_state='pending'`,
					value.orgID, value.environmentID, value.appID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE apps SET cleanup_state='cleanup_failed',cleanup_checkpoint=NULL,
					cleanup_error_code='kubernetes_unavailable',cleanup_error_message=NULL
					WHERE org_id=$1 AND id=$2 AND image_visibility='public' AND cleanup_state IN ('cleanup_requested','cleaning')`,
					value.orgID, value.appID); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `UPDATE environments SET registry_credential_cleanup_lease_owner=NULL,
				registry_credential_cleanup_lease_expires_at=NULL,registry_credential_cleanup_next_attempt_at=clock_timestamp()
				WHERE org_id=$1 AND id=$2 AND app_id=$3 AND registry_credential_cleanup_state='pending'`,
				value.orgID, value.environmentID, value.appID); err != nil {
				return err
			}
			recovered++
		}
		return nil
	})
	return recovered, err
}

func (s *Store) ListNamespaceCredentialOrgIDs(ctx context.Context, afterOrgID string, limit int) ([]string, error) {
	if limit < 1 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `SELECT org_id FROM (SELECT DISTINCT environment.org_id::text AS org_id FROM environments environment
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		JOIN registry_credential_generations generation ON generation.org_id=environment.org_id
		 AND generation.registry_id=app.registry_id AND generation.generation=environment.required_registry_credential_generation
		JOIN registries registry ON registry.org_id=generation.org_id AND registry.id=generation.registry_id
		JOIN registry_credential_operations operation ON operation.org_id=registry.org_id AND operation.id=registry.operation_id
		WHERE environment.registry_credential_state='pending' AND environment.registry_credential_lease_owner IS NULL
		AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL
		AND environment.registry_credential_attempts<12 AND environment.registry_credential_next_attempt_at<=clock_timestamp()
		AND app.image_visibility='private' AND app.deleted_at IS NULL AND operation.status='pending'
		AND ((operation.checkpoint='namespaces_reconciling' AND operation.candidate_generation=generation.generation
			AND generation.state='pending' AND registry.pending_generation=generation.generation)
		 OR (operation.checkpoint='rollback_reconciling' AND operation.previous_generation=generation.generation
			AND generation.state='active' AND registry.checkpoint='rollback_reconciling'
			AND registry.active_generation=generation.generation))
		UNION
		SELECT DISTINCT environment.org_id::text FROM environments environment
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		JOIN registry_credential_generations generation ON generation.org_id=environment.org_id
		 AND generation.registry_id=environment.registry_credential_cleanup_registry_id
		 AND generation.generation=environment.registry_credential_cleanup_generation
		WHERE environment.registry_credential_cleanup_state='pending'
		AND environment.registry_credential_cleanup_lease_owner IS NULL
		AND environment.registry_credential_cleanup_attempts<12
		AND environment.registry_credential_cleanup_next_attempt_at<=clock_timestamp()
		AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL
		AND app.image_visibility='public' AND app.deleted_at IS NULL) due
		WHERE org_id>$1 ORDER BY org_id LIMIT $2`, afterOrgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orgIDs []string
	for rows.Next() {
		var orgID string
		if err := rows.Scan(&orgID); err != nil {
			return nil, err
		}
		orgIDs = append(orgIDs, orgID)
	}
	return orgIDs, rows.Err()
}

func (s *Store) ClaimNamespaceCredentials(ctx context.Context, orgID, owner string, leaseDuration time.Duration, limit int) ([]NamespaceCredentialClaim, error) {
	if orgID == "" || strings.TrimSpace(owner) == "" || leaseDuration <= 0 || limit < 1 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS (SELECT environment.id,operation.id AS operation_id,generation.secret_ref
		FROM environments environment
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		JOIN registry_credential_generations generation ON generation.org_id=environment.org_id
		 AND generation.registry_id=app.registry_id AND generation.generation=environment.required_registry_credential_generation
		JOIN registries registry ON registry.org_id=generation.org_id AND registry.id=generation.registry_id
		JOIN registry_credential_operations operation ON operation.org_id=registry.org_id AND operation.id=registry.operation_id
		WHERE environment.org_id=$1 AND environment.registry_credential_state='pending'
		AND environment.registry_credential_lease_owner IS NULL
		AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL
		AND environment.registry_credential_attempts<12 AND environment.registry_credential_next_attempt_at<=clock_timestamp()
		AND app.image_visibility='private' AND app.deleted_at IS NULL AND app.registry_id IS NOT NULL
		AND operation.status='pending' AND operation.registry_id=app.registry_id
		AND ((operation.checkpoint='namespaces_reconciling' AND operation.candidate_generation=generation.generation
			AND generation.state='pending' AND registry.operation_id=operation.id AND registry.pending_generation=generation.generation)
		 OR (operation.checkpoint='rollback_reconciling' AND operation.previous_generation=generation.generation
			AND generation.state='active' AND registry.operation_id=operation.id AND registry.checkpoint='rollback_reconciling'
			AND registry.active_generation=generation.generation))
		ORDER BY environment.registry_credential_next_attempt_at,environment.created_at,environment.id
		LIMIT $4 FOR UPDATE OF environment SKIP LOCKED)
		UPDATE environments environment SET registry_credential_lease_owner=$2,
			registry_credential_lease_epoch=environment.registry_credential_lease_epoch+1,
			registry_credential_lease_expires_at=clock_timestamp()+$3::interval,
			registry_credential_attempts=environment.registry_credential_attempts+1
		FROM candidates,apps app WHERE environment.org_id=$1 AND environment.id=candidates.id
		AND app.org_id=environment.org_id AND app.id=environment.app_id AND app.image_visibility='private'
		AND app.registry_id IS NOT NULL
		RETURNING environment.org_id::text,environment.id::text,environment.app_id::text,environment.namespace,
			app.registry_id::text,candidates.operation_id::text,candidates.secret_ref,
			COALESCE(environment.registry_credential_checkpoint,''),environment.required_registry_credential_generation,
			environment.registry_credential_attempts,
			environment.registry_credential_lease_owner,environment.registry_credential_lease_epoch,
			environment.registry_credential_lease_expires_at`, orgID, owner, leaseDuration, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []NamespaceCredentialClaim
	for rows.Next() {
		var claim NamespaceCredentialClaim
		if err := rows.Scan(&claim.OrgID, &claim.EnvironmentID, &claim.AppID, &claim.Namespace, &claim.RegistryID,
			&claim.OperationID, &claim.SecretRef, &claim.Checkpoint, &claim.Generation,
			&claim.Attempts,
			&claim.Lease.Owner, &claim.Lease.Epoch, &claim.Lease.ExpiresAt); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (s *Store) ClaimNamespaceCredentialCleanups(ctx context.Context, orgID, owner string, leaseDuration time.Duration, limit int) ([]NamespaceCredentialCleanupClaim, error) {
	if orgID == "" || strings.TrimSpace(owner) == "" || leaseDuration <= 0 || limit < 1 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS (SELECT environment.id,generation.secret_ref
		FROM environments environment
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		JOIN registry_credential_generations generation ON generation.org_id=environment.org_id
		 AND generation.registry_id=environment.registry_credential_cleanup_registry_id
		 AND generation.generation=environment.registry_credential_cleanup_generation
		WHERE environment.org_id=$1 AND app.image_visibility='public' AND app.deleted_at IS NULL
		AND app.cleanup_state IN ('cleanup_requested','cleaning')
		AND environment.registry_credential_cleanup_state='pending'
		AND environment.registry_credential_cleanup_lease_owner IS NULL
		AND environment.registry_credential_cleanup_attempts<12
		AND environment.registry_credential_cleanup_next_attempt_at<=clock_timestamp()
		AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL
		ORDER BY environment.registry_credential_cleanup_next_attempt_at,environment.created_at,environment.id
		LIMIT $4 FOR UPDATE OF environment SKIP LOCKED)
		UPDATE environments environment SET registry_credential_cleanup_lease_owner=$2,
			registry_credential_cleanup_lease_epoch=environment.registry_credential_cleanup_lease_epoch+1,
			registry_credential_cleanup_lease_expires_at=clock_timestamp()+$3::interval,
			registry_credential_cleanup_attempts=environment.registry_credential_cleanup_attempts+1
		FROM candidates,apps app WHERE environment.org_id=$1 AND environment.id=candidates.id
		AND app.org_id=environment.org_id AND app.id=environment.app_id AND app.image_visibility='public'
		AND app.deleted_at IS NULL AND app.cleanup_state IN ('cleanup_requested','cleaning')
		RETURNING environment.org_id::text,environment.id::text,environment.app_id::text,environment.namespace,
			environment.registry_credential_cleanup_registry_id::text,candidates.secret_ref,
			environment.registry_credential_cleanup_generation,environment.registry_credential_cleanup_attempts,
			environment.registry_credential_cleanup_lease_owner,environment.registry_credential_cleanup_lease_epoch,
			environment.registry_credential_cleanup_lease_expires_at`, orgID, owner, leaseDuration, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []NamespaceCredentialCleanupClaim
	for rows.Next() {
		var claim NamespaceCredentialCleanupClaim
		if err := rows.Scan(&claim.OrgID, &claim.EnvironmentID, &claim.AppID, &claim.Namespace, &claim.RegistryID,
			&claim.SecretRef, &claim.Generation, &claim.Attempts, &claim.Lease.Owner, &claim.Lease.Epoch, &claim.Lease.ExpiresAt); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (s *Store) CompleteNamespaceCredentialCleanup(ctx context.Context, claim NamespaceCredentialCleanupClaim, lease LeaseToken) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE environments environment SET registry_credential_cleanup_state='complete',
			registry_credential_cleanup_lease_owner=NULL,registry_credential_cleanup_lease_expires_at=NULL
			FROM apps app WHERE environment.org_id=$1 AND environment.id=$2 AND environment.app_id=$3
			AND environment.namespace=$4 AND app.org_id=environment.org_id AND app.id=environment.app_id
			AND app.image_visibility='public' AND app.deleted_at IS NULL AND app.cleanup_state IN ('cleanup_requested','cleaning')
			AND environment.registry_credential_cleanup_registry_id=$5
			AND environment.registry_credential_cleanup_generation=$6 AND environment.registry_credential_cleanup_state='pending'
			AND environment.registry_credential_cleanup_lease_owner=$7 AND environment.registry_credential_cleanup_lease_epoch=$8
			AND environment.registry_credential_cleanup_lease_expires_at>clock_timestamp()
			AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL
			AND EXISTS (SELECT 1 FROM registry_credential_generations generation WHERE generation.org_id=environment.org_id
			 AND generation.registry_id=$5 AND generation.generation=$6 AND generation.secret_ref=$9)`,
			claim.OrgID, claim.EnvironmentID, claim.AppID, claim.Namespace, claim.RegistryID, claim.Generation,
			lease.Owner, lease.Epoch, claim.SecretRef)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseNotOwned
		}
		_, err = tx.Exec(ctx, `UPDATE apps app SET cleanup_state='cleaned',cleanup_checkpoint='namespace_credentials_removed',
			cleanup_error_code=NULL,cleanup_error_message=NULL
			WHERE app.org_id=$1 AND app.id=$2 AND app.image_visibility='public' AND app.deleted_at IS NULL
			AND app.cleanup_state IN ('cleanup_requested','cleaning')
			AND NOT EXISTS (SELECT 1 FROM environments environment WHERE environment.org_id=app.org_id
			 AND environment.app_id=app.id AND environment.torn_down_at IS NULL
			 AND environment.lifecycle_status<>'teardown_requested'
			 AND environment.registry_credential_cleanup_state<>'complete')`, claim.OrgID, claim.AppID)
		return err
	})
}

func (s *Store) RescheduleNamespaceCredentialCleanup(ctx context.Context, claim NamespaceCredentialCleanupClaim, lease LeaseToken, delay time.Duration) error {
	if delay < 0 {
		return ErrInvalidArgument
	}
	tag, err := s.db.Exec(ctx, `UPDATE environments environment SET registry_credential_cleanup_lease_owner=NULL,
		registry_credential_cleanup_lease_expires_at=NULL,registry_credential_cleanup_next_attempt_at=clock_timestamp()+$9::interval
		FROM apps app WHERE environment.org_id=$1 AND environment.id=$2 AND environment.app_id=$3
		AND environment.namespace=$4 AND app.org_id=environment.org_id AND app.id=environment.app_id
		AND app.image_visibility='public' AND app.deleted_at IS NULL AND app.cleanup_state IN ('cleanup_requested','cleaning')
		AND environment.registry_credential_cleanup_registry_id=$5 AND environment.registry_credential_cleanup_generation=$6
		AND environment.registry_credential_cleanup_state='pending' AND environment.registry_credential_cleanup_attempts<12
		AND environment.registry_credential_cleanup_lease_owner=$7 AND environment.registry_credential_cleanup_lease_epoch=$8
		AND environment.registry_credential_cleanup_lease_expires_at>clock_timestamp()
		AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL
		AND EXISTS (SELECT 1 FROM registry_credential_generations generation WHERE generation.org_id=environment.org_id
		 AND generation.registry_id=$5 AND generation.generation=$6 AND generation.secret_ref=$10)`,
		claim.OrgID, claim.EnvironmentID, claim.AppID, claim.Namespace, claim.RegistryID, claim.Generation,
		lease.Owner, lease.Epoch, delay, claim.SecretRef)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrLeaseNotOwned
	}
	return err
}

func (s *Store) FailNamespaceCredentialCleanup(ctx context.Context, claim NamespaceCredentialCleanupClaim, lease LeaseToken, code string) error {
	switch code {
	case "image_pull_secret_reconcile_failed", "kubernetes_ownership_conflict", "kubernetes_unavailable", "postgres_permanent":
	default:
		return ErrInvalidArgument
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE environments environment SET registry_credential_cleanup_state='failed',
			registry_credential_cleanup_lease_owner=NULL,registry_credential_cleanup_lease_expires_at=NULL
			FROM apps app WHERE environment.org_id=$1 AND environment.id=$2 AND environment.app_id=$3
			AND environment.namespace=$4 AND app.org_id=environment.org_id AND app.id=environment.app_id
			AND app.image_visibility='public' AND app.deleted_at IS NULL AND app.cleanup_state IN ('cleanup_requested','cleaning')
			AND environment.registry_credential_cleanup_registry_id=$5 AND environment.registry_credential_cleanup_generation=$6
			AND environment.registry_credential_cleanup_state='pending'
			AND environment.registry_credential_cleanup_lease_owner=$7 AND environment.registry_credential_cleanup_lease_epoch=$8
			AND environment.registry_credential_cleanup_lease_expires_at>clock_timestamp()
			AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL
			AND EXISTS (SELECT 1 FROM registry_credential_generations generation WHERE generation.org_id=environment.org_id
			 AND generation.registry_id=$5 AND generation.generation=$6 AND generation.secret_ref=$9)`,
			claim.OrgID, claim.EnvironmentID, claim.AppID, claim.Namespace, claim.RegistryID, claim.Generation,
			lease.Owner, lease.Epoch, claim.SecretRef)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseNotOwned
		}
		_, err = tx.Exec(ctx, `UPDATE apps SET cleanup_state='cleanup_failed',cleanup_checkpoint=NULL,
			cleanup_error_code=$3,cleanup_error_message=NULL WHERE org_id=$1 AND id=$2
			AND image_visibility='public' AND deleted_at IS NULL AND cleanup_state IN ('cleanup_requested','cleaning')`,
			claim.OrgID, claim.AppID, code)
		return err
	})
}

func (s *Store) CheckpointNamespaceCredential(ctx context.Context, claim NamespaceCredentialClaim, lease LeaseToken, checkpoint string) error {
	if checkpoint != "source_read" && checkpoint != "namespace_secret_written" {
		return ErrInvalidArgument
	}
	tag, err := s.db.Exec(ctx, `UPDATE environments environment SET registry_credential_checkpoint=$9
		FROM apps app WHERE environment.org_id=$1 AND environment.id=$2 AND environment.app_id=$3
		AND environment.namespace=$4 AND app.org_id=environment.org_id AND app.id=environment.app_id
		AND app.registry_id=$5 AND environment.required_registry_credential_generation=$6
		AND EXISTS (SELECT 1 FROM registry_credential_generations generation
			JOIN registry_credential_operations operation ON operation.org_id=generation.org_id AND operation.id=$10
			WHERE generation.org_id=environment.org_id AND generation.registry_id=app.registry_id
			AND generation.generation=environment.required_registry_credential_generation AND generation.secret_ref=$11
			AND operation.org_id=generation.org_id AND operation.id=$10 AND operation.registry_id=generation.registry_id
			AND operation.status='pending' AND ((operation.checkpoint='namespaces_reconciling'
				AND operation.candidate_generation=generation.generation AND generation.state='pending')
			 OR (operation.checkpoint='rollback_reconciling' AND operation.previous_generation=generation.generation
				AND generation.state='active')))
		AND environment.registry_credential_state='pending' AND environment.registry_credential_lease_owner=$7
		AND environment.registry_credential_lease_epoch=$8 AND environment.registry_credential_lease_expires_at>clock_timestamp()
		AND (($9='source_read' AND environment.registry_credential_checkpoint IS NULL)
			OR ($9='source_read' AND environment.registry_credential_checkpoint='source_read')
			OR ($9='namespace_secret_written' AND environment.registry_credential_checkpoint IN ('source_read','namespace_secret_written')))
		AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL`,
		claim.OrgID, claim.EnvironmentID, claim.AppID, claim.Namespace, claim.RegistryID, claim.Generation,
		lease.Owner, lease.Epoch, checkpoint, claim.OperationID, claim.SecretRef)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrLeaseNotOwned
	}
	return err
}

func (s *Store) CompleteNamespaceCredential(ctx context.Context, claim NamespaceCredentialClaim, lease LeaseToken) (bool, error) {
	var activated bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE environments environment SET registry_credential_state='ready',
			ready_registry_credential_generation=$6,registry_credential_checkpoint='namespace_secret_verified',
			registry_credential_error_code=NULL,registry_credential_error_message=NULL,
			registry_credential_lease_owner=NULL,registry_credential_lease_expires_at=NULL
			FROM apps app WHERE environment.org_id=$1 AND environment.id=$2 AND environment.app_id=$3
			AND environment.namespace=$4 AND app.org_id=environment.org_id AND app.id=environment.app_id
			AND app.registry_id=$5 AND environment.required_registry_credential_generation=$6
			AND environment.registry_credential_state='pending'
			AND environment.registry_credential_checkpoint='namespace_secret_written'
			AND environment.registry_credential_lease_owner=$7 AND environment.registry_credential_lease_epoch=$8
			AND environment.registry_credential_lease_expires_at>clock_timestamp()
			AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL
			AND EXISTS (SELECT 1 FROM registry_credential_generations generation
				JOIN registry_credential_operations operation ON operation.org_id=generation.org_id AND operation.id=$9
				WHERE generation.org_id=environment.org_id AND generation.registry_id=app.registry_id
				AND generation.generation=environment.required_registry_credential_generation AND generation.secret_ref=$10
				AND operation.org_id=generation.org_id AND operation.id=$9 AND operation.registry_id=generation.registry_id
				AND operation.status='pending' AND ((operation.checkpoint='namespaces_reconciling'
					AND operation.candidate_generation=generation.generation AND generation.state='pending')
				 OR (operation.checkpoint='rollback_reconciling' AND operation.previous_generation=generation.generation
					AND generation.state='active')))`,
			claim.OrgID, claim.EnvironmentID, claim.AppID, claim.Namespace, claim.RegistryID, claim.Generation,
			lease.Owner, lease.Epoch, claim.OperationID, claim.SecretRef)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseNotOwned
		}
		var checkpoint string
		if err := tx.QueryRow(ctx, `SELECT checkpoint FROM registry_credential_operations
			WHERE org_id=$1 AND id=$2 AND registry_id=$3 AND status='pending' FOR UPDATE`,
			claim.OrgID, claim.OperationID, claim.RegistryID).Scan(&checkpoint); err != nil {
			return err
		}
		if checkpoint == "rollback_reconciling" {
			activated, err = finalizeRegistryCredentialRollbackIfReady(ctx, tx, claim.OrgID, claim.RegistryID, claim.OperationID, claim.Generation, "")
			return err
		}
		activated, err = activateRegistryCredentialGenerationIfReady(ctx, tx, claim.OrgID, claim.RegistryID, claim.OperationID, claim.Generation, "")
		return err
	})
	return activated, err
}

func (s *Store) RescheduleNamespaceCredential(ctx context.Context, claim NamespaceCredentialClaim, lease LeaseToken, delay time.Duration) error {
	if delay < 0 {
		return ErrInvalidArgument
	}
	tag, err := s.db.Exec(ctx, `UPDATE environments environment SET registry_credential_lease_owner=NULL,
		registry_credential_lease_expires_at=NULL,registry_credential_next_attempt_at=clock_timestamp()+$9::interval
		FROM apps app WHERE environment.org_id=$1 AND environment.id=$2 AND environment.app_id=$3
		AND environment.namespace=$4 AND app.org_id=environment.org_id AND app.id=environment.app_id
		AND app.registry_id=$5 AND environment.required_registry_credential_generation=$6
		AND EXISTS (SELECT 1 FROM registry_credential_generations generation
			JOIN registry_credential_operations operation ON operation.org_id=generation.org_id
			WHERE generation.org_id=environment.org_id
			AND generation.registry_id=app.registry_id AND generation.generation=environment.required_registry_credential_generation
			AND generation.secret_ref=$11 AND operation.id=$10 AND operation.registry_id=generation.registry_id
			AND operation.status='pending' AND ((operation.checkpoint='namespaces_reconciling'
				AND operation.candidate_generation=generation.generation AND generation.state='pending')
			 OR (operation.checkpoint='rollback_reconciling' AND operation.previous_generation=generation.generation
				AND generation.state='active')))
		AND environment.registry_credential_state='pending' AND environment.registry_credential_lease_owner=$7
		AND environment.registry_credential_lease_epoch=$8 AND environment.registry_credential_lease_expires_at>clock_timestamp()
		AND environment.lifecycle_status<>'teardown_requested' AND environment.torn_down_at IS NULL`,
		claim.OrgID, claim.EnvironmentID, claim.AppID, claim.Namespace, claim.RegistryID, claim.Generation,
		lease.Owner, lease.Epoch, delay, claim.OperationID, claim.SecretRef)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrLeaseNotOwned
	}
	return err
}

func (s *Store) FailNamespaceCredential(ctx context.Context, claim NamespaceCredentialClaim, lease LeaseToken, code, message string) error {
	switch code {
	case "image_pull_secret_reconcile_failed", "kubernetes_ownership_conflict", "kubernetes_unavailable", "postgres_permanent":
	default:
		return ErrInvalidArgument
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var previous *int64
		var checkpoint string
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT operation.previous_generation,
			EXISTS (SELECT 1 FROM environments environment JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
			 WHERE environment.org_id=$1 AND environment.id=$2 AND environment.app_id=$3 AND environment.namespace=$4
			 AND app.registry_id=$5 AND environment.required_registry_credential_generation=$6
			 AND environment.registry_credential_state='pending' AND environment.registry_credential_lease_owner=$7
			 AND environment.registry_credential_lease_epoch=$8 AND environment.registry_credential_lease_expires_at>clock_timestamp())
			FROM registry_credential_operations operation WHERE operation.org_id=$1 AND operation.id=$9
			AND operation.registry_id=$5 AND operation.status='pending' FOR UPDATE`, claim.OrgID, claim.EnvironmentID,
			claim.AppID, claim.Namespace, claim.RegistryID, claim.Generation, lease.Owner, lease.Epoch, claim.OperationID).Scan(&previous, &owned); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT checkpoint FROM registry_credential_operations WHERE org_id=$1 AND id=$2`, claim.OrgID, claim.OperationID).Scan(&checkpoint); err != nil {
			return err
		}
		if !owned {
			return ErrLeaseNotOwned
		}
		if previous == nil {
			if tag, err := tx.Exec(ctx, `UPDATE environments SET registry_credential_state='failed',registry_credential_error_code=$3,
				registry_credential_error_message=NULLIF($4,''),registry_credential_lease_owner=NULL,registry_credential_lease_expires_at=NULL
				WHERE org_id=$1 AND id=$2`, claim.OrgID, claim.EnvironmentID, code, message); err != nil {
				return err
			} else if tag.RowsAffected() == 0 {
				return ErrFailedPrecondition
			}
			return terminalizeRegistryCredentialOperation(ctx, tx, claim.OrgID, claim.OperationID, code)
		}
		if checkpoint == "rollback_reconciling" {
			return terminalizeRegistryCredentialRollbackFailure(ctx, tx, claim.OrgID, claim.RegistryID, claim.OperationID, *previous, code)
		}
		return beginRegistryCredentialRollback(ctx, tx, claim.OrgID, claim.RegistryID, claim.OperationID, claim.Generation, *previous, code)
	})
}

func failExpiredNamespaceCredential(ctx context.Context, tx pgx.Tx, orgID, environmentID, code string) error {
	var registryID, operationID, checkpoint string
	var candidate int64
	var previous *int64
	if err := tx.QueryRow(ctx, `SELECT app.registry_id::text,operation.id::text,operation.checkpoint,
		operation.candidate_generation,operation.previous_generation FROM environments environment
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
		JOIN registry_credential_operations operation ON operation.org_id=registry.org_id AND operation.id=registry.operation_id
		WHERE environment.org_id=$1 AND environment.id=$2 AND environment.registry_credential_state='pending'
		AND environment.registry_credential_lease_expires_at<=clock_timestamp() AND operation.status='pending' FOR UPDATE OF environment,registry,operation`,
		orgID, environmentID).Scan(&registryID, &operationID, &checkpoint, &candidate, &previous); err != nil {
		return err
	}
	if previous == nil {
		tag, err := tx.Exec(ctx, `UPDATE environments SET registry_credential_state='failed',registry_credential_error_code=$3,
			registry_credential_error_message=NULL,registry_credential_lease_owner=NULL,registry_credential_lease_expires_at=NULL
			WHERE org_id=$1 AND id=$2 AND registry_credential_state='pending'`, orgID, environmentID, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrFailedPrecondition
		}
		return terminalizeRegistryCredentialOperation(ctx, tx, orgID, operationID, code)
	}
	if checkpoint == "rollback_reconciling" {
		return terminalizeRegistryCredentialRollbackFailure(ctx, tx, orgID, registryID, operationID, *previous, code)
	}
	return beginRegistryCredentialRollback(ctx, tx, orgID, registryID, operationID, candidate, *previous, code)
}

func terminalizeRegistryCredentialRollbackFailure(ctx context.Context, tx pgx.Tx, orgID, registryID, operationID string, previous int64, code string) error {
	if tag, err := tx.Exec(ctx, `UPDATE environments environment SET registry_credential_state='failed',
		registry_credential_error_code=$4,registry_credential_error_message=NULL,
		registry_credential_lease_owner=NULL,registry_credential_lease_expires_at=NULL
		FROM apps app WHERE environment.org_id=$1 AND app.org_id=environment.org_id AND app.id=environment.app_id
		AND app.registry_id=$2 AND environment.required_registry_credential_generation=$3
		AND environment.registry_credential_state='pending'`, orgID, registryID, previous, code); err != nil {
		return err
	} else if tag.RowsAffected() == 0 {
		return ErrFailedPrecondition
	}
	var candidate int64
	if err := tx.QueryRow(ctx, `UPDATE registry_credential_operations SET status='failed',failure_code=$3,failure_message=NULL,
		terminal_at=clock_timestamp(),next_attempt_at='infinity',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND status='pending' AND checkpoint='rollback_reconciling'
		RETURNING candidate_generation`, orgID, operationID, code).Scan(&candidate); err != nil {
		return err
	}
	if tag, err := tx.Exec(ctx, `UPDATE registry_credential_generations SET state='failed' WHERE org_id=$1 AND registry_id=$2
		AND generation=$3 AND operation_id=$4 AND state='pending'`, orgID, registryID, candidate, operationID); err != nil {
		return err
	} else if tag.RowsAffected() == 0 {
		return ErrFailedPrecondition
	}
	if tag, err := tx.Exec(ctx, `UPDATE registries SET state='ready',checkpoint='activated',pending_generation=NULL,
		pending_credential_secret_ref=NULL,operation_id=NULL,previous_generation=NULL,previous_credential_secret_ref=NULL,
		failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND operation_id=$3 AND checkpoint='rollback_reconciling'
		AND active_generation=$4 AND pending_generation=$5`, orgID, registryID, operationID, previous, candidate); err != nil {
		return err
	} else if tag.RowsAffected() == 0 {
		return ErrFailedPrecondition
	}
	return nil
}

func (s *Store) InvalidateNamespaceCredentialLeaseForTeardown(ctx context.Context, orgID, environmentID string) error {
	if orgID == "" || environmentID == "" {
		return ErrInvalidArgument
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var appID string
		var registryID *string
		if err := tx.QueryRow(ctx, `SELECT app.id::text,app.registry_id::text FROM environments environment
			JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
			WHERE environment.org_id=$1 AND environment.id=$2 FOR UPDATE OF environment,app`, orgID, environmentID).Scan(&appID, &registryID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var operationID, checkpoint *string
		var candidate int64
		var previous *int64
		if registryID != nil {
			err := tx.QueryRow(ctx, `SELECT operation.id::text,operation.checkpoint,operation.candidate_generation,operation.previous_generation
				FROM registries registry JOIN registry_credential_operations operation
				 ON operation.org_id=registry.org_id AND operation.id=registry.operation_id
				WHERE registry.org_id=$1 AND registry.id=$2 AND operation.status='pending'
				FOR UPDATE OF registry,operation`, orgID, *registryID).Scan(&operationID, &checkpoint, &candidate, &previous)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE environments SET registry_credential_lease_owner=NULL,
			registry_credential_lease_expires_at=NULL,registry_credential_lease_epoch=registry_credential_lease_epoch+1,
			registry_credential_cleanup_state=CASE WHEN registry_credential_cleanup_state='pending' THEN 'complete' ELSE registry_credential_cleanup_state END,
			registry_credential_cleanup_lease_owner=NULL,registry_credential_cleanup_lease_expires_at=NULL,
			registry_credential_cleanup_lease_epoch=registry_credential_cleanup_lease_epoch+1
			WHERE org_id=$1 AND id=$2`, orgID, environmentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE apps app SET cleanup_state='cleaned',cleanup_checkpoint='namespace_credentials_removed',
			cleanup_error_code=NULL,cleanup_error_message=NULL WHERE app.org_id=$1 AND app.id=$2
			AND app.image_visibility='public' AND app.deleted_at IS NULL AND app.cleanup_state IN ('cleanup_requested','cleaning')
			AND NOT EXISTS (SELECT 1 FROM environments environment WHERE environment.org_id=app.org_id
			 AND environment.app_id=app.id AND environment.torn_down_at IS NULL
			 AND environment.lifecycle_status<>'teardown_requested'
			 AND environment.id<>$3 AND environment.registry_credential_cleanup_state<>'complete')`, orgID, appID, environmentID); err != nil {
			return err
		}
		if registryID == nil || operationID == nil || checkpoint == nil {
			return nil
		}
		switch *checkpoint {
		case "namespaces_reconciling":
			_, err := activateRegistryCredentialGenerationIfReady(ctx, tx, orgID, *registryID, *operationID, candidate, environmentID)
			return err
		case "rollback_reconciling":
			if previous == nil {
				return ErrFailedPrecondition
			}
			_, err := finalizeRegistryCredentialRollbackIfReady(ctx, tx, orgID, *registryID, *operationID, *previous, environmentID)
			return err
		default:
			return nil
		}
	})
}

func activateRegistryCredentialGenerationIfReady(ctx context.Context, tx pgx.Tx, orgID, registryID, operationID string, generation int64, excludedEnvironmentID string) (bool, error) {
	var candidateSecretRef string
	var previous *int64
	var alreadySucceeded bool
	err := tx.QueryRow(ctx, `SELECT generation.secret_ref,operation.previous_generation,operation.status='succeeded'
		FROM registry_credential_generations generation
		JOIN registry_credential_operations operation ON operation.org_id=generation.org_id
		 AND operation.id=generation.operation_id AND operation.registry_id=generation.registry_id
		JOIN registries registry ON registry.org_id=generation.org_id AND registry.id=generation.registry_id
		WHERE generation.org_id=$1 AND generation.registry_id=$2 AND generation.generation=$3
		AND generation.operation_id=$4 AND registry.operation_id=$4 AND registry.pending_generation=$3
		FOR UPDATE OF registry,operation,generation`, orgID, registryID, generation, operationID).Scan(&candidateSecretRef, &previous, &alreadySucceeded)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrFailedPrecondition
	}
	if err != nil {
		return false, err
	}
	if alreadySucceeded {
		return false, nil
	}
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM environments environment
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		WHERE environment.org_id=$1 AND app.registry_id=$2 AND app.image_visibility='private'
		AND app.deleted_at IS NULL AND environment.torn_down_at IS NULL
		AND environment.lifecycle_status<>'teardown_requested'
		AND environment.id::text<>$4
		AND (environment.required_registry_credential_generation<>$3
			OR environment.registry_credential_state<>'ready'
			OR environment.ready_registry_credential_generation<>$3
			OR environment.registry_credential_checkpoint<>'namespace_secret_verified'
			OR environment.registry_credential_lease_owner IS NOT NULL))`, orgID, registryID, generation, excludedEnvironmentID).Scan(&blocked); err != nil {
		return false, err
	}
	if blocked {
		return false, nil
	}
	if previous != nil {
		if _, err := tx.Exec(ctx, `UPDATE registry_credential_generations SET state='retired',retired_at=clock_timestamp()
			WHERE org_id=$1 AND registry_id=$2 AND generation=$3 AND state='active'`, orgID, registryID, *previous); err != nil {
			return false, err
		}
	}
	if tag, err := tx.Exec(ctx, `UPDATE registry_credential_operations SET status='succeeded',checkpoint='activated',
		terminal_at=clock_timestamp(),next_attempt_at='infinity',lease_owner=NULL,lease_expires_at=NULL,
		failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND registry_id=$3 AND candidate_generation=$4
		AND status='pending' AND checkpoint='namespaces_reconciling'`, orgID, operationID, registryID, generation); err != nil {
		return false, err
	} else if tag.RowsAffected() == 0 {
		return false, ErrFailedPrecondition
	}
	if tag, err := tx.Exec(ctx, `UPDATE registry_credential_generations SET state='active',activated_at=clock_timestamp()
		WHERE org_id=$1 AND registry_id=$2 AND generation=$3 AND operation_id=$4 AND state='pending' AND validated_at IS NOT NULL`,
		orgID, registryID, generation, operationID); err != nil {
		return false, err
	} else if tag.RowsAffected() == 0 {
		return false, ErrFailedPrecondition
	}
	if tag, err := tx.Exec(ctx, `UPDATE registries SET state='ready',checkpoint='activated',active_generation=$3,
		active_credential_secret_ref=$4,pending_generation=NULL,pending_credential_secret_ref=NULL,
		previous_generation=$5,previous_credential_secret_ref=CASE WHEN $5::bigint IS NULL THEN NULL ELSE active_credential_secret_ref END,
		operation_id=NULL,activated_at=clock_timestamp(),failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND operation_id=$6 AND pending_generation=$3`,
		orgID, registryID, generation, candidateSecretRef, previous, operationID); err != nil {
		return false, err
	} else if tag.RowsAffected() == 0 {
		return false, ErrFailedPrecondition
	}
	return true, nil
}

func beginRegistryCredentialRollback(ctx context.Context, tx pgx.Tx, orgID, registryID, operationID string, candidate, previous int64, code string) error {
	var previousSecretRef string
	if err := tx.QueryRow(ctx, `SELECT secret_ref FROM registry_credential_generations
		WHERE org_id=$1 AND registry_id=$2 AND generation=$3 AND state='active'`, orgID, registryID, previous).Scan(&previousSecretRef); err != nil {
		return err
	}
	if tag, err := tx.Exec(ctx, `UPDATE registry_credential_operations SET checkpoint='rollback_reconciling',
		failure_code=NULL,failure_message=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND status='pending'`, orgID, operationID); err != nil {
		return err
	} else if tag.RowsAffected() == 0 {
		return ErrFailedPrecondition
	}
	if tag, err := tx.Exec(ctx, `UPDATE registries SET state='reconciling',checkpoint='rollback_reconciling',
		failure_code=NULL,failure_message=$6,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND operation_id=$3 AND pending_generation=$4 AND active_generation=$5`,
		orgID, registryID, operationID, candidate, previous, code); err != nil {
		return err
	} else if tag.RowsAffected() == 0 {
		return ErrFailedPrecondition
	}
	tag, err := tx.Exec(ctx, `UPDATE environments environment SET required_registry_credential_generation=$3,
		ready_registry_credential_generation=NULL,registry_credential_state='pending',registry_credential_checkpoint=NULL,
		registry_credential_error_code=NULL,registry_credential_error_message=NULL,registry_credential_attempts=0,
		registry_credential_next_attempt_at=clock_timestamp(),registry_credential_lease_owner=NULL,
		registry_credential_lease_expires_at=NULL,registry_credential_lease_epoch=registry_credential_lease_epoch+1
		FROM apps app WHERE environment.org_id=$1 AND app.org_id=environment.org_id AND app.id=environment.app_id
		AND app.registry_id=$2 AND app.image_visibility='private' AND app.deleted_at IS NULL
		AND environment.torn_down_at IS NULL AND environment.lifecycle_status<>'teardown_requested'
		AND EXISTS (SELECT 1 FROM registry_credential_generations generation WHERE generation.org_id=$1
			AND generation.registry_id=$2 AND generation.generation=$3 AND generation.secret_ref=$4 AND generation.state='active')`,
		orgID, registryID, previous, previousSecretRef)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrFailedPrecondition
	}
	return nil
}

func finalizeRegistryCredentialRollbackIfReady(ctx context.Context, tx pgx.Tx, orgID, registryID, operationID string, previous int64, excludedEnvironmentID string) (bool, error) {
	var candidate int64
	var code string
	if err := tx.QueryRow(ctx, `SELECT operation.candidate_generation,COALESCE(registry.failure_message,'image_pull_secret_reconcile_failed')
		FROM registry_credential_operations operation JOIN registries registry ON registry.org_id=operation.org_id
		 AND registry.id=operation.registry_id AND registry.operation_id=operation.id
		WHERE operation.org_id=$1 AND operation.id=$2 AND operation.registry_id=$3
		AND operation.status='pending' AND operation.checkpoint='rollback_reconciling' AND operation.previous_generation=$4
		FOR UPDATE OF operation,registry`,
		orgID, operationID, registryID, previous).Scan(&candidate, &code); err != nil {
		return false, err
	}
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM environments environment
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		WHERE environment.org_id=$1 AND app.registry_id=$2 AND app.image_visibility='private'
		AND app.deleted_at IS NULL AND environment.torn_down_at IS NULL
		AND environment.lifecycle_status<>'teardown_requested'
		AND environment.id::text<>$4
		AND (environment.required_registry_credential_generation<>$3
			OR environment.registry_credential_state<>'ready'
			OR environment.ready_registry_credential_generation<>$3
			OR environment.registry_credential_checkpoint<>'namespace_secret_verified'
			OR environment.registry_credential_lease_owner IS NOT NULL))`, orgID, registryID, previous, excludedEnvironmentID).Scan(&blocked); err != nil {
		return false, err
	}
	if blocked {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE registry_credential_generations SET state='failed'
		WHERE org_id=$1 AND registry_id=$2 AND generation=$3 AND operation_id=$4 AND state='pending'`,
		orgID, registryID, candidate, operationID); err != nil {
		return false, err
	}
	if tag, err := tx.Exec(ctx, `UPDATE registry_credential_operations SET status='failed',checkpoint='rollback_reconciling',
		failure_code=$3,failure_message=NULL,terminal_at=clock_timestamp(),next_attempt_at='infinity',updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND status='pending' AND checkpoint='rollback_reconciling'`, orgID, operationID, code); err != nil {
		return false, err
	} else if tag.RowsAffected() == 0 {
		return false, ErrFailedPrecondition
	}
	if tag, err := tx.Exec(ctx, `UPDATE registries SET state='ready',checkpoint='activated',pending_generation=NULL,
		pending_credential_secret_ref=NULL,operation_id=NULL,previous_generation=NULL,previous_credential_secret_ref=NULL,
		failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND operation_id=$3 AND checkpoint='rollback_reconciling'
		AND active_generation=$4 AND pending_generation=$5`, orgID, registryID, operationID, previous, candidate); err != nil {
		return false, err
	} else if tag.RowsAffected() == 0 {
		return false, ErrFailedPrecondition
	}
	return true, nil
}
