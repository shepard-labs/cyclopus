package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxImageResolutionAttempts = 20

var (
	claimImageResolutionFailpoint    func() error
	completeImageResolutionFailpoint func() error
)

type ImageResolutionWriteResult string

const (
	ImageResolutionProcessed       ImageResolutionWriteResult = "processed"
	ImageResolutionRequeued        ImageResolutionWriteResult = "requeued"
	ImageResolutionSuperseded      ImageResolutionWriteResult = "superseded"
	ImageResolutionRefreshRequired ImageResolutionWriteResult = "refresh_required"
)

type ImageResolutionEvidence struct {
	ConfigDigest, ConfigMediaType, Source, Revision *string
	AnnotationStatus                                string
	ResolvedRegistryID                              *string
	ResolvedRegistryGeneration                      *int64
	Deadline                                        *time.Time
	ErrorCode                                       *string
	Attempts                                        int
	NextAttemptAt                                   time.Time
	Lease                                           *LeaseToken
}

type ImageResolutionClaim struct {
	Job                          OrchestrationJob
	App                          App
	Environment                  Environment
	Registry                     *Registry
	RepositoryOwner              string
	RepositoryName               string
	RepositoryNodeID             string
	RegistryCredentialSecretRef  *string
	EnvironmentDesiredGeneration int64
	Lease                        LeaseToken
	Deadline                     time.Time
}

type CompleteImageResolutionParams struct {
	OrgID, JobID, Repository, TagReference, Digest, ImmutableReference string
	ManifestMediaType, ConfigDigest, ConfigMediaType                   string
	Source, Revision, AnnotationStatus                                 string
	RepositoryOwner, RepositoryName, RepositoryNodeID                  string
	DesiredGeneration                                                  int64
	CommitSHA                                                          string
	Visibility                                                         string
	RegistryID                                                         *string
	RegistryGeneration                                                 *int64
	Lease                                                              LeaseToken
	Claim                                                              ImageResolutionClaim
}

type RetryImageResolutionParams struct {
	Claim ImageResolutionClaim
	Lease LeaseToken
	Delay time.Duration
	Code  string
}

type FailImageResolutionParams struct {
	Claim ImageResolutionClaim
	Lease LeaseToken
	Code  string
}

const orchestrationJobV15Select = `SELECT id::text,org_id::text,idempotency_key,github_repository_id::text,
	github_workflow_path,workflow_revision,dispatch_ref,environment_id::text,desired_checkout_sha,app_id::text,
	desired_generation,pending_checkout_sha,pending_github_workflow_path,pending_workflow_revision,pending_dispatch_ref,
	pending_desired_generation,pending_idempotency_key,status,lease_expires_at,dispatch_requested_at,
	COALESCE(dispatch_ref_observed_sha,''),dispatch_deadline_at,run_deadline_at,github_run_id,reconciliation_lease_expires_at,
	attempt,oci_config_digest,oci_config_media_type,oci_source,oci_revision,oci_annotation_status,
	resolved_registry_id::text,resolved_registry_generation,oci_deadline_at,oci_error_code,oci_attempts,oci_next_attempt_at,
	oci_lease_owner,oci_lease_epoch,oci_lease_expires_at FROM orchestration_jobs`

func scanOrchestrationJobV15(row rowScanner, job *OrchestrationJob) error {
	var ociOwner *string
	var ociEpoch int64
	var ociExpiresAt *time.Time
	if err := row.Scan(&job.ID, &job.OrgID, &job.IdempotencyKey, &job.RepositoryID, &job.WorkflowID,
		&job.WorkflowRevision, &job.GitRef, &job.EnvironmentID, &job.CommitSHA,
		&job.AppID, &job.DesiredGeneration, &job.PendingCommitSHA, &job.PendingWorkflowID,
		&job.PendingWorkflowRevision, &job.PendingGitRef, &job.PendingDesiredGeneration,
		&job.PendingIdempotencyKey, &job.Status, &job.LeaseExpiresAt, &job.DispatchRequestedAt,
		&job.DispatchRefObservedSHA, &job.DispatchDeadlineAt, &job.RunDeadlineAt, &job.GitHubRunID,
		&job.ReconciliationLeaseEnd, &job.Attempt, &job.OCI.ConfigDigest, &job.OCI.ConfigMediaType,
		&job.OCI.Source, &job.OCI.Revision, &job.OCI.AnnotationStatus, &job.OCI.ResolvedRegistryID,
		&job.OCI.ResolvedRegistryGeneration, &job.OCI.Deadline, &job.OCI.ErrorCode, &job.OCI.Attempts,
		&job.OCI.NextAttemptAt, &ociOwner, &ociEpoch, &ociExpiresAt); err != nil {
		return err
	}
	if ociOwner != nil && ociExpiresAt != nil {
		job.OCI.Lease = &LeaseToken{Owner: *ociOwner, Epoch: ociEpoch, ExpiresAt: *ociExpiresAt}
	}
	return nil
}

func (s *Store) GetOrchestrationJob(ctx context.Context, orgID, id string) (OrchestrationJob, error) {
	var job OrchestrationJob
	err := scanOrchestrationJobV15(s.db.QueryRow(ctx, orchestrationJobV15Select+` WHERE org_id=$1 AND id=$2`, orgID, id), &job)
	return job, resourceQueryError("get orchestration job", err)
}

func (s *Store) GetImageResolutionClaim(ctx context.Context, orgID, id string) (ImageResolutionClaim, error) {
	return scanImageResolutionClaim(s.db.QueryRow(ctx, imageResolutionClaimSelect+` WHERE job.org_id=$1 AND job.id=$2`, orgID, id))
}

