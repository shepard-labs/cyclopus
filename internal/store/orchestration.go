package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	orchestrationDispatchObservationDeadline = 5 * time.Minute
	orchestrationRunDeadline                 = 45 * time.Minute
)

type OrchestrationJob struct {
	ID                     string
	OrgID                  string
	IdempotencyKey         string
	RepositoryID           string
	WorkflowID             string
	GitRef                 string
	EnvironmentID          string
	CommitSHA              string
	PendingCommitSHA       *string
	PendingIdempotencyKey  *string
	Status                 string
	LeaseExpiresAt         *time.Time
	DispatchRequestedAt    *time.Time
	DispatchDeadlineAt     *time.Time
	RunDeadlineAt          *time.Time
	GitHubRunID            *int64
	ReconciliationLeaseEnd *time.Time
	InstallationID         int64
	RepositoryFullName     string
	Attempt                int
}

type GitHubRecoveryJob = OrchestrationJob

type EnqueueOrchestrationParams struct {
	ID             string
	OrgID          string
	RepositoryID   string
	WorkflowID     string
	GitRef         string
	EnvironmentID  string
	CommitSHA      string
	IdempotencyKey string
}

func (s *Store) RequeueExpiredOrchestrationLeases(ctx context.Context) (int64, error) {
	var count int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		commandTag, err := tx.Exec(ctx, `
			UPDATE orchestration_jobs AS job
			SET status = 'queued', lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
			FROM environments AS environment
			JOIN apps AS app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects AS project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs AS org ON org.id = environment.org_id
			WHERE job.org_id = environment.org_id AND job.environment_id = environment.id
			  AND job.status = 'leased' AND job.lease_expires_at <= now()
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL`)
		if err != nil {
			return fmt.Errorf("requeue expired orchestration leases: %w", err)
		}
		count = commandTag.RowsAffected()
		return nil
	})
	return count, err
}

func (s *Store) ListOrchestrationRecoveryOrgIDs(ctx context.Context) ([]string, error) {
	var orgIDs []string
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT job.org_id::text
			FROM orchestration_jobs job
			JOIN environments environment ON environment.org_id = job.org_id AND environment.id = job.environment_id
			JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs org ON org.id = job.org_id
			WHERE job.status IN ('dispatch_reconciling', 'dispatched')
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			  AND (reconciliation_lease_expires_at IS NULL OR reconciliation_lease_expires_at <= now())
			  AND (reconcile_after <= now() OR run_deadline_at <= now())
			ORDER BY job.org_id::text`)
		if err != nil {
			return fmt.Errorf("list orchestration recovery organizations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var orgID string
			if err := rows.Scan(&orgID); err != nil {
				return fmt.Errorf("scan orchestration recovery organization: %w", err)
			}
			orgIDs = append(orgIDs, orgID)
		}
		return rows.Err()
	})
	return orgIDs, err
}

func (s *Store) ListOrchestrationOrgIDs(ctx context.Context) ([]string, error) {
	var orgIDs []string
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT job.org_id::text
			FROM orchestration_jobs job
			JOIN environments environment ON environment.org_id = job.org_id AND environment.id = job.environment_id
			JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs org ON org.id = job.org_id
			WHERE job.status = 'queued' AND (job.reconcile_after IS NULL OR job.reconcile_after <= now())
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			ORDER BY job.org_id::text`)
		if err != nil {
			return fmt.Errorf("list orchestration organizations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var orgID string
			if err := rows.Scan(&orgID); err != nil {
				return fmt.Errorf("scan orchestration organization: %w", err)
			}
			orgIDs = append(orgIDs, orgID)
		}
		return rows.Err()
	})
	return orgIDs, err
}

