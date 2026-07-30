package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type DeployLease struct {
	OrgID              string
	EnvironmentID      string
	ReleaseID          string
	Fence              time.Time
	ExpiresAt          time.Time
	Namespace          string
	PlannedHostname    string
	Timeout            time.Duration
	FailedRelease      ImmutableReleaseSnapshot
	PreviousRelease    *ImmutableReleaseSnapshot
	RecoveryCheckpoint string
}

type ExpiredDeployRecovery = DeployLease

type DeployRecoveryResult struct {
	Lease DeployLease
}

type DeployExecution = DeployLease

type ImmutableReleaseSnapshot struct {
	ID                string          `json:"id"`
	OrgID             string          `json:"org_id"`
	EnvironmentID     string          `json:"environment_id"`
	ImageRef          string          `json:"image_ref"`
	GitCommitSHA      *string         `json:"git_commit_sha"`
	TriggeredByUserID *string         `json:"triggered_by_user_id"`
	Kind              string          `json:"kind"`
	EnvVarRevisionID  *string         `json:"env_var_revision_id"`
	ConfigSnapshot    json.RawMessage `json:"config_snapshot"`
	ConfigHash        string          `json:"config_hash"`
	CreatedAt         time.Time       `json:"created_at"`
}

type ExpiredDeploy struct {
	OrgID         string
	EnvironmentID string
}