const imageResolutionClaimSelect = `SELECT
	job.id::text,job.org_id::text,job.idempotency_key,job.github_repository_id::text,job.github_workflow_path,
	job.workflow_revision,job.dispatch_ref,job.environment_id::text,job.desired_checkout_sha,job.app_id::text,
	job.desired_generation,job.pending_checkout_sha,job.pending_github_workflow_path,job.pending_workflow_revision,
	job.pending_dispatch_ref,job.pending_desired_generation,job.pending_idempotency_key,job.status,job.lease_expires_at,
	job.dispatch_requested_at,COALESCE(job.dispatch_ref_observed_sha,''),job.dispatch_deadline_at,job.run_deadline_at,
	job.github_run_id,job.reconciliation_lease_expires_at,job.attempt,job.oci_config_digest,job.oci_config_media_type,
	job.oci_source,job.oci_revision,job.oci_annotation_status,job.resolved_registry_id::text,
	job.resolved_registry_generation,job.oci_deadline_at,job.oci_error_code,job.oci_attempts,job.oci_next_attempt_at,
	job.oci_lease_owner,job.oci_lease_epoch,job.oci_lease_expires_at,
	app.id::text,app.org_id::text,app.project_id::text,app.name,app.slug,app.hostname_label,app.registry_id::text,
	registry.registry_host,app.image_repository,
	app.github_workflow_path,app.github_workflow_revision,app.production_branch,app.annotation_policy,
	app.image_visibility,app.desired_generation,app.cleanup_state,app.created_at,app.deleted_at,
	environment.id::text,environment.org_id::text,environment.app_id::text,environment.branch,environment.kind,
	environment.preview_slug,environment.namespace,environment.planned_hostname,environment.lifecycle_status,
	environment.pending_commit_sha,
	environment.created_at,environment.teardown_requested_at,environment.torn_down_at,
	environment.required_registry_credential_generation,environment.ready_registry_credential_generation,
	environment.registry_credential_state,
	repository.owner_login,repository.name,repository.node_id,environment.desired_generation,
	registry.id::text,registry.org_id::text,registry.provider,registry.registry_host,registry.state,
	registry.active_generation,registry.reconciliation_generation,registry.created_at,registry.updated_at,
	generation.secret_ref
	FROM orchestration_jobs job
	JOIN apps app ON app.org_id=job.org_id AND app.id=job.app_id
	JOIN environments environment ON environment.org_id=job.org_id AND environment.id=job.environment_id AND environment.app_id=app.id
	JOIN github_repositories repository ON repository.org_id=job.org_id AND repository.github_repository_id=job.github_repository_id
	LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
	LEFT JOIN registry_credential_generations generation ON generation.org_id=registry.org_id AND generation.registry_id=registry.id AND generation.generation=registry.active_generation AND generation.state='active'`

func scanImageResolutionClaim(row rowScanner) (ImageResolutionClaim, error) {
	var claim ImageResolutionClaim
	if err := scanOrchestrationJobV15Prefix(row, &claim); err != nil {
		return ImageResolutionClaim{}, resourceQueryError("get image resolution claim", err)
	}
	return claim, nil
}

