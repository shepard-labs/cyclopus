package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestImageResolutionClaimRequiresReadyEnvironmentIntegration(t *testing.T) {
	for name, lifecycle := range map[string]string{"teardown_requested": "teardown_requested", "failed": "failed", "provisioning": "reserved"} {
		t.Run(name, func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			jobID := seedImageResolutionJob(t, dataStore, pool, "lifecycle-"+lifecycle)
			if _, err := pool.Exec(t.Context(), `UPDATE environments SET lifecycle_status=$2 WHERE id=$1`, environmentOneID, lifecycle); err != nil {
				t.Fatal(err)
			}
			claims, err := dataStore.ClaimImageResolutions(t.Context(), orgOneID, "lifecycle-worker", time.Minute, time.Hour, nil, 1)
			if err != nil || len(claims) != 0 {
				t.Fatalf("claims=%+v err=%v", claims, err)
			}
			var attempts int
			if err := pool.QueryRow(t.Context(), `SELECT oci_attempts FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&attempts); err != nil || attempts != 0 {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
}

func TestImageResolutionClaimExcludesEnvironmentIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	jobID := seedImageResolutionJob(t, dataStore, pool, "excluded-environment")
	claims, err := dataStore.ClaimImageResolutions(t.Context(), orgOneID, "excluded-worker", time.Minute, time.Hour, []string{environmentOneID}, 1)
	if err != nil || len(claims) != 0 {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	var attempts int
	if err := pool.QueryRow(t.Context(), `SELECT oci_attempts FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestImageResolutionConcurrentClaimsSerializeEnvironmentIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	firstID := seedImageResolutionJob(t, dataStore, pool, "same-environment-first")
	if _, err := pool.Exec(t.Context(), `UPDATE orchestration_jobs SET status='failed',terminal_error_code='fixture',terminal_at=clock_timestamp() WHERE id=$1`, firstID); err != nil {
		t.Fatal(err)
	}
	seedImageResolutionJob(t, dataStore, pool, "same-environment-second")
	if _, err := pool.Exec(t.Context(), `DROP INDEX orchestration_jobs_scope_single_flight`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE orchestration_jobs DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE orchestration_jobs SET status='resolving_image',terminal_error_code=NULL,terminal_at=NULL WHERE id=$1`, firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE orchestration_jobs ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	stores := []*Store{dataStore, New(pool)}
	owners := []string{"concurrent-one", "concurrent-two"}
	results := make([][]ImageResolutionClaim, len(stores))
	errs := make([]error, len(stores))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = stores[i].ClaimImageResolutions(t.Context(), orgOneID, owners[i], time.Minute, time.Hour, nil, 1)
		}(i)
	}
	close(start)
	wg.Wait()
	claimed := make([]ImageResolutionClaim, 0, 1)
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("claim %d: %v", i, errs[i])
		}
		claimed = append(claimed, results[i]...)
	}
	if len(claimed) != 1 {
		t.Fatalf("concurrent claims=%+v", results)
	}
}

func TestImageResolutionClaimDeduplicatesEnvironmentsWithinBatchIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	firstID := seedImageResolutionJob(t, dataStore, pool, "batch-same-environment-first")
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET status='failed',terminal_error_code='fixture',terminal_at=clock_timestamp() WHERE id=$1`, firstID); err != nil {
		t.Fatal(err)
	}
	siblingID := seedImageResolutionJob(t, dataStore, pool, "batch-same-environment-sibling")
	if _, err := pool.Exec(ctx, `DROP INDEX orchestration_jobs_scope_single_flight`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE orchestration_jobs DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET status='resolving_image',terminal_error_code=NULL,terminal_at=NULL,
		desired_generation=(SELECT desired_generation FROM environments WHERE id=environment_id),
		oci_next_attempt_at=clock_timestamp()-interval '2 seconds' WHERE id=$1`, firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE orchestration_jobs ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}

	const otherEnvironmentID = "00000000-0000-0000-0000-000000000008"
	if _, err := pool.Exec(ctx, `INSERT INTO environments(id,org_id,app_id,branch,kind,preview_slug,namespace,planned_hostname,lifecycle_status,desired_generation)
		VALUES($1,$2,$3,'batch-other','preview','batch-other','batch-other','batch-other.example.test','ready',1)`, otherEnvironmentID, orgOneID, appOneID); err != nil {
		t.Fatal(err)
	}
	otherID := seedImageResolutionJobForEnvironment(t, dataStore, pool, otherEnvironmentID, "batch-other-environment")
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET oci_next_attempt_at=clock_timestamp()-CASE id
		WHEN $1::uuid THEN interval '3 seconds' WHEN $2::uuid THEN interval '2 seconds' ELSE interval '1 second' END
		WHERE id=ANY($3::uuid[])`, firstID, siblingID, []string{firstID, siblingID, otherID}); err != nil {
		t.Fatal(err)
	}
	var dueFirst string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM orchestration_jobs WHERE id=ANY($1::uuid[]) ORDER BY oci_next_attempt_at,created_at,id LIMIT 1`, []string{firstID, siblingID}).Scan(&dueFirst); err != nil || dueFirst != firstID {
		t.Fatalf("same-environment due-first = %s, %v; want %s", dueFirst, err, firstID)
	}

	claims, err := dataStore.ClaimImageResolutions(ctx, orgOneID, "batch-worker", time.Minute, time.Hour, nil, 3)
	if err != nil || len(claims) != 2 {
		t.Fatalf("batch claims = %+v, %v", claims, err)
	}
	claimed := make(map[string]ImageResolutionClaim, len(claims))
	environments := make(map[string]bool, len(claims))
	for _, claim := range claims {
		claimed[claim.Job.ID] = claim
		environments[claim.Environment.ID] = true
	}
	if len(environments) != 2 || claimed[firstID].Job.ID != firstID || claimed[otherID].Job.ID != otherID || claimed[siblingID].Job.ID != "" {
		t.Fatalf("claimed jobs=%v environments=%v", claimed, environments)
	}
	var firstAttempts, siblingAttempts, otherAttempts int
	var firstOwner, siblingOwner, otherOwner *string
	var firstExpiry, siblingExpiry, otherExpiry *time.Time
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT oci_attempts FROM orchestration_jobs WHERE id=$1),
		(SELECT oci_lease_owner FROM orchestration_jobs WHERE id=$1),
		(SELECT oci_lease_expires_at FROM orchestration_jobs WHERE id=$1),
		(SELECT oci_attempts FROM orchestration_jobs WHERE id=$2),
		(SELECT oci_lease_owner FROM orchestration_jobs WHERE id=$2),
		(SELECT oci_lease_expires_at FROM orchestration_jobs WHERE id=$2),
		(SELECT oci_attempts FROM orchestration_jobs WHERE id=$3),
		(SELECT oci_lease_owner FROM orchestration_jobs WHERE id=$3),
		(SELECT oci_lease_expires_at FROM orchestration_jobs WHERE id=$3)`, firstID, siblingID, otherID).
		Scan(&firstAttempts, &firstOwner, &firstExpiry, &siblingAttempts, &siblingOwner, &siblingExpiry, &otherAttempts, &otherOwner, &otherExpiry); err != nil {
		t.Fatal(err)
	}
	if firstAttempts != 1 || firstOwner == nil || firstExpiry == nil || siblingAttempts != 0 || siblingOwner != nil || siblingExpiry != nil || otherAttempts != 1 || otherOwner == nil || otherExpiry == nil {
		t.Fatalf("lease states first=%d/%v/%v sibling=%d/%v/%v other=%d/%v/%v", firstAttempts, firstOwner, firstExpiry, siblingAttempts, siblingOwner, siblingExpiry, otherAttempts, otherOwner, otherExpiry)
	}

	otherStore := New(pool)
	if concurrent, err := otherStore.ClaimImageResolutions(ctx, orgOneID, "concurrent-worker", time.Minute, time.Hour, nil, 3); err != nil || len(concurrent) != 0 {
		t.Fatalf("concurrent claim = %+v, %v", concurrent, err)
	}
	first := claimed[firstID]
	if _, err := dataStore.RetryImageResolution(ctx, RetryImageResolutionParams{Claim: first, Lease: first.Lease, Code: "registry_unavailable"}); err != nil {
		t.Fatal(err)
	}
	next, err := otherStore.ClaimImageResolutions(ctx, orgOneID, "next-worker", time.Minute, time.Hour, nil, 3)
	if err != nil || len(next) != 1 || next[0].Job.ID != siblingID || next[0].Environment.ID != environmentOneID || next[0].Job.OCI.Attempts != 1 || next[0].Lease.Owner != "next-worker" {
		t.Fatalf("next claim = %+v, %v", next, err)
	}
}

func TestImageResolutionClaimDeadlineAttemptsAndRecoveryIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	jobID := seedImageResolutionJob(t, dataStore, pool, "claim-deadline")

	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	claims, err := dataStore.ClaimImageResolutions(ctx, orgOneID, "first", time.Minute, 5*time.Minute, nil, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("first claim = %+v, %v", claims, err)
	}
	first := claims[0]
	if first.Job.ID != jobID || first.Job.OCI.Attempts != 1 || first.Lease.Epoch != 1 || !first.Deadline.After(before) {
		t.Fatalf("first claim = %+v before=%s", first, before)
	}
	if _, err := dataStore.RetryImageResolution(ctx, RetryImageResolutionParams{Claim: first, Lease: first.Lease, Code: "registry_unavailable"}); err != nil {
		t.Fatal(err)
	}

	var deadline time.Time
	for want := 2; want <= 20; want++ {
		claims, err = dataStore.ClaimImageResolutions(ctx, orgOneID, "retry", time.Minute, time.Hour, nil, 1)
		if err != nil || len(claims) != 1 || claims[0].Job.OCI.Attempts != want || claims[0].Deadline != first.Deadline {
			t.Fatalf("claim %d = %+v, %v; first deadline=%s", want, claims, err, first.Deadline)
		}
		deadline = claims[0].Deadline
		if want < 20 {
			if _, err := dataStore.RetryImageResolution(ctx, RetryImageResolutionParams{Claim: claims[0], Lease: claims[0].Lease, Code: "registry_unavailable"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if claims, err = dataStore.ClaimImageResolutions(ctx, orgOneID, "never-21", time.Minute, time.Hour, nil, 1); err != nil || len(claims) != 0 {
		t.Fatalf("attempt 21 claim = %+v, %v", claims, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET oci_lease_expires_at=clock_timestamp()-interval '1 microsecond' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := dataStore.RecoverExpiredImageResolutionLeases(ctx, 1); err != nil || recovered != 1 {
		t.Fatalf("recover exhausted = %d, %v", recovered, err)
	}
	var status, terminal string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,terminal_error_code,oci_attempts FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&status, &terminal, &attempts); err != nil || status != "failed" || terminal != "oci_attempts_exhausted" || attempts != 20 {
		t.Fatalf("exhausted state = %s/%s/%d, %v", status, terminal, attempts, err)
	}
	if deadline.IsZero() {
		t.Fatal("claim deadline was not persisted")
	}

	expiredID := seedImageResolutionJob(t, dataStore, pool, "claim-expired")
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET oci_deadline_at=clock_timestamp(),oci_next_attempt_at=clock_timestamp(),oci_resolution_state='polling' WHERE id=$1`, expiredID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := dataStore.RecoverExpiredImageResolutionLeases(ctx, 1); err != nil || recovered != 1 {
		t.Fatalf("recover expired deadline = %d, %v", recovered, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,terminal_error_code FROM orchestration_jobs WHERE id=$1`, expiredID).Scan(&status, &terminal); err != nil || status != "failed" || terminal != "image_not_published" {
		t.Fatalf("expired deadline state = %s/%s, %v", status, terminal, err)
	}
}

func TestImageResolutionRecoveryPromotesPendingCheckoutIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	jobID := seedImageResolutionJob(t, dataStore, pool, "recovery-pending")
	claim := claimImageResolution(t, dataStore, "expired-worker")
	if _, err := pool.Exec(t.Context(), `UPDATE orchestration_jobs SET pending_github_repository_id=github_repository_id,
		pending_github_workflow_path=github_workflow_path,pending_workflow_revision=workflow_revision,pending_dispatch_ref=dispatch_ref,
		pending_checkout_sha=$2,pending_desired_generation=(SELECT desired_generation FROM environments WHERE id=environment_id),
		pending_idempotency_key='replacement',oci_lease_expires_at=clock_timestamp()-interval '1 microsecond' WHERE id=$1`, jobID, commitSHA('b')); err != nil {
		t.Fatal(err)
	}
	if recovered, err := dataStore.RecoverExpiredImageResolutionLeases(t.Context(), 1); err != nil || recovered != 1 {
		t.Fatalf("recover = %d, %v", recovered, err)
	}
	var status, commit, resolution, annotation, deployment string
	var attempts, orchestrationAttempt int
	var owner *string
	if err := pool.QueryRow(t.Context(), `SELECT status,desired_checkout_sha,oci_resolution_state,oci_annotation_status,deployment_state,
		oci_attempts,attempt,oci_lease_owner FROM orchestration_jobs WHERE id=$1`, jobID).
		Scan(&status, &commit, &resolution, &annotation, &deployment, &attempts, &orchestrationAttempt, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || commit != commitSHA('b') || resolution != "pending" || annotation != "pending" || deployment != "not_started" || attempts != 0 || orchestrationAttempt != claim.Job.Attempt+1 || owner != nil {
		t.Fatalf("promoted = %s/%s/%s/%s/%s attempts=%d/%d owner=%v", status, commit, resolution, annotation, deployment, attempts, orchestrationAttempt, owner)
	}
}

func TestImageResolutionClaimHydrationFailureRollsBackLeaseIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	jobID := seedImageResolutionJob(t, dataStore, pool, "claim-rollback")
	claimImageResolutionFailpoint = func() error { return errors.New("claim scan failpoint") }
	t.Cleanup(func() { claimImageResolutionFailpoint = nil })
	if claims, err := dataStore.ClaimImageResolutions(t.Context(), orgOneID, "rollback-worker", time.Minute, time.Hour, nil, 1); err == nil || len(claims) != 0 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	var owner *string
	var attempts int
	if err := pool.QueryRow(t.Context(), `SELECT oci_lease_owner,oci_attempts FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&owner, &attempts); err != nil || owner != nil || attempts != 0 {
		t.Fatalf("stranded claim = owner:%v attempts:%d err:%v", owner, attempts, err)
	}
}

func TestImageResolutionRetryAndFailCASIntegration(t *testing.T) {
	for _, operation := range []string{"retry", "fail"} {
		for _, drift := range []string{"pending", "config", "teardown_requested", "failed", "reserved"} {
			t.Run(operation+"/"+drift, func(t *testing.T) {
				dataStore, pool := newIntegrationStore(t)
				seedTenant(t, pool, tenantOne())
				jobID := seedImageResolutionJob(t, dataStore, pool, operation+"-"+drift)
				claim := claimImageResolution(t, dataStore, "cas-worker")
				if drift == "pending" {
					_, _ = pool.Exec(t.Context(), `UPDATE orchestration_jobs SET pending_github_repository_id=github_repository_id,
						pending_github_workflow_path=github_workflow_path,pending_workflow_revision=workflow_revision,pending_dispatch_ref=dispatch_ref,
						pending_checkout_sha=$2,pending_desired_generation=(SELECT desired_generation FROM environments WHERE id=environment_id),pending_idempotency_key='new' WHERE id=$1`, jobID, commitSHA('b'))
				} else if drift == "config" {
					_, _ = pool.Exec(t.Context(), `UPDATE apps SET image_repository='acme/changed' WHERE id=$1`, appOneID)
				} else {
					_, _ = pool.Exec(t.Context(), `UPDATE environments SET lifecycle_status=$2 WHERE id=$1`, environmentOneID, drift)
				}
				var result ImageResolutionWriteResult
				var err error
				if operation == "retry" {
					result, err = dataStore.RetryImageResolution(t.Context(), RetryImageResolutionParams{Claim: claim, Lease: claim.Lease, Delay: time.Minute, Code: "registry_unavailable"})
				} else {
					result, err = dataStore.FailImageResolution(t.Context(), FailImageResolutionParams{Claim: claim, Lease: claim.Lease, Code: "registry_auth_failed"})
				}
				want := ImageResolutionRefreshRequired
				if drift == "pending" {
					want = ImageResolutionRequeued
				}
				if err != nil || result != want {
					t.Fatalf("result = %s, %v; want %s", result, err, want)
				}
				var status, resolution string
				var owner *string
				if err := pool.QueryRow(t.Context(), `SELECT status,oci_resolution_state,oci_lease_owner FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&status, &resolution, &owner); err != nil {
					t.Fatal(err)
				}
				if owner != nil || status == "failed" || (drift == "pending" && (status != "queued" || resolution != "pending")) || (drift == "config" && (status != "resolving_image" || resolution != "pending")) {
					t.Fatalf("state = %s/%s owner=%v", status, resolution, owner)
				}
			})
		}
	}
}