func (s *Store) ListForwardDeployOrgIDs(ctx context.Context, resumeOnly bool) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT environment.org_id::text
		FROM environments environment
		JOIN releases release ON release.org_id = environment.org_id
		  AND release.environment_id = environment.id AND release.id = environment.in_progress_release_id
		JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
		JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
		JOIN orgs org ON org.id = environment.org_id
		WHERE release.status = CASE WHEN $1 THEN 'deploying' ELSE 'pending' END
		  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
		  AND environment.deploy_recovery_quarantined_at IS NULL
		  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
		  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
		ORDER BY environment.org_id::text`, resumeOnly)
	if err != nil {
		return nil, fmt.Errorf("list forward deploy organizations: %w", err)
	}
	defer rows.Close()
	var orgIDs []string
	for rows.Next() {
		var orgID string
		if err := rows.Scan(&orgID); err != nil {
			return nil, fmt.Errorf("scan forward deploy organization: %w", err)
		}
		orgIDs = append(orgIDs, orgID)
	}
	return orgIDs, rows.Err()
}

func (s *Store) ClaimPendingDeploy(ctx context.Context, orgID string, excludedEnvironmentIDs []string) (DeployExecution, error) {
	return s.claimForwardDeploy(ctx, orgID, "pending", excludedEnvironmentIDs)
}

func (s *Store) ClaimUnexpiredDeploying(ctx context.Context, orgID string, excludedEnvironmentIDs []string) (DeployExecution, error) {
	return s.claimForwardDeploy(ctx, orgID, "deploying", excludedEnvironmentIDs)
}

func (s *Store) claimForwardDeploy(ctx context.Context, orgID, status string, excludedEnvironmentIDs []string) (DeployExecution, error) {
	var execution DeployExecution
	var timeoutSeconds int64
	var currentSnapshot, previousSnapshot []byte
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			WITH candidate AS (
				SELECT environment.id, environment.in_progress_release_id
				FROM environments environment
				JOIN releases release ON release.org_id = environment.org_id
				  AND release.environment_id = environment.id AND release.id = environment.in_progress_release_id
				JOIN apps app ON app.org_id = environment.org_id AND app.id = environment.app_id
				JOIN projects project ON project.org_id = app.org_id AND project.id = app.project_id
				JOIN orgs org ON org.id = environment.org_id
				WHERE environment.org_id = $1 AND release.status = $2
				  AND NOT (environment.id::text = ANY(COALESCE($3::text[], ARRAY[]::text[])))
				  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
				  AND environment.deploy_recovery_quarantined_at IS NULL
				  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
				  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
				ORDER BY release.created_at, release.id
				FOR UPDATE OF environment, release SKIP LOCKED LIMIT 1
			), claimed AS (
				UPDATE environments environment
				SET lock_acquired_at = GREATEST(clock_timestamp(), environment.lock_acquired_at + interval '1 microsecond')
				FROM candidate, apps app
				WHERE environment.org_id = $1 AND environment.id = candidate.id
				  AND app.org_id = environment.org_id AND app.id = environment.app_id
				RETURNING environment.org_id, environment.id, environment.in_progress_release_id,
				  environment.namespace, environment.planned_hostname, environment.lock_acquired_at,
				  app.deploy_timeout_seconds
			), transitioned AS (
				UPDATE releases release SET status = 'deploying'
				FROM claimed WHERE release.org_id = claimed.org_id
				  AND release.environment_id = claimed.id AND release.id = claimed.in_progress_release_id
				  AND release.status = $2
				RETURNING release.*
			)
			SELECT claimed.org_id::text, claimed.id::text, current.id::text,
				claimed.lock_acquired_at,
				claimed.lock_acquired_at + make_interval(secs => claimed.deploy_timeout_seconds),
				claimed.namespace, claimed.planned_hostname, claimed.deploy_timeout_seconds,
				jsonb_build_object('id', current.id, 'org_id', current.org_id,
				  'environment_id', current.environment_id, 'image_ref', current.image_ref,
				  'git_commit_sha', current.git_commit_sha, 'triggered_by_user_id', current.triggered_by_user_id,
				  'kind', current.kind, 'env_var_revision_id', current.env_var_revision_id,
				  'config_snapshot', current.config_snapshot, 'config_hash', current.config_hash,
				  'created_at', current.created_at),
				CASE WHEN previous.id IS NULL THEN NULL ELSE jsonb_build_object(
				  'id', previous.id, 'org_id', previous.org_id, 'environment_id', previous.environment_id,
				  'image_ref', previous.image_ref, 'git_commit_sha', previous.git_commit_sha,
				  'triggered_by_user_id', previous.triggered_by_user_id, 'kind', previous.kind,
				  'env_var_revision_id', previous.env_var_revision_id, 'config_snapshot', previous.config_snapshot,
				  'config_hash', previous.config_hash, 'created_at', previous.created_at) END
			FROM claimed JOIN transitioned current ON current.id = claimed.in_progress_release_id
			LEFT JOIN LATERAL (SELECT release.* FROM releases release
			  WHERE release.org_id = claimed.org_id AND release.environment_id = claimed.id
			    AND release.id <> current.id AND release.status = 'live'
			  ORDER BY release.created_at DESC, release.id DESC LIMIT 1) previous ON true`,
			orgID, status, excludedEnvironmentIDs).Scan(&execution.OrgID, &execution.EnvironmentID, &execution.ReleaseID,
			&execution.Fence, &execution.ExpiresAt, &execution.Namespace, &execution.PlannedHostname,
			&timeoutSeconds, &currentSnapshot, &previousSnapshot); err != nil {
			return err
		}
		execution.Timeout = time.Duration(timeoutSeconds) * time.Second
		if err := json.Unmarshal(currentSnapshot, &execution.FailedRelease); err != nil {
			return fmt.Errorf("decode current deploy record: %w", err)
		}
		if _, err := DecodeReleaseSnapshot(execution.FailedRelease.ConfigSnapshot); err != nil {
			return fmt.Errorf("validate current deploy snapshot: %w", err)
		}
		if previousSnapshot != nil {
			execution.PreviousRelease = new(ImmutableReleaseSnapshot)
			if err := json.Unmarshal(previousSnapshot, execution.PreviousRelease); err != nil {
				return fmt.Errorf("decode previous live record: %w", err)
			}
			if _, err := DecodeReleaseSnapshot(execution.PreviousRelease.ConfigSnapshot); err != nil {
				return fmt.Errorf("validate previous live snapshot: %w", err)
			}
		}
		return nil
	})
	if err == pgx.ErrNoRows {
		return DeployExecution{}, ErrNotFound
	}
	if err != nil {
		return DeployExecution{}, fmt.Errorf("claim %s deploy: %w", status, err)
	}
	return execution, nil
}

