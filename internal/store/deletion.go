package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type TombstoneParams struct {
	OrgID                      string
	ID                         string
	ExternalCredentialsRevoked bool
	Audit                      *AuditEvent
	Idempotency                *CompleteIdempotencyParams
}

func (s *Store) TombstoneApp(ctx context.Context, params TombstoneParams) error {
	return s.tombstone(ctx, params, "apps", "app", "app.deleted",
		`NOT EXISTS (SELECT 1 FROM environments WHERE org_id = $1 AND app_id = $2
			AND (torn_down_at IS NULL OR in_progress_release_id IS NOT NULL OR lock_acquired_at IS NOT NULL))`)
}

func (s *Store) TombstoneProject(ctx context.Context, params TombstoneParams) error {
	return s.tombstone(ctx, params, "projects", "project", "project.deleted",
		`NOT EXISTS (SELECT 1 FROM environments environment
			JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
			WHERE app.org_id = $1 AND app.project_id = $2
			  AND (environment.torn_down_at IS NULL OR environment.in_progress_release_id IS NOT NULL
				OR environment.lock_acquired_at IS NOT NULL))`)
}

func (s *Store) TombstoneOrg(ctx context.Context, params TombstoneParams) error {
	if params.ID != params.OrgID {
		return fmt.Errorf("%w: organization tombstone ID must equal tenant ID", ErrInvalidArgument)
	}
	return s.tombstone(ctx, params, "orgs", "org", "org.deleted",
		`NOT EXISTS (SELECT 1 FROM environments WHERE org_id = $1
			AND (torn_down_at IS NULL OR in_progress_release_id IS NOT NULL OR lock_acquired_at IS NOT NULL))`)
}

func (s *Store) tombstone(ctx context.Context, params TombstoneParams, table, targetType, action, teardownPredicate string) error {
	if !params.ExternalCredentialsRevoked {
		return fmt.Errorf("%w: external credential revocation must be confirmed", ErrInvalidArgument)
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := lockOrganizationTombstoneBoundary(ctx, tx, params.OrgID); err != nil {
			return err
		}
		query := fmt.Sprintf(`SELECT jsonb_build_object('id', id, 'deleted_at', deleted_at)
			FROM %s WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
			AND %s FOR UPDATE`, pgx.Identifier{table}.Sanitize(), teardownPredicate)
		if table == "orgs" {
			query = fmt.Sprintf(`SELECT jsonb_build_object('id', id, 'deleted_at', deleted_at)
				FROM %s WHERE id = $1 AND id = $2 AND deleted_at IS NULL
				AND %s FOR UPDATE`, pgx.Identifier{table}.Sanitize(), teardownPredicate)
		}
		var before json.RawMessage
		if err := tx.QueryRow(ctx, query, params.OrgID, params.ID).Scan(&before); err == pgx.ErrNoRows {
			return ErrConflict
		} else if err != nil {
			return fmt.Errorf("lock %s tombstone: %w", targetType, err)
		}
		update := fmt.Sprintf(`UPDATE %s SET external_credentials_revoked_at = now(), deleted_at = now()
			WHERE id = $1 AND deleted_at IS NULL RETURNING jsonb_build_object(
			'id', id, 'deleted_at', deleted_at,
			'external_credentials_revoked_at', external_credentials_revoked_at)`, pgx.Identifier{table}.Sanitize())
		var after json.RawMessage
		if err := tx.QueryRow(ctx, update, params.ID).Scan(&after); err != nil {
			return fmt.Errorf("tombstone %s: %w", targetType, err)
		}
		if params.Idempotency != nil {
			if err := finishAcceptedMutation(ctx, tx, params.OrgID, targetType, params.ID, params.Idempotency); err != nil {
				return err
			}
		}
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, action, targetType,
			params.ID, before, after)
	})
}

func lockOrganizationTombstoneBoundary(ctx context.Context, tx pgx.Tx, orgID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
		return fmt.Errorf("lock organization tombstone boundary: %w", err)
	}
	return nil
}
