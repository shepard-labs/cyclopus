package store

import (
	"context"
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

type NamespaceCredentialClaim struct {
	OrgID, EnvironmentID, AppID, Namespace string
	RegistryID                             *string
	Generation                             int64
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
		AND torn_down_at IS NULL AND registry_credential_attempts<12
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
		(p.Kind != "create" && p.Kind != "import_create" && p.Kind != "rotate") || !oci.ValidRepository(p.TargetRepository, 1) || !validValidationReference(p.TargetReference) {
		return RegistryCredentialOperation{}, ErrInvalidArgument
	}
	var result RegistryCredentialOperation
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.OrgID+":"+p.RegistryID); err != nil {
			return err
		}
		if err := scanRegistryCredentialOperation(tx.QueryRow(ctx, registryCredentialOperationSelect+` WHERE org_id=$1 AND idempotency_key=$2`, p.OrgID, p.IdempotencyKey), &result); err == nil {
			if result.RegistryID != p.RegistryID || result.Kind != p.Kind || result.TargetRepository == nil || *result.TargetRepository != p.TargetRepository ||
				result.TargetReference == nil || *result.TargetReference != p.TargetReference {
				return ErrConflict
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
		if _, err := tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,candidate_generation,previous_generation,target_repository,target_reference,idempotency_key) VALUES($1,$2,$3,$4,'awaiting_source','reserved',$5,$6,$7,$8,$9)`, operationID, p.OrgID, p.RegistryID, p.Kind, candidate, previous, p.TargetRepository, p.TargetReference, p.IdempotencyKey); err != nil {
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
		 targets AS (SELECT repository,reference FROM latest UNION SELECT $4,$5)
		 INSERT INTO registry_credential_validation_targets(operation_id,org_id,registry_id,repository,reference)
		 SELECT $3,$1,$2,repository,reference FROM targets`, p.OrgID, p.RegistryID, operationID, p.TargetRepository, p.TargetReference); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE registries SET checkpoint='reserved',pending_generation=$3,pending_credential_secret_ref=NULL,operation_id=$4,failure_code=NULL,failure_message=NULL,attempts=0,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, p.OrgID, p.RegistryID, candidate, operationID); err != nil {
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
		if existing != nil {
			if *existing != p.SecretRef || generationState != "pending" || result.Checkpoint != "source_secret_written" || result.Status != "pending" {
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
		if _, err := tx.Exec(ctx, `UPDATE registry_credential_operations SET status='pending',checkpoint='source_secret_written',next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, p.OrgID, result.ID); err != nil {
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
		rows, err := tx.Query(ctx, `SELECT org_id::text,id::text FROM registry_credential_operations WHERE status='awaiting_source' AND created_at<=$1 ORDER BY created_at,id LIMIT $2 FOR UPDATE SKIP LOCKED`, olderThan, limit)
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
		_, err = tx.Exec(ctx, `UPDATE registries SET checkpoint='reconciling_namespaces',state='reconciling',updated_at=clock_timestamp() WHERE org_id=$1 AND operation_id=$2`, operation.OrgID, operation.ID)
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
