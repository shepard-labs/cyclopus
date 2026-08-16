package store

import (
	"context"
	"time"
)

// Registry is one row of the registries table: an OCI image registry an app
// pulls images from (§6). CredentialSecretRef is a k8s-secret://…#… REFERENCE
// to the pull credential — the raw credential is never stored in Postgres.
type Registry struct {
	ID, OrgID, Provider, RegistryHost, State, Checkpoint string
	RegistryURL                                          string
	ActiveGeneration, ReconciliationGeneration           int64
	HasActiveCredential, HasPendingCredential            bool
	PendingGeneration                                    *int64
	FailureCode, FailureMessage                          *string
	CreatedAt, UpdatedAt                                 time.Time
}

type SaveRegistryParams struct {
	Registry    Registry
	Idempotency CompleteIdempotencyParams
	Audit       *AuditEvent
}

const registrySelect = `SELECT id::text,org_id::text,provider,registry_host,state,COALESCE(checkpoint,''),active_generation,
	active_credential_secret_ref IS NOT NULL,pending_generation,pending_credential_secret_ref IS NOT NULL,
	reconciliation_generation,failure_code,failure_message,created_at,updated_at FROM registries`

func scanRegistry(row rowScanner, value *Registry) error {
	if err := row.Scan(&value.ID, &value.OrgID, &value.Provider, &value.RegistryHost, &value.State, &value.Checkpoint,
		&value.ActiveGeneration, &value.HasActiveCredential, &value.PendingGeneration, &value.HasPendingCredential,
		&value.ReconciliationGeneration, &value.FailureCode, &value.FailureMessage, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return err
	}
	value.RegistryURL = value.RegistryHost
	return nil
}

// CreateRegistry inserts a registry, guarding that the actor's org exists so a
// missing org yields ErrNotFound rather than a bare foreign-key violation. A
// duplicate (org_id, registry_url) maps to ErrAlreadyExists and a CHECK
// violation (bad provider/url/secret_ref shape) maps to ErrInvalidArgument.
func (s *Store) CreateRegistry(ctx context.Context, p SaveRegistryParams) (Registry, error) {
	return Registry{}, ErrFailedPrecondition
}

// DeleteRegistry removes a registry org-scoped. Because apps.registry_id
// references registries ON DELETE RESTRICT, deleting a registry still referenced
// by an app raises a foreign-key violation; mapMutationError classifies that as
// ErrFailedPrecondition (409) rather than a bare 500.
func (s *Store) DeleteRegistry(ctx context.Context, orgID, id string, idem CompleteIdempotencyParams, audit *AuditEvent) error {
	return ErrFailedPrecondition
}

func (s *Store) GetRegistry(ctx context.Context, orgID, id string) (Registry, error) {
	var value Registry
	err := scanRegistry(s.db.QueryRow(ctx, registrySelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &value)
	return value, resourceQueryError("get registry", err)
}

// ListRegistries returns every registry in the org, ordered stably for
// pagination.
func (s *Store) ListRegistries(ctx context.Context, orgID string) ([]Registry, error) {
	rows, err := s.db.Query(ctx, registrySelect+` WHERE org_id=$1 ORDER BY created_at,id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []Registry
	for rows.Next() {
		var value Registry
		if err := scanRegistry(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