func (s *Store) ListExpiredDeploys(ctx context.Context) ([]ExpiredDeploy, error) {
	var deploys []ExpiredDeploy
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT environment.org_id::text, environment.id::text
			FROM environments AS environment
			JOIN apps AS app ON app.org_id = environment.org_id AND app.id = environment.app_id
			JOIN projects AS project ON project.org_id = app.org_id AND project.id = app.project_id
			JOIN orgs AS org ON org.id = environment.org_id
			WHERE environment.in_progress_release_id IS NOT NULL
			  AND environment.deploy_recovery_quarantined_at IS NULL
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			  AND environment.lock_acquired_at <= now() - make_interval(secs => app.deploy_timeout_seconds)
			ORDER BY environment.org_id, environment.id`)
		if err != nil {
			return fmt.Errorf("list expired deploys: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var deploy ExpiredDeploy
			if err := rows.Scan(&deploy.OrgID, &deploy.EnvironmentID); err != nil {
				return fmt.Errorf("scan expired deploy: %w", err)
			}
			deploys = append(deploys, deploy)
		}
		return rows.Err()
	})
	return deploys, err
}

func (s *Store) AcquireDeployLease(ctx context.Context, orgID, environmentID, releaseID string) (DeployLease, error) {
	var lease DeployLease
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE environments AS environment
			SET in_progress_release_id = release.id, lock_acquired_at = clock_timestamp(),
				deploy_recovery_checkpoint = NULL
			FROM releases AS release, apps AS app, projects AS project, orgs AS org
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND release.org_id = environment.org_id AND release.environment_id = environment.id AND release.id = $3
			  AND app.org_id = environment.org_id AND app.id = environment.app_id
			  AND project.org_id = app.org_id AND project.id = app.project_id
			  AND org.id = environment.org_id
			  AND environment.torn_down_at IS NULL AND environment.in_progress_release_id IS NULL
			  AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			  AND release.status IN ('pending', 'deploying')
			RETURNING environment.org_id::text, environment.id::text, release.id::text,
				environment.lock_acquired_at,
				environment.lock_acquired_at + make_interval(secs => app.deploy_timeout_seconds)`,
			orgID, environmentID, releaseID).Scan(&lease.OrgID, &lease.EnvironmentID,
			&lease.ReleaseID, &lease.Fence, &lease.ExpiresAt)
	})
	if err == pgx.ErrNoRows {
		return DeployLease{}, ErrConflict
	}
	if err != nil {
		return DeployLease{}, fmt.Errorf("acquire deploy lease: %w", err)
	}
	return lease, nil
}

