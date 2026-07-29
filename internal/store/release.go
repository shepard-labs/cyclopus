package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type FinalizeReleaseParams struct {
	Lease    DeployLease
	Hostname string
	Audit    *AuditEvent
}

type FinalizeReleaseResult struct {
	PendingCommitSHA *string
}

func (s *Store) FinalizeReleaseLive(ctx context.Context, params FinalizeReleaseParams) (FinalizeReleaseResult, error) {
	var result FinalizeReleaseResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		before, err := lockOwnedDeployLease(ctx, tx, params.Lease)
		if err != nil {
			return err
		}

		commandTag, err := tx.Exec(ctx, `
			UPDATE releases
			SET status = 'superseded'
			WHERE org_id = $1 AND environment_id = $2 AND id <> $3 AND status = 'live'`,
			params.Lease.OrgID, params.Lease.EnvironmentID, params.Lease.ReleaseID)
		if err != nil {
			return fmt.Errorf("supersede previous live release: %w", err)
		}
		_ = commandTag

		commandTag, err = tx.Exec(ctx, `
			UPDATE releases
			SET status = 'live', failure_reason = NULL
			WHERE org_id = $1 AND environment_id = $2 AND id = $3
			  AND status IN ('pending', 'deploying')`, params.Lease.OrgID,
			params.Lease.EnvironmentID, params.Lease.ReleaseID)
		if err != nil {
			return fmt.Errorf("mark release live: %w", err)
		}
		if err := rowsAffected(commandTag, ErrConflict); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environments AS environment
			SET current_env_var_revision_id = release.env_var_revision_id
			FROM releases AS release
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND release.org_id = environment.org_id AND release.environment_id = environment.id
			  AND release.id = $3`, params.Lease.OrgID, params.Lease.EnvironmentID,
			params.Lease.ReleaseID); err != nil {
			return fmt.Errorf("publish release env-var revision: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO aliases (org_id, environment_id, hostname, release_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (environment_id) DO UPDATE
			SET hostname = EXCLUDED.hostname, release_id = EXCLUDED.release_id, updated_at = now()
			WHERE aliases.org_id = EXCLUDED.org_id`, params.Lease.OrgID,
			params.Lease.EnvironmentID, params.Hostname, params.Lease.ReleaseID); err != nil {
			return fmt.Errorf("update release alias: %w", err)
		}

		if err := takePendingAndReleaseLease(ctx, tx, params.Lease, &result.PendingCommitSHA); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"status": "live", "hostname": params.Hostname})
		return insertAuditEvent(ctx, tx, params.Lease.OrgID, params.Audit,
			"release.finalized_live", "release", params.Lease.ReleaseID, before, after)
	})
	return result, err
}

func (s *Store) AdoptRecoveredReleaseLive(ctx context.Context, lease DeployLease) (FinalizeReleaseResult, error) {
	var result FinalizeReleaseResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		before, err := lockOwnedDeployLease(ctx, tx, lease)
		if err != nil {
			return err
		}
		var checkpoint string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(deploy_recovery_checkpoint, '') FROM environments
			WHERE org_id = $1 AND id = $2 FOR UPDATE`, lease.OrgID, lease.EnvironmentID).Scan(&checkpoint); err != nil {
			return fmt.Errorf("read late-ready checkpoint: %w", err)
		}
		if checkpoint != "late_ready_observed" {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE releases SET status = 'superseded'
			WHERE org_id = $1 AND environment_id = $2 AND id <> $3 AND status = 'live'`,
			lease.OrgID, lease.EnvironmentID, lease.ReleaseID); err != nil {
			return fmt.Errorf("supersede release during recovery adoption: %w", err)
		}
		commandTag, err := tx.Exec(ctx, `UPDATE releases SET status = 'live', failure_reason = NULL
			WHERE org_id = $1 AND environment_id = $2 AND id = $3 AND status IN ('pending', 'deploying')`,
			lease.OrgID, lease.EnvironmentID, lease.ReleaseID)
		if err != nil {
			return fmt.Errorf("adopt recovered release live: %w", err)
		}
		if err := rowsAffected(commandTag, ErrConflict); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO aliases (org_id, environment_id, hostname, release_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (environment_id) DO UPDATE
			SET hostname = EXCLUDED.hostname, release_id = EXCLUDED.release_id, updated_at = now()
			WHERE aliases.org_id = EXCLUDED.org_id`, lease.OrgID, lease.EnvironmentID,
			lease.PlannedHostname, lease.ReleaseID); err != nil {
			return fmt.Errorf("adopt recovered release alias: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE environments AS environment
			SET current_env_var_revision_id = release.env_var_revision_id
			FROM releases AS release WHERE environment.org_id = $1 AND environment.id = $2
			  AND release.org_id = environment.org_id AND release.environment_id = environment.id
			  AND release.id = $3`, lease.OrgID, lease.EnvironmentID, lease.ReleaseID); err != nil {
			return fmt.Errorf("adopt recovered env-var revision: %w", err)
		}
		if err := takePendingAndReleaseLease(ctx, tx, lease, &result.PendingCommitSHA); err != nil {
			return err
		}
		if result.PendingCommitSHA != nil {
			if err := promotePendingCommit(ctx, tx, lease, *result.PendingCommitSHA); err != nil {
				return err
			}
		}
		after, _ := json.Marshal(map[string]any{"status": "live", "recovery": "late_ready_adopted"})
		_, err = tx.Exec(ctx, `INSERT INTO audit_events
			(id, org_id, action, target_type, target_id, before, after)
			VALUES (uuidv7(), $1, 'release.recovery_adopted_live', 'release', $2, $3, $4)`,
			lease.OrgID, lease.ReleaseID, before, after)
		return err
	})
	return result, err
}