func TestImageResolutionLeaseFencingAndCompletionIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	jobID := seedImageResolutionJob(t, dataStore, pool, "lease-fence")
	first := claimImageResolution(t, dataStore, "worker-one")
	other := New(pool)
	if claims, err := other.ClaimImageResolutions(ctx, orgOneID, "worker-two", time.Minute, time.Hour, nil, 1); err != nil || len(claims) != 0 {
		t.Fatalf("unexpired exclusive claim = %+v, %v", claims, err)
	}
	renewed, err := dataStore.RenewImageResolutionLease(ctx, orgOneID, jobID, first.Lease, time.Minute)
	if err != nil || renewed.Epoch != first.Lease.Epoch || !renewed.ExpiresAt.After(first.Lease.ExpiresAt) {
		t.Fatalf("renew = %+v, %v", renewed, err)
	}
	rotated, err := dataStore.RotateImageResolutionLease(ctx, orgOneID, jobID, renewed, time.Minute)
	if err != nil || rotated.Epoch != renewed.Epoch+1 {
		t.Fatalf("rotate = %+v, %v", rotated, err)
	}
	if _, err := dataStore.RetryImageResolution(ctx, RetryImageResolutionParams{Claim: first, Lease: renewed, Delay: time.Second, Code: "registry_unavailable"}); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("old retry error = %v", err)
	}
	if _, err := dataStore.FailImageResolution(ctx, FailImageResolutionParams{Claim: first, Lease: renewed, Code: "registry_unavailable"}); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("old fail error = %v", err)
	}
	oldCompletion := imageResolutionCompleteParams(jobID, renewed)
	oldCompletion.Claim = first
	if _, err := dataStore.CompleteImageResolution(ctx, oldCompletion); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("old complete error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET oci_lease_expires_at=clock_timestamp()-interval '1 microsecond' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := dataStore.RecoverExpiredImageResolutionLeases(ctx, 1); err != nil || recovered != 1 {
		t.Fatalf("recover expired lease = %d, %v", recovered, err)
	}
	second := claimImageResolution(t, other, "worker-two")
	if second.Lease.Owner != "worker-two" || second.Lease.Epoch <= rotated.Epoch {
		t.Fatalf("reclaimed lease = %+v, old=%+v", second.Lease, rotated)
	}
	completion := imageResolutionCompleteParams(jobID, second.Lease)
	completion.Repository = strings.TrimPrefix(*second.App.ImageRepository, "ghcr.io/")
	completion.TagReference = "ghcr.io/" + completion.Repository + ":" + completion.CommitSHA
	completion.ImmutableReference = "ghcr.io/" + completion.Repository + "@" + completion.Digest
	completion.DesiredGeneration = second.Job.DesiredGeneration
	completion.Claim = second
	if _, err := other.CompleteImageResolution(ctx, completion); err != nil {
		t.Fatal(err)
	}
	var status, resolution, deployment, tag, ref, digest, media, configDigest, configMedia, source, revision, annotation string
	var releases int
	if err := pool.QueryRow(ctx, `SELECT status,oci_resolution_state,deployment_state,image_tag_ref,immutable_image_ref,resolved_image_digest,
		oci_media_type,oci_config_digest,oci_config_media_type,oci_source,oci_revision,oci_annotation_status FROM orchestration_jobs WHERE id=$1`, jobID).
		Scan(&status, &resolution, &deployment, &tag, &ref, &digest, &media, &configDigest, &configMedia, &source, &revision, &annotation); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM releases`).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if status != "deploying" || resolution != "resolved" || deployment != "ready" || tag != "ghcr.io/acme/app-one:"+commitSHA('a') ||
		ref != "ghcr.io/acme/app-one@sha256:"+strings.Repeat("d", 64) || digest != "sha256:"+strings.Repeat("d", 64) ||
		media != "application/vnd.oci.image.manifest.v1+json" || configDigest != "sha256:"+strings.Repeat("c", 64) ||
		configMedia != "application/vnd.oci.image.config.v1+json" || source != "acme/one" || revision != commitSHA('a') || annotation != "matched" || releases != 0 {
		t.Fatalf("completion status=%s/%s/%s evidence=%s/%s/%s/%s/%s/%s/%s/%s/%s releases=%d", status, resolution, deployment, tag, ref, digest, media, configDigest, configMedia, source, revision, annotation, releases)
	}
}

func TestCompleteImageResolutionPrivateActiveGenerationIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	jobID := seedImageResolutionJob(t, dataStore, pool, "private-complete")
	registryID := seedActiveImageResolutionRegistry(t, pool)
	claim := claimImageResolution(t, dataStore, "private-worker")
	if claim.Registry == nil || claim.Registry.ID != registryID || claim.RegistryCredentialSecretRef == nil {
		t.Fatalf("private claim = %+v", claim)
	}
	completion := imageResolutionCompleteParams(jobID, claim.Lease)
	completion.Visibility, completion.Repository = "private", "acme/private"
	completion.TagReference = "ghcr.io/" + completion.Repository + ":" + completion.CommitSHA
	completion.ImmutableReference = "ghcr.io/" + completion.Repository + "@sha256:" + strings.Repeat("d", 64)
	completion.RegistryID, completion.RegistryGeneration = &registryID, ptrImageResolution(int64(1))
	completion.DesiredGeneration = claim.Job.DesiredGeneration
	completion.Claim = claim
	if _, err := dataStore.CompleteImageResolution(t.Context(), completion); err != nil {
		t.Fatalf("complete private claim=%+v params=%+v: %v", claim, completion, err)
	}
	var status, resolution, deployment, digest, configDigest, annotation string
	var resolvedRegistry string
	var generation int64
	if err := pool.QueryRow(t.Context(), `SELECT status,oci_resolution_state,deployment_state,resolved_registry_id::text,resolved_registry_generation,
		resolved_image_digest,oci_config_digest,oci_annotation_status FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&status, &resolution, &deployment, &resolvedRegistry, &generation, &digest, &configDigest, &annotation); err != nil || status != "resolving_image" || resolution != "resolved" || deployment != "waiting_credential" || resolvedRegistry != registryID || generation != 1 || digest == "" || configDigest == "" || annotation != "matched" {
		t.Fatalf("private completion = %s/%s/%s/%s/%d, %v", status, resolution, deployment, resolvedRegistry, generation, err)
	}
	var rawJob string
	if err := pool.QueryRow(t.Context(), `SELECT to_jsonb(orchestration_jobs)::text FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&rawJob); err != nil || strings.Contains(rawJob, "credential-sentinel") {
		t.Fatalf("raw credential persisted = %q, %v", rawJob, err)
	}
}

func TestCompleteImageResolutionRejectsMutationWithoutEvidenceIntegration(t *testing.T) {
	for _, test := range []struct {
		name    string
		private bool
		mutate  func(t *testing.T, pool DB, jobID string) bool
	}{
		{"environment generation", false, func(t *testing.T, pool DB, _ string) bool {
			_, err := pool.Exec(t.Context(), `UPDATE environments SET desired_generation=desired_generation+1 WHERE id=$1`, environmentOneID)
			if err != nil {
				return true
			}
			return false
		}},
		{"app repository", false, func(t *testing.T, pool DB, _ string) bool {
			_, err := pool.Exec(t.Context(), `UPDATE apps SET image_repository='acme/other' WHERE id=$1`, appOneID)
			if err != nil {
				t.Fatal(err)
			}
			return false
		}},
		{"app visibility registry", true, func(t *testing.T, pool DB, _ string) bool {
			// The registry update that the transition requires invalidates the
			// live environment credential snapshot until the environment is
			// reset, so both apps_environments_valid and
			// registries_environments_valid must see the end state.
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = tx.Exec(t.Context(), `SET CONSTRAINTS apps_environments_valid DEFERRED`); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(t.Context(), `UPDATE registries SET state='reconciling',checkpoint='rollback_reconciling' WHERE id=(SELECT registry_id FROM apps WHERE id=$1)`, appOneID); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(t.Context(), `UPDATE apps SET image_visibility='public',registry_id=NULL,image_repository='acme/one',github_workflow_path='.github/workflows/deploy.yml',production_branch='main' WHERE id=$1`, appOneID); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(t.Context(), `UPDATE environments SET required_registry_credential_generation=NULL,ready_registry_credential_generation=NULL,registry_credential_state='not_required',registry_credential_checkpoint=NULL,registry_credential_error_code=NULL,registry_credential_error_message=NULL WHERE id=$1`, environmentOneID); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			return false
		}},
		{"github repository identity", false, func(t *testing.T, pool DB, jobID string) bool {
			_, err := pool.Exec(t.Context(), `UPDATE github_repositories SET node_id='changed-node' WHERE org_id=$1 AND github_repository_id=(SELECT github_repository_id FROM orchestration_jobs WHERE id=$2)`, orgOneID, jobID)
			if err != nil {
				t.Fatal(err)
			}
			return false
		}},
		{"registry active generation", true, func(t *testing.T, pool DB, _ string) bool {
			// Retire the active generation before clearing the registry pointer:
			// the unconditional generation consistency triggers require the
			// generation row state and the registry pointer to move together.
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = tx.Exec(t.Context(), `UPDATE registry_credential_generations SET state='retired',retired_at=clock_timestamp() WHERE registry_id=(SELECT registry_id FROM apps WHERE id=$1) AND state='active'`, appOneID); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(t.Context(), `UPDATE registries SET active_generation=0,active_credential_secret_ref=NULL,activated_at=NULL,state='absent',checkpoint=NULL WHERE id=(SELECT registry_id FROM apps WHERE id=$1)`, appOneID); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			return false
		}},
		{"environment teardown", false, func(t *testing.T, pool DB, _ string) bool {
			_, err := pool.Exec(t.Context(), `UPDATE environments SET torn_down_at=clock_timestamp() WHERE id=$1`, environmentOneID)
			if err != nil {
				t.Fatal(err)
			}
			return false
		}},
		{"environment teardown requested", false, func(t *testing.T, pool DB, _ string) bool {
			_, err := pool.Exec(t.Context(), `UPDATE environments SET lifecycle_status='teardown_requested' WHERE id=$1`, environmentOneID)
			if err != nil {
				t.Fatal(err)
			}
			return false
		}},
		{"environment failed", false, func(t *testing.T, pool DB, _ string) bool {
			_, err := pool.Exec(t.Context(), `UPDATE environments SET lifecycle_status='failed' WHERE id=$1`, environmentOneID)
			if err != nil {
				t.Fatal(err)
			}
			return false
		}},
		{"environment provisioning", false, func(t *testing.T, pool DB, _ string) bool {
			_, err := pool.Exec(t.Context(), `UPDATE environments SET lifecycle_status='reserved' WHERE id=$1`, environmentOneID)
			if err != nil {
				t.Fatal(err)
			}
			return false
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			if test.private {
				seedActiveImageResolutionRegistry(t, pool)
			}
			jobID := seedImageResolutionJob(t, dataStore, pool, "mutation-"+test.name)
			claim := claimImageResolution(t, dataStore, "mutation-worker")
			completion := imageResolutionCompleteParams(jobID, claim.Lease)
			completion.DesiredGeneration = claim.Job.DesiredGeneration
			if test.private {
				completion.Visibility, completion.Repository = "private", "acme/private"
				completion.TagReference, completion.ImmutableReference = "ghcr.io/"+completion.Repository+":"+completion.CommitSHA, "ghcr.io/"+completion.Repository+"@sha256:"+strings.Repeat("d", 64)
				completion.RegistryID = claim.App.RegistryID
				completion.RegistryGeneration = ptrImageResolution(int64(1))
			}
			if test.mutate(t, pool, jobID) {
				return
			}
			completion.Claim = claim
			result, err := dataStore.CompleteImageResolution(t.Context(), completion)
			if test.name == "environment generation" && err != nil {
				return
			}
			if err != nil || result != ImageResolutionRefreshRequired {
				t.Fatalf("completion result = %s, %v", result, err)
			}
			assertImageResolutionUnresolved(t, pool, jobID)
		})
	}
}

func TestSupersedeImageResolutionMarksJobSupersededIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	jobID := seedImageResolutionJob(t, dataStore, pool, "superseded")
	claim := claimImageResolution(t, dataStore, "supersede-worker")
	setPendingImageResolutionCheckout(t, pool, jobID, "superseded-replacement")
	claim = currentImageResolutionClaim(t, dataStore, jobID)
	if err := dataStore.SupersedeImageResolution(t.Context(), orgOneID, jobID, claim.Lease); err != nil {
		t.Fatal(err)
	}
	var status string
	var terminal *string
	if err := pool.QueryRow(t.Context(), `SELECT status,terminal_error_code FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&status, &terminal); err != nil || status != "queued" || terminal != nil {
		t.Fatalf("superseded state = %s/%v, %v", status, terminal, err)
	}
}

