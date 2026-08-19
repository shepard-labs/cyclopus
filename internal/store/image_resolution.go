package store

import (
	"context"
	"time"
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
	Job         OrchestrationJob
	App         App
	Environment Environment
	Registry    *Registry
	Lease       LeaseToken
	Deadline    time.Time
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
		&job.WorkflowRevision, &job.GitRef, &job.EnvironmentID, &job.CommitSHA, &job.AppID,
		&job.DesiredGeneration, &job.PendingCommitSHA, &job.PendingWorkflowID, &job.PendingWorkflowRevision,
		&job.PendingGitRef, &job.PendingDesiredGeneration, &job.PendingIdempotencyKey, &job.Status,
		&job.LeaseExpiresAt, &job.DispatchRequestedAt, &job.DispatchRefObservedSHA, &job.DispatchDeadlineAt,
		&job.RunDeadlineAt, &job.GitHubRunID, &job.ReconciliationLeaseEnd, &job.Attempt,
		&job.OCI.ConfigDigest, &job.OCI.ConfigMediaType, &job.OCI.Source, &job.OCI.Revision,
		&job.OCI.AnnotationStatus, &job.OCI.ResolvedRegistryID, &job.OCI.ResolvedRegistryGeneration, &job.OCI.Deadline,
		&job.OCI.ErrorCode, &job.OCI.Attempts, &job.OCI.NextAttemptAt, &ociOwner, &ociEpoch, &ociExpiresAt); err != nil {
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
	job, err := s.GetOrchestrationJob(ctx, orgID, id)
	if err != nil {
		return ImageResolutionClaim{}, err
	}
	app, err := s.GetApp(ctx, orgID, job.AppID)
	if err != nil {
		return ImageResolutionClaim{}, err
	}
	environment, err := s.GetEnvironment(ctx, orgID, job.EnvironmentID)
	if err != nil {
		return ImageResolutionClaim{}, err
	}
	claim := ImageResolutionClaim{Job: job, App: app, Environment: environment}
	if app.RegistryID != nil {
		registry, err := s.GetRegistry(ctx, orgID, *app.RegistryID)
		if err != nil {
			return ImageResolutionClaim{}, err
		}
		claim.Registry = &registry
	}
	if job.OCI.Lease != nil {
		claim.Lease = *job.OCI.Lease
	}
	if job.OCI.Deadline != nil {
		claim.Deadline = *job.OCI.Deadline
	}
	return claim, nil
}
