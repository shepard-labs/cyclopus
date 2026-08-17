package store

import (
	"context"
	"time"
)

type Registry struct {
	ID, OrgID, Provider, RegistryHost, State, Checkpoint string
	RegistryURL                                          string
	ActiveGeneration, ReconciliationGeneration           int64
	HasActiveCredential, HasPendingCredential            bool
	PendingGeneration                                    *int64
	FailureCode, FailureMessage                          *string
	CreatedAt, UpdatedAt                                 time.Time
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