// scanOrchestrationJobV15Prefix scans the joined claim projection. It is kept
// separate from scanOrchestrationJobV15 because pgx rows cannot be partially
// consumed by nested scanners.
func scanOrchestrationJobV15Prefix(row rowScanner, claim *ImageResolutionClaim) error {
	job := &claim.Job
	var owner *string
	var epoch int64
	var expires *time.Time
	var workflowPath, productionBranch *string
	var registryID, registryOrgID, registryProvider, registryHost, registryState *string
	var registryActiveGeneration, registryReconciliationGeneration *int64
	var registryCreatedAt, registryUpdatedAt *time.Time
	if err := row.Scan(&job.ID, &job.OrgID, &job.IdempotencyKey, &job.RepositoryID, &job.WorkflowID,
		&job.WorkflowRevision, &job.GitRef, &job.EnvironmentID, &job.CommitSHA, &job.AppID, &job.DesiredGeneration,
		&job.PendingCommitSHA, &job.PendingWorkflowID, &job.PendingWorkflowRevision, &job.PendingGitRef,
		&job.PendingDesiredGeneration, &job.PendingIdempotencyKey, &job.Status, &job.LeaseExpiresAt,
		&job.DispatchRequestedAt, &job.DispatchRefObservedSHA, &job.DispatchDeadlineAt, &job.RunDeadlineAt,
		&job.GitHubRunID, &job.ReconciliationLeaseEnd, &job.Attempt, &job.OCI.ConfigDigest, &job.OCI.ConfigMediaType,
		&job.OCI.Source, &job.OCI.Revision, &job.OCI.AnnotationStatus, &job.OCI.ResolvedRegistryID,
		&job.OCI.ResolvedRegistryGeneration, &job.OCI.Deadline, &job.OCI.ErrorCode, &job.OCI.Attempts,
		&job.OCI.NextAttemptAt, &owner, &epoch, &expires,
		&claim.App.ID, &claim.App.OrgID, &claim.App.ProjectID, &claim.App.Name, &claim.App.Slug,
		&claim.App.HostnameLabel, &claim.App.RegistryID, &claim.App.RegistryURL, &claim.App.ImageRepository,
		&workflowPath, &claim.App.GitHubWorkflowRevision, &productionBranch,
		&claim.App.AnnotationPolicy, &claim.App.ImageVisibility, &claim.App.DesiredGeneration, &claim.App.CleanupState,
		&claim.App.CreatedAt, &claim.App.DeletedAt,
		&claim.Environment.ID, &claim.Environment.OrgID, &claim.Environment.AppID, &claim.Environment.Branch,
		&claim.Environment.Kind, &claim.Environment.PreviewSlug, &claim.Environment.Namespace,
		&claim.Environment.PlannedHostname, &claim.Environment.LifecycleStatus, &claim.Environment.PendingCommitSHA,
		&claim.Environment.CreatedAt, &claim.Environment.TeardownRequestedAt,
		&claim.Environment.TornDownAt, &claim.Environment.RequiredRegistryCredentialGeneration,
		&claim.Environment.ReadyRegistryCredentialGeneration, &claim.Environment.RegistryCredentialState,
		&claim.RepositoryOwner, &claim.RepositoryName, &claim.RepositoryNodeID, &claim.EnvironmentDesiredGeneration,
		&registryID, &registryOrgID, &registryProvider, &registryHost, &registryState,
		&registryActiveGeneration, &registryReconciliationGeneration, &registryCreatedAt, &registryUpdatedAt,
		&claim.RegistryCredentialSecretRef); err != nil {
		return err
	}
	if workflowPath != nil {
		claim.App.GitHubWorkflowPath = *workflowPath
	}
	if productionBranch != nil {
		claim.App.ProductionBranch = *productionBranch
	}
	if registryID != nil && registryOrgID != nil && registryProvider != nil && registryHost != nil && registryState != nil && registryActiveGeneration != nil && registryReconciliationGeneration != nil && registryCreatedAt != nil && registryUpdatedAt != nil {
		claim.Registry = &Registry{ID: *registryID, OrgID: *registryOrgID, Provider: *registryProvider,
			RegistryHost: *registryHost, RegistryURL: *registryHost, State: *registryState,
			ActiveGeneration: *registryActiveGeneration, ReconciliationGeneration: *registryReconciliationGeneration,
			CreatedAt: *registryCreatedAt, UpdatedAt: *registryUpdatedAt, HasActiveCredential: claim.RegistryCredentialSecretRef != nil}
	}
	if owner != nil && expires != nil {
		claim.Lease = LeaseToken{Owner: *owner, Epoch: epoch, ExpiresAt: *expires}
		job.OCI.Lease = &claim.Lease
	}
	if job.OCI.Deadline != nil {
		claim.Deadline = *job.OCI.Deadline
	}
	return nil
}

func (s *Store) RecoverExpiredImageResolutionLeases(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalidArgument
	}
	var recovered int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		promoted, err := tx.Exec(ctx, `WITH candidates AS (SELECT id FROM orchestration_jobs
			WHERE status='resolving_image' AND pending_checkout_sha IS NOT NULL
			AND oci_lease_owner IS NOT NULL AND oci_lease_expires_at<=clock_timestamp()
			ORDER BY oci_lease_expires_at,id FOR UPDATE SKIP LOCKED LIMIT $1)
			UPDATE orchestration_jobs job SET status='queued',
			github_repository_id=COALESCE(pending_github_repository_id,github_repository_id),github_workflow_path=COALESCE(pending_github_workflow_path,github_workflow_path),
			workflow_revision=COALESCE(pending_workflow_revision,workflow_revision),dispatch_ref=COALESCE(pending_dispatch_ref,dispatch_ref),desired_checkout_sha=pending_checkout_sha,
			desired_generation=COALESCE(pending_desired_generation,desired_generation),idempotency_key=COALESCE(pending_idempotency_key,idempotency_key),
			pending_github_repository_id=NULL,pending_github_workflow_path=NULL,pending_workflow_revision=NULL,pending_dispatch_ref=NULL,pending_checkout_sha=NULL,pending_desired_generation=NULL,pending_idempotency_key=NULL,
			lease_owner=NULL,lease_expires_at=NULL,dispatch_requested_at=NULL,dispatch_ref_observed_sha=NULL,dispatch_ambiguity_state='none',reconcile_after=NULL,dispatch_deadline_at=NULL,run_deadline_at=NULL,
			reconciliation_owner=NULL,reconciliation_lease_expires_at=NULL,github_dispatch_identity=NULL,github_run_id=NULL,github_run_head_sha=NULL,github_run_status=NULL,github_run_conclusion=NULL,github_status_observed_at=NULL,
			cancellation_state='not_requested',cancellation_requested_at=NULL,superseded_by_job_id=NULL,superseded_at=NULL,
			oci_resolution_state='pending',image_tag_ref=NULL,resolved_image_digest=NULL,immutable_image_ref=NULL,oci_media_type=NULL,oci_config_digest=NULL,oci_config_media_type=NULL,oci_source=NULL,oci_revision=NULL,
			oci_annotation_status='pending',resolved_registry_id=NULL,resolved_registry_generation=NULL,oci_deadline_at=NULL,oci_error_code=NULL,oci_attempts=0,oci_next_attempt_at=clock_timestamp(),oci_lease_owner=NULL,oci_lease_expires_at=NULL,
			deployment_state='not_started',deployment_claim_owner=NULL,deployment_claim_expires_at=NULL,release_id=NULL,terminal_error_code=NULL,terminal_error_message=NULL,terminal_at=NULL,attempt=attempt+1,updated_at=clock_timestamp()
			FROM candidates WHERE job.id=candidates.id`, limit)
		if err != nil {
			return err
		}
		recovered = promoted.RowsAffected()
		remaining := limit - int(recovered)
		if remaining == 0 {
			return nil
		}
		tag, err := tx.Exec(ctx, `WITH candidates AS (SELECT id FROM orchestration_jobs
		WHERE status='resolving_image' AND oci_resolution_state IN ('pending','polling') AND (
			oci_lease_owner IS NOT NULL AND oci_lease_expires_at<=clock_timestamp()
			OR oci_lease_owner IS NULL AND (oci_attempts>=20 OR oci_deadline_at<=clock_timestamp()))
		ORDER BY COALESCE(oci_lease_expires_at,oci_deadline_at,oci_next_attempt_at),id FOR UPDATE SKIP LOCKED LIMIT $1)
		UPDATE orchestration_jobs job SET
			status=CASE WHEN job.oci_deadline_at<=clock_timestamp() OR job.oci_attempts>=20 THEN 'failed' ELSE job.status END,
			oci_resolution_state=CASE WHEN job.oci_deadline_at<=clock_timestamp() OR job.oci_attempts>=20 THEN 'failed' ELSE 'polling' END,
			oci_error_code=CASE WHEN job.oci_deadline_at<=clock_timestamp() THEN 'image_not_published' WHEN job.oci_attempts>=20 THEN 'registry_unavailable' ELSE job.oci_error_code END,
			terminal_error_code=CASE WHEN job.oci_deadline_at<=clock_timestamp() THEN 'image_not_published' WHEN job.oci_attempts>=20 THEN 'oci_attempts_exhausted' ELSE job.terminal_error_code END,
			terminal_error_message=CASE WHEN job.oci_deadline_at<=clock_timestamp() THEN 'image publication deadline reached' WHEN job.oci_attempts>=20 THEN 'OCI resolution attempts exhausted' ELSE job.terminal_error_message END,
			terminal_at=CASE WHEN job.oci_deadline_at<=clock_timestamp() OR job.oci_attempts>=20 THEN clock_timestamp() ELSE job.terminal_at END,
			oci_next_attempt_at=CASE WHEN job.oci_attempts<20 THEN clock_timestamp() ELSE job.oci_next_attempt_at END,
			oci_lease_owner=NULL,oci_lease_expires_at=NULL,updated_at=clock_timestamp()
		FROM candidates WHERE job.id=candidates.id`, remaining)
		if err == nil {
			recovered += tag.RowsAffected()
		}
		return err
	})
	return recovered, err
}

