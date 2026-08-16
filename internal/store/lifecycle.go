package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type EnvironmentLifecycleJob struct {
	ID, OrgID, EnvironmentID, Kind, Status, Namespace string
	Checkpoint, Reason, ErrorCode, ErrorMessage       *string
	LeaseOwner                                        *string
	LeaseExpiresAt                                    *time.Time
	Fence                                             int64
	Attempt, MaxAttempts                              int
	NotBefore                                         time.Time
	EnvironmentKind                                   string
	PlannedHostname                                   *string
	App                                               App
}

func (s *Store) RequeueExpiredLifecycleLeases(ctx context.Context) (int64, error) {
	command, err := s.db.Exec(ctx, `UPDATE environment_lifecycle_jobs SET status='queued',lease_owner=NULL,lease_expires_at=NULL,
		updated_at=now() WHERE status='leased' AND lease_expires_at<=now()`)
	if err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}

func (s *Store) ListLifecycleOrgIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT org_id::text FROM environment_lifecycle_jobs
		WHERE (status='queued' AND not_before<=now()) OR (status='leased' AND lease_expires_at<=now()) ORDER BY org_id::text`)
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

func (s *Store) ClaimEnvironmentLifecycleJob(ctx context.Context, orgID, owner string, leaseDuration time.Duration) (EnvironmentLifecycleJob, error) {
	if err := requirePositiveDuration("lifecycle lease duration", leaseDuration); err != nil {
		return EnvironmentLifecycleJob{}, err
	}
	var job EnvironmentLifecycleJob
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `WITH candidate AS (
			SELECT job.id FROM environment_lifecycle_jobs job
			JOIN environments environment ON environment.org_id=job.org_id AND environment.id=job.environment_id
			WHERE job.org_id=$1 AND ((job.status='queued' AND job.not_before<=now()) OR
			  (job.status='leased' AND job.lease_expires_at<=now()))
			  AND ((job.kind='provision' AND environment.lifecycle_status IN ('reserved','policy_blocked'))
			    OR (job.kind='teardown' AND environment.lifecycle_status='teardown_requested'))
			  AND (job.kind<>'teardown' OR (environment.in_progress_release_id IS NULL AND NOT EXISTS (
			    SELECT 1 FROM orchestration_jobs orchestration WHERE orchestration.org_id=job.org_id
			      AND orchestration.environment_id=job.environment_id
			      AND orchestration.status IN ('leased','dispatch_reconciling','dispatched','resolving_image','deploying')
			  )))
			ORDER BY CASE job.kind WHEN 'teardown' THEN 0 ELSE 1 END,job.created_at,job.id
			FOR UPDATE OF job SKIP LOCKED LIMIT 1
		), claimed AS (
			UPDATE environment_lifecycle_jobs job SET status='leased',lease_owner=$2,
			lease_expires_at=now()+make_interval(secs=>$3),fence=job.fence+1,attempt=job.attempt+1,updated_at=now()
			FROM candidate WHERE job.id=candidate.id RETURNING job.*
		)
		SELECT claimed.id::text,claimed.org_id::text,claimed.environment_id::text,claimed.kind,claimed.status,
			claimed.checkpoint,claimed.reason,claimed.error_code,claimed.error_message,claimed.lease_owner,
			claimed.lease_expires_at,claimed.fence,claimed.attempt,claimed.max_attempts,claimed.not_before,
			environment.namespace,environment.kind,environment.planned_hostname,`+appSelectColumns("app")+`
		FROM claimed JOIN environments environment ON environment.org_id=claimed.org_id AND environment.id=claimed.environment_id
		JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
		LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id`, orgID, owner, leaseDuration.Seconds())
		return scanLifecycleJob(row, &job)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentLifecycleJob{}, ErrNotFound
	}
	return job, err
}

func appSelectColumns(alias string) string {
	return alias + `.id::text,` + alias + `.org_id::text,` + alias + `.project_id::text,` + alias + `.name,` + alias + `.slug,` + alias + `.hostname_label,
		` + alias + `.registry_id::text,registry.registry_host,` + alias + `.image_repository,` + alias + `.container_port,` + alias + `.health_check_path,` + alias + `.scaling_mode,
		` + alias + `.min_replicas,` + alias + `.max_replicas,` + alias + `.hpa_target_cpu_percent,` + alias + `.keda_cron_schedules,` + alias + `.keda_http_target_pending_requests,
		` + alias + `.cpu_request_millicores,` + alias + `.cpu_limit_millicores,
		` + alias + `.memory_request_mib,` + alias + `.memory_limit_mib,` + alias + `.previews_enabled,` + alias + `.max_preview_environments,` + alias + `.preview_idle_ttl_hours,
		` + alias + `.deploy_timeout_seconds,` + alias + `.release_retention_count,` + alias + `.runtime_class,` + alias + `.github_workflow_path,` + alias + `.production_branch,` + alias + `.annotation_policy,` + alias + `.image_visibility,` + alias + `.desired_generation,` + alias + `.cleanup_state,` + alias + `.created_at,` + alias + `.deleted_at`
}

func scanLifecycleJob(row rowScanner, job *EnvironmentLifecycleJob) error {
	var schedules []byte
	err := row.Scan(&job.ID, &job.OrgID, &job.EnvironmentID, &job.Kind, &job.Status, &job.Checkpoint,
		&job.Reason, &job.ErrorCode, &job.ErrorMessage, &job.LeaseOwner, &job.LeaseExpiresAt,
		&job.Fence, &job.Attempt, &job.MaxAttempts, &job.NotBefore, &job.Namespace,
		&job.EnvironmentKind, &job.PlannedHostname,
		&job.App.ID, &job.App.OrgID, &job.App.ProjectID, &job.App.Name, &job.App.Slug, &job.App.HostnameLabel,
		&job.App.RegistryID, &job.App.RegistryURL, &job.App.ImageRepository, &job.App.ContainerPort,
		&job.App.HealthCheckPath, &job.App.ScalingMode, &job.App.MinReplicas, &job.App.MaxReplicas,
		&job.App.HPATargetCPUPercent, &schedules, &job.App.KEDAHTTPTargetPendingRequests,
		&job.App.CPURequestMillicores, &job.App.CPULimitMillicores,
		&job.App.MemoryRequestMiB, &job.App.MemoryLimitMiB, &job.App.PreviewsEnabled,
		&job.App.MaxPreviewEnvironments, &job.App.PreviewIdleTTLHours, &job.App.DeployTimeoutSeconds,
		&job.App.ReleaseRetentionCount, &job.App.RuntimeClass, &job.App.GitHubWorkflowPath, &job.App.ProductionBranch,
		&job.App.AnnotationPolicy, &job.App.ImageVisibility, &job.App.DesiredGeneration, &job.App.CleanupState,
		&job.App.CreatedAt, &job.App.DeletedAt)
	if err != nil {
		return err
	}
	if len(schedules) > 0 {
		if err := json.Unmarshal(schedules, &job.App.KEDACronSchedules); err != nil {
			return fmt.Errorf("decode app keda cron schedules: %w", err)
		}
	}
	if job.App.KEDACronSchedules == nil {
		job.App.KEDACronSchedules = []KEDACronSchedule{}
	}
	return nil
}

func (s *Store) CheckpointEnvironmentLifecycle(ctx context.Context, job EnvironmentLifecycleJob, checkpoint string) error {
	return s.updateLifecycleOwned(ctx, job, `checkpoint=$5,updated_at=now()`, checkpoint)
}

func (s *Store) BlockEnvironmentLifecycle(ctx context.Context, job EnvironmentLifecycleJob, checkpoint, code, message string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		command, err := tx.Exec(ctx, `UPDATE environment_lifecycle_jobs SET status='blocked',checkpoint=$5,
			error_code=$6,error_message=$7,lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
			WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND fence=$4 AND lease_expires_at>now()`,
			job.OrgID, job.ID, stringPointerValue(job.LeaseOwner), job.Fence, checkpoint, code, message)
		if err != nil {
			return err
		}
		if err := rowsAffected(command, ErrLeaseNotOwned); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE environments SET lifecycle_status='policy_blocked',lifecycle_reason=$3
			WHERE org_id=$1 AND id=$2 AND lifecycle_status='reserved'`, job.OrgID, job.EnvironmentID, code)
		return err
	})
}