func (s *Store) EnqueueOrCoalesceOrchestration(ctx context.Context, params EnqueueOrchestrationParams) (OrchestrationJob, error) {
	var job OrchestrationJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, params.OrgID); err != nil {
			return fmt.Errorf("lock orchestration tombstone boundary: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1330205774))`, params.OrgID); err != nil {
			return fmt.Errorf("lock orchestration idempotency namespace: %w", err)
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM environments environment
			JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs org ON org.id = environment.org_id
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			FOR KEY SHARE OF environment, app, project, org)`, params.OrgID, params.EnvironmentID).Scan(&active); err != nil {
			return fmt.Errorf("validate orchestration scope: %w", err)
		}
		if !active {
			return ErrConflict
		}
		var historical OrchestrationJob
		err := tx.QueryRow(ctx, `
			SELECT job.id::text, job.org_id::text, request.idempotency_key, request.repository_id::text,
				request.workflow_id, request.git_ref, request.environment_id::text, request.commit_sha,
				job.pending_commit_sha, job.pending_idempotency_key, job.status
			FROM orchestration_requests AS request
			JOIN orchestration_jobs AS job
			  ON job.org_id = request.org_id AND job.id = request.orchestration_job_id
			WHERE request.org_id = $1 AND request.idempotency_key = $2`,
			params.OrgID, params.IdempotencyKey).Scan(&historical.ID, &historical.OrgID,
			&historical.IdempotencyKey, &historical.RepositoryID, &historical.WorkflowID,
			&historical.GitRef, &historical.EnvironmentID, &historical.CommitSHA,
			&historical.PendingCommitSHA, &historical.PendingIdempotencyKey, &historical.Status)
		if err == nil {
			if historical.RepositoryID != params.RepositoryID || historical.WorkflowID != params.WorkflowID ||
				historical.GitRef != params.GitRef || historical.EnvironmentID != params.EnvironmentID ||
				historical.CommitSHA != params.CommitSHA {
				return ErrConflict
			}
			job = historical
			return nil
		}
		if err != pgx.ErrNoRows {
			return fmt.Errorf("read historical orchestration key: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO orchestration_jobs (
				id, org_id, repository_id, workflow_id, git_ref, environment_id,
				commit_sha, idempotency_key, status
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued')
			ON CONFLICT (org_id, repository_id, workflow_id, git_ref, environment_id)
				WHERE status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched')
			DO UPDATE SET
				commit_sha = CASE WHEN orchestration_jobs.status = 'queued' THEN EXCLUDED.commit_sha ELSE orchestration_jobs.commit_sha END,
				idempotency_key = CASE WHEN orchestration_jobs.status = 'queued' THEN EXCLUDED.idempotency_key ELSE orchestration_jobs.idempotency_key END,
				pending_commit_sha = CASE WHEN orchestration_jobs.status = 'queued' THEN NULL ELSE EXCLUDED.commit_sha END,
				pending_idempotency_key = CASE WHEN orchestration_jobs.status = 'queued' THEN NULL ELSE EXCLUDED.idempotency_key END,
				updated_at = now()
			RETURNING id::text, org_id::text, idempotency_key, repository_id::text,
				workflow_id, git_ref, environment_id::text, commit_sha, pending_commit_sha,
				pending_idempotency_key, status`,
			params.ID, params.OrgID, params.RepositoryID, params.WorkflowID, params.GitRef,
			params.EnvironmentID, params.CommitSHA, params.IdempotencyKey).Scan(&job.ID,
			&job.OrgID, &job.IdempotencyKey, &job.RepositoryID, &job.WorkflowID, &job.GitRef,
			&job.EnvironmentID, &job.CommitSHA, &job.PendingCommitSHA, &job.PendingIdempotencyKey,
			&job.Status); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO orchestration_requests (
				org_id, idempotency_key, orchestration_job_id, repository_id,
				workflow_id, git_ref, environment_id, commit_sha
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, params.OrgID,
			params.IdempotencyKey, job.ID, params.RepositoryID, params.WorkflowID,
			params.GitRef, params.EnvironmentID, params.CommitSHA)
		return err
	})
	if err != nil {
		return OrchestrationJob{}, fmt.Errorf("enqueue orchestration job: %w", err)
	}
	return job, nil
}