func (s *Store) ListImageResolutionOrgIDs(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT org_id::text FROM orchestration_jobs
		WHERE status='resolving_image' AND oci_resolution_state IN ('pending','polling') AND oci_attempts<20
		AND oci_lease_owner IS NULL AND oci_next_attempt_at<=clock_timestamp()
		ORDER BY org_id::text LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) ClaimImageResolutions(ctx context.Context, orgID, owner string, leaseDuration, publicationWindow time.Duration, excludedEnvironmentIDs []string, limit int) ([]ImageResolutionClaim, error) {
	if orgID == "" || owner == "" || leaseDuration <= 0 || publicationWindow <= 0 || limit < 1 || limit > 100 || len(excludedEnvironmentIDs) > 100 {
		return nil, ErrInvalidArgument
	}
	var claims []ImageResolutionClaim
	err := s.withGitHubEventTxRetry(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH ranked AS (SELECT job.id,job.environment_id,job.oci_next_attempt_at,job.created_at,
			row_number() OVER (PARTITION BY job.environment_id ORDER BY job.oci_next_attempt_at,job.created_at,job.id) AS environment_rank
			FROM orchestration_jobs job
			JOIN apps app ON app.org_id=job.org_id AND app.id=job.app_id AND app.deleted_at IS NULL
			JOIN environments environment ON environment.org_id=job.org_id AND environment.id=job.environment_id
			JOIN github_repositories repository ON repository.org_id=job.org_id AND repository.github_repository_id=job.github_repository_id
			LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
			LEFT JOIN registry_credential_generations generation ON generation.org_id=registry.org_id AND generation.registry_id=registry.id AND generation.generation=registry.active_generation AND generation.state='active'
			WHERE job.org_id=$1 AND job.status='resolving_image' AND job.oci_resolution_state IN ('pending','polling')
			AND job.oci_lease_owner IS NULL AND job.oci_attempts<20 AND job.oci_next_attempt_at<=clock_timestamp()
			AND (job.oci_deadline_at IS NULL OR job.oci_deadline_at>clock_timestamp())
			AND job.github_run_status='completed' AND job.github_run_conclusion='success'
			AND job.pending_checkout_sha IS NULL AND job.desired_generation=environment.desired_generation
			AND environment.lifecycle_status='ready' AND environment.torn_down_at IS NULL
			AND (COALESCE(cardinality($6::uuid[]),0)=0 OR NOT (job.environment_id=ANY($6::uuid[])))
			AND repository.access_state='active'
			AND NOT EXISTS (SELECT 1 FROM orchestration_jobs active
				WHERE active.org_id=job.org_id AND active.environment_id=job.environment_id AND active.id<>job.id
				AND active.status='resolving_image' AND active.oci_lease_owner IS NOT NULL
				AND active.oci_lease_expires_at>clock_timestamp())
			AND app.image_repository IS NOT NULL AND ((app.image_visibility='public' AND app.registry_id IS NULL)
			 OR (app.image_visibility='private' AND registry.state='ready' AND registry.active_generation>0
			 AND generation.generation=registry.active_generation))
		), candidates AS (SELECT job.id FROM orchestration_jobs job JOIN ranked ON ranked.id=job.id
			WHERE ranked.environment_rank=1
			AND pg_try_advisory_xact_lock(hashtextextended('oci-env:'||ranked.environment_id::text,0))
			ORDER BY ranked.oci_next_attempt_at,ranked.created_at,ranked.id FOR UPDATE OF job SKIP LOCKED LIMIT $2),
			claimed AS (UPDATE orchestration_jobs job SET oci_resolution_state='polling',
			oci_deadline_at=COALESCE(job.oci_deadline_at,clock_timestamp()+($5*interval '1 microsecond')),
			oci_attempts=job.oci_attempts+1,oci_lease_owner=$3,oci_lease_epoch=job.oci_lease_epoch+1,
			oci_lease_expires_at=clock_timestamp()+($4*interval '1 microsecond'),updated_at=clock_timestamp()
			FROM candidates WHERE job.id=candidates.id RETURNING job.id)
			SELECT id::text FROM claimed`, orgID, limit, owner, leaseDuration.Microseconds(), publicationWindow.Microseconds(), excludedEnvironmentIDs)
		if err != nil {
			return err
		}
		var claimedIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			claimedIDs = append(claimedIDs, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(claimedIDs) != 0 {
			rows, err = tx.Query(ctx, imageResolutionClaimSelect+` WHERE job.id=ANY($1::uuid[]) ORDER BY job.oci_next_attempt_at,job.created_at,job.id`, claimedIDs)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				claim, err := scanImageResolutionClaim(rows)
				if err != nil {
					return err
				}
				claims = append(claims, claim)
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		if claimImageResolutionFailpoint != nil {
			return claimImageResolutionFailpoint()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *Store) RenewImageResolutionLease(ctx context.Context, orgID, jobID string, token LeaseToken, duration time.Duration) (LeaseToken, error) {
	return s.imageResolutionLease(ctx, orgID, jobID, token, duration, false)
}

func (s *Store) RotateImageResolutionLease(ctx context.Context, orgID, jobID string, token LeaseToken, duration time.Duration) (LeaseToken, error) {
	return s.imageResolutionLease(ctx, orgID, jobID, token, duration, true)
}

func (s *Store) imageResolutionLease(ctx context.Context, orgID, jobID string, token LeaseToken, duration time.Duration, rotate bool) (LeaseToken, error) {
	if orgID == "" || jobID == "" || token.Owner == "" || token.Epoch < 1 || duration <= 0 {
		return LeaseToken{}, ErrInvalidArgument
	}
	epoch := "oci_lease_epoch"
	if rotate {
		epoch = "oci_lease_epoch+1"
	}
	query := fmt.Sprintf(`UPDATE orchestration_jobs SET oci_lease_epoch=%s,oci_lease_expires_at=%s,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND status='resolving_image' AND oci_resolution_state IN ('pending','polling')
		AND oci_lease_owner=$3 AND oci_lease_epoch=$4 AND oci_lease_expires_at>clock_timestamp()
		RETURNING oci_lease_owner,oci_lease_epoch,oci_lease_expires_at`, epoch,
		map[bool]string{true: "clock_timestamp()+($5*interval '1 microsecond')", false: "GREATEST(oci_lease_expires_at,clock_timestamp()+($5*interval '1 microsecond'))"}[rotate])
	var out LeaseToken
	err := s.db.QueryRow(ctx, query, orgID, jobID, token.Owner, token.Epoch, duration.Microseconds()).Scan(&out.Owner, &out.Epoch, &out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseToken{}, ErrLeaseNotOwned
	}
	return out, err
}

func (s *Store) RetryImageResolution(ctx context.Context, p RetryImageResolutionParams) (ImageResolutionWriteResult, error) {
	if p.Delay < 0 || p.Delay > 24*time.Hour || !allowedImageResolutionCode(p.Code) {
		return "", ErrInvalidArgument
	}
	return s.imageResolutionSnapshotWrite(ctx, p.Claim, p.Lease, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orchestration_jobs job SET
		status=CASE WHEN oci_attempts>=20 OR clock_timestamp()+($4*interval '1 microsecond')>=oci_deadline_at THEN 'failed' ELSE status END,
		oci_resolution_state=CASE WHEN oci_attempts>=20 OR clock_timestamp()+($4*interval '1 microsecond')>=oci_deadline_at THEN 'failed' ELSE 'polling' END,
		oci_error_code=CASE WHEN clock_timestamp()+($4*interval '1 microsecond')>=oci_deadline_at THEN 'image_not_published' WHEN oci_attempts>=20 THEN 'registry_unavailable' ELSE $5 END,
		terminal_error_code=CASE WHEN clock_timestamp()+($4*interval '1 microsecond')>=oci_deadline_at THEN 'image_not_published' WHEN oci_attempts>=20 THEN 'oci_attempts_exhausted' ELSE NULL END,
		terminal_error_message=CASE WHEN clock_timestamp()+($4*interval '1 microsecond')>=oci_deadline_at THEN 'image publication deadline reached' WHEN oci_attempts>=20 THEN 'OCI resolution attempts exhausted' ELSE NULL END,
		terminal_at=CASE WHEN oci_attempts>=20 OR clock_timestamp()+($4*interval '1 microsecond')>=oci_deadline_at THEN clock_timestamp() ELSE NULL END,
		oci_next_attempt_at=LEAST(clock_timestamp()+($4*interval '1 microsecond'),oci_deadline_at),
		oci_lease_owner=NULL,oci_lease_expires_at=NULL,updated_at=clock_timestamp()
			FROM apps app,environments environment,github_repositories repository LEFT JOIN registries registry ON registry.org_id=$1 AND registry.id=NULLIF($11,'')::uuid
		WHERE job.org_id=$1 AND job.id=$2 AND job.status='resolving_image' AND job.oci_resolution_state IN ('pending','polling')
		AND oci_deadline_at>clock_timestamp() AND oci_lease_owner=$3
		AND oci_lease_epoch=$6 AND oci_lease_expires_at>clock_timestamp()
		AND job.desired_checkout_sha=$7 AND job.desired_generation=$8 AND job.pending_checkout_sha IS NULL
			AND environment.org_id=job.org_id AND environment.id=job.environment_id AND environment.desired_generation=$9
			AND environment.lifecycle_status=$10 AND $10='ready' AND environment.torn_down_at IS NULL
			AND app.org_id=job.org_id AND app.id=job.app_id AND app.image_repository IS NOT DISTINCT FROM $12 AND app.image_visibility=$13 AND app.registry_id IS NOT DISTINCT FROM NULLIF($11,'')::uuid
		AND repository.org_id=job.org_id AND repository.github_repository_id=job.github_repository_id AND repository.github_repository_id::text=$14 AND repository.node_id=$15
		AND repository.owner_login=$18 AND repository.name=$19
		AND ($13<>'private' OR (registry.id=NULLIF($11,'')::uuid AND registry.active_generation=$16 AND registry.state=$17))`,
			p.Claim.Job.OrgID, p.Claim.Job.ID, p.Lease.Owner, p.Delay.Microseconds(), p.Code, p.Lease.Epoch,
			p.Claim.Job.CommitSHA, p.Claim.Job.DesiredGeneration, p.Claim.EnvironmentDesiredGeneration, p.Claim.Environment.LifecycleStatus,
			nonEmptyStringPointer(p.Claim.App.RegistryID), p.Claim.App.ImageRepository, p.Claim.App.ImageVisibility, p.Claim.Job.RepositoryID, p.Claim.RepositoryNodeID,
			registryGeneration(p.Claim.Registry), registryState(p.Claim.Registry), p.Claim.RepositoryOwner, p.Claim.RepositoryName)
		return tag.RowsAffected(), err
	})
}

