package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	orchestrationDispatchObservationDeadline = 5 * time.Minute
	orchestrationRunDeadline                 = 45 * time.Minute
)

type OrchestrationJob struct {
	ID, OrgID, IdempotencyKey, RepositoryID, WorkflowID, GitRef, EnvironmentID, CommitSHA string
	AppID, WorkflowRevision, DispatchRefObservedSHA                                       string
	DesiredGeneration                                                                     int64
	PendingCommitSHA                                                                      *string
	PendingWorkflowID, PendingWorkflowRevision, PendingGitRef                             *string
	PendingDesiredGeneration                                                              *int64
	PendingIdempotencyKey                                                                 *string
	Status                                                                                string
	LeaseExpiresAt, DispatchRequestedAt, DispatchDeadlineAt, RunDeadlineAt                *time.Time
	GitHubRunID                                                                           *int64
	ReconciliationLeaseEnd                                                                *time.Time
	InstallationID                                                                        int64
	RepositoryFullName                                                                    string
	Attempt                                                                               int
	AcceptedGeneration                                                                    int64
	EnqueueResult                                                                         string
}

type GitHubRecoveryJob = OrchestrationJob

type EnqueueOrchestrationParams struct {
	OrgID, RepositoryID, WorkflowID, GitRef, EnvironmentID, CommitSHA, IdempotencyKey string
	WorkflowRevision                                                                  string
}

func scanEnqueuedJob(row rowScanner, job *OrchestrationJob) error {
	return row.Scan(&job.ID, &job.OrgID, &job.AppID, &job.EnvironmentID, &job.RepositoryID,
		&job.WorkflowID, &job.WorkflowRevision, &job.GitRef, &job.CommitSHA, &job.DesiredGeneration,
		&job.IdempotencyKey, &job.PendingWorkflowID, &job.PendingWorkflowRevision, &job.PendingGitRef,
		&job.PendingCommitSHA, &job.PendingDesiredGeneration,
		&job.PendingIdempotencyKey, &job.Status)
}

func (s *Store) RequeueExpiredOrchestrationLeases(ctx context.Context) (int64, error) {
	command, err := s.db.Exec(ctx, `UPDATE orchestration_jobs SET status='queued',lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
		WHERE status='leased' AND lease_expires_at<=now()`)
	if err != nil {
		return 0, fmt.Errorf("requeue expired orchestration leases: %w", err)
	}
	return command.RowsAffected(), nil
}

func (s *Store) ListOrchestrationRecoveryOrgIDs(ctx context.Context) ([]string, error) {
	return s.listOrchestrationOrgIDs(ctx, `status IN ('dispatch_reconciling','dispatched') AND
		(reconciliation_lease_expires_at IS NULL OR reconciliation_lease_expires_at<=now()) AND
		(reconcile_after<=now() OR dispatch_deadline_at<=now() OR run_deadline_at<=now())`)
}

func (s *Store) ListOrchestrationOrgIDs(ctx context.Context) ([]string, error) {
	return s.listOrchestrationOrgIDs(ctx, `status='queued'`)
}

