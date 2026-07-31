package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type ReconciliationRun struct {
	ID                 string
	OrgID              string
	ActorUserID        *string
	ScopeKind          string
	ScopeID            string
	ScopeAppID         *string
	ScopeEnvironmentID *string
	Categories         []string
	DryRun             bool
	Prune              bool
	Status             string
	Plan               json.RawMessage
	Report             json.RawMessage
	LeaseExpiresAt     *time.Time
	CreatedAt          time.Time
	StartedAt          *time.Time
	CompletedAt        *time.Time
	ErrorCode          *string
	ErrorMessage       *string
}

type CreateReconciliationRunParams struct {
	ID                 string
	OrgID              string
	ActorUserID        *string
	ScopeKind          string
	ScopeID            string
	ScopeAppID         *string
	ScopeEnvironmentID *string
	Categories         []string
	DryRun             *bool
	Prune              bool
	Idempotency        *CompleteIdempotencyParams
}

func (s *Store) CreateReconciliationRun(ctx context.Context, params CreateReconciliationRunParams) (ReconciliationRun, error) {
	dryRun := true
	if params.DryRun != nil {
		dryRun = *params.DryRun
	}
	categories := params.Categories
	if categories == nil {
		categories = []string{}
	}
	var run ReconciliationRun
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, params.OrgID); err != nil {
			return fmt.Errorf("lock reconciliation tombstone boundary: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO reconciliation_runs (
				id, org_id, actor_user_id, scope_kind, scope_id, scope_app_id,
				scope_environment_id, categories, dry_run, prune
			) SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
			WHERE EXISTS (
				SELECT 1 FROM orgs org
				WHERE org.id = $2 AND org.deleted_at IS NULL
				  AND ($4 = 'org' OR EXISTS (
					SELECT 1 FROM apps app
					JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
					WHERE app.org_id = $2 AND app.id = $6
					  AND app.deleted_at IS NULL AND project.deleted_at IS NULL
					  AND ($4 = 'app' OR EXISTS (
						SELECT 1 FROM environments environment
						WHERE environment.org_id = app.org_id AND environment.app_id = app.id
						  AND environment.id = $7 AND environment.torn_down_at IS NULL)))))
			RETURNING id::text, org_id::text, actor_user_id::text, scope_kind, scope_id::text,
				scope_app_id::text, scope_environment_id::text, categories, dry_run, prune, status, created_at`,
			params.ID, params.OrgID, params.ActorUserID, params.ScopeKind, params.ScopeID,
			params.ScopeAppID, params.ScopeEnvironmentID, categories, dryRun, params.Prune).Scan(
			&run.ID, &run.OrgID, &run.ActorUserID, &run.ScopeKind, &run.ScopeID,
			&run.ScopeAppID, &run.ScopeEnvironmentID, &run.Categories, &run.DryRun, &run.Prune, &run.Status, &run.CreatedAt); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrAlreadyExists
			}
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23514") {
				return ErrInvalidArgument
			}
			return fmt.Errorf("insert reconciliation run: %w", err)
		}
		if params.Idempotency != nil {
			resourceType := "reconciliation_run"
			params.Idempotency.OrgID = params.OrgID
			params.Idempotency.ResourceType = &resourceType
			params.Idempotency.ResourceID = &run.ID
			if err := completeIdempotency(ctx, tx, *params.Idempotency); err != nil {
				return err
			}
		}
		return nil
	})
	return run, err
}

func (s *Store) ClaimReconciliationRun(ctx context.Context, orgID, owner string, leaseDuration time.Duration) (*ReconciliationRun, error) {
	if err := requirePositiveDuration("reconciliation run lease duration", leaseDuration); err != nil {
		return nil, err
	}
	var run ReconciliationRun
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock reconciliation tombstone boundary: %w", err)
		}
		err := tx.QueryRow(ctx, `
			WITH candidate AS (
				SELECT run.id FROM reconciliation_runs run
				JOIN orgs org ON org.id = run.org_id
				LEFT JOIN apps app ON app.org_id = run.org_id AND app.id = run.scope_app_id
				LEFT JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
				LEFT JOIN environments environment ON environment.org_id = run.org_id
				  AND environment.app_id = run.scope_app_id AND environment.id = run.scope_environment_id
				WHERE run.org_id = $1 AND (
					run.status = 'queued' OR (run.status IN ('leased', 'running') AND run.lease_expires_at <= now())
				)
				  AND org.deleted_at IS NULL
				  AND (run.scope_kind = 'org' OR (app.deleted_at IS NULL AND project.deleted_at IS NULL
					AND (run.scope_kind = 'app' OR environment.torn_down_at IS NULL)))
				ORDER BY run.created_at, run.id FOR UPDATE OF run SKIP LOCKED LIMIT 1
			)
			UPDATE reconciliation_runs AS run
			SET status = 'leased', lease_owner = $2,
				lease_expires_at = now() + make_interval(secs => $3),
				started_at = COALESCE(started_at, now())
			FROM candidate WHERE run.id = candidate.id
			RETURNING run.id::text, run.org_id::text, run.actor_user_id::text,
				run.scope_kind, run.scope_id::text, run.scope_app_id::text,
				run.scope_environment_id::text, run.categories, run.dry_run, run.prune, run.status,
				run.plan, run.report, run.lease_expires_at, run.created_at, run.completed_at`, orgID, owner,
			leaseDuration.Seconds()).Scan(&run.ID, &run.OrgID, &run.ActorUserID,
			&run.ScopeKind, &run.ScopeID, &run.ScopeAppID, &run.ScopeEnvironmentID, &run.Categories,
			&run.DryRun, &run.Prune, &run.Status, &run.Plan, &run.Report, &run.LeaseExpiresAt, &run.CreatedAt, &run.CompletedAt)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("claim reconciliation run: %w", err)
	}
	return &run, nil
}

func (s *Store) StartReconciliationRun(ctx context.Context, orgID, runID, owner string, plan json.RawMessage, audit *AuditEvent) error {
	if !json.Valid(plan) {
		return fmt.Errorf("%w: reconciliation plan must be valid JSON", ErrInvalidArgument)
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock reconciliation tombstone boundary: %w", err)
		}
		commandTag, err := tx.Exec(ctx, `
			UPDATE reconciliation_runs AS run
			SET status = 'running', plan = $4
			WHERE run.org_id = $1 AND run.id = $2 AND run.status = 'leased'
			  AND run.lease_owner = $3 AND run.lease_expires_at > now()
			  AND EXISTS (SELECT 1 FROM orgs org WHERE org.id = run.org_id AND org.deleted_at IS NULL
				AND (run.scope_kind = 'org' OR EXISTS (SELECT 1 FROM apps app
					JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
					WHERE app.org_id = run.org_id AND app.id = run.scope_app_id
					  AND app.deleted_at IS NULL AND project.deleted_at IS NULL
					  AND (run.scope_kind = 'app' OR EXISTS (SELECT 1 FROM environments environment
						WHERE environment.org_id = run.org_id AND environment.app_id = run.scope_app_id
						  AND environment.id = run.scope_environment_id AND environment.torn_down_at IS NULL)))))`, orgID, runID, owner, plan)
		if err != nil {
			return fmt.Errorf("start reconciliation run: %w", err)
		}
		if err := rowsAffected(commandTag, ErrLeaseNotOwned); err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, orgID, audit, "reconciliation.invoked",
			"reconciliation_run", runID, nil, plan)
	})
}

type ReconciliationEvidence struct {
	Sequence        int64
	Kind            string
	ObjectKind      *string
	ObjectNamespace *string
	ObjectName      *string
	Action          *string
	DesiredHash     *string
	ObservedHash    *string
	Details         json.RawMessage
	ObservedAt      time.Time
	Audit           *AuditEvent
}

func (s *Store) ListReconciliationEvidence(ctx context.Context, orgID, runID string, afterSequence int64, limit int) ([]ReconciliationEvidence, error) {
	if afterSequence < 0 || limit <= 0 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `
		SELECT evidence.sequence, evidence.evidence_kind, evidence.object_kind,
			evidence.object_namespace, evidence.object_name, evidence.action,
			evidence.desired_hash, evidence.observed_hash, evidence.details,
			evidence.observed_at
		FROM reconciliation_evidence evidence
		JOIN reconciliation_runs run ON run.id = evidence.reconciliation_run_id
		WHERE run.org_id = $1 AND run.id = $2 AND evidence.sequence > $3
		ORDER BY evidence.sequence
		LIMIT $4`, orgID, runID, afterSequence, limit)
	if err != nil {
		return nil, fmt.Errorf("list reconciliation evidence: %w", err)
	}
	defer rows.Close()
	values := make([]ReconciliationEvidence, 0, limit)
	for rows.Next() {
		var value ReconciliationEvidence
		if err := rows.Scan(&value.Sequence, &value.Kind, &value.ObjectKind,
			&value.ObjectNamespace, &value.ObjectName, &value.Action, &value.DesiredHash,
			&value.ObservedHash, &value.Details, &value.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan reconciliation evidence: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list reconciliation evidence: %w", err)
	}
	if len(values) == 0 {
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM reconciliation_runs WHERE org_id = $1 AND id = $2
		)`, orgID, runID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("check reconciliation run: %w", err)
		}
		if !exists {
			return nil, ErrNotFound
		}
	}
	return values, nil
}