func (s *Store) FailImageResolution(ctx context.Context, p FailImageResolutionParams) (ImageResolutionWriteResult, error) {
	if !allowedImageResolutionCode(p.Code) {
		return "", ErrInvalidArgument
	}
	return s.imageResolutionSnapshotWrite(ctx, p.Claim, p.Lease, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orchestration_jobs job SET status='failed',oci_resolution_state='failed',oci_error_code=$4,
		oci_annotation_status=CASE WHEN $4 IN ('oci_annotation_conflict','oci_source_mismatch','oci_revision_mismatch') THEN 'mismatched' ELSE oci_annotation_status END,
		terminal_error_code=$4,terminal_at=clock_timestamp(),oci_lease_owner=NULL,oci_lease_expires_at=NULL,updated_at=clock_timestamp()
			FROM apps app,environments environment,github_repositories repository LEFT JOIN registries registry ON registry.org_id=$1 AND registry.id=NULLIF($10,'')::uuid
		WHERE job.org_id=$1 AND job.id=$2 AND job.status='resolving_image' AND job.oci_resolution_state IN ('pending','polling')
		AND job.oci_lease_owner=$3 AND job.oci_lease_epoch=$5 AND job.oci_lease_expires_at>clock_timestamp()
		AND job.desired_checkout_sha=$6 AND job.desired_generation=$7 AND job.pending_checkout_sha IS NULL
			AND environment.org_id=job.org_id AND environment.id=job.environment_id AND environment.desired_generation=$8
			AND environment.lifecycle_status=$9 AND $9='ready' AND environment.torn_down_at IS NULL
			AND app.org_id=job.org_id AND app.id=job.app_id AND app.image_repository IS NOT DISTINCT FROM $11 AND app.image_visibility=$12 AND app.registry_id IS NOT DISTINCT FROM NULLIF($10,'')::uuid
		AND repository.org_id=job.org_id AND repository.github_repository_id=job.github_repository_id AND repository.github_repository_id::text=$13 AND repository.node_id=$14
		AND repository.owner_login=$17 AND repository.name=$18
		AND ($12<>'private' OR (registry.id=NULLIF($10,'')::uuid AND registry.active_generation=$15 AND registry.state=$16))`,
			p.Claim.Job.OrgID, p.Claim.Job.ID, p.Lease.Owner, p.Code, p.Lease.Epoch, p.Claim.Job.CommitSHA,
			p.Claim.Job.DesiredGeneration, p.Claim.EnvironmentDesiredGeneration, p.Claim.Environment.LifecycleStatus,
			nonEmptyStringPointer(p.Claim.App.RegistryID), p.Claim.App.ImageRepository, p.Claim.App.ImageVisibility, p.Claim.Job.RepositoryID,
			p.Claim.RepositoryNodeID, registryGeneration(p.Claim.Registry), registryState(p.Claim.Registry), p.Claim.RepositoryOwner, p.Claim.RepositoryName)
		return tag.RowsAffected(), err
	})
}

func (s *Store) CompleteImageResolution(ctx context.Context, p CompleteImageResolutionParams) (ImageResolutionWriteResult, error) {
	canonicalRepository := strings.TrimPrefix(p.Repository, "ghcr.io/")
	if p.OrgID == "" || p.JobID == "" || p.Lease.Owner == "" || p.Lease.Epoch < 1 || p.DesiredGeneration < 1 || canonicalRepository == "" || strings.Contains(p.Repository, "://") || p.Digest == "" || p.ConfigDigest == "" || p.ConfigMediaType == "" || (p.AnnotationStatus != "matched" && p.AnnotationStatus != "absent") {
		return "", ErrInvalidArgument
	}
	result := ImageResolutionProcessed
	err := s.withGitHubEventTxRetry(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orchestration_jobs job SET image_tag_ref='ghcr.io/'||$19||':'||$18,resolved_image_digest=$5,
			immutable_image_ref='ghcr.io/'||$19||'@'||$5,oci_media_type=$7,oci_config_digest=$8,oci_config_media_type=$9,
			oci_source=NULLIF($10,''),oci_revision=NULLIF($11,''),oci_annotation_status=$12,
			resolved_registry_id=NULLIF($13,'')::uuid,resolved_registry_generation=$14,oci_error_code=NULL,oci_resolution_state='resolved',
			oci_resolved_at=clock_timestamp(),status=CASE WHEN $15='private' THEN 'resolving_image' ELSE 'deploying' END,
			deployment_state=CASE WHEN $15='private' THEN 'waiting_credential' ELSE 'ready' END,
			oci_lease_owner=NULL,oci_lease_expires_at=NULL,updated_at=clock_timestamp()
			FROM apps app,environments environment,github_repositories repository
			LEFT JOIN registries registry ON registry.org_id=$1 AND registry.id=NULLIF($13,'')::uuid
			LEFT JOIN registry_credential_generations generation ON generation.org_id=registry.org_id AND generation.registry_id=registry.id AND generation.generation=$14
			WHERE job.org_id=$1 AND job.id=$2 AND job.status='resolving_image' AND job.oci_resolution_state IN ('pending','polling')
			AND job.oci_lease_owner=$3 AND job.oci_lease_epoch=$16 AND job.oci_lease_expires_at>clock_timestamp()
			AND job.desired_generation=$17 AND job.desired_checkout_sha=$18 AND job.pending_checkout_sha IS NULL
			AND job.github_repository_id::text=$24
			AND app.org_id=job.org_id AND app.id=job.app_id AND app.deleted_at IS NULL
				AND app.image_repository=$19
			AND $4::text IS NOT NULL AND $6::text IS NOT NULL
			AND app.image_visibility=$15 AND app.registry_id IS NOT DISTINCT FROM NULLIF($13,'')::uuid
			AND environment.org_id=job.org_id AND environment.id=job.environment_id AND environment.app_id=app.id
			AND environment.desired_generation=$17 AND environment.lifecycle_status=$23 AND $23='ready' AND environment.torn_down_at IS NULL
			AND repository.org_id=job.org_id AND repository.github_repository_id=job.github_repository_id AND repository.access_state='active'
			AND repository.owner_login=$20 AND repository.name=$21 AND repository.node_id=$22
		AND (($15='public' AND $13 IS NULL AND $14 IS NULL) OR ($15='private' AND registry.state='ready'
		AND registry.active_generation=$14 AND generation.state='active'
		AND environment.registry_credential_state='pending'
		AND environment.required_registry_credential_generation=$14
		AND environment.ready_registry_credential_generation IS NULL))`, p.OrgID, p.JobID, p.Lease.Owner,
			p.TagReference, p.Digest, p.ImmutableReference, p.ManifestMediaType, p.ConfigDigest, p.ConfigMediaType,
			p.Source, p.Revision, p.AnnotationStatus, nonEmptyStringPointer(p.RegistryID), p.RegistryGeneration, p.Visibility,
			p.Lease.Epoch, p.DesiredGeneration, p.CommitSHA, canonicalRepository, p.RepositoryOwner, p.RepositoryName, p.RepositoryNodeID,
			p.Claim.Environment.LifecycleStatus, p.Claim.Job.RepositoryID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var err error
			result, err = resolveImageResolutionCASMiss(ctx, tx, p.Claim, p.Lease)
			return err
		}
		if completeImageResolutionFailpoint != nil {
			if err := completeImageResolutionFailpoint(); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) SupersedeImageResolution(ctx context.Context, orgID, jobID string, token LeaseToken) error {
	tag, err := s.db.Exec(ctx, `UPDATE orchestration_jobs SET
		status='queued',
		github_repository_id=COALESCE(pending_github_repository_id,github_repository_id),
		github_workflow_path=COALESCE(pending_github_workflow_path,github_workflow_path),workflow_revision=COALESCE(pending_workflow_revision,workflow_revision),
		dispatch_ref=COALESCE(pending_dispatch_ref,dispatch_ref),desired_checkout_sha=COALESCE(pending_checkout_sha,desired_checkout_sha),
		desired_generation=COALESCE(pending_desired_generation,desired_generation),idempotency_key=COALESCE(pending_idempotency_key,idempotency_key),
		pending_github_repository_id=NULL,pending_github_workflow_path=NULL,pending_workflow_revision=NULL,pending_dispatch_ref=NULL,
		pending_checkout_sha=NULL,pending_desired_generation=NULL,pending_idempotency_key=NULL,
		oci_resolution_state='pending',oci_annotation_status='pending',oci_deadline_at=NULL,oci_error_code=NULL,oci_attempts=0,
		oci_next_attempt_at=clock_timestamp(),oci_lease_owner=NULL,oci_lease_expires_at=NULL,
		superseded_at=NULL,terminal_error_code=NULL,terminal_error_message=NULL,terminal_at=NULL,attempt=attempt+1,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND status='resolving_image' AND oci_lease_owner=$3 AND oci_lease_epoch=$4
		AND oci_lease_expires_at>clock_timestamp() AND pending_checkout_sha IS NOT NULL`, orgID, jobID, token.Owner, token.Epoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseNotOwned
	}
	return nil
}

func (s *Store) imageResolutionSnapshotWrite(ctx context.Context, claim ImageResolutionClaim, lease LeaseToken, write func(pgx.Tx) (int64, error)) (ImageResolutionWriteResult, error) {
	result := ImageResolutionProcessed
	err := s.withGitHubEventTxRetry(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		count, err := write(tx)
		if err != nil || count != 0 {
			return err
		}
		result, err = resolveImageResolutionCASMiss(ctx, tx, claim, lease)
		return err
	})
	return result, err
}