func (s *Store) UnblockEnvironmentProvision(ctx context.Context, orgID, environmentID string) error {
	command, err := s.db.Exec(ctx, `UPDATE environment_lifecycle_jobs SET status='queued',not_before=now(),error_code=NULL,
		error_message=NULL,updated_at=now() WHERE org_id=$1 AND environment_id=$2 AND kind='provision' AND status='blocked'`, orgID, environmentID)
	if err != nil {
		return err
	}
	return rowsAffected(command, ErrNotFound)
}

func (s *Store) CompleteEnvironmentProvision(ctx context.Context, job EnvironmentLifecycleJob, plannedHostname *string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		command, err := tx.Exec(ctx, `UPDATE environment_lifecycle_jobs SET status='completed',checkpoint='ready',
			lease_owner=NULL,lease_expires_at=NULL,error_code=NULL,error_message=NULL,completed_at=now(),updated_at=now()
			WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND fence=$4 AND lease_expires_at>now()`,
			job.OrgID, job.ID, stringPointerValue(job.LeaseOwner), job.Fence)
		if err != nil {
			return err
		}
		if err := rowsAffected(command, ErrLeaseNotOwned); err != nil {
			return err
		}
		command, err = tx.Exec(ctx, `UPDATE environments SET lifecycle_status='ready',planned_hostname=$3,
			lifecycle_reason=NULL,lifecycle_error_code=NULL,lifecycle_error_message=NULL
			WHERE org_id=$1 AND id=$2 AND lifecycle_status IN ('reserved','policy_blocked')`, job.OrgID, job.EnvironmentID, plannedHostname)
		if err != nil {
			return err
		}
		if err := rowsAffected(command, ErrConflict); err != nil {
			return err
		}
		return enqueueWebhookEvent(ctx, tx, job.OrgID, "environment.created", job.EnvironmentID, nil,
			map[string]any{"hostname": plannedHostname, "namespace": job.Namespace})
	})
}