func (s *Store) AppendReconciliationEvidence(ctx context.Context, orgID, runID, owner string, evidence ReconciliationEvidence) error {
	if !json.Valid(evidence.Details) {
		return fmt.Errorf("%w: reconciliation evidence details must be valid JSON", ErrInvalidArgument)
	}
	mutationAction, isMutation := reconciliationMutationAction(evidence.Action)
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock reconciliation tombstone boundary: %w", err)
		}
		var dryRun bool
		if err := tx.QueryRow(ctx, `SELECT dry_run FROM reconciliation_runs
			WHERE org_id = $1 AND id = $2 AND status = 'running'
			  AND lease_owner = $3 AND lease_expires_at > now()
			  AND EXISTS (SELECT 1 FROM orgs org WHERE org.id = reconciliation_runs.org_id AND org.deleted_at IS NULL
				AND (reconciliation_runs.scope_kind = 'org' OR EXISTS (SELECT 1 FROM apps app
					JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
					WHERE app.org_id = reconciliation_runs.org_id AND app.id = reconciliation_runs.scope_app_id
					  AND app.deleted_at IS NULL AND project.deleted_at IS NULL
					  AND (reconciliation_runs.scope_kind = 'app' OR EXISTS (SELECT 1 FROM environments environment
						WHERE environment.org_id = reconciliation_runs.org_id
						  AND environment.app_id = reconciliation_runs.scope_app_id
						  AND environment.id = reconciliation_runs.scope_environment_id
						  AND environment.torn_down_at IS NULL))))) FOR UPDATE`,
			orgID, runID, owner).Scan(&dryRun); err == pgx.ErrNoRows {
			return ErrLeaseNotOwned
		} else if err != nil {
			return fmt.Errorf("lock reconciliation run: %w", err)
		}
		if !dryRun && isMutation && evidence.Audit == nil {
			return fmt.Errorf("%w: non-dry-run reconciliation mutation audit is required", ErrInvalidArgument)
		}
		commandTag, err := tx.Exec(ctx, `
			INSERT INTO reconciliation_evidence (
				reconciliation_run_id, evidence_kind, object_kind, object_namespace,
				object_name, action, desired_hash, observed_hash, details
			)
			SELECT id, $4, $5, $6, $7, $8, $9, $10, $11
			FROM reconciliation_runs
			WHERE org_id = $1 AND id = $2 AND status = 'running'
			  AND lease_owner = $3 AND lease_expires_at > now()`, orgID, runID, owner,
			evidence.Kind, evidence.ObjectKind, evidence.ObjectNamespace, evidence.ObjectName,
			evidence.Action, evidence.DesiredHash, evidence.ObservedHash, evidence.Details)
		if err != nil {
			return fmt.Errorf("append reconciliation evidence: %w", err)
		}
		if err := rowsAffected(commandTag, ErrLeaseNotOwned); err != nil {
			return err
		}
		if dryRun || !isMutation {
			return nil
		}
		after, err := json.Marshal(map[string]any{
			"reconciliation_run_id": runID,
			"object_kind":           evidence.ObjectKind,
			"object_namespace":      evidence.ObjectNamespace,
			"object_name":           evidence.ObjectName,
			"operation":             *evidence.Action,
			"details":               evidence.Details,
		})
		if err != nil {
			return fmt.Errorf("marshal reconciliation mutation audit: %w", err)
		}
		return insertAuditEvent(ctx, tx, orgID, evidence.Audit, mutationAction,
			"reconciliation_object", runID, nil, after)
	})
}