func TestCompleteImageResolutionRetriesTransactionalFailuresIntegration(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			jobID := seedImageResolutionJob(t, dataStore, pool, "retry-"+code)
			claim := claimImageResolution(t, dataStore, "retry-worker")
			calls := 0
			completeImageResolutionFailpoint = func() error {
				calls++
				if calls == 1 {
					return &pgconn.PgError{Code: code}
				}
				return nil
			}
			t.Cleanup(func() { completeImageResolutionFailpoint = nil })
			completion := imageResolutionCompleteParams(jobID, claim.Lease)
			completion.DesiredGeneration = claim.Job.DesiredGeneration
			completion.Claim = claim
			if _, err := dataStore.CompleteImageResolution(t.Context(), completion); err != nil || calls != 2 {
				t.Fatalf("retry completion = %v, calls=%d", err, calls)
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM orchestration_jobs WHERE id=$1 AND oci_resolution_state='resolved' AND resolved_image_digest IS NOT NULL`, jobID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("resolved evidence = %d, %v", count, err)
			}
		})
	}
	t.Run("exhaustion rolls back evidence", func(t *testing.T) {
		dataStore, pool := newIntegrationStore(t)
		seedTenant(t, pool, tenantOne())
		jobID := seedImageResolutionJob(t, dataStore, pool, "retry-exhausted")
		claim := claimImageResolution(t, dataStore, "retry-worker")
		calls := 0
		completeImageResolutionFailpoint = func() error { calls++; return &pgconn.PgError{Code: "40001"} }
		t.Cleanup(func() { completeImageResolutionFailpoint = nil })
		completion := imageResolutionCompleteParams(jobID, claim.Lease)
		completion.DesiredGeneration = claim.Job.DesiredGeneration
		completion.Claim = claim
		_, err := dataStore.CompleteImageResolution(t.Context(), completion)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40001" || calls != 3 {
			t.Fatalf("exhaustion = %v, calls=%d", err, calls)
		}
		var status, resolution string
		var digest, ref *string
		if err := pool.QueryRow(t.Context(), `SELECT status,oci_resolution_state,resolved_image_digest,immutable_image_ref FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&status, &resolution, &digest, &ref); err != nil || status != "resolving_image" || resolution != "polling" || digest != nil || ref != nil {
			t.Fatalf("transactional rollback state = %s/%s/%v/%v, %v", status, resolution, digest, ref, err)
		}
	})
}