func (s *Store) ClaimExpiredDeployLease(ctx context.Context, orgID, environmentID string) (ExpiredDeployRecovery, error) {
	var recovery ExpiredDeployRecovery
	var timeoutSeconds int64
	var failedSnapshot, previousSnapshot []byte
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			WITH claimed AS (
				UPDATE environments AS environment
				SET lock_acquired_at = GREATEST(clock_timestamp(), environment.lock_acquired_at + interval '1 microsecond')
				FROM releases AS release, apps AS app, projects AS project, orgs AS org
				WHERE environment.org_id = $1 AND environment.id = $2
				  AND release.org_id = environment.org_id AND release.environment_id = environment.id
				  AND release.id = environment.in_progress_release_id
				  AND app.org_id = environment.org_id AND app.id = environment.app_id
				  AND project.org_id = app.org_id AND project.id = app.project_id
				  AND org.id = environment.org_id
				  AND environment.torn_down_at IS NULL AND environment.deploy_recovery_quarantined_at IS NULL
				  AND app.deleted_at IS NULL
				  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
				  AND environment.lock_acquired_at <= now() - make_interval(secs => app.deploy_timeout_seconds)
				RETURNING environment.org_id, environment.id, release.id AS release_id,
					environment.namespace, environment.planned_hostname, environment.lock_acquired_at,
					app.deploy_timeout_seconds, COALESCE(environment.deploy_recovery_checkpoint, '') AS deploy_recovery_checkpoint
			)
			SELECT claimed.org_id::text, claimed.id::text, claimed.release_id::text,
				claimed.lock_acquired_at,
				claimed.lock_acquired_at + make_interval(secs => claimed.deploy_timeout_seconds),
				claimed.namespace, claimed.planned_hostname, claimed.deploy_timeout_seconds, claimed.deploy_recovery_checkpoint,
				jsonb_build_object(
					'id', failed.id, 'org_id', failed.org_id,
					'environment_id', failed.environment_id, 'image_ref', failed.image_ref,
					'git_commit_sha', failed.git_commit_sha,
					'triggered_by_user_id', failed.triggered_by_user_id, 'kind', failed.kind,
					'env_var_revision_id', failed.env_var_revision_id,
					'config_snapshot', failed.config_snapshot, 'config_hash', failed.config_hash,
					'created_at', failed.created_at),
				CASE WHEN previous.id IS NULL THEN NULL ELSE jsonb_build_object(
					'id', previous.id, 'org_id', previous.org_id,
					'environment_id', previous.environment_id, 'image_ref', previous.image_ref,
					'git_commit_sha', previous.git_commit_sha,
					'triggered_by_user_id', previous.triggered_by_user_id, 'kind', previous.kind,
					'env_var_revision_id', previous.env_var_revision_id,
					'config_snapshot', previous.config_snapshot, 'config_hash', previous.config_hash,
					'created_at', previous.created_at) END
			FROM claimed
			JOIN releases AS failed ON failed.org_id = claimed.org_id
			  AND failed.environment_id = claimed.id AND failed.id = claimed.release_id
			LEFT JOIN LATERAL (
				SELECT release.* FROM releases AS release
				WHERE release.org_id = claimed.org_id AND release.environment_id = claimed.id
				  AND release.id <> claimed.release_id AND release.status = 'live'
				ORDER BY release.created_at DESC, release.id DESC LIMIT 1
			) AS previous ON true`, orgID, environmentID).Scan(&recovery.OrgID,
			&recovery.EnvironmentID, &recovery.ReleaseID, &recovery.Fence,
			&recovery.ExpiresAt, &recovery.Namespace, &recovery.PlannedHostname, &timeoutSeconds, &recovery.RecoveryCheckpoint, &failedSnapshot,
			&previousSnapshot)
	})
	if err == pgx.ErrNoRows {
		return ExpiredDeployRecovery{}, ErrNotFound
	}
	if err != nil {
		return ExpiredDeployRecovery{}, fmt.Errorf("claim expired deploy lease: %w", err)
	}
	recovery.Timeout = time.Duration(timeoutSeconds) * time.Second
	if err := json.Unmarshal(failedSnapshot, &recovery.FailedRelease); err != nil {
		return ExpiredDeployRecovery{}, fmt.Errorf("decode failed release recovery snapshot: %w", err)
	}
	if previousSnapshot != nil {
		recovery.PreviousRelease = new(ImmutableReleaseSnapshot)
		if err := json.Unmarshal(previousSnapshot, recovery.PreviousRelease); err != nil {
			return ExpiredDeployRecovery{}, fmt.Errorf("decode previous release recovery snapshot: %w", err)
		}
	}
	return recovery, nil
}

func (s *Store) RenewDeployLease(ctx context.Context, lease DeployLease) (DeployLease, error) {
	var renewed DeployLease
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE environments AS environment
			SET lock_acquired_at = GREATEST(clock_timestamp(), environment.lock_acquired_at + interval '1 microsecond')
			FROM apps AS app, projects AS project, orgs AS org
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
			  AND app.org_id = environment.org_id AND app.id = environment.app_id
			  AND project.org_id = app.org_id AND project.id = app.project_id
			  AND org.id = environment.org_id
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
			RETURNING environment.org_id::text, environment.id::text,
				environment.in_progress_release_id::text, environment.lock_acquired_at,
				environment.lock_acquired_at + make_interval(secs => app.deploy_timeout_seconds)`,
			lease.OrgID, lease.EnvironmentID, lease.ReleaseID, lease.Fence).Scan(&renewed.OrgID,
			&renewed.EnvironmentID, &renewed.ReleaseID, &renewed.Fence, &renewed.ExpiresAt)
	})
	if err == pgx.ErrNoRows {
		return DeployLease{}, ErrLeaseNotOwned
	}
	if err != nil {
		return DeployLease{}, fmt.Errorf("renew deploy lease: %w", err)
	}
	renewed.Namespace = lease.Namespace
	renewed.PlannedHostname = lease.PlannedHostname
	renewed.Timeout = lease.Timeout
	renewed.FailedRelease = lease.FailedRelease
	renewed.PreviousRelease = lease.PreviousRelease
	renewed.RecoveryCheckpoint = lease.RecoveryCheckpoint
	return renewed, nil
}

