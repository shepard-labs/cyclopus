package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	RegistryProviderGHCR = "ghcr"
	RegistryProviderGCR  = "gcr"
	RegistryHostGHCR     = "ghcr.io"
	RegistryHostGCR      = "gcr.io"
)

func ValidRegistryIdentity(provider, host string) bool {
	return (provider == RegistryProviderGHCR && host == RegistryHostGHCR) ||
		(provider == RegistryProviderGCR && host == RegistryHostGCR)
}

type Registry struct {
	ID, OrgID, Provider, RegistryHost, State, Checkpoint string
	RegistryURL                                          string
	ActiveGeneration, ReconciliationGeneration           int64
	HasActiveCredential, HasPendingCredential            bool
	PendingGeneration, PreviousGeneration                *int64
	HasPreviousCredential                                bool
	OperationID, LeaseOwner                              *string
	LeaseEpoch                                           int64
	LeaseExpiresAt, ActivatedAt                          *time.Time
	Attempts                                             int
	NextAttemptAt                                        time.Time
	FailureCode, FailureMessage                          *string
	CreatedAt, UpdatedAt                                 time.Time
}

const registrySelect = `SELECT id::text,org_id::text,provider,registry_host,state,COALESCE(checkpoint,''),active_generation,
	active_credential_secret_ref IS NOT NULL,pending_generation,pending_credential_secret_ref IS NOT NULL,
	reconciliation_generation,failure_code,failure_message,created_at,updated_at,previous_generation,
	previous_credential_secret_ref IS NOT NULL,operation_id::text,lease_owner,lease_epoch,lease_expires_at,attempts,
	next_attempt_at,activated_at FROM registries`

func scanRegistry(row rowScanner, value *Registry) error {
	if err := row.Scan(&value.ID, &value.OrgID, &value.Provider, &value.RegistryHost, &value.State, &value.Checkpoint,
		&value.ActiveGeneration, &value.HasActiveCredential, &value.PendingGeneration, &value.HasPendingCredential,
		&value.ReconciliationGeneration, &value.FailureCode, &value.FailureMessage, &value.CreatedAt, &value.UpdatedAt,
		&value.PreviousGeneration, &value.HasPreviousCredential, &value.OperationID, &value.LeaseOwner,
		&value.LeaseEpoch, &value.LeaseExpiresAt, &value.Attempts, &value.NextAttemptAt, &value.ActivatedAt); err != nil {
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

func (s *Store) GetGHCRRegistry(ctx context.Context, orgID string) (Registry, error) {
	rows, err := s.db.Query(ctx, registrySelect+` WHERE org_id=$1 AND provider=$2 AND registry_host=$3 ORDER BY id LIMIT 2`, orgID, RegistryProviderGHCR, RegistryHostGHCR)
	if err != nil {
		return Registry{}, err
	}
	defer rows.Close()
	var values []Registry
	for rows.Next() {
		var value Registry
		if err := scanRegistry(rows, &value); err != nil {
			return Registry{}, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return Registry{}, err
	}
	if len(values) == 0 {
		return Registry{}, ErrNotFound
	}
	if len(values) > 1 {
		return Registry{}, ErrFailedPrecondition
	}
	return values[0], nil
}

// EnsureGHCRRegistryID transactionally returns the ID of the org's canonical
// GHCR registry row, creating the absent placeholder when none exists.
// Creation races settle on the unique (org_id, provider, registry_host) key.
func (s *Store) EnsureGHCRRegistryID(ctx context.Context, orgID string) (string, error) {
	if orgID == "" {
		return "", ErrInvalidArgument
	}
	var id string
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO registries(org_id,provider,registry_host)
			VALUES($1,$2,$3) ON CONFLICT (org_id,provider,registry_host) DO NOTHING
			RETURNING id::text`, orgID, RegistryProviderGHCR, RegistryHostGHCR).Scan(&id); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			return tx.QueryRow(ctx, `SELECT id::text FROM registries WHERE org_id=$1 AND provider=$2 AND registry_host=$3`, orgID, RegistryProviderGHCR, RegistryHostGHCR).Scan(&id)
		}
		return nil
	})
	return id, resourceQueryError("ensure ghcr registry", err)
}

// EnsureGHCRRegistry transactionally returns the org's canonical GHCR
// registry row, creating the absent placeholder when none exists. Creation
// races settle on the unique (org_id, provider, registry_host) key. The
// helper is written against the store's connection pool interface so service
// construction can keep its narrow credential store abstraction.
func EnsureGHCRRegistry(ctx context.Context, s *Store, orgID string) (Registry, error) {
	if orgID == "" {
		return Registry{}, ErrInvalidArgument
	}
	var value Registry
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO registries(org_id,provider,registry_host)
			VALUES($1,$2,$3) ON CONFLICT (org_id,provider,registry_host) DO NOTHING
			RETURNING id::text`, orgID, RegistryProviderGHCR, RegistryHostGHCR).Scan(&id); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT id::text FROM registries WHERE org_id=$1 AND provider=$2 AND registry_host=$3`, orgID, RegistryProviderGHCR, RegistryHostGHCR).Scan(&id); err != nil {
				return err
			}
		}
		return scanRegistry(tx.QueryRow(ctx, registrySelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &value)
	})
	return value, resourceQueryError("ensure ghcr registry", err)
}

func ValidateRegistry(registry Registry) error {
	if !ValidRegistryIdentity(registry.Provider, registry.RegistryHost) {
		return errors.New("store: unsupported or noncanonical registry identity")
	}
	return nil
}
