package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type IdempotencyDisposition uint8

const (
	IdempotencyClaimed IdempotencyDisposition = iota + 1
	IdempotencyReplay
	IdempotencyConflict
	IdempotencyInProgress
)

type IdempotencyClaimParams struct {
	OrgID         string
	Scope         string
	Key           string
	RequestHash   string
	Owner         string
	LeaseDuration time.Duration
	Retention     time.Duration
}

type IdempotencyResult struct {
	Disposition    IdempotencyDisposition
	Status         string
	StatusCode     int
	Body           json.RawMessage
	ResourceType   *string
	ResourceID     *string
	ErrorCode      *string
	LeaseExpiresAt *time.Time
}

func (s *Store) ClaimIdempotency(ctx context.Context, params IdempotencyClaimParams) (IdempotencyResult, error) {
	if !sha256Pattern.MatchString(params.RequestHash) {
		return IdempotencyResult{}, fmt.Errorf("%w: request hash must be lowercase SHA-256", ErrInvalidArgument)
	}
	if err := requirePositiveDuration("idempotency lease duration", params.LeaseDuration); err != nil {
		return IdempotencyResult{}, err
	}
	if err := requirePositiveDuration("idempotency retention", params.Retention); err != nil {
		return IdempotencyResult{}, err
	}

	var result IdempotencyResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var inserted bool
		if err := tx.QueryRow(ctx, `
			INSERT INTO api_idempotency_records (
				org_id, scope, idempotency_key, request_hash, status,
				lease_owner, lease_expires_at, expires_at
			) VALUES ($1, $2, $3, $4, 'in_progress', $5,
				now() + make_interval(secs => $6), now() + make_interval(secs => $7))
			ON CONFLICT (org_id, scope, idempotency_key) DO NOTHING
			RETURNING true`, params.OrgID, params.Scope, params.Key, params.RequestHash,
			params.Owner, params.LeaseDuration.Seconds(), params.Retention.Seconds()).Scan(&inserted); err == nil {
			result.Disposition = IdempotencyClaimed
			return tx.QueryRow(ctx, `
				SELECT status, lease_expires_at FROM api_idempotency_records
				WHERE org_id = $1 AND scope = $2 AND idempotency_key = $3`,
				params.OrgID, params.Scope, params.Key).Scan(&result.Status, &result.LeaseExpiresAt)
		} else if err != pgx.ErrNoRows {
			return fmt.Errorf("insert idempotency record: %w", err)
		}

		var storedHash, status string
		var statusCode *int
		var body json.RawMessage
		var leaseExpiresAt *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT request_hash, status, response_code, response_body, resource_type,
				resource_id::text, error_code, lease_expires_at
			FROM api_idempotency_records
			WHERE org_id = $1 AND scope = $2 AND idempotency_key = $3
			FOR UPDATE`, params.OrgID, params.Scope, params.Key).Scan(&storedHash, &status,
			&statusCode, &body, &result.ResourceType, &result.ResourceID, &result.ErrorCode,
			&leaseExpiresAt); err != nil {
			return fmt.Errorf("lock idempotency record: %w", err)
		}
		result.Status = status
		if storedHash != params.RequestHash {
			result.Disposition = IdempotencyConflict
			return nil
		}
		if status != "in_progress" {
			result.Disposition = IdempotencyReplay
			if statusCode != nil {
				result.StatusCode = *statusCode
			}
			result.Body = append(json.RawMessage(nil), body...)
			return nil
		}

		commandTag, err := tx.Exec(ctx, `
			UPDATE api_idempotency_records
			SET lease_owner = $4, lease_expires_at = now() + make_interval(secs => $5),
				expires_at = GREATEST(expires_at, now() + make_interval(secs => $6)), updated_at = now()
			WHERE org_id = $1 AND scope = $2 AND idempotency_key = $3
			  AND status = 'in_progress' AND lease_expires_at <= now()`, params.OrgID,
			params.Scope, params.Key, params.Owner, params.LeaseDuration.Seconds(), params.Retention.Seconds())
		if err != nil {
			return fmt.Errorf("take over stale idempotency record: %w", err)
		}
		if commandTag.RowsAffected() == 0 {
			result.Disposition = IdempotencyInProgress
			result.LeaseExpiresAt = leaseExpiresAt
			return nil
		}
		result.Disposition = IdempotencyClaimed
		result.LeaseExpiresAt = nil
		return tx.QueryRow(ctx, `SELECT lease_expires_at FROM api_idempotency_records
			WHERE org_id = $1 AND scope = $2 AND idempotency_key = $3`, params.OrgID,
			params.Scope, params.Key).Scan(&result.LeaseExpiresAt)
	})
	return result, err
}

func (s *Store) DeleteExpiredIdempotencyRecords(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("%w: idempotency cleanup limit must be greater than zero", ErrInvalidArgument)
	}
	var deleted int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM api_idempotency_records
			WHERE id IN (SELECT id FROM api_idempotency_records WHERE expires_at <= now()
				ORDER BY expires_at, id LIMIT $1 FOR UPDATE SKIP LOCKED)`, limit)
		if err != nil {
			return fmt.Errorf("delete expired idempotency records: %w", err)
		}
		deleted = tag.RowsAffected()
		return nil
	})
	return deleted, err
}