func TestCompleteImageResolutionRejectsSupersededJobIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	jobID := seedImageResolutionJob(t, dataStore, pool, "completion-superseded")
	claim := claimImageResolution(t, dataStore, "completion-worker")
	setPendingImageResolutionCheckout(t, pool, jobID, "completion-replacement")
	claim = currentImageResolutionClaim(t, dataStore, jobID)
	if err := dataStore.SupersedeImageResolution(t.Context(), orgOneID, jobID, claim.Lease); err != nil {
		t.Fatal(err)
	}
	completion := imageResolutionCompleteParams(jobID, claim.Lease)
	completion.Claim = claim
	if _, err := dataStore.CompleteImageResolution(t.Context(), completion); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("superseded completion error = %v", err)
	}
	var status string
	var digest *string
	if err := pool.QueryRow(t.Context(), `SELECT status,resolved_image_digest FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&status, &digest); err != nil || status != "queued" || digest != nil {
		t.Fatalf("superseded evidence = %s/%v, %v", status, digest, err)
	}
}

func seedActiveImageResolutionRegistry(t *testing.T, pool DB) string {
	t.Helper()
	ctx := t.Context()
	registryID, operationID := "00000000-0000-0000-0000-000000000881", "00000000-0000-0000-0000-000000000882"
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	// Insert the registry before its credential generation exists, then activate
	// it once the operation and generation rows are in place: the unconditional
	// enforce_registry_credential_references trigger requires the generation row
	// to exist before active_generation references it.
	if _, err = tx.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host,state) VALUES($1,$2,'ghcr','ghcr.io','absent')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,candidate_generation,idempotency_key,terminal_at) VALUES($1,$2,$3,'create','succeeded','activated',1,'image-resolution-registry',clock_timestamp())`, operationID, orgOneID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id,validated_at,activated_at) VALUES($1,$2,1,'k8s-secret://cyclopus/credential-sentinel#dockerconfigjson','active',$3,clock_timestamp(),clock_timestamp())`, orgOneID, registryID, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE registries SET state='ready',checkpoint='activated',active_generation=1,active_credential_secret_ref='k8s-secret://cyclopus/credential-sentinel#dockerconfigjson',activated_at=clock_timestamp() WHERE id=$1`, registryID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Binding the app to the registry flips its environments to private, which
	// the apps_environments_valid constraint trigger (INITIALLY IMMEDIATE)
	// asserts against a consistent credential snapshot, so defer it while the
	// environment is moved to the matching pending state in one transaction.
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET CONSTRAINTS apps_environments_valid DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE apps SET registry_id=$2,image_repository='acme/private',image_visibility='private' WHERE id=$1`, appOneID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE environments SET required_registry_credential_generation=1,ready_registry_credential_generation=NULL,registry_credential_state='pending' WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return registryID
}

func TestCompleteImageResolutionPrivateActiveCredentialReadyEnvironmentIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	jobID := seedImageResolutionJob(t, dataStore, pool, "private-ready-completion")
	registryID := seedActiveImageResolutionRegistry(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE environments SET ready_registry_credential_generation=1,registry_credential_state='ready',registry_credential_checkpoint='activated' WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	claim := claimImageResolution(t, dataStore, "private-ready-worker")
	completion := imageResolutionCompleteParams(jobID, claim.Lease)
	completion.Claim, completion.Visibility, completion.Repository, completion.RegistryID, completion.RegistryGeneration = claim, "private", "acme/private", &registryID, ptrImageResolution(1)
	completion.TagReference = "ghcr.io/" + completion.Repository + ":" + completion.CommitSHA
	completion.ImmutableReference = "ghcr.io/" + completion.Repository + "@sha256:" + strings.Repeat("d", 64)
	completion.DesiredGeneration = claim.Job.DesiredGeneration
	if result, err := dataStore.CompleteImageResolution(t.Context(), completion); err != nil || result != ImageResolutionProcessed {
		t.Fatalf("complete private ready = %q, %v", result, err)
	}
	var status, deployment string
	if err := pool.QueryRow(t.Context(), `SELECT status,deployment_state FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&status, &deployment); err != nil || status != "deploying" || deployment != "ready" {
		t.Fatalf("private ready completion = %q/%q, %v", status, deployment, err)
	}
}