func (s *Store) ClaimOrchestration(ctx context.Context, orgID, owner string, leaseDuration time.Duration) (*OrchestrationJob, error) {
	if err := requirePositiveDuration("orchestration lease duration", leaseDuration); err != nil {
		return nil, err
	}
	var job OrchestrationJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock orchestration tombstone boundary: %w", err)
		}
		err := tx.QueryRow(ctx, `
			WITH candidate AS (
				SELECT job.id, repository.installation_id, repository.repo_full_name
				FROM orchestration_jobs job
				JOIN git_connections repository ON repository.org_id = job.org_id AND repository.id = job.repository_id
				JOIN environments environment ON environment.org_id = job.org_id AND environment.id = job.environment_id
				JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
				JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
				JOIN orgs org ON org.id = job.org_id
				WHERE job.org_id = $1 AND ((job.status = 'queued' AND (job.reconcile_after IS NULL OR job.reconcile_after <= now()))
				  OR (job.status = 'leased' AND job.lease_expires_at <= now()))
				  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
				  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
				ORDER BY job.created_at, job.id FOR UPDATE OF job SKIP LOCKED LIMIT 1
			)
			UPDATE orchestration_jobs AS job
			SET status = 'leased', lease_owner = $2,
				lease_expires_at = now() + make_interval(secs => $3),
				reconcile_after = NULL,
				github_run_status = NULL, github_run_conclusion = NULL,
				github_status_observed_at = NULL, terminal_error_code = NULL,
				terminal_error_message = NULL, terminal_at = NULL,
				attempt = attempt + 1, updated_at = now()
			FROM candidate WHERE job.id = candidate.id
			RETURNING job.id::text, job.org_id::text, job.idempotency_key,
				job.repository_id::text, job.workflow_id, job.git_ref,
				job.environment_id::text, job.commit_sha, job.status, job.lease_expires_at,
				candidate.installation_id, candidate.repo_full_name, job.attempt`,
			orgID, owner, leaseDuration.Seconds()).Scan(&job.ID, &job.OrgID,
			&job.IdempotencyKey, &job.RepositoryID, &job.WorkflowID, &job.GitRef,
			&job.EnvironmentID, &job.CommitSHA, &job.Status, &job.LeaseExpiresAt,
			&job.InstallationID, &job.RepositoryFullName, &job.Attempt)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("claim orchestration job: %w", err)
	}
	return &job, nil
}