func reconciliationMutationAction(action *string) (string, bool) {
	if action == nil {
		return "", false
	}
	switch *action {
	case "create":
		return "reconciliation.object_created", true
	case "patch":
		return "reconciliation.object_patched", true
	case "delete":
		return "reconciliation.object_deleted", true
	default:
		return "", false
	}
}

type CompleteReconciliationRunParams struct {
	OrgID        string
	RunID        string
	Owner        string
	Status       string
	Report       json.RawMessage
	ErrorCode    *string
	ErrorMessage *string
}

func (s *Store) CompleteReconciliationRun(ctx context.Context, params CompleteReconciliationRunParams) error {
	if params.Status != "completed" && params.Status != "failed" {
		return fmt.Errorf("%w: reconciliation status must be completed or failed", ErrInvalidArgument)
	}
	if !json.Valid(params.Report) {
		return fmt.Errorf("%w: reconciliation report must be valid JSON", ErrInvalidArgument)
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, params.OrgID); err != nil {
			return fmt.Errorf("lock reconciliation tombstone boundary: %w", err)
		}
		var locked int
		if err := tx.QueryRow(ctx, `SELECT 1
			FROM reconciliation_runs WHERE org_id = $1 AND id = $2 AND status = 'running'
			AND lease_owner = $3 AND lease_expires_at > now()
			AND EXISTS (SELECT 1 FROM orgs org WHERE org.id = reconciliation_runs.org_id AND org.deleted_at IS NULL
				AND (reconciliation_runs.scope_kind = 'org' OR EXISTS (SELECT 1 FROM apps app
					JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
					WHERE app.org_id = reconciliation_runs.org_id AND app.id = reconciliation_runs.scope_app_id
					  AND app.deleted_at IS NULL AND project.deleted_at IS NULL
					  AND (reconciliation_runs.scope_kind = 'app' OR EXISTS (SELECT 1 FROM environments environment
						WHERE environment.org_id = reconciliation_runs.org_id
						  AND environment.app_id = reconciliation_runs.scope_app_id
						  AND environment.id = reconciliation_runs.scope_environment_id
						  AND environment.torn_down_at IS NULL))))) FOR UPDATE`, params.OrgID,
			params.RunID, params.Owner).Scan(&locked); err == pgx.ErrNoRows {
			return ErrLeaseNotOwned
		} else if err != nil {
			return fmt.Errorf("lock reconciliation run: %w", err)
		}
		commandTag, err := tx.Exec(ctx, `UPDATE reconciliation_runs
			SET status = $4, report = $5, error_code = $6, error_message = $7,
				lease_owner = NULL, lease_expires_at = NULL, completed_at = now()
			WHERE org_id = $1 AND id = $2 AND status = 'running' AND lease_owner = $3`,
			params.OrgID, params.RunID, params.Owner, params.Status, params.Report,
			params.ErrorCode, params.ErrorMessage)
		if err != nil {
			return fmt.Errorf("complete reconciliation run: %w", err)
		}
		if err := rowsAffected(commandTag, ErrLeaseNotOwned); err != nil {
			return err
		}
		return nil
	})
}

// ListReconciliationOrgIDs returns org IDs that have reconciliation runs
// in a claimable state (queued, or leased/running with an expired lease).
func (s *Store) ListReconciliationOrgIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT org_id::text FROM reconciliation_runs
		WHERE status = 'queued'
		   OR (status IN ('leased', 'running') AND lease_expires_at <= now())
		ORDER BY org_id::text`)
	if err != nil {
		return nil, fmt.Errorf("list reconciliation org IDs: %w", err)
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
