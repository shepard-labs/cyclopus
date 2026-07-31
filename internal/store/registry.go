package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Registry is one row of the registries table: an OCI image registry an app
// pulls images from (§6). CredentialSecretRef is a k8s-secret://…#… REFERENCE
// to the pull credential — the raw credential is never stored in Postgres.
type Registry struct {
	ID, OrgID, Provider, RegistryURL, CredentialSecretRef string
	CreatedAt                                             time.Time
}

type SaveRegistryParams struct {
	Registry    Registry
	Idempotency CompleteIdempotencyParams
	Audit       *AuditEvent
}

const registrySelect = `SELECT id::text,org_id::text,provider,registry_url,credential_secret_ref,created_at FROM registries`

func scanRegistry(row rowScanner, value *Registry) error {
	return row.Scan(&value.ID, &value.OrgID, &value.Provider, &value.RegistryURL, &value.CredentialSecretRef, &value.CreatedAt)
}

// CreateRegistry inserts a registry, guarding that the actor's org exists so a
// missing org yields ErrNotFound rather than a bare foreign-key violation. A
// duplicate (org_id, registry_url) maps to ErrAlreadyExists and a CHECK
// violation (bad provider/url/secret_ref shape) maps to ErrInvalidArgument.
func (s *Store) CreateRegistry(ctx context.Context, p SaveRegistryParams) (Registry, error) {
	var value Registry
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanRegistry(tx.QueryRow(ctx, `INSERT INTO registries(id,org_id,provider,registry_url,credential_secret_ref)
			SELECT $1,$2,$3,$4,$5 WHERE EXISTS(SELECT 1 FROM orgs WHERE id=$2)
			RETURNING id::text,org_id::text,provider,registry_url,credential_secret_ref,created_at`,
			p.Registry.ID, p.Registry.OrgID, p.Registry.Provider, p.Registry.RegistryURL, p.Registry.CredentialSecretRef), &value); err != nil {
			return mapMutationError("create registry", err)
		}
		if err := finishAcceptedMutation(ctx, tx, value.OrgID, "registry", value.ID, &p.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, value.OrgID, p.Audit, "registry.created", "registry", value.ID, nil, after)
	})
	return value, err
}

// DeleteRegistry removes a registry org-scoped. Because apps.registry_id
// references registries ON DELETE RESTRICT, deleting a registry still referenced
// by an app raises a foreign-key violation; mapMutationError classifies that as
// ErrFailedPrecondition (409) rather than a bare 500.
func (s *Store) DeleteRegistry(ctx context.Context, orgID, id string, idem CompleteIdempotencyParams, audit *AuditEvent) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(registries) FROM registries WHERE org_id=$1 AND id=$2 FOR UPDATE`, orgID, id).Scan(&before); err != nil {
			return mapMutationError("lock registry", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM registries WHERE org_id=$1 AND id=$2`, orgID, id); err != nil {
			return mapMutationError("delete registry", err)
		}
		if err := completeIdempotency(ctx, tx, idem); err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, orgID, audit, "registry.deleted", "registry", id, before, nil)
	})
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