type FailReleaseParams struct {
	Lease         DeployLease
	FailureReason string
	Audit         *AuditEvent
}

func (s *Store) FinalizeReleaseFailed(ctx context.Context, params FailReleaseParams) (FinalizeReleaseResult, error) {
	return s.finalizeReleaseFailed(ctx, params, false)
}

func (s *Store) FinalizeRecoveredReleaseFailed(ctx context.Context, lease DeployLease, failureReason string) (FinalizeReleaseResult, error) {
	return s.finalizeReleaseFailed(ctx, FailReleaseParams{Lease: lease, FailureReason: failureReason}, true)
}

func (s *Store) finalizeReleaseFailed(ctx context.Context, params FailReleaseParams, recovery bool) (FinalizeReleaseResult, error) {
	var result FinalizeReleaseResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		before, err := lockOwnedDeployLease(ctx, tx, params.Lease)
		if err != nil {
			return err
		}
		if recovery && params.Lease.PreviousRelease == nil {
			var checkpoint string
			if err := tx.QueryRow(ctx, `SELECT COALESCE(deploy_recovery_checkpoint, '') FROM environments
				WHERE org_id = $1 AND id = $2`, params.Lease.OrgID, params.Lease.EnvironmentID).Scan(&checkpoint); err != nil {
				return fmt.Errorf("read first-deploy recovery checkpoint: %w", err)
			}
			if checkpoint != "first_deployment_missing" && checkpoint != "claimed" {
				return ErrConflict
			}
		}
		commandTag, err := tx.Exec(ctx, `
			UPDATE releases
			SET status = 'failed', failure_reason = $4
			WHERE org_id = $1 AND environment_id = $2 AND id = $3
			  AND status IN ('pending', 'deploying')`, params.Lease.OrgID,
			params.Lease.EnvironmentID, params.Lease.ReleaseID, params.FailureReason)
		if err != nil {
			return fmt.Errorf("mark release failed: %w", err)
		}
		if err := rowsAffected(commandTag, ErrConflict); err != nil {
			return err
		}
		if recovery && params.Lease.PreviousRelease != nil && params.Lease.FailedRelease.Kind == "env_change" {
			if _, err := tx.Exec(ctx, `UPDATE environments SET current_env_var_revision_id = $3
				WHERE org_id = $1 AND id = $2`, params.Lease.OrgID, params.Lease.EnvironmentID,
				params.Lease.PreviousRelease.EnvVarRevisionID); err != nil {
				return fmt.Errorf("restore previous env-var revision: %w", err)
			}
		}
		if err := takePendingAndReleaseLease(ctx, tx, params.Lease, &result.PendingCommitSHA); err != nil {
			return err
		}
		if recovery && result.PendingCommitSHA != nil {
			if err := promotePendingCommit(ctx, tx, params.Lease, *result.PendingCommitSHA); err != nil {
				return err
			}
		}
		after, _ := json.Marshal(map[string]any{"status": "failed", "failure_reason": params.FailureReason})
		if recovery {
			_, err = tx.Exec(ctx, `INSERT INTO audit_events
				(id, org_id, action, target_type, target_id, before, after)
				VALUES (uuidv7(), $1, 'release.recovery_finalized_failed', 'release', $2, $3, $4)`,
				params.Lease.OrgID, params.Lease.ReleaseID, before, after)
			return err
		}
		return insertAuditEvent(ctx, tx, params.Lease.OrgID, params.Audit,
			"release.finalized_failed", "release", params.Lease.ReleaseID, before, after)
	})
	return result, err
}

