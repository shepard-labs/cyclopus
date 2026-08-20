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
	DesiredGeneration  int64
}

type ExpiredDeployRecovery = DeployLease

type DeployRecoveryResult struct {
	Lease DeployLease
}

type DeployExecution = DeployLease

type ImmutableReleaseSnapshot struct {
	ID                           string          `json:"id"`
	OrgID                        string          `json:"org_id"`
	EnvironmentID                string          `json:"environment_id"`
	ImageRef                     string          `json:"image_ref"`
	GitCommitSHA                 *string         `json:"git_commit_sha"`
	TriggeredByUserID            *string         `json:"triggered_by_user_id"`
	Kind                         string          `json:"kind"`
	EnvVarRevisionID             *string         `json:"env_var_revision_id"`
	ConfigSnapshot               json.RawMessage `json:"config_snapshot"`
	ConfigHash                   string          `json:"config_hash"`
	RegistryCredentialRegistryID *string         `json:"registry_credential_registry_id"`
	RegistryCredentialGeneration *int64          `json:"registry_credential_generation"`
	DesiredGeneration            int64           `json:"desired_generation"`
	OrchestrationJobID           *string         `json:"orchestration_job_id"`
	Provenance                   string          `json:"provenance"`
	CreatedAt                    time.Time       `json:"created_at"`
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
		LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
		LEFT JOIN registry_credential_generations generation ON generation.org_id=registry.org_id
		  AND generation.registry_id=registry.id AND generation.generation=release.registry_credential_generation
		WHERE release.status = CASE WHEN $1 THEN 'deploying' ELSE 'pending' END
		  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
		  AND environment.deploy_recovery_quarantined_at IS NULL
		  AND release.desired_generation=environment.desired_generation AND app.desired_generation=environment.desired_generation
		  AND release.provenance<>'legacy_unverified'
		  AND ((app.image_visibility='public' AND app.registry_id IS NULL
		    AND release.registry_credential_registry_id IS NULL AND release.registry_credential_generation IS NULL
		    AND environment.registry_credential_state='not_required'
		    AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
		   OR (app.image_visibility='private' AND release.registry_credential_registry_id=app.registry_id
		    AND release.registry_credential_generation=registry.active_generation AND registry.state='ready'
		    AND generation.state='active' AND environment.registry_credential_state='ready'
		    AND environment.required_registry_credential_generation=release.registry_credential_generation
		    AND environment.ready_registry_credential_generation=release.registry_credential_generation))
		  AND environment.lifecycle_status='ready' AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
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
	execution, err := s.claimForwardDeploy(ctx, orgID, "pending", excludedEnvironmentIDs)
	if err == ErrNotFound {
		return s.ClaimResolvedJobDeploy(ctx, orgID, excludedEnvironmentIDs)
	}
	return execution, err
}

// ClaimResolvedJobDeploy atomically materializes a resolved orchestration job
// into its deterministic release, binds the job exactly once, and acquires the
// environment deploy fence. All inputs are persisted PostgreSQL evidence.
func (s *Store) ClaimResolvedJobDeploy(ctx context.Context, orgID string, excludedEnvironmentIDs []string) (DeployExecution, error) {
	var execution DeployExecution
	var app App
	var environment Environment
	var visibility, imageRef, commitSHA, jobID string
	var registryID *string
	var registryGeneration *int64
	err := s.withGitHubEventTxRetry(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT job.id::text,job.immutable_image_ref,job.desired_checkout_sha,
			app.image_visibility,app.registry_id::text,job.resolved_registry_generation
			FROM orchestration_jobs job
			JOIN apps app ON app.org_id=job.org_id AND app.id=job.app_id
			JOIN environments environment ON environment.org_id=job.org_id AND environment.id=job.environment_id AND environment.app_id=app.id
			LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
			LEFT JOIN registry_credential_generations generation ON generation.org_id=registry.org_id
			 AND generation.registry_id=registry.id AND generation.generation=registry.active_generation
			WHERE job.org_id=$1 AND job.status='deploying' AND job.deployment_state='ready'
			 AND job.oci_resolution_state='resolved' AND job.release_id IS NULL AND job.pending_checkout_sha IS NULL
			 AND job.desired_generation=environment.desired_generation AND app.desired_generation=environment.desired_generation
			 AND NOT (environment.id::text=ANY(COALESCE($2::text[],ARRAY[]::text[])))
			 AND environment.in_progress_release_id IS NULL AND environment.lifecycle_status='ready'
			 AND environment.torn_down_at IS NULL AND environment.planned_hostname IS NOT NULL
			 AND app.deleted_at IS NULL
			 AND ((job.oci_annotation_status='matched' AND job.oci_revision=job.desired_checkout_sha)
			   OR (job.oci_annotation_status='absent' AND job.oci_source IS NULL AND job.oci_revision IS NULL))
			 AND ((app.image_visibility='public' AND app.registry_id IS NULL AND job.resolved_registry_id IS NULL
			   AND job.resolved_registry_generation IS NULL AND environment.registry_credential_state='not_required'
			   AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
			  OR (app.image_visibility='private' AND job.resolved_registry_id=app.registry_id
			   AND job.resolved_registry_generation=registry.active_generation AND registry.state='ready' AND generation.state='active'
			   AND environment.registry_credential_state='ready'
			   AND environment.required_registry_credential_generation=registry.active_generation
			   AND environment.ready_registry_credential_generation=registry.active_generation))
			ORDER BY job.updated_at,job.id FOR UPDATE OF job,environment SKIP LOCKED LIMIT 1`, orgID, excludedEnvironmentIDs).Scan(
			&jobID, &imageRef, &commitSHA, &visibility, &registryID, &registryGeneration)
		if err != nil {
			return err
		}
		app, err = scanApp(tx.QueryRow(ctx, appSelect+` WHERE app.org_id=$1 AND app.id=(SELECT app_id FROM orchestration_jobs WHERE id=$2)`, orgID, jobID))
		if err != nil {
			return err
		}
		if err = scanEnvironment(tx.QueryRow(ctx, environmentSelect+` WHERE environment.org_id=$1 AND environment.id=(SELECT environment_id FROM orchestration_jobs WHERE id=$2)`, orgID, jobID), &environment); err != nil {
			return err
		}
		snapshot, hash, err := buildContextualReleaseSnapshot(environment, app, imageRef, jobID, visibility)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO releases(id,org_id,environment_id,image_ref,git_commit_sha,kind,
			config_snapshot,config_hash,status,registry_credential_registry_id,registry_credential_generation,
			desired_generation,orchestration_job_id,provenance)
			VALUES($1,$2,$3,$4,$5,'deploy',$6,$7,'deploying',$8,$9,$10,$1,'orchestration')`, jobID, orgID,
			environment.ID, imageRef, commitSHA, snapshot, hash, registryID, registryGeneration, environment.DesiredGeneration); err != nil {
			return err
		}
		if tag, updateErr := tx.Exec(ctx, `UPDATE orchestration_jobs SET release_id=$2,updated_at=clock_timestamp()
			WHERE org_id=$1 AND id=$2 AND release_id IS NULL`, orgID, jobID); updateErr != nil || tag.RowsAffected() != 1 {
			if updateErr != nil {
				return updateErr
			}
			return ErrConflict
		}
		var timeoutSeconds int64
		if err = tx.QueryRow(ctx, `UPDATE environments environment SET in_progress_release_id=$3,
			lock_acquired_at=clock_timestamp(),deploy_recovery_checkpoint=NULL FROM apps app
			WHERE environment.org_id=$1 AND environment.id=$2 AND environment.in_progress_release_id IS NULL
			 AND environment.desired_generation=$4 AND app.org_id=environment.org_id AND app.id=environment.app_id
			RETURNING environment.lock_acquired_at,environment.lock_acquired_at+make_interval(secs=>app.deploy_timeout_seconds),
			environment.namespace,environment.planned_hostname,app.deploy_timeout_seconds`, orgID, environment.ID, jobID,
			environment.DesiredGeneration).Scan(&execution.Fence, &execution.ExpiresAt, &execution.Namespace,
			&execution.PlannedHostname, &timeoutSeconds); err != nil {
			return err
		}
		execution.OrgID, execution.EnvironmentID, execution.ReleaseID = orgID, environment.ID, jobID
		execution.DesiredGeneration, execution.Timeout = environment.DesiredGeneration, time.Duration(timeoutSeconds)*time.Second
		execution.FailedRelease = ImmutableReleaseSnapshot{ID: jobID, OrgID: orgID, EnvironmentID: environment.ID,
			ImageRef: imageRef, GitCommitSHA: &commitSHA, Kind: "deploy", ConfigSnapshot: snapshot, ConfigHash: hash,
			RegistryCredentialRegistryID: registryID, RegistryCredentialGeneration: registryGeneration,
			DesiredGeneration: environment.DesiredGeneration, OrchestrationJobID: &jobID, Provenance: "orchestration"}
		return enqueueWebhookEvent(ctx, tx, orgID, "deploy.started", environment.ID, &jobID, map[string]any{"kind": "deploy"})
	})
	if err == pgx.ErrNoRows {
		return DeployExecution{}, ErrNotFound
	}
	if err != nil {
		return DeployExecution{}, fmt.Errorf("claim resolved job deploy: %w", err)
	}
	return execution, nil
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
				LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
				LEFT JOIN registry_credential_generations generation ON generation.org_id=registry.org_id
				  AND generation.registry_id=registry.id AND generation.generation=release.registry_credential_generation
				WHERE environment.org_id = $1 AND release.status = $2
				  AND NOT (environment.id::text = ANY(COALESCE($3::text[], ARRAY[]::text[])))
				  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
				  AND environment.deploy_recovery_quarantined_at IS NULL
				  AND release.desired_generation=environment.desired_generation AND app.desired_generation=environment.desired_generation
				  AND release.provenance<>'legacy_unverified'
				  AND ((app.image_visibility='public' AND app.registry_id IS NULL
				    AND release.registry_credential_registry_id IS NULL AND release.registry_credential_generation IS NULL
				    AND environment.registry_credential_state='not_required'
				    AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
				   OR (app.image_visibility='private' AND release.registry_credential_registry_id=app.registry_id
				    AND release.registry_credential_generation=registry.active_generation AND registry.state='ready'
				    AND generation.state='active' AND environment.registry_credential_state='ready'
				    AND environment.required_registry_credential_generation=release.registry_credential_generation
				    AND environment.ready_registry_credential_generation=release.registry_credential_generation))
			  AND environment.torn_down_at IS NULL AND environment.lifecycle_status='ready'
				  AND environment.planned_hostname IS NOT NULL AND app.deleted_at IS NULL
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
				  app.deploy_timeout_seconds, environment.desired_generation
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
				claimed.namespace, claimed.planned_hostname, claimed.deploy_timeout_seconds, claimed.desired_generation,
				jsonb_build_object('id', current.id, 'org_id', current.org_id,
				  'environment_id', current.environment_id, 'image_ref', current.image_ref,
				  'git_commit_sha', current.git_commit_sha, 'triggered_by_user_id', current.triggered_by_user_id,
				  'kind', current.kind, 'env_var_revision_id', current.env_var_revision_id,
				  'config_snapshot', current.config_snapshot, 'config_hash', current.config_hash,
				  'registry_credential_registry_id', current.registry_credential_registry_id,
				  'registry_credential_generation', current.registry_credential_generation,
				  'desired_generation', current.desired_generation,
				  'orchestration_job_id', current.orchestration_job_id,
				  'provenance', current.provenance,
				  'created_at', current.created_at),
				CASE WHEN previous.id IS NULL THEN NULL ELSE jsonb_build_object(
				  'id', previous.id, 'org_id', previous.org_id, 'environment_id', previous.environment_id,
				  'image_ref', previous.image_ref, 'git_commit_sha', previous.git_commit_sha,
				  'triggered_by_user_id', previous.triggered_by_user_id, 'kind', previous.kind,
				  'env_var_revision_id', previous.env_var_revision_id, 'config_snapshot', previous.config_snapshot,
				  'config_hash', previous.config_hash,
				  'registry_credential_registry_id', previous.registry_credential_registry_id,
				  'registry_credential_generation', previous.registry_credential_generation,
				  'desired_generation', previous.desired_generation,
				  'orchestration_job_id', previous.orchestration_job_id,
				  'provenance', previous.provenance,
				  'created_at', previous.created_at) END
			FROM claimed JOIN transitioned current ON current.id = claimed.in_progress_release_id
			LEFT JOIN LATERAL (SELECT release.* FROM releases release
			  WHERE release.org_id = claimed.org_id AND release.environment_id = claimed.id
			    AND release.id <> current.id AND release.status = 'live'
			  ORDER BY release.created_at DESC, release.id DESC LIMIT 1) previous ON true`,
			orgID, status, excludedEnvironmentIDs).Scan(&execution.OrgID, &execution.EnvironmentID, &execution.ReleaseID,
			&execution.Fence, &execution.ExpiresAt, &execution.Namespace, &execution.PlannedHostname,
			&timeoutSeconds, &execution.DesiredGeneration, &currentSnapshot, &previousSnapshot); err != nil {
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
		if status == "pending" {
			return enqueueWebhookEvent(ctx, tx, execution.OrgID, "deploy.started", execution.EnvironmentID,
				&execution.ReleaseID, map[string]any{"kind": execution.FailedRelease.Kind})
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
				  AND environment.torn_down_at IS NULL AND environment.lifecycle_status IN ('ready','teardown_requested')
				  AND environment.planned_hostname IS NOT NULL AND app.deleted_at IS NULL
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
				  AND environment.lifecycle_status='ready' AND environment.desired_generation=release.desired_generation
				  AND release.provenance<>'legacy_unverified'
				  AND ((app.image_visibility='public' AND app.registry_id IS NULL
			    AND release.registry_credential_registry_id IS NULL AND release.registry_credential_generation IS NULL
			    AND environment.registry_credential_state='not_required'
			    AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
			   OR (app.image_visibility='private' AND release.registry_credential_registry_id=app.registry_id
			    AND environment.registry_credential_state='ready'
			    AND environment.required_registry_credential_generation=release.registry_credential_generation
			    AND environment.ready_registry_credential_generation=release.registry_credential_generation
			    AND EXISTS(SELECT 1 FROM registries registry JOIN registry_credential_generations generation
			      ON generation.org_id=registry.org_id AND generation.registry_id=registry.id
			      AND generation.generation=registry.active_generation AND generation.state='active'
			      WHERE registry.org_id=release.org_id AND registry.id=release.registry_credential_registry_id
			      AND registry.state='ready' AND registry.active_generation=release.registry_credential_generation)))
			  AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			  AND release.status IN ('pending', 'deploying')
			RETURNING environment.org_id::text, environment.id::text, release.id::text,
				environment.lock_acquired_at,
				 environment.lock_acquired_at + make_interval(secs => app.deploy_timeout_seconds), release.desired_generation`,
			orgID, environmentID, releaseID).Scan(&lease.OrgID, &lease.EnvironmentID,
			&lease.ReleaseID, &lease.Fence, &lease.ExpiresAt, &lease.DesiredGeneration)
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
				  AND environment.lifecycle_status='ready' AND environment.desired_generation=release.desired_generation
				  AND release.provenance<>'legacy_unverified'
				  AND ((app.image_visibility='public' AND app.registry_id IS NULL
				    AND release.registry_credential_registry_id IS NULL AND release.registry_credential_generation IS NULL
				    AND environment.registry_credential_state='not_required'
				    AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
				   OR (app.image_visibility='private' AND release.registry_credential_registry_id=app.registry_id
				    AND environment.registry_credential_state='ready'
				    AND environment.required_registry_credential_generation=release.registry_credential_generation
				    AND environment.ready_registry_credential_generation=release.registry_credential_generation
				    AND EXISTS(SELECT 1 FROM registries registry JOIN registry_credential_generations generation
				      ON generation.org_id=registry.org_id AND generation.registry_id=registry.id
				      AND generation.generation=registry.active_generation AND generation.state='active'
				      WHERE registry.org_id=release.org_id AND registry.id=release.registry_credential_registry_id
				      AND registry.state='ready' AND registry.active_generation=release.registry_credential_generation)))
				  AND app.deleted_at IS NULL
				  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
				  AND environment.lock_acquired_at <= now() - make_interval(secs => app.deploy_timeout_seconds)
				RETURNING environment.org_id, environment.id, release.id AS release_id,
					environment.namespace, environment.planned_hostname, environment.lock_acquired_at,
					app.deploy_timeout_seconds, COALESCE(environment.deploy_recovery_checkpoint, '') AS deploy_recovery_checkpoint,
					environment.desired_generation
			)
			SELECT claimed.org_id::text, claimed.id::text, claimed.release_id::text,
				claimed.lock_acquired_at,
				claimed.lock_acquired_at + make_interval(secs => claimed.deploy_timeout_seconds),
				claimed.namespace, claimed.planned_hostname, claimed.deploy_timeout_seconds, claimed.deploy_recovery_checkpoint,
				claimed.desired_generation,
				jsonb_build_object(
					'id', failed.id, 'org_id', failed.org_id,
					'environment_id', failed.environment_id, 'image_ref', failed.image_ref,
					'git_commit_sha', failed.git_commit_sha,
					'triggered_by_user_id', failed.triggered_by_user_id, 'kind', failed.kind,
					'env_var_revision_id', failed.env_var_revision_id,
					'config_snapshot', failed.config_snapshot, 'config_hash', failed.config_hash,
					'registry_credential_registry_id', failed.registry_credential_registry_id,
					'registry_credential_generation', failed.registry_credential_generation,
					'desired_generation', failed.desired_generation,
					'orchestration_job_id', failed.orchestration_job_id,
					'provenance', failed.provenance,
					'created_at', failed.created_at),
				CASE WHEN previous.id IS NULL THEN NULL ELSE jsonb_build_object(
					'id', previous.id, 'org_id', previous.org_id,
					'environment_id', previous.environment_id, 'image_ref', previous.image_ref,
					'git_commit_sha', previous.git_commit_sha,
					'triggered_by_user_id', previous.triggered_by_user_id, 'kind', previous.kind,
					'env_var_revision_id', previous.env_var_revision_id,
					'config_snapshot', previous.config_snapshot, 'config_hash', previous.config_hash,
					'registry_credential_registry_id', previous.registry_credential_registry_id,
					'registry_credential_generation', previous.registry_credential_generation,
					'desired_generation', previous.desired_generation,
					'orchestration_job_id', previous.orchestration_job_id,
					'provenance', previous.provenance,
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
			&recovery.ExpiresAt, &recovery.Namespace, &recovery.PlannedHostname, &timeoutSeconds, &recovery.RecoveryCheckpoint, &recovery.DesiredGeneration, &failedSnapshot,
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
			FROM apps AS app, projects AS project, orgs AS org, releases AS release
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
			  AND app.org_id = environment.org_id AND app.id = environment.app_id
			  AND project.org_id = app.org_id AND project.id = app.project_id
			  AND org.id = environment.org_id
			  AND release.org_id=environment.org_id AND release.environment_id=environment.id AND release.id=environment.in_progress_release_id
			  AND release.desired_generation=environment.desired_generation AND app.desired_generation=environment.desired_generation
			  AND release.provenance<>'legacy_unverified'
			  AND ((app.image_visibility='public' AND app.registry_id IS NULL
			    AND release.registry_credential_registry_id IS NULL AND release.registry_credential_generation IS NULL
			    AND environment.registry_credential_state='not_required'
			    AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
			   OR (app.image_visibility='private' AND release.registry_credential_registry_id=app.registry_id
			    AND environment.registry_credential_state='ready'
			    AND environment.required_registry_credential_generation=release.registry_credential_generation
			    AND environment.ready_registry_credential_generation=release.registry_credential_generation
			    AND EXISTS(SELECT 1 FROM registries registry JOIN registry_credential_generations generation
			      ON generation.org_id=registry.org_id AND generation.registry_id=registry.id
			      AND generation.generation=registry.active_generation AND generation.state='active'
			      WHERE registry.org_id=release.org_id AND registry.id=release.registry_credential_registry_id
			      AND registry.state='ready' AND registry.active_generation=release.registry_credential_generation)))
			  AND environment.lifecycle_status='ready' AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
			  AND project.deleted_at IS NULL AND org.deleted_at IS NULL
			  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
			RETURNING environment.org_id::text, environment.id::text,
				environment.in_progress_release_id::text, environment.lock_acquired_at,
				environment.lock_acquired_at + make_interval(secs => app.deploy_timeout_seconds), environment.desired_generation`,
			lease.OrgID, lease.EnvironmentID, lease.ReleaseID, lease.Fence).Scan(&renewed.OrgID,
			&renewed.EnvironmentID, &renewed.ReleaseID, &renewed.Fence, &renewed.ExpiresAt, &renewed.DesiredGeneration)
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

// VerifyDeployFence proves that lease still owns the deployment fence and that
// the persisted release evidence is still the environment's current desired
// generation and registry credential generation. It never extends the lease.
// A zero lease fence is reserved for reconciliation of the current live release.
func (s *Store) VerifyDeployFence(ctx context.Context, lease DeployLease, evidence ImmutableReleaseSnapshot) error {
	if evidence.ID != lease.ReleaseID {
		return ErrLeaseNotOwned
	}
	if lease.Fence.IsZero() {
		return s.withTx(ctx, func(tx pgx.Tx) error {
			var valid bool
			err := tx.QueryRow(ctx, `SELECT true FROM environments environment
				JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
				JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
				JOIN orgs org ON org.id=environment.org_id
				JOIN releases release ON release.org_id=environment.org_id AND release.environment_id=environment.id
				 AND release.id=$3
				WHERE environment.org_id=$1 AND environment.id=$2
				 AND environment.in_progress_release_id IS NULL AND environment.lock_acquired_at IS NULL
				 AND release.status='live' AND release.provenance<>'legacy_unverified'
				 AND release.id=(SELECT current.id FROM releases current
				   WHERE current.org_id=environment.org_id AND current.environment_id=environment.id AND current.status='live'
				   ORDER BY current.created_at DESC,current.id DESC LIMIT 1)
				 AND release.desired_generation=environment.desired_generation AND release.desired_generation=$4
				 AND app.desired_generation=environment.desired_generation AND environment.lifecycle_status='ready'
				 AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL AND project.deleted_at IS NULL AND org.deleted_at IS NULL
				 AND ((app.image_visibility='public' AND app.registry_id IS NULL AND release.registry_credential_registry_id IS NULL
				   AND release.registry_credential_generation IS NULL AND environment.registry_credential_state='not_required'
				   AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
				  OR (app.image_visibility='private' AND release.registry_credential_registry_id=app.registry_id
				   AND environment.registry_credential_state='ready'
				   AND environment.required_registry_credential_generation=release.registry_credential_generation
				   AND environment.ready_registry_credential_generation=release.registry_credential_generation
				   AND EXISTS(SELECT 1 FROM registries registry JOIN registry_credential_generations generation
				    ON generation.org_id=registry.org_id AND generation.registry_id=registry.id
				    AND generation.generation=registry.active_generation AND generation.state='active'
				    WHERE registry.org_id=release.org_id AND registry.id=release.registry_credential_registry_id
				    AND registry.state='ready' AND registry.active_generation=release.registry_credential_generation)))`,
				lease.OrgID, lease.EnvironmentID, lease.ReleaseID, evidence.DesiredGeneration).Scan(&valid)
			if err == pgx.ErrNoRows {
				return ErrLeaseNotOwned
			}
			if err != nil {
				return fmt.Errorf("verify live reconciliation fence: %w", err)
			}
			return nil
		})
	}
	return s.withTx(ctx, func(tx pgx.Tx) error { return verifyDeployFenceTx(ctx, tx, lease) })
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
			FROM apps AS app, projects AS project, orgs AS org, releases AS release
			WHERE environment.org_id = $1 AND environment.id = $2
			  AND environment.in_progress_release_id = $3 AND environment.lock_acquired_at = $4
			  AND app.org_id = environment.org_id AND app.id = environment.app_id
			  AND project.org_id = app.org_id AND project.id = app.project_id
			  AND org.id = environment.org_id
			  AND release.org_id=environment.org_id AND release.environment_id=environment.id AND release.id=environment.in_progress_release_id
			  AND release.desired_generation=environment.desired_generation AND app.desired_generation=environment.desired_generation
			  AND ((app.image_visibility='public' AND app.registry_id IS NULL
			    AND release.registry_credential_registry_id IS NULL AND release.registry_credential_generation IS NULL
			    AND environment.registry_credential_state='not_required'
			    AND environment.required_registry_credential_generation IS NULL AND environment.ready_registry_credential_generation IS NULL)
			   OR (app.image_visibility='private' AND release.registry_credential_registry_id=app.registry_id
			    AND environment.registry_credential_state='ready'
			    AND environment.required_registry_credential_generation=release.registry_credential_generation
			    AND environment.ready_registry_credential_generation=release.registry_credential_generation
			    AND EXISTS(SELECT 1 FROM registries registry JOIN registry_credential_generations generation
			      ON generation.org_id=registry.org_id AND generation.registry_id=registry.id
			      AND generation.generation=registry.active_generation AND generation.state='active'
			      WHERE registry.org_id=release.org_id AND registry.id=release.registry_credential_registry_id
			      AND registry.state='ready' AND registry.active_generation=release.registry_credential_generation)))
			  AND environment.lifecycle_status='ready' AND environment.torn_down_at IS NULL AND app.deleted_at IS NULL
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
		if err := verifyDeployFenceTx(ctx, tx, lease); err != nil {
			return err
		}
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
		if err := verifyDeployFenceTx(ctx, tx, lease); err != nil {
			return err
		}
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
		var params EnqueueOrchestrationParams
		if err := tx.QueryRow(ctx, `SELECT github_workflow_path,workflow_revision,dispatch_ref
			FROM orchestration_jobs WHERE org_id=$1 AND environment_id=$2
			ORDER BY created_at DESC,id DESC LIMIT 1`, lease.OrgID, lease.EnvironmentID).Scan(
			&params.WorkflowID, &params.WorkflowRevision, &params.GitRef); err != nil {
			if err == pgx.ErrNoRows {
				return fmt.Errorf("set pending commit SHA: %w: no orchestration route for environment", ErrConflict)
			}
			return fmt.Errorf("set pending commit SHA: %w", err)
		}
		params.OrgID = lease.OrgID
		params.EnvironmentID = lease.EnvironmentID
		params.CommitSHA = commitSHA
		params.IdempotencyKey = "deploy-pending:" + lease.ReleaseID + ":" + commitSHA
		job, err := enqueueOrCoalesceOrchestrationTx(ctx, tx, params)
		if err != nil {
			return fmt.Errorf("set pending commit SHA: %w", err)
		}
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
			  AND environment.lock_acquired_at > now() - make_interval(secs => app.deploy_timeout_seconds)
			  AND environment.desired_generation = $6`,
			lease.OrgID, lease.EnvironmentID, lease.ReleaseID, lease.Fence, commitSHA, job.AcceptedGeneration)
		if err != nil {
			return fmt.Errorf("set pending commit SHA: %w", err)
		}
		return rowsAffected(commandTag, ErrLeaseNotOwned)
	})
}

func (s *Store) TakePendingCommitSHA(ctx context.Context, lease DeployLease) (*string, error) {
	var sha *string
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := verifyDeployFenceTx(ctx, tx, lease); err != nil {
			return err
		}
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
	if err := verifyDeployFenceTx(ctx, tx, lease); err != nil {
		return err
	}
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