func (s *Store) CheckpointDeployRecovery(ctx context.Context, lease DeployLease, checkpoint string) error {
	switch checkpoint {
	case "claimed", "first_deployment_missing", "late_ready_observed", "secret_revision_verified", "deployment_restored", "desired_state_restored":
	default:
		return fmt.Errorf("%w: invalid deploy recovery checkpoint", ErrInvalidArgument)
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		commandTag, err := tx.Exec(ctx, `
			UPDATE environments AS environment
			SET deploy_recovery_checkpoint = $5
			FROM apps AS app, projects AS project, orgs AS org
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
			  AND app.org_id = environment.org_id AND app.id = environment.app_id
			  AND project.org_id = app.org_id AND project.id = app.project_id
			  AND org.id = environment.org_id
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)`,
			lease.OrgID, lease.EnvironmentID, lease.ReleaseID, lease.Fence, checkpoint)
		if err != nil {
			return fmt.Errorf("checkpoint deploy recovery: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) RescheduleDeployRecovery(ctx context.Context, lease DeployLease, delay time.Duration) error {
	if err := requirePositiveDuration("deploy recovery retry delay", delay); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		commandTag, err := tx.Exec(ctx, `
			UPDATE environments AS environment
			SET lock_acquired_at = now() - make_interval(secs => app.deploy_timeout_seconds)
				+ make_interval(secs => $5)
			FROM apps AS app, projects AS project, orgs AS org
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
			  AND app.org_id = environment.org_id AND app.id = environment.app_id
			  AND project.org_id = app.org_id AND project.id = app.project_id
			  AND org.id = environment.org_id
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL`, lease.OrgID,
			lease.EnvironmentID, lease.ReleaseID, lease.Fence, delay.Seconds())
		if err != nil {
			return fmt.Errorf("reschedule deploy recovery: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) QuarantineDeployRecovery(ctx context.Context, lease DeployLease, code, message string) error {
	if code == "" || message == "" {
		return fmt.Errorf("%w: recovery quarantine code and message are required", ErrInvalidArgument)
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		commandTag, err := tx.Exec(ctx, `
			UPDATE environments
			SET deploy_recovery_error_code = $5, deploy_recovery_error_message = $6,
				deploy_recovery_quarantined_at = now()
			WHERE org_id = $1 AND id = $2 AND in_progress_release_id = $3
			  AND lock_acquired_at = $4 AND deploy_recovery_quarantined_at IS NULL`,
			lease.OrgID, lease.EnvironmentID, lease.ReleaseID, lease.Fence, code, message)
		if err != nil {
			return fmt.Errorf("quarantine deploy recovery: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) ReleaseDeployLease(ctx context.Context, lease DeployLease, audit *AuditEvent) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := releaseLease(ctx, tx, lease); err != nil {
			return err
		}
		before, err := json.Marshal(lease)
		if err != nil {
			return fmt.Errorf("marshal deploy lease audit state: %w", err)
		}
		return insertAuditEvent(ctx, tx, lease.OrgID, audit, "deploy.lease_released",
			"environment", lease.EnvironmentID, before, json.RawMessage(`{"in_progress_release_id":null}`))
	})
}

func (s *Store) SetPendingCommitSHA(ctx context.Context, lease DeployLease, commitSHA string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		commandTag, err := tx.Exec(ctx, `
			UPDATE environments AS environment
			SET pending_commit_sha = $5
			FROM apps AS app, projects AS project, orgs AS org
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
			  AND app.org_id = environment.org_id AND app.id = environment.app_id
			  AND project.org_id = app.org_id AND project.id = app.project_id
			  AND org.id = environment.org_id
			  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)`,
			lease.OrgID, lease.EnvironmentID, lease.ReleaseID, lease.Fence, commitSHA)
		if err != nil {
			return fmt.Errorf("set pending commit SHA: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) TakePendingCommitSHA(ctx context.Context, lease DeployLease) (*string, error) {
	var sha *string
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE environments AS environment
			SET pending_commit_sha = NULL
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
			lease.EnvironmentID, lease.ReleaseID, lease.Fence).Scan(&sha)
		if err == pgx.ErrNoRows {
			return ErrLeaseNotOwned
		}
		return err
	})
	return sha, err
}

func releaseLease(ctx context.Context, tx pgx.Tx, lease DeployLease) error {
	commandTag, err := tx.Exec(ctx, `
		UPDATE environments AS environment
		SET in_progress_release_id = NULL, lock_acquired_at = NULL, deploy_recovery_checkpoint = NULL
		FROM apps AS app, projects AS project, orgs AS org
		WHERE environment.org_id = $1 AND environment.id = $2
		  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
		  AND app.org_id = environment.org_id AND app.id = environment.app_id
		  AND project.org_id = app.org_id AND project.id = app.project_id
		  AND org.id = environment.org_id
		  AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
		  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
		  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)`,
		lease.OrgID, lease.EnvironmentID, lease.ReleaseID, lease.Fence)
	if err != nil {
		return fmt.Errorf("release deploy lease: %w", err)
	}
	return rowsAffected(commandTag, ErrLeaseNotOwned)
}