func TestGetImportDeploymentStatusComposesAuthoritativeReleaseTerminalStateIntegration(t *testing.T) {
	for _, test := range []struct {
		name, releaseStatus, wantStatus, wantDeployment string
	}{
		{name: "live", releaseStatus: "live", wantStatus: "completed", wantDeployment: "live"},
		{name: "superseded", releaseStatus: "superseded", wantStatus: "completed", wantDeployment: "live"},
		{name: "failed", releaseStatus: "failed", wantStatus: "failed", wantDeployment: "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			jobID := seedImageResolutionJob(t, dataStore, pool, "import-status-"+test.name)
			claim := claimImageResolution(t, dataStore, "status-worker")
			completion := imageResolutionCompleteParams(jobID, claim.Lease)
			completion.Claim = claim
			if _, err := dataStore.CompleteImageResolution(t.Context(), completion); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO releases(id,org_id,environment_id,image_ref,kind,config_snapshot,config_hash,status,desired_generation,orchestration_job_id,provenance,failure_reason)
				VALUES($1,$2,$3,$4,'deploy',$5,repeat('a',64),$6,$7,$1,'orchestration',$8)`, jobID, orgOneID, environmentOneID,
				completion.ImmutableReference, testReleaseSnapshot(completion.ImmutableReference), test.releaseStatus, claim.Job.DesiredGeneration,
				map[bool]any{true: "rollout failed", false: nil}[test.releaseStatus == "failed"]); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE orchestration_jobs SET release_id=$1 WHERE id=$1`, jobID); err != nil {
				t.Fatal(err)
			}
			if test.releaseStatus == "superseded" {
				if _, err := pool.Exec(t.Context(), `UPDATE environments SET lifecycle_status='failed',lifecycle_error_code='later_environment_failure',
					lifecycle_error_message='a newer release failed' WHERE id=$1`, environmentOneID); err != nil {
					t.Fatal(err)
				}
			}
			status, err := dataStore.GetImportDeploymentStatus(t.Context(), orgOneID, jobID)
			if err != nil || status.Status != test.wantStatus || status.DeploymentState != test.wantDeployment ||
				test.releaseStatus == "superseded" && (status.TerminalErrorCode != nil || status.TerminalErrorMessage != nil) {
				t.Fatalf("status = %+v, %v", status, err)
			}
		})
	}
}