func (s *Store) RescheduleEnvironmentLifecycle(ctx context.Context, job EnvironmentLifecycleJob, delay time.Duration, code, message string) error {
	if delay <= 0 {
		return ErrInvalidArgument
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		terminal := job.Attempt >= job.MaxAttempts
		status := "queued"
		if terminal {
			status = "failed"
		}
		command, err := tx.Exec(ctx, `UPDATE environment_lifecycle_jobs SET status=$5,not_before=now()+make_interval(secs=>$6),
			error_code=$7,error_message=$8,lease_owner=NULL,lease_expires_at=NULL,
			completed_at=CASE WHEN $5='failed' THEN now() ELSE NULL END,updated_at=now()
			WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND fence=$4 AND lease_expires_at>now()`,
			job.OrgID, job.ID, stringPointerValue(job.LeaseOwner), job.Fence, status, delay.Seconds(), code, message)
		if err != nil {
			return err
		}
		if err := rowsAffected(command, ErrLeaseNotOwned); err != nil {
			return err
		}
		if terminal {
			_, err = tx.Exec(ctx, `UPDATE environments SET lifecycle_status='failed',lifecycle_error_code=$3,
				lifecycle_error_message=$4 WHERE org_id=$1 AND id=$2 AND lifecycle_status<>'torn_down'`, job.OrgID, job.EnvironmentID, code, message)
			if err == nil && job.Kind == "provision" {
				_, err = tx.Exec(ctx, `UPDATE orchestration_jobs SET status='failed',pending_github_workflow_path=NULL,
					pending_workflow_revision=NULL,pending_dispatch_ref=NULL,pending_checkout_sha=NULL,
					pending_desired_generation=NULL,pending_idempotency_key=NULL,terminal_error_code='environment_provision_failed',
					terminal_error_message=$3,terminal_at=now(),updated_at=now()
					WHERE org_id=$1 AND environment_id=$2 AND status='queued'`, job.OrgID, job.EnvironmentID, message)
			}
		}
		return err
	})
}

func (s *Store) CompleteEnvironmentTeardown(ctx context.Context, job EnvironmentLifecycleJob) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var active bool
		if err := tx.QueryRow(ctx, `SELECT in_progress_release_id IS NOT NULL OR EXISTS(SELECT 1 FROM orchestration_jobs
			WHERE org_id=$1 AND environment_id=$2 AND status IN ('leased','dispatch_reconciling','dispatched','resolving_image','deploying'))
			FROM environments WHERE org_id=$1 AND id=$2 FOR UPDATE`, job.OrgID, job.EnvironmentID).Scan(&active); err != nil {
			return err
		}
		if active {
			return ErrConflict
		}
		command, err := tx.Exec(ctx, `UPDATE environment_lifecycle_jobs SET status='completed',checkpoint='namespace_absent',
			lease_owner=NULL,lease_expires_at=NULL,completed_at=now(),updated_at=now()
			WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND fence=$4 AND lease_expires_at>now()`,
			job.OrgID, job.ID, stringPointerValue(job.LeaseOwner), job.Fence)
		if err != nil {
			return err
		}
		if err := rowsAffected(command, ErrLeaseNotOwned); err != nil {
			return err
		}
		command, err = tx.Exec(ctx, `UPDATE environments SET lifecycle_status='torn_down',torn_down_at=now(),
			pending_commit_sha=NULL,lifecycle_error_code=NULL,lifecycle_error_message=NULL
			WHERE org_id=$1 AND id=$2 AND lifecycle_status='teardown_requested'`, job.OrgID, job.EnvironmentID)
		if err != nil {
			return err
		}
		if err := rowsAffected(command, ErrConflict); err != nil {
			return err
		}
		return enqueueWebhookEvent(ctx, tx, job.OrgID, "environment.torn_down", job.EnvironmentID, nil,
			map[string]any{"namespace": job.Namespace, "reason": job.Reason})
	})
}