func resolveImageResolutionCASMiss(ctx context.Context, tx pgx.Tx, claim ImageResolutionClaim, lease LeaseToken) (ImageResolutionWriteResult, error) {
	var owned, pending bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(oci_lease_owner=$3 AND oci_lease_epoch=$4 AND oci_lease_expires_at>clock_timestamp(),false),pending_checkout_sha IS NOT NULL
		FROM orchestration_jobs WHERE org_id=$1 AND id=$2 FOR UPDATE`, claim.Job.OrgID, claim.Job.ID, lease.Owner, lease.Epoch).Scan(&owned, &pending); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrLeaseNotOwned
		}
		return "", err
	}
	if !owned {
		return "", ErrLeaseNotOwned
	}
	if pending {
		tag, err := tx.Exec(ctx, `UPDATE orchestration_jobs SET status='queued',
			github_repository_id=COALESCE(pending_github_repository_id,github_repository_id),github_workflow_path=COALESCE(pending_github_workflow_path,github_workflow_path),
			workflow_revision=COALESCE(pending_workflow_revision,workflow_revision),dispatch_ref=COALESCE(pending_dispatch_ref,dispatch_ref),
			desired_checkout_sha=pending_checkout_sha,desired_generation=COALESCE(pending_desired_generation,desired_generation),idempotency_key=COALESCE(pending_idempotency_key,idempotency_key),
			pending_github_repository_id=NULL,pending_github_workflow_path=NULL,pending_workflow_revision=NULL,pending_dispatch_ref=NULL,pending_checkout_sha=NULL,pending_desired_generation=NULL,pending_idempotency_key=NULL,
			lease_owner=NULL,lease_expires_at=NULL,dispatch_requested_at=NULL,dispatch_ref_observed_sha=NULL,dispatch_ambiguity_state='none',reconcile_after=NULL,dispatch_deadline_at=NULL,run_deadline_at=NULL,
			reconciliation_owner=NULL,reconciliation_lease_expires_at=NULL,github_dispatch_identity=NULL,github_run_id=NULL,github_run_head_sha=NULL,github_run_status=NULL,github_run_conclusion=NULL,github_status_observed_at=NULL,
			cancellation_state='not_requested',cancellation_requested_at=NULL,superseded_by_job_id=NULL,superseded_at=NULL,
			oci_resolution_state='pending',image_tag_ref=NULL,resolved_image_digest=NULL,immutable_image_ref=NULL,oci_media_type=NULL,oci_config_digest=NULL,oci_config_media_type=NULL,oci_source=NULL,oci_revision=NULL,
			oci_annotation_status='pending',resolved_registry_id=NULL,resolved_registry_generation=NULL,oci_deadline_at=NULL,oci_error_code=NULL,oci_attempts=0,oci_next_attempt_at=clock_timestamp(),oci_lease_owner=NULL,oci_lease_expires_at=NULL,
			deployment_state='not_started',deployment_claim_owner=NULL,deployment_claim_expires_at=NULL,release_id=NULL,terminal_error_code=NULL,terminal_error_message=NULL,terminal_at=NULL,attempt=attempt+1,updated_at=clock_timestamp()
			WHERE org_id=$1 AND id=$2 AND oci_lease_owner=$3 AND oci_lease_epoch=$4`, claim.Job.OrgID, claim.Job.ID, lease.Owner, lease.Epoch)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			return "", ErrLeaseNotOwned
		}
		return ImageResolutionRequeued, nil
	}
	_, err := tx.Exec(ctx, `UPDATE orchestration_jobs SET oci_resolution_state='pending',oci_next_attempt_at=clock_timestamp(),oci_lease_owner=NULL,oci_lease_expires_at=NULL,updated_at=clock_timestamp()
		WHERE org_id=$1 AND id=$2 AND oci_lease_owner=$3 AND oci_lease_epoch=$4`, claim.Job.OrgID, claim.Job.ID, lease.Owner, lease.Epoch)
	return ImageResolutionRefreshRequired, err
}

func registryGeneration(registry *Registry) int64 {
	if registry == nil {
		return 0
	}
	return registry.ActiveGeneration
}

func nonEmptyStringPointer(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}
func registryState(registry *Registry) string {
	if registry == nil {
		return ""
	}
	return registry.State
}

func allowedImageResolutionCode(code string) bool {
	switch code {
	case "image_not_published", "registry_auth_required", "registry_auth_failed", "registry_auth_challenge_invalid",
		"registry_forbidden", "registry_rate_limited", "registry_unavailable", "registry_response_invalid",
		"registry_digest_invalid", "registry_media_type_unsupported", "registry_config_invalid",
		"oci_annotation_conflict", "oci_source_mismatch", "oci_revision_mismatch", "postgres_retryable", "postgres_permanent":
		return true
	}
	return false
}