type CompleteIdempotencyParams struct {
	OrgID        string
	Scope        string
	Key          string
	RequestHash  string
	Owner        string
	Status       string
	StatusCode   int
	Body         json.RawMessage
	ResourceType *string
	ResourceID   *string
	ErrorCode    *string
	Audit        *AuditEvent
}

func (s *Store) CompleteIdempotency(ctx context.Context, params CompleteIdempotencyParams) error {
	if params.Status != "completed" && params.Status != "failed" {
		return fmt.Errorf("%w: idempotency status must be completed or failed", ErrInvalidArgument)
	}
	if !json.Valid(params.Body) {
		return fmt.Errorf("%w: response body must be valid JSON", ErrInvalidArgument)
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := completeIdempotency(ctx, tx, params); err != nil {
			return err
		}
		targetType, targetID := "api_idempotency_record", idempotencyAuditTarget(params)
		if params.ResourceType != nil {
			targetType = *params.ResourceType
		}
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit,
			"idempotency."+params.Status, targetType, targetID, nil, params.Body)
	})
}

func completeIdempotency(ctx context.Context, tx pgx.Tx, params CompleteIdempotencyParams) error {
	if params.Status != "completed" && params.Status != "failed" {
		return fmt.Errorf("%w: idempotency status must be completed or failed", ErrInvalidArgument)
	}
	if !json.Valid(params.Body) {
		return fmt.Errorf("%w: response body must be valid JSON", ErrInvalidArgument)
	}
	if err := validateIdempotencyResource(ctx, tx, params.OrgID, params.ResourceType, params.ResourceID); err != nil {
		return err
	}
	commandTag, err := tx.Exec(ctx, `
			UPDATE api_idempotency_records
			SET status = $6, response_code = $7, response_body = $8,
				resource_type = $9, resource_id = $10, error_code = $11,
				lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
			WHERE org_id = $1 AND scope = $2 AND idempotency_key = $3
			  AND request_hash = $4 AND status = 'in_progress'
			  AND lease_owner = $5 AND lease_expires_at > now()`, params.OrgID, params.Scope,
		params.Key, params.RequestHash, params.Owner, params.Status, params.StatusCode,
		params.Body, params.ResourceType, params.ResourceID, params.ErrorCode)
	if err != nil {
		return fmt.Errorf("complete idempotency record: %w", err)
	}
	return rowsAffected(commandTag, ErrLeaseNotOwned)
}

func validateIdempotencyResource(ctx context.Context, tx pgx.Tx, orgID string, resourceType, resourceID *string) error {
	if (resourceType == nil) != (resourceID == nil) {
		return fmt.Errorf("%w: resource type and ID must be provided together", ErrInvalidArgument)
	}
	if resourceType == nil {
		return nil
	}
	tables := map[string]string{
		"release": "releases", "environment": "environments", "app": "apps",
		"project": "projects", "reconciliation_run": "reconciliation_runs",
		"orchestration_job": "orchestration_jobs", "env_var": "env_vars",
		"api_token": "api_tokens", "webhook": "webhooks", "org": "orgs",
		"domain": "domains",
	}
	table, ok := tables[*resourceType]
	if !ok {
		return fmt.Errorf("%w: unsupported idempotency resource type %q", ErrInvalidArgument, *resourceType)
	}
	var exists bool
	query := "SELECT EXISTS (SELECT 1 FROM " + pgx.Identifier{table}.Sanitize() + " WHERE org_id = $1 AND id = $2)"
	if *resourceType == "org" {
		query = `SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1 AND id = $2)`
	}
	if *resourceType == "api_token" {
		query = `SELECT EXISTS (SELECT 1 FROM api_tokens token JOIN users user_account ON user_account.id = token.user_id
			WHERE user_account.org_id = $1 AND token.id = $2)`
	}
	if err := tx.QueryRow(ctx, query, orgID, *resourceID).Scan(&exists); err != nil {
		return fmt.Errorf("validate idempotency resource: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func idempotencyAuditTarget(params CompleteIdempotencyParams) string {
	if params.ResourceID != nil {
		return *params.ResourceID
	}
	return "00000000-0000-0000-0000-000000000000"
}