func (s *Store) RequestIdlePreviewTeardown(ctx context.Context, candidate IdlePreviewCandidate) (bool, error) {
	requested := false
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var appID string
		var idle bool
		if err := tx.QueryRow(ctx, `SELECT environment.app_id::text,
			GREATEST(environment.created_at,COALESCE((SELECT max(release.created_at) FROM releases release
			  WHERE release.org_id=environment.org_id AND release.environment_id=environment.id),environment.created_at))
			  <= now()-make_interval(hours=>app.preview_idle_ttl_hours)
			  AND environment.in_progress_release_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM orchestration_jobs orchestration WHERE orchestration.org_id=environment.org_id
			    AND orchestration.environment_id=environment.id AND orchestration.status IN ('leased','dispatch_reconciling','dispatched','resolving_image','deploying'))
			FROM environments environment JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
			WHERE environment.org_id=$1 AND environment.id=$2 AND environment.kind='preview'
			  AND environment.lifecycle_status IN ('reserved','policy_blocked','ready','failed') FOR UPDATE OF environment,app`,
			candidate.OrgID, candidate.EnvironmentID).Scan(&appID, &idle); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if !idle {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE environments SET lifecycle_status='teardown_requested',
			lifecycle_reason='idle_expired',teardown_requested_at=COALESCE(teardown_requested_at,now()),pending_commit_sha=NULL,
			desired_generation=desired_generation+1
			WHERE org_id=$1 AND id=$2`, candidate.OrgID, candidate.EnvironmentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE orchestration_jobs SET status='cancelled',pending_github_workflow_path=NULL,
			pending_workflow_revision=NULL,pending_dispatch_ref=NULL,pending_checkout_sha=NULL,
			pending_desired_generation=NULL,pending_idempotency_key=NULL,lease_owner=NULL,lease_expires_at=NULL,
			reconcile_after=NULL,reconciliation_owner=NULL,reconciliation_lease_expires_at=NULL,
			deployment_claim_owner=NULL,deployment_claim_expires_at=NULL,cancellation_state='requested',
			cancellation_requested_at=COALESCE(cancellation_requested_at,now()),
			deployment_state=CASE WHEN deployment_state IN ('ready','claimed','applying') THEN 'cancelled' ELSE deployment_state END,
			terminal_error_code='environment_idle_expired',terminal_error_message='preview idle TTL expired',terminal_at=now(),updated_at=now()
			WHERE org_id=$1 AND environment_id=$2 AND status IN ('queued','leased','dispatch_reconciling','dispatched','resolving_image','deploying')`, candidate.OrgID, candidate.EnvironmentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_lifecycle_jobs SET status='failed',completed_at=now(),
			error_code='environment_idle_expired',error_message='preview idle TTL expired',
			lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
			WHERE org_id=$1 AND environment_id=$2 AND kind='provision' AND status IN ('queued','leased','blocked')`, candidate.OrgID, candidate.EnvironmentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_lifecycle_jobs
			(org_id,environment_id,kind,status,checkpoint,reason) VALUES($1,$2,'teardown','queued','requested','idle_expired')
			ON CONFLICT (environment_id,kind) WHERE status IN ('queued','leased','blocked')
			DO UPDATE SET reason='idle_expired',updated_at=now()`, candidate.OrgID, candidate.EnvironmentID); err != nil {
			return err
		}
		requested = true
		_ = appID
		return nil
	})
	return requested, err
}

func (s *Store) updateLifecycleOwned(ctx context.Context, job EnvironmentLifecycleJob, assignment string, value any) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		command, err := tx.Exec(ctx, `UPDATE environment_lifecycle_jobs SET `+assignment+`
			WHERE org_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND fence=$4 AND lease_expires_at>now()`,
			job.OrgID, job.ID, stringPointerValue(job.LeaseOwner), job.Fence, value)
		if err != nil {
			return fmt.Errorf("update lifecycle job: %w", err)
		}
		return rowsAffected(command, ErrLeaseNotOwned)
	})
}
