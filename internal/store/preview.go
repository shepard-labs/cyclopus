package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const PreviewIdentityAttempts = 16

type PreviewIdentity struct {
	EnvironmentID string
	Slug          string
	Namespace     string
}

type PreviewPushParams struct {
	OrgID, AppID, Branch, CommitSHA, IdempotencyKey string
	OrchestrationJobID                              string
	Identities                                      []PreviewIdentity
	Audit                                           *AuditEvent
}

type PreviewPushResult struct {
	Environment Environment
	Disposition string
	Dropped     bool
}

func (s *Store) AcceptPreviewPush(ctx context.Context, params PreviewPushParams) (PreviewPushResult, error) {
	if len(params.Identities) == 0 || len(params.Identities) > PreviewIdentityAttempts {
		return PreviewPushResult{}, fmt.Errorf("%w: preview identities are required", ErrInvalidArgument)
	}
	var result PreviewPushResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := replayPreviewRequest(ctx, tx, params.OrgID, params.IdempotencyKey, "push", params.AppID, params.Branch, params.CommitSHA, "", "", &result); err != nil {
			return err
		}
		if result.Disposition != "" {
			return nil
		}
		var enabled bool
		var cap int
		var repositoryID, workflowID *string
		var defaultBranch *string
		if err := tx.QueryRow(ctx, `SELECT app.previews_enabled,app.max_preview_environments,
			project.git_connection_id::text,app.ci_workflow_id,git.default_branch FROM apps app
			JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
			LEFT JOIN git_connections git ON git.org_id=project.org_id AND git.id=project.git_connection_id
			JOIN orgs org ON org.id=app.org_id
			WHERE app.org_id=$1 AND app.id=$2 AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL FOR UPDATE OF app`, params.OrgID, params.AppID).
			Scan(&enabled, &cap, &repositoryID, &workflowID, &defaultBranch); err != nil {
			return mapMutationError("lock preview app", err)
		}
		if !enabled {
			result = PreviewPushResult{Disposition: "dropped", Dropped: true}
			if err := insertPreviewRequest(ctx, tx, params, nil, result.Disposition); err != nil {
				return err
			}
			after, _ := json.Marshal(map[string]any{"branch": params.Branch, "commit_sha": params.CommitSHA, "disposition": result.Disposition})
			return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, "preview.push_dropped", "app", params.AppID, nil, after)
		}
		if repositoryID == nil || workflowID == nil || defaultBranch == nil {
			return ErrFailedPrecondition
		}
		if params.Branch == *defaultBranch {
			return ErrInvalidArgument
		}

		var environmentID string
		err := tx.QueryRow(ctx, `SELECT id::text FROM environments
			WHERE org_id=$1 AND app_id=$2 AND branch=$3 AND kind='preview' AND torn_down_at IS NULL
			FOR UPDATE`, params.OrgID, params.AppID, params.Branch).Scan(&environmentID)
		created := false
		if errors.Is(err, pgx.ErrNoRows) {
			var active int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM environments
				WHERE org_id=$1 AND app_id=$2 AND kind='preview' AND torn_down_at IS NULL`, params.OrgID, params.AppID).Scan(&active); err != nil {
				return err
			}
			if active >= cap {
				return ErrFailedPrecondition
			}
			for _, identity := range params.Identities {
				err = tx.QueryRow(ctx, `INSERT INTO environments
					(id,org_id,app_id,branch,kind,preview_slug,namespace,planned_hostname,lifecycle_status,pending_commit_sha)
					VALUES($1,$2,$3,$4,'preview',$5,$6,NULL,'reserved',NULL)
					ON CONFLICT DO NOTHING RETURNING id::text`, identity.EnvironmentID, params.OrgID,
					params.AppID, params.Branch, identity.Slug, identity.Namespace).Scan(&environmentID)
				if err == nil {
					created = true
					break
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
			if environmentID == "" {
				return ErrAlreadyExists
			}
			if _, err := tx.Exec(ctx, `INSERT INTO environment_lifecycle_jobs
				(org_id,environment_id,kind,status,checkpoint,reason)
				VALUES($1,$2,'provision','queued','reserved','preview_push')`, params.OrgID, environmentID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		var lifecycleStatus string
		var activeRelease *string
		if err := tx.QueryRow(ctx, `SELECT lifecycle_status,in_progress_release_id::text
			FROM environments WHERE org_id=$1 AND id=$2 FOR UPDATE`, params.OrgID, environmentID).
			Scan(&lifecycleStatus, &activeRelease); err != nil {
			return err
		}
		if lifecycleStatus == "teardown_requested" || lifecycleStatus == "torn_down" || lifecycleStatus == "failed" {
			return ErrConflict
		}
		if activeRelease != nil {
			if _, err := tx.Exec(ctx, `UPDATE environments SET pending_commit_sha=$3
				WHERE org_id=$1 AND id=$2`, params.OrgID, environmentID, params.CommitSHA); err != nil {
				return err
			}
			result.Disposition = "coalesced"
		} else {
			jobParams := EnqueueOrchestrationParams{ID: params.OrchestrationJobID, OrgID: params.OrgID,
				RepositoryID: *repositoryID, WorkflowID: *workflowID, GitRef: "refs/heads/" + params.Branch,
				EnvironmentID: environmentID, CommitSHA: params.CommitSHA, IdempotencyKey: params.IdempotencyKey}
			if _, err := enqueueOrCoalesceOrchestrationTx(ctx, tx, jobParams); err != nil {
				return err
			}
			if created {
				result.Disposition = "reserved"
			} else {
				result.Disposition = "queued"
			}
		}
		result.Environment, err = getEnvironmentTx(ctx, tx, params.OrgID, environmentID)
		if err != nil {
			return err
		}
		if err := insertPreviewRequest(ctx, tx, params, &environmentID, result.Disposition); err != nil {
			return err
		}
		after, _ := json.Marshal(result.Environment)
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, "preview.push_accepted", "environment", environmentID, nil, after)
	})
	if err != nil {
		return PreviewPushResult{}, fmt.Errorf("accept preview push: %w", err)
	}
	return result, nil
}

type PreviewTeardownParams struct {
	OrgID, AppID, Branch, EnvironmentID, IdempotencyKey, EventKind, Reason string
	Audit                                                                  *AuditEvent
}

func (s *Store) RequestPreviewTeardown(ctx context.Context, params PreviewTeardownParams) (Environment, error) {
	var environment Environment
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var result PreviewPushResult
		if err := replayPreviewRequest(ctx, tx, params.OrgID, params.IdempotencyKey, params.EventKind, params.AppID, params.Branch, "", params.EnvironmentID, params.Reason, &result); err != nil {
			return err
		}
		if result.Disposition != "" {
			environment = result.Environment
			return nil
		}
		query := `SELECT id::text FROM environments WHERE org_id=$1 AND app_id=$2 AND kind='preview' AND torn_down_at IS NULL`
		args := []any{params.OrgID, params.AppID}
		if params.EnvironmentID != "" {
			query += ` AND id=$3`
			args = append(args, params.EnvironmentID)
		} else {
			query += ` AND branch=$3`
			args = append(args, params.Branch)
		}
		query += ` FOR UPDATE`
		var environmentID string
		if err := tx.QueryRow(ctx, query, args...).Scan(&environmentID); err != nil {
			return mapMutationError("lock preview teardown", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE environments SET lifecycle_status='teardown_requested',
			lifecycle_reason=$3,teardown_requested_at=COALESCE(teardown_requested_at,now()),pending_commit_sha=NULL
			WHERE org_id=$1 AND id=$2`, params.OrgID, environmentID, params.Reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE orchestration_jobs SET
			status=CASE WHEN status IN ('queued','leased') THEN 'cancelled' ELSE status END,
			pending_commit_sha=NULL,pending_idempotency_key=NULL,
			lease_owner=CASE WHEN status IN ('queued','leased') THEN NULL ELSE lease_owner END,
			lease_expires_at=CASE WHEN status IN ('queued','leased') THEN NULL ELSE lease_expires_at END,
			terminal_error_code=CASE WHEN status IN ('queued','leased') THEN 'environment_teardown_requested' ELSE terminal_error_code END,
			terminal_error_message=CASE WHEN status IN ('queued','leased') THEN 'preview teardown was requested' ELSE terminal_error_message END,
			terminal_at=CASE WHEN status IN ('queued','leased') THEN now() ELSE terminal_at END,updated_at=now()
			WHERE org_id=$1 AND environment_id=$2 AND status IN ('queued','leased','dispatch_reconciling','dispatched')`, params.OrgID, environmentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_lifecycle_jobs SET status='failed',completed_at=now(),
			error_code='environment_teardown_requested',error_message='preview teardown superseded provisioning',
			lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
			WHERE org_id=$1 AND environment_id=$2 AND kind='provision' AND status IN ('queued','leased','blocked')`, params.OrgID, environmentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_lifecycle_jobs
			(org_id,environment_id,kind,status,checkpoint,reason)
			VALUES($1,$2,'teardown','queued','requested',$3)
			ON CONFLICT (environment_id,kind) WHERE status IN ('queued','leased','blocked')
			DO UPDATE SET reason=EXCLUDED.reason,updated_at=now()`, params.OrgID, environmentID, params.Reason); err != nil {
			return err
		}
		var err error
		environment, err = getEnvironmentTx(ctx, tx, params.OrgID, environmentID)
		if err != nil {
			return err
		}
		push := PreviewPushParams{OrgID: params.OrgID, AppID: params.AppID, Branch: environment.Branch,
			IdempotencyKey: params.IdempotencyKey}
		if err := insertPreviewRequestKind(ctx, tx, push, &environmentID, "teardown_requested", params.EventKind, params.EnvironmentID, params.Reason); err != nil {
			return err
		}
		after, _ := json.Marshal(environment)
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, "preview.teardown_requested", "environment", environmentID, nil, after)
	})
	return environment, err
}

type IdlePreviewCandidate struct{ OrgID, EnvironmentID string }

func (s *Store) ListIdlePreviewCandidates(ctx context.Context, limit int) ([]IdlePreviewCandidate, error) {
	rows, err := s.db.Query(ctx, `SELECT environment.org_id::text,environment.id::text
		FROM environments environment JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		WHERE environment.kind='preview' AND environment.lifecycle_status IN ('reserved','policy_blocked','ready','failed')
		  AND GREATEST(environment.created_at,COALESCE((SELECT max(release.created_at) FROM releases release
		    WHERE release.org_id=environment.org_id AND release.environment_id=environment.id),environment.created_at))
		    <= now()-make_interval(hours=>app.preview_idle_ttl_hours)
		ORDER BY environment.created_at,environment.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []IdlePreviewCandidate
	for rows.Next() {
		var candidate IdlePreviewCandidate
		if err := rows.Scan(&candidate.OrgID, &candidate.EnvironmentID); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func replayPreviewRequest(ctx context.Context, tx pgx.Tx, orgID, key, kind, appID, branch, commit, requestedEnvironmentID, reason string, result *PreviewPushResult) error {
	var storedKind, storedAppID, storedBranch string
	var storedCommit, environmentID, storedRequestedEnvironmentID, storedReason *string
	err := tx.QueryRow(ctx, `SELECT event_kind,app_id::text,branch,commit_sha,environment_id::text,
		requested_environment_id::text,reason,disposition
		FROM preview_lifecycle_requests WHERE org_id=$1 AND idempotency_key=$2`, orgID, key).
		Scan(&storedKind, &storedAppID, &storedBranch, &storedCommit, &environmentID,
			&storedRequestedEnvironmentID, &storedReason, &result.Disposition)
	if errors.Is(err, pgx.ErrNoRows) {
		result.Disposition = ""
		return nil
	}
	if err != nil {
		return err
	}
	if storedKind != kind || storedAppID != appID || storedBranch != branch || stringPointerValue(storedCommit) != commit ||
		stringPointerValue(storedRequestedEnvironmentID) != requestedEnvironmentID || stringPointerValue(storedReason) != reason {
		return ErrConflict
	}
	result.Dropped = result.Disposition == "dropped"
	if environmentID != nil {
		var err error
		result.Environment, err = getEnvironmentTx(ctx, tx, orgID, *environmentID)
		return err
	}
	return nil
}

func insertPreviewRequest(ctx context.Context, tx pgx.Tx, params PreviewPushParams, environmentID *string, disposition string) error {
	eventKind := "teardown"
	if params.CommitSHA != "" {
		eventKind = "push"
	}
	return insertPreviewRequestKind(ctx, tx, params, environmentID, disposition, eventKind, "", "")
}

func insertPreviewRequestKind(ctx context.Context, tx pgx.Tx, params PreviewPushParams, environmentID *string, disposition, eventKind, requestedEnvironmentID, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO preview_lifecycle_requests
		(org_id,idempotency_key,event_kind,app_id,branch,commit_sha,environment_id,requested_environment_id,reason,disposition)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,NULLIF($8,'')::uuid,NULLIF($9,''),$10)`, params.OrgID,
		params.IdempotencyKey, eventKind, params.AppID, params.Branch, params.CommitSHA, environmentID,
		requestedEnvironmentID, reason, disposition)
	return err
}

func getEnvironmentTx(ctx context.Context, tx pgx.Tx, orgID, environmentID string) (Environment, error) {
	var environment Environment
	err := scanEnvironment(tx.QueryRow(ctx, environmentSelect+` WHERE environment.org_id=$1 AND environment.id=$2`, orgID, environmentID), &environment)
	return environment, err
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

var _ = time.Second