func (s *Store) RescheduleOrchestration(ctx context.Context, orgID, jobID, owner string, delay time.Duration) error {
	if err := requirePositiveDuration("orchestration retry delay", delay); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		commandTag, err := tx.Exec(ctx, `
			UPDATE orchestration_jobs AS job
			SET status = 'queued', lease_owner = NULL, lease_expires_at = NULL,
				reconcile_after = now() + make_interval(secs => $4), updated_at = now()
			WHERE job.org_id = $1 AND job.id = $2 AND job.status = 'leased'
			  AND job.lease_owner = $3 AND job.lease_expires_at > now()`,
			orgID, jobID, owner, delay.Seconds())
		if err != nil {
			return fmt.Errorf("reschedule orchestration job: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) BeginDispatchReconciliation(ctx context.Context, orgID, jobID, owner string, reconcileDelay time.Duration) error {
	if err := requirePositiveDuration("dispatch reconciliation delay", reconcileDelay); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock orchestration tombstone boundary: %w", err)
		}
		commandTag, err := tx.Exec(ctx, `
			UPDATE orchestration_jobs AS job
			SET status = 'dispatch_reconciling', dispatch_requested_at = now(),
				reconcile_after = now() + make_interval(secs => $4),
				dispatch_deadline_at = COALESCE(dispatch_deadline_at, now() + make_interval(secs => $5)),
				lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
			FROM environments environment
			JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs org ON org.id = environment.org_id
			WHERE job.org_id = $1 AND job.id = $2 AND job.status = 'leased'
			  AND job.environment_id = environment.id AND job.lease_owner = $3 AND job.lease_expires_at > now()
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL`, orgID, jobID, owner,
			reconcileDelay.Seconds(), orchestrationDispatchObservationDeadline.Seconds())
		if err != nil {
			return fmt.Errorf("begin dispatch reconciliation: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) ClaimDispatchReconciliation(ctx context.Context, orgID, owner string, leaseDuration time.Duration) (*GitHubRecoveryJob, error) {
	if err := requirePositiveDuration("reconciliation lease duration", leaseDuration); err != nil {
		return nil, err
	}
	var job OrchestrationJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock orchestration tombstone boundary: %w", err)
		}
		err := tx.QueryRow(ctx, `
			WITH candidate AS (
				SELECT job.id, repository.installation_id, repository.repo_full_name
				FROM orchestration_jobs job
				JOIN git_connections repository ON repository.org_id = job.org_id AND repository.id = job.repository_id
				JOIN environments environment ON environment.org_id = job.org_id AND environment.id = job.environment_id
				JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
				JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
				JOIN orgs org ON org.id = job.org_id
				WHERE job.org_id = $1 AND job.status IN ('dispatch_reconciling', 'dispatched')
				  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
				  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
				  AND (reconciliation_lease_expires_at IS NULL OR reconciliation_lease_expires_at <= now())
				  AND (reconcile_after <= now() OR run_deadline_at <= now())
				ORDER BY LEAST(COALESCE(job.reconcile_after, 'infinity'), COALESCE(job.dispatch_deadline_at, 'infinity'), COALESCE(job.run_deadline_at, 'infinity')), job.id
				FOR UPDATE OF job SKIP LOCKED LIMIT 1
			)
			UPDATE orchestration_jobs AS job
			SET reconciliation_owner = $2,
				reconciliation_lease_expires_at = now() + make_interval(secs => $3), updated_at = now()
			FROM candidate WHERE job.id = candidate.id
			RETURNING job.id::text, job.org_id::text, job.status, job.workflow_id, job.commit_sha,
				job.pending_commit_sha, job.dispatch_requested_at, job.dispatch_deadline_at,
				job.run_deadline_at, job.github_run_id, job.reconciliation_lease_expires_at,
				candidate.installation_id, candidate.repo_full_name, job.git_ref, job.attempt`,
			orgID, owner, leaseDuration.Seconds()).Scan(&job.ID, &job.OrgID, &job.Status,
			&job.WorkflowID, &job.CommitSHA, &job.PendingCommitSHA, &job.DispatchRequestedAt,
			&job.DispatchDeadlineAt, &job.RunDeadlineAt, &job.GitHubRunID,
			&job.ReconciliationLeaseEnd, &job.InstallationID, &job.RepositoryFullName,
			&job.GitRef, &job.Attempt)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("claim dispatch reconciliation: %w", err)
	}
	return &job, nil
}

func (s *Store) RescheduleDispatchReconciliation(ctx context.Context, orgID, jobID, owner string, delay time.Duration) error {
	if err := requirePositiveDuration("dispatch reconciliation retry delay", delay); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		commandTag, err := tx.Exec(ctx, `
			UPDATE orchestration_jobs
			SET reconcile_after = LEAST(now() + make_interval(secs => $4),
				CASE status
					WHEN 'dispatch_reconciling' THEN dispatch_deadline_at
					WHEN 'dispatched' THEN run_deadline_at
				END),
				reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL, updated_at = now()
			WHERE org_id = $1 AND id = $2 AND status IN ('dispatch_reconciling', 'dispatched')
			  AND CASE status
					WHEN 'dispatch_reconciling' THEN dispatch_deadline_at
					WHEN 'dispatched' THEN run_deadline_at
				  END > now()
			  AND ((reconciliation_owner = $3 AND reconciliation_lease_expires_at > now())
			    OR (status = 'dispatch_reconciling' AND reconciliation_owner IS NULL))`,
			orgID, jobID, owner, delay.Seconds())
		if err != nil {
			return fmt.Errorf("reschedule dispatch reconciliation: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

type FailOrchestrationParams struct {
	OrgID   string
	JobID   string
	Owner   string
	Code    string
	Message string
}

func (s *Store) FailOrchestration(ctx context.Context, params FailOrchestrationParams) error {
	if params.Code == "" {
		return fmt.Errorf("%w: orchestration failure code is required", ErrInvalidArgument)
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		commandTag, err := tx.Exec(ctx, `
			WITH failed AS (
				INSERT INTO orchestration_job_run_evidence (
					orchestration_job_id, attempt, github_run_id, github_run_status,
					observed_at, terminal_error_code, terminal_error_message, terminal_at
				)
				SELECT id, attempt, github_run_id, 'failed', clock_timestamp(), $4, $5, now()
				FROM orchestration_jobs
				WHERE org_id = $1 AND id = $2 AND (
					(status = 'leased' AND lease_owner = $3 AND lease_expires_at > now())
					OR (status IN ('dispatch_reconciling', 'dispatched')
						AND reconciliation_owner = $3 AND reconciliation_lease_expires_at > now())
					OR (status = 'dispatch_reconciling' AND reconciliation_owner IS NULL)
				)
				RETURNING orchestration_job_id
			)
			UPDATE orchestration_jobs AS job
			SET status = CASE WHEN pending_commit_sha IS NOT NULL THEN 'queued' ELSE 'failed' END,
				commit_sha = COALESCE(pending_commit_sha, commit_sha),
				idempotency_key = COALESCE(pending_idempotency_key, idempotency_key),
				pending_commit_sha = NULL, pending_idempotency_key = NULL,
				lease_owner = NULL, lease_expires_at = NULL, dispatch_requested_at = NULL,
				reconcile_after = NULL, dispatch_deadline_at = NULL, run_deadline_at = NULL,
				reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
				github_dispatch_identity = NULL, github_run_id = NULL,
				github_run_status = 'failed', github_run_conclusion = NULL,
				github_status_observed_at = now(), terminal_error_code = $4,
				terminal_error_message = $5, terminal_at = now(), updated_at = now()
			FROM failed WHERE job.id = failed.orchestration_job_id`,
			params.OrgID, params.JobID, params.Owner, params.Code, params.Message)
		if err != nil {
			return fmt.Errorf("fail orchestration job: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) RecordDispatchedRun(ctx context.Context, orgID, jobID, owner, identity string, runID int64, runStatus string, pollDelay time.Duration) error {
	if err := requirePositiveDuration("orchestration poll delay", pollDelay); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock orchestration tombstone boundary: %w", err)
		}
		commandTag, err := tx.Exec(ctx, `
			UPDATE orchestration_jobs AS job
			SET status = 'dispatched', github_dispatch_identity = $4, github_run_id = $5,
				github_run_status = $6, github_run_conclusion = NULL,
				github_status_observed_at = now(), reconcile_after = now() + make_interval(secs => $7),
				run_deadline_at = now() + make_interval(secs => $8), reconciliation_owner = NULL,
				reconciliation_lease_expires_at = NULL, updated_at = now()
			FROM environments environment
			JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs org ON org.id = environment.org_id
			WHERE job.org_id = $1 AND job.id = $2 AND job.status = 'dispatch_reconciling'
			  AND job.environment_id = environment.id AND job.github_run_id IS NULL AND job.reconciliation_owner = $3
			  AND job.reconciliation_lease_expires_at > now() AND environment.torn_down_at IS NULL
			  AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND org.deleted_at IS NULL`, orgID, jobID, owner, identity,
			runID, runStatus, pollDelay.Seconds(), orchestrationRunDeadline.Seconds())
		if err != nil {
			return fmt.Errorf("record dispatched run: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) ContinueDispatchReconciliation(ctx context.Context, orgID, jobID, owner string, pollDelay time.Duration) error {
	if err := requirePositiveDuration("dispatch reconciliation poll delay", pollDelay); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock orchestration tombstone boundary: %w", err)
		}
		commandTag, err := tx.Exec(ctx, `
			UPDATE orchestration_jobs AS job
			SET reconcile_after = LEAST(
					now() + make_interval(secs => $4), dispatch_deadline_at),
				reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL, updated_at = now()
			FROM environments environment
			JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs org ON org.id = environment.org_id
			WHERE job.org_id = $1 AND job.id = $2 AND job.status = 'dispatch_reconciling'
			  AND job.environment_id = environment.id AND job.github_run_id IS NULL
			  AND job.reconcile_after <= now() AND job.dispatch_deadline_at > now()
			  AND job.reconciliation_owner = $3 AND job.reconciliation_lease_expires_at > now()
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL`,
			orgID, jobID, owner, pollDelay.Seconds())
		if err != nil {
			return fmt.Errorf("continue dispatch reconciliation: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) ContinueRunReconciliation(ctx context.Context, orgID, jobID, owner, runStatus string, runConclusion *string, pollDelay time.Duration) error {
	if err := requirePositiveDuration("orchestration poll delay", pollDelay); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgID); err != nil {
			return fmt.Errorf("lock orchestration tombstone boundary: %w", err)
		}
		commandTag, err := tx.Exec(ctx, `
			WITH evidence AS (
				INSERT INTO orchestration_job_run_evidence (
					orchestration_job_id, attempt, github_run_id, github_run_status,
					github_run_conclusion, observed_at
				)
				SELECT id, attempt, github_run_id, $4, $5, clock_timestamp()
				FROM orchestration_jobs
				WHERE org_id = $1 AND id = $2 AND status = 'dispatched'
				  AND reconciliation_owner = $3 AND reconciliation_lease_expires_at > now()
				  AND $4 IN ('queued', 'in_progress')
				  AND EXISTS (SELECT 1 FROM environments environment
					JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
					JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
					JOIN orgs org ON org.id = environment.org_id
					WHERE environment.org_id = orchestration_jobs.org_id AND environment.id = orchestration_jobs.environment_id
					  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
					  AND project.deleted_at IS NULL AND org.deleted_at IS NULL)
				RETURNING orchestration_job_id
			)
			UPDATE orchestration_jobs AS job
			SET github_run_status = $4, github_run_conclusion = $5,
				github_status_observed_at = now(), reconcile_after = now() + make_interval(secs => $6),
				reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL, updated_at = now()
			FROM evidence WHERE job.id = evidence.orchestration_job_id`, orgID, jobID, owner,
			runStatus, runConclusion, pollDelay.Seconds())
		if err != nil {
			return fmt.Errorf("record orchestration status: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) RecordOrchestrationStatus(ctx context.Context, orgID, jobID, owner, runStatus string, runConclusion *string, pollDelay time.Duration) error {
	return s.ContinueRunReconciliation(ctx, orgID, jobID, owner, runStatus, runConclusion, pollDelay)
}

type ResolveOrchestrationParams struct {
	OrgID           string
	JobID           string
	Owner           string
	RunStatus       string
	RunConclusion   string
	TerminalCode    *string
	TerminalMessage *string
}

func (s *Store) ResolveOrchestration(ctx context.Context, params ResolveOrchestrationParams) (OrchestrationJob, error) {
	if !validOrchestrationResolution(params.RunConclusion, params.TerminalCode) {
		return OrchestrationJob{}, fmt.Errorf("%w: invalid GitHub terminal conclusion or recovery code", ErrInvalidArgument)
	}
	var job OrchestrationJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, params.OrgID); err != nil {
			return fmt.Errorf("lock orchestration tombstone boundary: %w", err)
		}
		return tx.QueryRow(ctx, `
			WITH terminal_evidence AS (
				INSERT INTO orchestration_job_run_evidence (
					orchestration_job_id, attempt, github_run_id, github_run_status,
					github_run_conclusion, observed_at, terminal_error_code,
					terminal_error_message, terminal_at
				)
				SELECT id, attempt, github_run_id, $4, NULLIF($5, ''), clock_timestamp(), $6, $7, now()
				FROM orchestration_jobs
				WHERE org_id = $1 AND id = $2 AND status IN ('dispatch_reconciling', 'dispatched')
				  AND reconciliation_owner = $3 AND reconciliation_lease_expires_at > now()
				  AND (NULLIF($5, '') IN ('success', 'failure', 'neutral', 'cancelled', 'skipped',
						'timed_out', 'action_required', 'stale', 'startup_failure')
					OR (status = 'dispatch_reconciling' AND github_run_id IS NULL
						AND dispatch_deadline_at <= now() AND NULLIF($5, '') IS NULL
						AND $6 IN ('github_run_missing', 'github_run_ambiguous'))
					OR (status = 'dispatched' AND run_deadline_at <= now()
						AND NULLIF($5, '') IS NULL AND $6 = 'github_run_timed_out'))
				  AND EXISTS (SELECT 1 FROM environments environment
					JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
					JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
					JOIN orgs org ON org.id = environment.org_id
					WHERE environment.org_id = orchestration_jobs.org_id AND environment.id = orchestration_jobs.environment_id
					  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
					  AND project.deleted_at IS NULL AND org.deleted_at IS NULL)
				RETURNING orchestration_job_id
			)
			UPDATE orchestration_jobs AS job
			SET status = CASE WHEN pending_commit_sha IS NOT NULL THEN 'queued'
				WHEN $5 = 'success' THEN 'completed' ELSE 'failed' END,
				commit_sha = COALESCE(pending_commit_sha, commit_sha),
				idempotency_key = COALESCE(pending_idempotency_key, idempotency_key),
				pending_commit_sha = NULL, pending_idempotency_key = NULL,
				dispatch_requested_at = NULL, reconcile_after = NULL,
				dispatch_deadline_at = NULL, run_deadline_at = NULL,
				reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
				github_dispatch_identity = NULL, github_run_id = NULL,
				github_run_status = $4, github_run_conclusion = NULLIF($5, ''),
				github_status_observed_at = now(), terminal_error_code = $6,
				terminal_error_message = $7, terminal_at = now(), updated_at = now()
			FROM terminal_evidence WHERE job.id = terminal_evidence.orchestration_job_id
			RETURNING job.id::text, job.org_id::text, job.status, job.commit_sha, job.pending_commit_sha`,
			params.OrgID, params.JobID, params.Owner, params.RunStatus, params.RunConclusion,
			params.TerminalCode, params.TerminalMessage).Scan(&job.ID, &job.OrgID, &job.Status,
			&job.CommitSHA, &job.PendingCommitSHA)
	})
	if err == pgx.ErrNoRows {
		return OrchestrationJob{}, ErrLeaseNotOwned
	}
	if err != nil {
		return OrchestrationJob{}, fmt.Errorf("resolve orchestration job: %w", err)
	}
	return job, nil
}

func validOrchestrationResolution(conclusion string, terminalCode *string) bool {
	if conclusion == "" {
		return terminalCode != nil && (*terminalCode == "github_run_missing" || *terminalCode == "github_run_ambiguous" || *terminalCode == "github_run_timed_out")
	}
	switch conclusion {
	case "success", "failure", "neutral", "cancelled", "skipped", "timed_out",
		"action_required", "stale", "startup_failure":
		return true
	default:
		return false
	}
}