func (s *Store) listOrchestrationOrgIDs(ctx context.Context, predicate string) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT job.org_id::text FROM orchestration_jobs job
		JOIN environments environment ON environment.org_id=job.org_id AND environment.id=job.environment_id
		WHERE (`+predicate+`) AND environment.lifecycle_status NOT IN ('policy_blocked','teardown_requested','torn_down','failed')
		ORDER BY job.org_id::text`)
	if err != nil {
		return nil, err
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

func (s *Store) EnqueueOrCoalesceOrchestration(ctx context.Context, params EnqueueOrchestrationParams) (OrchestrationJob, error) {
	var job OrchestrationJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		job, err = enqueueOrCoalesceOrchestrationTx(ctx, tx, params)
		return err
	})
	if err != nil {
		return OrchestrationJob{}, fmt.Errorf("enqueue orchestration job: %w", err)
	}
	return job, nil
}

func enqueueOrCoalesceOrchestrationTx(ctx context.Context, tx pgx.Tx, params EnqueueOrchestrationParams) (OrchestrationJob, error) {
	var job OrchestrationJob
	var appID, connectionID, workflowPath string
	var repositoryID int64
	err := tx.QueryRow(ctx, `SELECT app.id::text,connection.id::text,app.github_workflow_path,connection.github_repository_id
		FROM environments environment
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
		JOIN git_connections connection ON connection.org_id=project.org_id AND connection.id=project.git_connection_id
		JOIN github_repositories repository ON repository.org_id=connection.org_id AND repository.github_repository_id=connection.github_repository_id
		JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
		WHERE environment.org_id=$1 AND environment.id=$2 AND environment.torn_down_at IS NULL
		AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND connection.state='active'
		AND connection.access_state='active' AND repository.access_state='active' AND NOT repository.archived
		AND installation.access_state='active'`, params.OrgID, params.EnvironmentID).
		Scan(&appID, &connectionID, &workflowPath, &repositoryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return job, fmt.Errorf("resolve orchestration target: %w", ErrFailedPrecondition)
		}
		return job, fmt.Errorf("resolve orchestration target: %w", err)
	}
	if (params.RepositoryID != "" && params.RepositoryID != connectionID && params.RepositoryID != fmt.Sprint(repositoryID)) ||
		(params.WorkflowID != "" && params.WorkflowID != workflowPath) {
		return job, fmt.Errorf("resolve orchestration target: %w", ErrFailedPrecondition)
	}
	workflowRevision := params.WorkflowRevision
	if workflowRevision == "" {
		workflowRevision = params.CommitSHA
	}
	err = scanEnqueuedPush(tx.QueryRow(ctx, `SELECT result.orchestration_job_id::text,result.org_id::text,result.app_id::text,
		result.environment_id::text,result.github_repository_id::text,result.github_workflow_path,result.workflow_revision,
		result.dispatch_ref,result.desired_checkout_sha,result.desired_generation,result.idempotency_key,
		result.pending_github_workflow_path,result.pending_workflow_revision,result.pending_dispatch_ref,
		result.pending_checkout_sha,result.pending_desired_generation,result.pending_idempotency_key,
		result.orchestration_status,result.accepted_generation,result.enqueue_result
		FROM enqueue_orchestration_push($1,$2,$3,$4,$5,$6,$7,$8,$9) result`, params.OrgID, appID,
		params.EnvironmentID, repositoryID, workflowPath, workflowRevision, params.GitRef, params.CommitSHA,
		params.IdempotencyKey), &job)
	if err != nil {
		return job, mapEnqueueOrchestrationError(err)
	}
	return job, nil
}

func scanEnqueuedPush(row rowScanner, job *OrchestrationJob) error {
	return row.Scan(&job.ID, &job.OrgID, &job.AppID, &job.EnvironmentID, &job.RepositoryID,
		&job.WorkflowID, &job.WorkflowRevision, &job.GitRef, &job.CommitSHA, &job.DesiredGeneration,
		&job.IdempotencyKey, &job.PendingWorkflowID, &job.PendingWorkflowRevision, &job.PendingGitRef,
		&job.PendingCommitSHA, &job.PendingDesiredGeneration,
		&job.PendingIdempotencyKey, &job.Status, &job.AcceptedGeneration, &job.EnqueueResult)
}

func mapEnqueueOrchestrationError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("persist orchestration job: %w", err)
	}
	message := strings.ToLower(pgErr.Message)
	switch pgErr.Code {
	case "40001", "40P01":
		return fmt.Errorf("persist orchestration job: %w", err)
	case "23505":
		if strings.Contains(message, "idempotency") || strings.Contains(message, "different request") {
			return fmt.Errorf("persist orchestration job: %w", ErrConflict)
		}
	case "23503":
		return fmt.Errorf("persist orchestration job: %w", ErrFailedPrecondition)
	case "22003":
		return fmt.Errorf("persist orchestration job: generation exhausted: %w", ErrFailedPrecondition)
	case "22001", "22023", "22P02", "23502":
		return fmt.Errorf("persist orchestration job: %w", ErrInvalidArgument)
	case "23514":
		for _, fragment := range []string{"inactive", "tombstoned", "torn-down", "inaccessible", "stale", "incoherent", "does not exist", "does not match", "generation cannot exceed"} {
			if strings.Contains(message, fragment) {
				return fmt.Errorf("persist orchestration job: %w", ErrFailedPrecondition)
			}
		}
		return fmt.Errorf("persist orchestration job: %w", ErrInvalidArgument)
	}
	return fmt.Errorf("persist orchestration job: %w", err)
}

func (s *Store) ClaimOrchestration(ctx context.Context, orgID, owner string, leaseDuration time.Duration) (*OrchestrationJob, error) {
	if err := requirePositiveDuration("orchestration lease duration", leaseDuration); err != nil {
		return nil, err
	}
	return s.claimGitHubJob(ctx, orgID, owner, leaseDuration, false)
}

func (s *Store) ClaimDispatchReconciliation(ctx context.Context, orgID, owner string, leaseDuration time.Duration) (*GitHubRecoveryJob, error) {
	if err := requirePositiveDuration("reconciliation lease duration", leaseDuration); err != nil {
		return nil, err
	}
	return s.claimGitHubJob(ctx, orgID, owner, leaseDuration, true)
}

func (s *Store) claimGitHubJob(ctx context.Context, orgID, owner string, leaseDuration time.Duration, reconciliation bool) (*OrchestrationJob, error) {
	var job OrchestrationJob
	var dispatchRefObservedSHA *string
	statusPredicate := `job.status='queued' OR (job.status='leased' AND job.lease_expires_at<=now())`
	assignment := `status='leased',lease_owner=$2,lease_expires_at=now()+make_interval(secs=>$3),
		dispatch_requested_at=NULL,dispatch_ref_observed_sha=NULL,dispatch_ambiguity_state='none',reconcile_after=NULL,
		dispatch_deadline_at=NULL,run_deadline_at=NULL,reconciliation_owner=NULL,reconciliation_lease_expires_at=NULL,
		github_dispatch_identity=NULL,github_run_id=NULL,github_run_head_sha=NULL,github_run_status=NULL,
		github_run_conclusion=NULL,github_status_observed_at=NULL,cancellation_state='not_requested',
		cancellation_requested_at=NULL,superseded_by_job_id=NULL,superseded_at=NULL,oci_resolution_state='pending',
		image_tag_ref=NULL,resolved_image_digest=NULL,immutable_image_ref=NULL,oci_media_type=NULL,oci_resolved_at=NULL,
		deployment_state='not_started',deployment_claim_owner=NULL,deployment_claim_expires_at=NULL,release_id=NULL,
		terminal_error_code=NULL,terminal_error_message=NULL,terminal_at=NULL,attempt=attempt+1`
	if reconciliation {
		statusPredicate = `job.status IN ('dispatch_reconciling','dispatched') AND (job.reconciliation_lease_expires_at IS NULL OR job.reconciliation_lease_expires_at<=now()) AND (job.reconcile_after<=now() OR job.dispatch_deadline_at<=now() OR job.run_deadline_at<=now())`
		assignment = `reconciliation_owner=$2,reconciliation_lease_expires_at=now()+make_interval(secs=>$3)`
	}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `WITH candidate AS (
			SELECT job.id,repository.github_installation_id,repository.full_name
			FROM orchestration_jobs job
			JOIN github_repositories repository ON repository.org_id=job.org_id AND repository.github_repository_id=job.github_repository_id
			JOIN github_installations installation ON installation.org_id=repository.org_id AND installation.github_installation_id=repository.github_installation_id
			JOIN environments environment ON environment.org_id=job.org_id AND environment.id=job.environment_id
			JOIN apps app ON app.org_id=job.org_id AND app.id=job.app_id
				WHERE job.org_id=$1 AND (`+statusPredicate+`) AND environment.torn_down_at IS NULL
				AND environment.lifecycle_status NOT IN ('policy_blocked','teardown_requested','torn_down','failed')
				AND app.deleted_at IS NULL AND repository.access_state='active' AND installation.access_state='active'
				AND (job.desired_generation=environment.desired_generation OR job.pending_desired_generation=environment.desired_generation)
			ORDER BY job.created_at,job.id FOR UPDATE OF job SKIP LOCKED LIMIT 1)
			UPDATE orchestration_jobs job SET `+assignment+`,updated_at=now() FROM candidate WHERE job.id=candidate.id
			RETURNING job.id::text,job.org_id::text,job.idempotency_key,job.github_repository_id::text,
			job.github_workflow_path,job.workflow_revision,job.dispatch_ref,job.app_id::text,job.environment_id::text,
			job.desired_checkout_sha,job.desired_generation,job.pending_github_workflow_path,job.pending_workflow_revision,
			job.pending_dispatch_ref,job.pending_checkout_sha,job.pending_desired_generation,
			job.pending_idempotency_key,job.status,job.lease_expires_at,job.dispatch_requested_at,
			job.dispatch_ref_observed_sha,job.dispatch_deadline_at,job.run_deadline_at,job.github_run_id,job.reconciliation_lease_expires_at,
			candidate.github_installation_id,candidate.full_name,job.attempt`, orgID, owner, leaseDuration.Seconds())
		return row.Scan(&job.ID, &job.OrgID, &job.IdempotencyKey, &job.RepositoryID, &job.WorkflowID,
			&job.WorkflowRevision, &job.GitRef, &job.AppID, &job.EnvironmentID, &job.CommitSHA,
			&job.DesiredGeneration, &job.PendingWorkflowID, &job.PendingWorkflowRevision, &job.PendingGitRef,
			&job.PendingCommitSHA, &job.PendingDesiredGeneration,
			&job.PendingIdempotencyKey, &job.Status, &job.LeaseExpiresAt, &job.DispatchRequestedAt,
			&dispatchRefObservedSHA, &job.DispatchDeadlineAt, &job.RunDeadlineAt, &job.GitHubRunID, &job.ReconciliationLeaseEnd,
			&job.InstallationID, &job.RepositoryFullName, &job.Attempt)
	})
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if dispatchRefObservedSHA != nil {
		job.DispatchRefObservedSHA = *dispatchRefObservedSHA
	}
	return &job, nil
}

func (s *Store) RescheduleOrchestration(ctx context.Context, orgID, jobID, owner string, delay time.Duration) error {
	if err := requirePositiveDuration("orchestration retry delay", delay); err != nil {
		return err
	}
	command, err := s.db.Exec(ctx, `UPDATE orchestration_jobs SET status='queued',lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
		WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND lease_expires_at>now()`, orgID, jobID, owner)
	if err != nil {
		return err
	}
	return rowsAffected(command, ErrLeaseNotOwned)
}

func (s *Store) BeginDispatchReconciliation(ctx context.Context, orgID, jobID, owner, observedRefSHA, workflowPath, workflowRevision string, reconcileDelay time.Duration) error {
	if err := requirePositiveDuration("dispatch reconciliation delay", reconcileDelay); err != nil {
		return err
	}
	command, err := s.db.Exec(ctx, `UPDATE orchestration_jobs SET status='dispatch_reconciling',dispatch_requested_at=now(),
		dispatch_ref_observed_sha=$4,github_workflow_path=$5,workflow_revision=$6,dispatch_ambiguity_state='possible',
		reconcile_after=now()+make_interval(secs=>$7),dispatch_deadline_at=now()+make_interval(secs=>$8),
		lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
		WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND lease_expires_at>now()
		AND $4 ~ '^[0-9a-f]{40}$' AND $5 ~ '^\.github/workflows/[^/[:space:]]+\.(yml|yaml)$'
		AND $6 ~ '^[0-9a-f]{40}$'`, orgID, jobID, owner, observedRefSHA, workflowPath, workflowRevision,
		reconcileDelay.Seconds(), orchestrationDispatchObservationDeadline.Seconds())
	if err != nil {
		return err
	}
	return rowsAffected(command, ErrLeaseNotOwned)
}

func (s *Store) RescheduleDispatchReconciliation(ctx context.Context, orgID, jobID, owner string, delay time.Duration) error {
	if err := requirePositiveDuration("dispatch reconciliation retry delay", delay); err != nil {
		return err
	}
	command, err := s.db.Exec(ctx, `UPDATE orchestration_jobs SET reconcile_after=LEAST(now()+make_interval(secs=>$4),
		CASE status WHEN 'dispatch_reconciling' THEN dispatch_deadline_at ELSE run_deadline_at END),
		reconciliation_owner=NULL,reconciliation_lease_expires_at=NULL,updated_at=now()
		WHERE org_id=$1 AND id=$2 AND status IN ('dispatch_reconciling','dispatched') AND reconciliation_owner=$3
		AND reconciliation_lease_expires_at>now() AND CASE status WHEN 'dispatch_reconciling' THEN dispatch_deadline_at ELSE run_deadline_at END>now()`, orgID, jobID, owner, delay.Seconds())
	if err != nil {
		return err
	}
	return rowsAffected(command, ErrLeaseNotOwned)
}

type FailOrchestrationParams struct{ OrgID, JobID, Owner, Code, Message string }

func (s *Store) FailOrchestration(ctx context.Context, params FailOrchestrationParams) error {
	if params.Code == "" {
		return fmt.Errorf("%w: orchestration failure code is required", ErrInvalidArgument)
	}
	command, err := s.db.Exec(ctx, `UPDATE orchestration_jobs SET status='failed',lease_owner=NULL,lease_expires_at=NULL,
		reconciliation_owner=NULL,reconciliation_lease_expires_at=NULL,terminal_error_code=$4,terminal_error_message=$5,
		terminal_at=now(),updated_at=now() WHERE org_id=$1 AND id=$2 AND ((status='leased' AND lease_owner=$3) OR
		(status IN ('dispatch_reconciling','dispatched') AND reconciliation_owner=$3))`,
		params.OrgID, params.JobID, params.Owner, params.Code, params.Message)
	if err != nil {
		return err
	}
	return rowsAffected(command, ErrLeaseNotOwned)
}

func (s *Store) RecordDispatchedRun(ctx context.Context, orgID, jobID, owner, identity string, runID int64, runStatus, runHeadSHA string, pollDelay time.Duration) error {
	if err := requirePositiveDuration("orchestration poll delay", pollDelay); err != nil {
		return err
	}
	command, err := s.db.Exec(ctx, `UPDATE orchestration_jobs SET status='dispatched',github_dispatch_identity=$4,
		github_run_id=$5,github_run_head_sha=$7,github_run_status=$6,dispatch_ambiguity_state='matched',
		github_status_observed_at=now(),reconcile_after=now()+make_interval(secs=>$8),run_deadline_at=now()+make_interval(secs=>$9),
		reconciliation_owner=NULL,reconciliation_lease_expires_at=NULL,updated_at=now()
		WHERE org_id=$1 AND id=$2 AND status='dispatch_reconciling' AND reconciliation_owner=$3
		AND reconciliation_lease_expires_at>now() AND github_run_id IS NULL AND $7 ~ '^[0-9a-f]{40}$'`,
		orgID, jobID, owner, identity, runID, runStatus, runHeadSHA, pollDelay.Seconds(), orchestrationRunDeadline.Seconds())
	if err != nil {
		return err
	}
	return rowsAffected(command, ErrLeaseNotOwned)
}

func (s *Store) ContinueDispatchReconciliation(ctx context.Context, orgID, jobID, owner string, pollDelay time.Duration) error {
	return s.RescheduleDispatchReconciliation(ctx, orgID, jobID, owner, pollDelay)
}

func (s *Store) ContinueRunReconciliation(ctx context.Context, orgID, jobID, owner, runStatus string, runConclusion *string, pollDelay time.Duration) error {
	if err := requirePositiveDuration("orchestration poll delay", pollDelay); err != nil {
		return err
	}
	command, err := s.db.Exec(ctx, `WITH evidence AS (INSERT INTO orchestration_job_run_evidence(
		orchestration_job_id,attempt,github_run_id,github_run_status,github_run_conclusion,observed_head_sha,observed_at)
		SELECT id,attempt,github_run_id,$4,$5,github_run_head_sha,now() FROM orchestration_jobs
		WHERE org_id=$1 AND id=$2 AND status='dispatched' AND reconciliation_owner=$3
		AND reconciliation_lease_expires_at>now() RETURNING orchestration_job_id)
		UPDATE orchestration_jobs job SET github_run_status=$4,github_run_conclusion=$5,
		github_status_observed_at=now(),reconcile_after=now()+make_interval(secs=>$6),reconciliation_owner=NULL,
		reconciliation_lease_expires_at=NULL,updated_at=now() FROM evidence WHERE job.id=evidence.orchestration_job_id AND $4 IN ('queued','in_progress')`,
		orgID, jobID, owner, runStatus, runConclusion, pollDelay.Seconds())
	if err != nil {
		return err
	}
	return rowsAffected(command, ErrLeaseNotOwned)
}

func (s *Store) RecordOrchestrationStatus(ctx context.Context, orgID, jobID, owner, runStatus string, runConclusion *string, pollDelay time.Duration) error {
	return s.ContinueRunReconciliation(ctx, orgID, jobID, owner, runStatus, runConclusion, pollDelay)
}

type ResolveOrchestrationParams struct {
	OrgID, JobID, Owner, RunStatus, RunConclusion string
	TerminalCode, TerminalMessage                 *string
}

func (s *Store) ResolveOrchestration(ctx context.Context, params ResolveOrchestrationParams) (OrchestrationJob, error) {
	if !validOrchestrationResolution(params.RunConclusion, params.TerminalCode) {
		return OrchestrationJob{}, fmt.Errorf("%w: invalid GitHub terminal conclusion or recovery code", ErrInvalidArgument)
	}
	var job OrchestrationJob
	err := s.db.QueryRow(ctx, `WITH terminal_evidence AS (INSERT INTO orchestration_job_run_evidence(
		orchestration_job_id,attempt,github_run_id,github_run_status,github_run_conclusion,observed_head_sha,observed_at,
		terminal_error_code,terminal_error_message,terminal_at)
		SELECT id,attempt,github_run_id,$4,NULLIF($5,''),github_run_head_sha,now(),$6::text,$7::text,now()
		FROM orchestration_jobs WHERE org_id=$1 AND id=$2 AND status IN ('dispatch_reconciling','dispatched')
		AND reconciliation_owner=$3 AND reconciliation_lease_expires_at>now()
		AND (pending_desired_generation IS NULL OR pending_desired_generation=(SELECT desired_generation FROM environments
		WHERE org_id=$1 AND id=orchestration_jobs.environment_id))
		AND ((status='dispatched' AND github_run_id IS NOT NULL AND $4='completed' AND $5 IN ('success','failure','cancelled','timed_out','action_required','neutral','skipped','stale') AND $6::text IS NULL)
		OR (status='dispatch_reconciling' AND github_run_id IS NULL AND dispatch_deadline_at<=now() AND $4='completed' AND $5='' AND $6::text IN ('github_run_missing','github_run_ambiguous'))
		OR (status='dispatched' AND github_run_id IS NOT NULL AND run_deadline_at<=now() AND $4 IN ('queued','in_progress') AND $5='' AND $6::text='github_run_timed_out'))
		RETURNING orchestration_job_id)
		UPDATE orchestration_jobs job SET status=CASE WHEN pending_checkout_sha IS NOT NULL THEN 'queued'
		WHEN $5='success' THEN 'resolving_image' ELSE 'failed' END,
		github_workflow_path=COALESCE(pending_github_workflow_path,github_workflow_path),
		workflow_revision=COALESCE(pending_workflow_revision,workflow_revision),dispatch_ref=COALESCE(pending_dispatch_ref,dispatch_ref),
		desired_checkout_sha=COALESCE(pending_checkout_sha,desired_checkout_sha),desired_generation=COALESCE(pending_desired_generation,desired_generation),
		idempotency_key=COALESCE(pending_idempotency_key,idempotency_key),pending_github_workflow_path=NULL,
		pending_workflow_revision=NULL,pending_dispatch_ref=NULL,pending_checkout_sha=NULL,pending_desired_generation=NULL,
		pending_idempotency_key=NULL,lease_owner=NULL,lease_expires_at=NULL,
		dispatch_requested_at=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE dispatch_requested_at END,
		dispatch_ref_observed_sha=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE dispatch_ref_observed_sha END,
		dispatch_ambiguity_state=CASE WHEN pending_checkout_sha IS NOT NULL THEN 'none' ELSE dispatch_ambiguity_state END,
		reconcile_after=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE reconcile_after END,
		dispatch_deadline_at=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE dispatch_deadline_at END,
		run_deadline_at=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE run_deadline_at END,
		reconciliation_owner=NULL,reconciliation_lease_expires_at=NULL,
		github_dispatch_identity=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE github_dispatch_identity END,
		github_run_id=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE github_run_id END,
		github_run_head_sha=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE github_run_head_sha END,
		github_run_status=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE $4 END,
		github_run_conclusion=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE NULLIF($5,'') END,
		github_status_observed_at=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE now() END,
		cancellation_state=CASE WHEN pending_checkout_sha IS NOT NULL THEN 'not_requested' ELSE cancellation_state END,
		cancellation_requested_at=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE cancellation_requested_at END,
		superseded_by_job_id=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE superseded_by_job_id END,
		superseded_at=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE superseded_at END,
		oci_resolution_state=CASE WHEN pending_checkout_sha IS NOT NULL THEN 'pending' ELSE oci_resolution_state END,
		image_tag_ref=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE image_tag_ref END,
		resolved_image_digest=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE resolved_image_digest END,
		immutable_image_ref=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE immutable_image_ref END,
		oci_media_type=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE oci_media_type END,
		oci_resolved_at=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE oci_resolved_at END,
		deployment_state=CASE WHEN pending_checkout_sha IS NOT NULL THEN 'not_started' ELSE deployment_state END,
		deployment_claim_owner=NULL,deployment_claim_expires_at=NULL,
		release_id=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE release_id END,
		terminal_error_code=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE $6::text END,
		terminal_error_message=CASE WHEN pending_checkout_sha IS NOT NULL THEN NULL ELSE $7::text END,
		terminal_at=CASE WHEN pending_checkout_sha IS NULL AND $5<>'success' THEN now() ELSE NULL END,
		attempt=attempt+CASE WHEN pending_checkout_sha IS NOT NULL THEN 1 ELSE 0 END,updated_at=now()
		FROM terminal_evidence WHERE job.id=terminal_evidence.orchestration_job_id
		RETURNING id::text,org_id::text,status,desired_checkout_sha,pending_checkout_sha,desired_generation`,
		params.OrgID, params.JobID, params.Owner, params.RunStatus, params.RunConclusion,
		params.TerminalCode, params.TerminalMessage).Scan(&job.ID, &job.OrgID, &job.Status, &job.CommitSHA,
		&job.PendingCommitSHA, &job.DesiredGeneration)
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
	case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale":
		return true
	default:
		return false
	}
}