func promotePendingCommit(ctx context.Context, tx pgx.Tx, lease DeployLease, commitSHA string) error {
	var jobID string
	err := tx.QueryRow(ctx, `
		WITH source AS (
			SELECT repository_id, workflow_id, git_ref
			FROM orchestration_jobs
			WHERE org_id = $1 AND environment_id = $2
			ORDER BY created_at DESC, id DESC LIMIT 1
		), inserted AS (
			INSERT INTO orchestration_jobs (
				id, org_id, repository_id, workflow_id, git_ref, environment_id,
				commit_sha, idempotency_key, status
			)
			SELECT uuidv7(), $1, repository_id, workflow_id, git_ref, $2, $3,
				'recovery:' || $4 || ':' || $3, 'queued' FROM source
			ON CONFLICT (org_id, repository_id, workflow_id, git_ref, environment_id)
				WHERE status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched')
			DO UPDATE SET
				commit_sha = CASE WHEN orchestration_jobs.status = 'queued' THEN EXCLUDED.commit_sha ELSE orchestration_jobs.commit_sha END,
				idempotency_key = CASE WHEN orchestration_jobs.status = 'queued' THEN EXCLUDED.idempotency_key ELSE orchestration_jobs.idempotency_key END,
				pending_commit_sha = CASE WHEN orchestration_jobs.status = 'queued' THEN NULL ELSE EXCLUDED.commit_sha END,
				pending_idempotency_key = CASE WHEN orchestration_jobs.status = 'queued' THEN NULL ELSE EXCLUDED.idempotency_key END,
				updated_at = now()
			RETURNING id::text
		)
		SELECT id FROM inserted`, lease.OrgID, lease.EnvironmentID, commitSHA, lease.ReleaseID).Scan(&jobID)
	if err == pgx.ErrNoRows {
		return fmt.Errorf("promote pending commit: %w: no orchestration route for environment", ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("promote pending commit: %w", err)
	}
	return nil
}

func lockOwnedDeployLease(ctx context.Context, tx pgx.Tx, lease DeployLease) (json.RawMessage, error) {
	var before json.RawMessage
	err := tx.QueryRow(ctx, `
		SELECT jsonb_build_object('status', release.status, 'failure_reason', release.failure_reason)
		FROM environments AS environment
		JOIN apps AS app ON app.org_id = environment.org_id AND app.id = environment.app_id
		JOIN projects AS project ON project.org_id = app.org_id AND project.id = app.project_id
		JOIN orgs AS org ON org.id = environment.org_id
		JOIN releases AS release ON release.org_id = environment.org_id
		  AND release.environment_id = environment.id AND release.id = environment.in_progress_release_id
		WHERE environment.org_id = $1 AND environment.id = $2
		  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
		  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
		  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
		  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
		FOR UPDATE OF environment, release`, lease.OrgID, lease.EnvironmentID, lease.ReleaseID, lease.Fence).Scan(&before)
	if err == pgx.ErrNoRows {
		return nil, ErrLeaseNotOwned
	}
	if err != nil {
		return nil, fmt.Errorf("lock deploy lease: %w", err)
	}
	return before, nil
}

func takePendingAndReleaseLease(ctx context.Context, tx pgx.Tx, lease DeployLease, pending **string) error {
	err := tx.QueryRow(ctx, `
		UPDATE environments AS environment
		SET pending_commit_sha = NULL, in_progress_release_id = NULL, lock_acquired_at = NULL,
			deploy_recovery_checkpoint = NULL
		FROM apps AS app, projects AS project, orgs AS org
		WHERE environment.org_id = $1 AND environment.id = $2
		  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
		  AND app.org_id = environment.org_id AND app.id = environment.app_id
		  AND project.org_id = app.org_id AND project.id = app.project_id
		  AND org.id = environment.org_id
		  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
		  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
		  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
		RETURNING WITH (OLD AS old_row) old_row.pending_commit_sha`, lease.OrgID,
		lease.EnvironmentID, lease.ReleaseID, lease.Fence).Scan(pending)
	if err == pgx.ErrNoRows {
		return ErrLeaseNotOwned
	}
	if err != nil {
		return fmt.Errorf("finalize deploy lease: %w", err)
	}
	return nil
}