func assertImageResolutionUnresolved(t *testing.T, pool DB, jobID string) {
	t.Helper()
	var status, resolution string
	var digest, ref, owner *string
	if err := pool.QueryRow(t.Context(), `SELECT status,oci_resolution_state,resolved_image_digest,immutable_image_ref,oci_lease_owner FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&status, &resolution, &digest, &ref, &owner); err != nil || status != "resolving_image" || resolution != "pending" || digest != nil || ref != nil || owner != nil {
		t.Fatalf("unexpected evidence state = %s/%s/%v/%v owner=%v, %v", status, resolution, digest, ref, owner, err)
	}
}

func ptrImageResolution(value int64) *int64 { return &value }

func setPendingImageResolutionCheckout(t *testing.T, pool DB, jobID, key string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `UPDATE orchestration_jobs SET pending_github_repository_id=github_repository_id,
		pending_github_workflow_path=github_workflow_path,pending_workflow_revision=workflow_revision,pending_dispatch_ref=dispatch_ref,
		pending_checkout_sha=$2,pending_desired_generation=(SELECT desired_generation FROM environments WHERE id=environment_id),pending_idempotency_key=$3
		WHERE id=$1`, jobID, commitSHA('b'), key); err != nil {
		t.Fatal(err)
	}
}

func currentImageResolutionClaim(t *testing.T, dataStore *Store, jobID string) ImageResolutionClaim {
	t.Helper()
	claim, err := dataStore.GetImageResolutionClaim(t.Context(), orgOneID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func seedImageResolutionJob(t *testing.T, dataStore *Store, pool DB, key string) string {
	return seedImageResolutionJobForEnvironment(t, dataStore, pool, environmentOneID, key)
}

func seedImageResolutionJobForEnvironment(t *testing.T, dataStore *Store, pool DB, environmentID, key string) string {
	t.Helper()
	ctx := t.Context()
	if _, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID, repositoryOneID, environmentID, "", commitSHA('a'), key)); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `UPDATE orchestration_jobs SET status='resolving_image',github_run_status='completed',github_run_conclusion='success',github_run_head_sha=desired_checkout_sha
		WHERE org_id=$1 AND idempotency_key=$2 RETURNING id::text`, orgOneID, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func claimImageResolution(t *testing.T, dataStore *Store, owner string) ImageResolutionClaim {
	t.Helper()
	claims, err := dataStore.ClaimImageResolutions(t.Context(), orgOneID, owner, time.Minute, time.Hour, nil, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	return claims[0]
}

func imageResolutionCompleteParams(jobID string, lease LeaseToken) CompleteImageResolutionParams {
	return CompleteImageResolutionParams{OrgID: orgOneID, JobID: jobID, Repository: "acme/app-one", TagReference: "ghcr.io/acme/app-one:" + commitSHA('a'),
		Digest: "sha256:" + strings.Repeat("d", 64), ImmutableReference: "ghcr.io/acme/app-one@sha256:" + strings.Repeat("d", 64),
		ManifestMediaType: "application/vnd.oci.image.manifest.v1+json", ConfigDigest: "sha256:" + strings.Repeat("c", 64), ConfigMediaType: "application/vnd.oci.image.config.v1+json",
		Source: "acme/one", Revision: commitSHA('a'), AnnotationStatus: "matched", DesiredGeneration: 1, CommitSHA: commitSHA('a'), Visibility: "public", Lease: lease,
		RepositoryOwner: "acme", RepositoryName: "one", RepositoryNodeID: "node-one"}
}
