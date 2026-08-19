package store

import (
	"context"
	"time"
)

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
	TargetRepository, FailureCode, FailureMessage                   *string
	Lease                                                           *LeaseToken
	Attempts                                                        int
	NextAttemptAt, CreatedAt, UpdatedAt                             time.Time
	TerminalAt                                                      *time.Time
}

type RegistryCredentialValidationTarget struct {
	OperationID, OrgID, RegistryID, Repository, Status string
	Attempts                                           int
	NextAttemptAt                                      time.Time
	FailureCode                                        *string
	ValidatedAt                                        *time.Time
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
		WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp()
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

const registryCredentialOperationSelect = `SELECT id::text,org_id::text,registry_id::text,kind,status,checkpoint,
	candidate_generation,previous_generation,target_repository,idempotency_key,lease_owner,lease_epoch,lease_expires_at,
	attempts,next_attempt_at,failure_code,failure_message,created_at,updated_at,terminal_at FROM registry_credential_operations`

func scanRegistryCredentialOperation(row rowScanner, value *RegistryCredentialOperation) error {
	var owner *string
	var epoch int64
	var expiresAt *time.Time
	if err := row.Scan(&value.ID, &value.OrgID, &value.RegistryID, &value.Kind, &value.Status, &value.Checkpoint,
		&value.CandidateGeneration, &value.PreviousGeneration, &value.TargetRepository, &value.IdempotencyKey,
		&owner, &epoch, &expiresAt, &value.Attempts, &value.NextAttemptAt, &value.FailureCode, &value.FailureMessage,
		&value.CreatedAt, &value.UpdatedAt, &value.TerminalAt); err != nil {
		return err
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

const registryCredentialGenerationSelect = `SELECT org_id::text,registry_id::text,generation,secret_ref,state,operation_id::text,
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

const registryCredentialValidationTargetSelect = `SELECT operation_id::text,org_id::text,registry_id::text,repository,status,
	attempts,next_attempt_at,failure_code,validated_at FROM registry_credential_validation_targets`

func scanRegistryCredentialValidationTarget(row rowScanner, value *RegistryCredentialValidationTarget) error {
	return row.Scan(&value.OperationID, &value.OrgID, &value.RegistryID, &value.Repository, &value.Status,
		&value.Attempts, &value.NextAttemptAt, &value.FailureCode, &value.ValidatedAt)
}

func (s *Store) GetRegistryCredentialValidationTarget(ctx context.Context, orgID, operationID, repository string) (RegistryCredentialValidationTarget, error) {
	var value RegistryCredentialValidationTarget
	err := scanRegistryCredentialValidationTarget(s.db.QueryRow(ctx, registryCredentialValidationTargetSelect+` WHERE org_id=$1 AND operation_id=$2 AND repository=$3`, orgID, operationID, repository), &value)
	return value, resourceQueryError("get registry credential validation target", err)
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
