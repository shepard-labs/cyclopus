package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	orgOneID         = "00000000-0000-0000-0000-000000000001"
	projectOneID     = "00000000-0000-0000-0000-000000000002"
	appOneID         = "00000000-0000-0000-0000-000000000003"
	environmentOneID = "00000000-0000-0000-0000-000000000004"
	releaseOneID     = "00000000-0000-0000-0000-000000000005"
	releaseTwoID     = "00000000-0000-0000-0000-000000000006"
	repositoryOneID  = "00000000-0000-0000-0000-000000000007"
	orgTwoID         = "00000000-0000-0000-0000-000000000011"
	projectTwoID     = "00000000-0000-0000-0000-000000000012"
	appTwoID         = "00000000-0000-0000-0000-000000000013"
	environmentTwoID = "00000000-0000-0000-0000-000000000014"
	releaseThreeID   = "00000000-0000-0000-0000-000000000015"
	repositoryTwoID  = "00000000-0000-0000-0000-000000000017"
)

func TestDurationValidation(t *testing.T) {
	store := New(nil)
	if _, err := store.ClaimIdempotency(context.Background(), IdempotencyClaimParams{
		RequestHash: requestHash("request"), LeaseDuration: 0, Retention: time.Hour,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ClaimIdempotency() error = %v", err)
	}
	if _, err := store.ClaimOrchestration(context.Background(), orgOneID, "worker", 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ClaimOrchestration() error = %v", err)
	}
	if err := store.BeginDispatchReconciliation(context.Background(), orgOneID, releaseOneID, "worker", commitSHA('a'), ".github/workflows/deploy.yml", commitSHA('b'), 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("BeginDispatchReconciliation() error = %v", err)
	}
}

func TestOrchestrationHardDeadlines(t *testing.T) {
	if orchestrationDispatchObservationDeadline != 5*time.Minute {
		t.Fatalf("dispatch observation deadline = %s, want 5m", orchestrationDispatchObservationDeadline)
	}
	if orchestrationRunDeadline != 45*time.Minute {
		t.Fatalf("run deadline = %s, want 45m", orchestrationRunDeadline)
	}
}

func TestPreviewDisabledAcknowledgesWithoutEnvironmentIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	if _, err := pool.Exec(t.Context(), `UPDATE apps SET previews_enabled=false WHERE id=$1`, appOneID); err != nil {
		t.Fatal(err)
	}
	result, err := dataStore.AcceptPreviewPush(t.Context(), previewPushParams("disabled", "feature/disabled", commitSHA('1')))
	if err != nil || !result.Dropped || result.Disposition != "dropped" {
		t.Fatalf("AcceptPreviewPush() = %+v, %v", result, err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM environments WHERE app_id=$1 AND kind='preview'`, appOneID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("preview count = %d, %v", count, err)
	}
}

func TestPreviewConcurrentCapAndIdentityReuseIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	if _, err := pool.Exec(t.Context(), `UPDATE apps SET max_preview_environments=1 WHERE id=$1`, appOneID); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsOut := make(chan error, 2)
	var wait sync.WaitGroup
	for index, branch := range []string{"feature/one", "feature/two"} {
		wait.Add(1)
		go func(index int, branch string) {
			defer wait.Done()
			<-start
			_, err := dataStore.AcceptPreviewPush(context.Background(), previewPushParams(fmt.Sprintf("cap-%d", index), branch, commitSHA(byte('1'+index))))
			errorsOut <- err
		}(index, branch)
	}
	close(start)
	wait.Wait()
	close(errorsOut)
	succeeded, capped := 0, 0
	for err := range errorsOut {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrFailedPrecondition) {
			capped++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || capped != 1 {
		t.Fatalf("concurrent cap = succeeded:%d capped:%d", succeeded, capped)
	}
	environments, err := dataStore.ListEnvironments(t.Context(), orgOneID, appOneID, false)
	if err != nil || len(environments) != 2 {
		t.Fatalf("environments = %d, %v", len(environments), err)
	}
	var preview Environment
	for _, environment := range environments {
		if environment.Kind == "preview" {
			preview = environment
		}
	}
	result, err := dataStore.AcceptPreviewPush(t.Context(), previewPushParams("reuse", preview.Branch, commitSHA('9')))
	if err != nil || result.Environment.ID != preview.ID || result.Environment.PreviewSlug == nil || *result.Environment.PreviewSlug != *preview.PreviewSlug {
		t.Fatalf("reuse = %+v, %v", result, err)
	}
}

func TestPreviewTornDownBranchGetsFreshIdentityAndNoSlugReuseIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	first, err := dataStore.AcceptPreviewPush(t.Context(), previewPushParams("first", "feature/recreate", commitSHA('1')))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE environments SET torn_down_at=now() WHERE id=$1`, first.Environment.ID); err != nil {
		t.Fatal(err)
	}
	secondParams := previewPushParams("second", "feature/recreate", commitSHA('2'))
	secondParams.Identities[0].Slug = *first.Environment.PreviewSlug
	secondParams.Identities[0].Namespace = first.Environment.Namespace
	second, err := dataStore.AcceptPreviewPush(t.Context(), secondParams)
	if err != nil || second.Environment.ID == first.Environment.ID || second.Environment.PreviewSlug == nil || *second.Environment.PreviewSlug == *first.Environment.PreviewSlug {
		t.Fatalf("fresh identity = %+v, %v", second, err)
	}
}

func TestPreviewPolicyBlockedCoalescesWithoutDispatchAndStaleLifecycleFenceIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	first, err := dataStore.AcceptPreviewPush(t.Context(), previewPushParams("blocked-one", "feature/blocked", commitSHA('1')))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE environments SET lifecycle_status='policy_blocked' WHERE id=$1`, first.Environment.ID); err != nil {
		t.Fatal(err)
	}
	second, err := dataStore.AcceptPreviewPush(t.Context(), previewPushParams("blocked-two", "feature/blocked", commitSHA('2')))
	if err != nil || second.Disposition != "queued" {
		t.Fatalf("blocked coalesce = %+v, %v", second, err)
	}
	if jobs, err := dataStore.ListOrchestrationOrgIDs(t.Context()); err != nil || len(jobs) != 0 {
		t.Fatalf("blocked jobs became dispatchable: %v, %v", jobs, err)
	}
	job, err := dataStore.ClaimEnvironmentLifecycleJob(t.Context(), orgOneID, "worker-one", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE environment_lifecycle_jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	newJob, err := dataStore.ClaimEnvironmentLifecycleJob(t.Context(), orgOneID, "worker-two", time.Minute)
	if err != nil || newJob.Fence <= job.Fence {
		t.Fatalf("new lifecycle fence = %+v, %v", newJob, err)
	}
	if err := dataStore.CheckpointEnvironmentLifecycle(t.Context(), job, "stale"); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale lifecycle fence error = %v", err)
	}
}

func TestPreviewActiveDeployPushUsesCanonicalGenerationAndIdempotencyIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	first, err := dataStore.AcceptPreviewPush(ctx, previewPushParams("preview-active-one", "feature/active", commitSHA('1')))
	if err != nil {
		t.Fatal(err)
	}
	seedRelease(t, pool, orgOneID, first.Environment.ID, releaseOneID, "deploying")
	if _, err := dataStore.AcquireDeployLease(ctx, orgOneID, first.Environment.ID, releaseOneID); err != nil {
		t.Fatal(err)
	}
	var firstGeneration int64
	if err := pool.QueryRow(ctx, `SELECT desired_generation FROM orchestration_jobs WHERE environment_id=$1`, first.Environment.ID).Scan(&firstGeneration); err != nil {
		t.Fatal(err)
	}
	params := previewPushParams("preview-active-two", "feature/active", commitSHA('2'))
	second, err := dataStore.AcceptPreviewPush(ctx, params)
	if err != nil || second.Disposition != "coalesced" {
		t.Fatalf("active preview push = %+v, %v", second, err)
	}
	var appGeneration, environmentGeneration, acceptedGeneration int64
	var pendingSHA, pendingKey string
	if err := pool.QueryRow(ctx, `SELECT app.desired_generation,environment.desired_generation,
		job.pending_checkout_sha,job.pending_idempotency_key,request.accepted_generation
		FROM environments environment JOIN apps app ON app.id=environment.app_id
		JOIN orchestration_jobs job ON job.environment_id=environment.id
		JOIN orchestration_push_requests request ON request.org_id=job.org_id AND request.idempotency_key=$2
		WHERE environment.id=$1`, first.Environment.ID, params.IdempotencyKey).Scan(&appGeneration,
		&environmentGeneration, &pendingSHA, &pendingKey, &acceptedGeneration); err != nil {
		t.Fatal(err)
	}
	if acceptedGeneration != firstGeneration+1 || appGeneration != 1 || environmentGeneration != acceptedGeneration ||
		pendingSHA != params.CommitSHA || pendingKey != params.IdempotencyKey {
		t.Fatalf("canonical preview generation = first:%d accepted:%d app:%d environment:%d pending:%s/%s",
			firstGeneration, acceptedGeneration, appGeneration, environmentGeneration, pendingSHA, pendingKey)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET deployment_state='waiting_credential'
		WHERE environment_id=$1 AND desired_generation=$2`, first.Environment.ID, firstGeneration); err == nil {
		t.Fatal("old preview orchestration generation was not fenced")
	}
	if _, err := dataStore.AcceptPreviewPush(ctx, params); err != nil {
		t.Fatal(err)
	}
	var replayGeneration int64
	if err := pool.QueryRow(ctx, `SELECT desired_generation FROM environments WHERE id=$1`, first.Environment.ID).Scan(&replayGeneration); err != nil {
		t.Fatal(err)
	}
	if replayGeneration != acceptedGeneration {
		t.Fatalf("preview replay advanced generation = %d, want %d", replayGeneration, acceptedGeneration)
	}
}

func previewPushParams(key, branch, commit string) PreviewPushParams {
	identities := make([]PreviewIdentity, 0, PreviewIdentityAttempts)
	for index := range PreviewIdentityAttempts {
		identities = append(identities, PreviewIdentity{
			EnvironmentID: fmt.Sprintf("00000000-0000-0000-1000-%012d", index+1),
			Slug:          fmt.Sprintf("calm-otter-%04x", index),
			Namespace:     fmt.Sprintf("preview-calm-otter-%04x", index),
		})
	}
	return PreviewPushParams{OrgID: orgOneID, AppID: appOneID, Branch: branch, CommitSHA: commit,
		IdempotencyKey: key, OrchestrationJobID: "00000000-0000-0000-2000-" + requestHash(key)[:12], Identities: identities,
		Audit: &AuditEvent{ID: "00000000-0000-0000-3000-" + requestHash(key)[:12]}}
}

func TestOrchestrationResolutionValidation(t *testing.T) {
	for _, conclusion := range []string{
		"success", "failure", "neutral", "cancelled", "skipped", "timed_out",
		"action_required", "stale",
	} {
		if !validOrchestrationResolution(conclusion, nil) {
			t.Fatalf("terminal conclusion %q was rejected", conclusion)
		}
	}
	for _, code := range []string{"github_run_missing", "github_run_timed_out"} {
		if !validOrchestrationResolution("", &code) {
			t.Fatalf("conclusionless recovery %q was rejected", code)
		}
	}
	for _, input := range []struct {
		conclusion string
		code       *string
	}{
		{}, {conclusion: "unknown"}, {code: stringPointer("github_run_failed")},
	} {
		if validOrchestrationResolution(input.conclusion, input.code) {
			t.Fatalf("invalid resolution %+v was accepted", input)
		}
	}
}

func TestIdempotencyLeaseTakeoverReplayConflictAndTenantIsolationIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "pending")
	seedRelease(t, pool, orgTwoID, environmentTwoID, releaseThreeID, "pending")
	ctx := context.Background()
	hash := requestHash("request-one")
	claim := idempotencyClaim(orgOneID, "owner-one", hash)

	claimed, err := store.ClaimIdempotency(ctx, claim)
	if err != nil || claimed.Disposition != IdempotencyClaimed || claimed.LeaseExpiresAt == nil {
		t.Fatalf("initial claim = %+v, %v", claimed, err)
	}
	inProgress, err := store.ClaimIdempotency(ctx, idempotencyClaim(orgOneID, "owner-two", hash))
	if err != nil || inProgress.Disposition != IdempotencyInProgress {
		t.Fatalf("active claim = %+v, %v", inProgress, err)
	}
	conflict, err := store.ClaimIdempotency(ctx, idempotencyClaim(orgOneID, "owner-two", requestHash("different")))
	if err != nil || conflict.Disposition != IdempotencyConflict {
		t.Fatalf("conflicting claim = %+v, %v", conflict, err)
	}
	tenantTwoClaim, err := store.ClaimIdempotency(ctx, idempotencyClaim(orgTwoID, "owner-two", hash))
	if err != nil || tenantTwoClaim.Disposition != IdempotencyClaimed {
		t.Fatalf("cross-tenant claim = %+v, %v", tenantTwoClaim, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE api_idempotency_records
		SET lease_expires_at = now() - interval '1 second'
		WHERE org_id = $1 AND scope = 'deploy' AND idempotency_key = 'same-key'`, orgOneID); err != nil {
		t.Fatal(err)
	}
	taken, err := store.ClaimIdempotency(ctx, idempotencyClaim(orgOneID, "owner-two", hash))
	if err != nil || taken.Disposition != IdempotencyClaimed {
		t.Fatalf("stale takeover = %+v, %v", taken, err)
	}
	resourceType := "release"
	resourceID := releaseOneID
	audit := testAudit("00000000-0000-0000-0000-000000000021", "deploy.requested", releaseOneID)
	complete := CompleteIdempotencyParams{
		OrgID: orgOneID, Scope: "deploy", Key: "same-key", RequestHash: hash,
		Owner: "owner-two", Status: "completed", StatusCode: 202,
		Body: json.RawMessage(`{"release":"one"}`), ResourceType: &resourceType,
		ResourceID: &resourceID, Audit: audit,
	}
	if err := store.CompleteIdempotency(ctx, complete); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteIdempotency(ctx, CompleteIdempotencyParams{
		OrgID: orgTwoID, Scope: "deploy", Key: "same-key", RequestHash: hash,
		Owner: "owner-two", Status: "completed", StatusCode: 202,
		Body: json.RawMessage(`{"release":"three"}`), ResourceType: &resourceType,
		ResourceID: stringPointer(releaseThreeID), Audit: testAudit("00000000-0000-0000-0000-000000000022", "deploy.requested", releaseThreeID),
	}); err != nil {
		t.Fatal(err)
	}
	replay, err := store.ClaimIdempotency(ctx, idempotencyClaim(orgOneID, "owner-three", hash))
	var replayBody map[string]string
	if err == nil {
		err = json.Unmarshal(replay.Body, &replayBody)
	}
	if err != nil || replay.Disposition != IdempotencyReplay || replay.StatusCode != 202 ||
		replayBody["release"] != "one" || replay.ResourceID == nil || *replay.ResourceID != releaseOneID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	assertCount(t, pool, "audit_events", 2)
}

func TestIdempotencyTakeoverUsesElapsedWallClockAfterRowLockWaitIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	claim := idempotencyClaim(orgOneID, "owner-one", requestHash("wall-clock"))
	claim.LeaseDuration = 400 * time.Millisecond
	claim.Retention = 800 * time.Millisecond
	claimed, err := dataStore.ClaimIdempotency(t.Context(), claim)
	if err != nil || claimed.LeaseExpiresAt == nil {
		t.Fatalf("initial claim = %+v, %v", claimed, err)
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM api_idempotency_records WHERE org_id=$1 AND scope=$2 AND idempotency_key=$3 FOR UPDATE`, claim.OrgID, claim.Scope, claim.Key); err != nil {
		t.Fatal(err)
	}
	result := make(chan IdempotencyResult, 1)
	errs := make(chan error, 1)
	go func() {
		claim.Owner = "owner-two"
		value, err := dataStore.ClaimIdempotency(context.Background(), claim)
		result <- value
		errs <- err
	}()
	time.Sleep(550 * time.Millisecond)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	taken := <-result
	if err := <-errs; err != nil || taken.Disposition != IdempotencyClaimed || taken.LeaseExpiresAt == nil || !taken.LeaseExpiresAt.After(time.Now()) {
		t.Fatalf("takeover after lock wait = %+v, %v", taken, err)
	}
	var retentionExpiresAt time.Time
	if err := pool.QueryRow(t.Context(), `SELECT expires_at FROM api_idempotency_records WHERE org_id=$1 AND scope=$2 AND idempotency_key=$3`, claim.OrgID, claim.Scope, claim.Key).Scan(&retentionExpiresAt); err != nil {
		t.Fatal(err)
	}
	if retentionExpiresAt.Before(time.Now().Add(650 * time.Millisecond)) {
		t.Fatalf("takeover retention was not extended from wall clock: %s", retentionExpiresAt)
	}
}

func TestIdempotencyLeaseExtensionAndNoEarlyStealUseWallClockIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	claim := idempotencyClaim(orgOneID, "owner-one", requestHash("wall-clock-extension"))
	claim.LeaseDuration = 2 * time.Second
	claim.Retention = 3 * time.Second
	claimed, err := dataStore.ClaimIdempotency(t.Context(), claim)
	if err != nil || claimed.LeaseExpiresAt == nil {
		t.Fatalf("initial claim = %+v, %v", claimed, err)
	}
	beforeRenew := time.Now()
	claim.LeaseDuration = 4 * time.Second
	if err := dataStore.RenewIdempotency(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	var leaseExpiresAt, retentionExpiresAt, updatedAt time.Time
	if err := pool.QueryRow(t.Context(), `SELECT lease_expires_at,expires_at,updated_at FROM api_idempotency_records WHERE org_id=$1 AND scope=$2 AND idempotency_key=$3`, claim.OrgID, claim.Scope, claim.Key).Scan(&leaseExpiresAt, &retentionExpiresAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if leaseExpiresAt.Before(beforeRenew.Add(3500*time.Millisecond)) || updatedAt.Before(beforeRenew) {
		t.Fatalf("renewed lease/updated time = %s/%s, before %s", leaseExpiresAt, updatedAt, beforeRenew)
	}
	other := claim
	other.Owner = "owner-two"
	inProgress, err := dataStore.ClaimIdempotency(t.Context(), other)
	if err != nil || inProgress.Disposition != IdempotencyInProgress || inProgress.LeaseExpiresAt == nil || !inProgress.LeaseExpiresAt.Equal(leaseExpiresAt) {
		t.Fatalf("early takeover = %+v, %v", inProgress, err)
	}
	if retentionExpiresAt.Before(claimed.LeaseExpiresAt.Add(-time.Second)) {
		t.Fatalf("retention unexpectedly shortened: %s", retentionExpiresAt)
	}
}

func TestIdempotencyResultResourceAuditAreAtomicIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "pending")
	hash := requestHash("atomic")
	if _, err := store.ClaimIdempotency(context.Background(), idempotencyClaim(orgOneID, "owner", hash)); err != nil {
		t.Fatal(err)
	}
	badAudit := testAudit("not-a-uuid", "deploy.requested", releaseOneID)
	err := store.CompleteIdempotency(context.Background(), CompleteIdempotencyParams{
		OrgID: orgOneID, Scope: "deploy", Key: "same-key", RequestHash: hash,
		Owner: "owner", Status: "completed", StatusCode: 202,
		Body: json.RawMessage(`{"ok":true}`), ResourceType: stringPointer("release"),
		ResourceID: stringPointer(releaseOneID), Audit: badAudit,
	})
	if err == nil {
		t.Fatal("CompleteIdempotency() succeeded with invalid audit identity")
	}
	var status string
	var responseCode *int
	if err := pool.QueryRow(context.Background(), `SELECT status, response_code
		FROM api_idempotency_records WHERE org_id = $1 AND scope = 'deploy' AND idempotency_key = 'same-key'`,
		orgOneID).Scan(&status, &responseCode); err != nil {
		t.Fatal(err)
	}
	if status != "in_progress" || responseCode != nil {
		t.Fatalf("record status/code = %s/%v, want rolled back", status, responseCode)
	}
}

func TestIdempotencyCompletionRejectsCrossTenantResourceIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	seedRelease(t, pool, orgTwoID, environmentTwoID, releaseThreeID, "pending")
	hash := requestHash("cross-tenant-resource")
	claim := idempotencyClaim(orgOneID, "owner", hash)
	if _, err := store.ClaimIdempotency(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	err := store.CompleteIdempotency(context.Background(), CompleteIdempotencyParams{
		OrgID: orgOneID, Scope: claim.Scope, Key: claim.Key, RequestHash: hash,
		Owner: "owner", Status: "completed", StatusCode: 202,
		Body: json.RawMessage(`{"ok":true}`), ResourceType: stringPointer("release"),
		ResourceID: stringPointer(releaseThreeID), Audit: testAudit("00000000-0000-0000-0000-000000000024", "ignored", releaseThreeID),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant resource completion error = %v", err)
	}
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM api_idempotency_records
		WHERE org_id = $1 AND scope = $2 AND idempotency_key = $3`, orgOneID,
		claim.Scope, claim.Key).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "in_progress" {
		t.Fatalf("idempotency status = %s, want in_progress", status)
	}
}

func TestIdempotencyCleanupAndSecretConstraintsIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO api_idempotency_records
			(org_id, scope, idempotency_key, request_hash, status, response_code, response_body, created_at, expires_at)
			VALUES ($1, 'cleanup', $2, repeat('a', 64), 'completed', 200, '{"ok":true}',
				now() - interval '2 hours', now() - interval '1 hour')`,
			orgOneID, fmt.Sprintf("cleanup-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if deleted, err := dataStore.DeleteExpiredIdempotencyRecords(ctx, 2); err != nil || deleted != 2 {
		t.Fatalf("DeleteExpiredIdempotencyRecords() = %d, %v", deleted, err)
	}
	if deleted, err := dataStore.DeleteExpiredIdempotencyRecords(ctx, 2); err != nil || deleted != 1 {
		t.Fatalf("DeleteExpiredIdempotencyRecords() remainder = %d, %v", deleted, err)
	}
	secretJSON := `{"token":"cyp_v1_AAAAAAAAAAAA_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}`
	if _, err := pool.Exec(ctx, `INSERT INTO api_idempotency_records
		(org_id, scope, idempotency_key, request_hash, status, response_code, response_body, expires_at)
		VALUES ($1, 'secret', 'secret', repeat('b', 64), 'completed', 200, $2, now() + interval '1 hour')`, orgOneID, secretJSON); err == nil {
		t.Fatal("database accepted plaintext bearer secret in idempotency response")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (org_id, action, target_type, target_id, after)
		VALUES ($1, 'secret.test', 'org', $1, $2)`, orgOneID, secretJSON); err == nil {
		t.Fatal("database accepted plaintext bearer secret in audit JSON")
	}
}

func TestDeployLeaseRequiresDedicatedFencedRecoveryIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedCompletedOrchestrationRoute(t, store, pool, orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000024", commitSHA('1'), "expired-finalize-route")
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	seedRelease(t, pool, orgOneID, environmentOneID, releaseTwoID, "pending")
	ctx := context.Background()

	lease, err := store.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at = now() - interval '2 seconds'
		WHERE org_id = $1 AND id = $2`, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseTwoID); !errors.Is(err, ErrConflict) {
		t.Fatalf("ordinary acquisition stole expired lease: %v", err)
	}
	recovery, err := store.ClaimExpiredDeployLease(ctx, orgOneID, environmentOneID)
	if err != nil || recovery.ReleaseID != releaseOneID || recovery.Fence.Equal(lease.Fence) {
		t.Fatalf("recovery lease = %+v, %v", recovery, err)
	}
	if _, err := store.RenewDeployLease(ctx, lease); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale fence renewed: %v", err)
	}
	if err := store.SetPendingCommitSHA(ctx, lease, commitSHA('a')); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale fence wrote pending SHA: %v", err)
	}
	if err := store.SetPendingCommitSHA(ctx, recovery, commitSHA('b')); err != nil {
		t.Fatal(err)
	}
	result, err := store.FinalizeReleaseFailed(ctx, FailReleaseParams{
		Lease: recovery, FailureReason: "orphaned deploy",
		Audit: testAudit("00000000-0000-0000-0000-000000000023", "deploy.lease_swept", releaseOneID),
	})
	if err != nil || result.PendingCommitSHA == nil || *result.PendingCommitSHA != commitSHA('b') {
		t.Fatalf("expired-lease finalization = %+v, %v", result, err)
	}
	if _, err := store.AcquireDeployLease(ctx, orgTwoID, environmentOneID, releaseOneID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-tenant acquisition error = %v", err)
	}
}

func TestExpiredDeployRecoveryReturnsCompleteAtomicContractIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	previousSnapshot := testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("a", 64))
	failedSnapshot := testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("b", 64))
	seedReleaseSnapshot(t, pool, orgOneID, environmentOneID, releaseOneID, "live", commitSHA('1'), previousSnapshot)
	seedReleaseSnapshot(t, pool, orgOneID, environmentOneID, releaseTwoID, "deploying", commitSHA('2'), failedSnapshot)
	lease, err := store.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseTwoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at = now() - interval '2 seconds'
		WHERE org_id = $1 AND id = $2`, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.ClaimExpiredDeployLease(ctx, orgOneID, environmentOneID)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Namespace != "app-one-production" || recovery.Timeout != time.Second ||
		recovery.FailedRelease.ID != releaseTwoID || recovery.FailedRelease.GitCommitSHA == nil ||
		*recovery.FailedRelease.GitCommitSHA != commitSHA('2') || recovery.PreviousRelease == nil ||
		recovery.PreviousRelease.ID != releaseOneID || recovery.PreviousRelease.GitCommitSHA == nil ||
		*recovery.PreviousRelease.GitCommitSHA != commitSHA('1') || recovery.Fence.Equal(lease.Fence) {
		t.Fatalf("expired deploy recovery = %+v", recovery)
	}
	renewed, err := store.RenewDeployLease(ctx, recovery)
	if err != nil || renewed.Namespace != recovery.Namespace || renewed.Timeout != recovery.Timeout ||
		renewed.PreviousRelease == nil || renewed.PreviousRelease.ID != releaseOneID ||
		renewed.FailedRelease.ID != releaseTwoID {
		t.Fatalf("renewed recovery contract = %+v, %v", renewed, err)
	}
	recovery = renewed
	assertJSONEqual(t, recovery.FailedRelease.ConfigSnapshot, failedSnapshot)
	assertJSONEqual(t, recovery.PreviousRelease.ConfigSnapshot, previousSnapshot)
	var persistedFence time.Time
	if err := pool.QueryRow(ctx, `SELECT lock_acquired_at FROM environments
		WHERE org_id = $1 AND id = $2`, orgOneID, environmentOneID).Scan(&persistedFence); err != nil {
		t.Fatal(err)
	}
	if !persistedFence.Equal(recovery.Fence) {
		t.Fatalf("persisted recovery fence = %v, returned %v", persistedFence, recovery.Fence)
	}
}

func TestDeployClaimPendingAndCrashResumeReturnImmutableExecutionContractIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	previousSnapshot := testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("a", 64))
	currentSnapshot := testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("b", 64))
	seedReleaseSnapshot(t, pool, orgOneID, environmentOneID, releaseOneID, "live", commitSHA('1'), previousSnapshot)
	seedReleaseSnapshot(t, pool, orgOneID, environmentOneID, releaseTwoID, "pending", commitSHA('2'), currentSnapshot)
	if _, err := dataStore.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseTwoID); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimPendingDeploy(ctx, orgOneID, nil)
	if err != nil || claimed.Namespace != "app-one-production" || claimed.PlannedHostname != "planned-one.example.test" || claimed.Timeout != time.Second || claimed.FailedRelease.ID != releaseTwoID || claimed.PreviousRelease == nil || claimed.PreviousRelease.ID != releaseOneID {
		t.Fatalf("ClaimPendingDeploy() = %+v, %v", claimed, err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM releases WHERE id=$1`, releaseTwoID).Scan(&status); err != nil || status != "deploying" {
		t.Fatalf("release status = %q, %v", status, err)
	}
	resumed, err := dataStore.ClaimUnexpiredDeploying(ctx, orgOneID, nil)
	if err != nil || !resumed.Fence.After(claimed.Fence) || resumed.FailedRelease.ConfigHash != claimed.FailedRelease.ConfigHash {
		t.Fatalf("ClaimUnexpiredDeploying() = %+v, %v", resumed, err)
	}
	if _, err := dataStore.RenewDeployLease(ctx, claimed); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale forward fence renewed: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at=now()-interval '2 seconds' WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.ClaimUnexpiredDeploying(ctx, orgOneID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired deploying work was forward claimed: %v", err)
	}
}

func TestDeployClaimExclusionsTreatNilAsEmptyAndHonorNonEmptyIntegration(t *testing.T) {
	t.Run("nil exclusion claims candidate", func(t *testing.T) {
		dataStore, pool := newIntegrationStore(t)
		seedTenant(t, pool, tenantOne())
		seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "pending")
		if _, err := dataStore.AcquireDeployLease(t.Context(), orgOneID, environmentOneID, releaseOneID); err != nil {
			t.Fatal(err)
		}
		claimed, err := dataStore.ClaimPendingDeploy(t.Context(), orgOneID, nil)
		if err != nil || claimed.EnvironmentID != environmentOneID {
			t.Fatalf("ClaimPendingDeploy(nil) = %+v, %v", claimed, err)
		}
	})
	t.Run("non-empty exclusion skips candidate", func(t *testing.T) {
		dataStore, pool := newIntegrationStore(t)
		seedTenant(t, pool, tenantOne())
		seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "pending")
		if _, err := dataStore.AcquireDeployLease(t.Context(), orgOneID, environmentOneID, releaseOneID); err != nil {
			t.Fatal(err)
		}
		if _, err := dataStore.ClaimPendingDeploy(t.Context(), orgOneID, []string{environmentOneID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ClaimPendingDeploy(excluded) error = %v, want ErrNotFound", err)
		}
		var status string
		if err := pool.QueryRow(t.Context(), `SELECT status FROM releases WHERE id=$1`, releaseOneID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "pending" {
			t.Fatalf("excluded release status = %q, want pending", status)
		}
	})
}

func TestDeployLeaseConcurrentPendingClaimsUseSkipLockedIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "pending")
	if _, err := dataStore.AcquireDeployLease(t.Context(), orgOneID, environmentOneID, releaseOneID); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			_, err := dataStore.ClaimPendingDeploy(context.Background(), orgOneID, nil)
			results <- err
		}()
	}
	close(start)
	claimed, notFound := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			claimed++
		} else if errors.Is(err, ErrNotFound) {
			notFound++
		} else {
			t.Fatal(err)
		}
	}
	if claimed != 1 || notFound != 1 {
		t.Fatalf("concurrent claims = claimed:%d not-found:%d", claimed, notFound)
	}
}

func TestDeployClaimSnapshotValidationFailureRollsBackFenceAndStatusIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "pending")
	lease, err := dataStore.AcquireDeployLease(t.Context(), orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE releases DISABLE TRIGGER releases_snapshot_valid`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE releases DISABLE TRIGGER releases_snapshot_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE releases SET config_snapshot='{"version":2,"desired_state":{}}' WHERE id=$1`, releaseOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.ClaimPendingDeploy(t.Context(), orgOneID, nil); err == nil {
		t.Fatal("invalid snapshot claim succeeded")
	}
	var status string
	var fence time.Time
	if err := pool.QueryRow(t.Context(), `SELECT release.status,environment.lock_acquired_at FROM releases release JOIN environments environment ON environment.in_progress_release_id=release.id WHERE release.id=$1`, releaseOneID).Scan(&status, &fence); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || !fence.Equal(lease.Fence) {
		t.Fatalf("claim rollback status=%s fence=%v want=%v", status, fence, lease.Fence)
	}
}

func TestRecoveryScanClaimsWithoutFinalizingExternalCompensationIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()
	job, err := store.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID,
		repositoryOneID, environmentOneID, "00000000-0000-0000-0000-000000000051",
		commitSHA('5'), "recovery-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOrchestration(ctx, orgOneID, "crashed-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET lease_expires_at = now() - interval '1 second'
		WHERE org_id = $1 AND id = $2`, orgOneID, job.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RequeueExpiredOrchestrationLeases(ctx); err != nil || count != 1 {
		t.Fatalf("RequeueExpiredOrchestrationLeases() = %d, %v", count, err)
	}
	var jobStatus string
	var leaseOwner *string
	if err := pool.QueryRow(ctx, `SELECT status, lease_owner FROM orchestration_jobs WHERE id = $1`,
		job.ID).Scan(&jobStatus, &leaseOwner); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "queued" || leaseOwner != nil {
		t.Fatalf("requeued orchestration = %s/%v, want queued with no owner", jobStatus, leaseOwner)
	}

	if _, err := store.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at = now() - interval '2 seconds'
		WHERE org_id = $1 AND id = $2`, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}

	expired, err := store.ListExpiredDeploys(ctx)
	if err != nil || len(expired) != 1 {
		t.Fatalf("ListExpiredDeploys() = %+v, %v", expired, err)
	}
	lease, err := store.ClaimExpiredDeployLease(ctx, expired[0].OrgID, expired[0].EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	var status, inProgressReleaseID string
	if err := pool.QueryRow(ctx, `SELECT release.status, environment.in_progress_release_id::text
		FROM releases AS release
		JOIN environments AS environment ON environment.id = release.environment_id
		WHERE release.id = $1`, lease.ReleaseID).Scan(&status, &inProgressReleaseID); err != nil {
		t.Fatal(err)
	}
	if status != "deploying" || inProgressReleaseID != releaseOneID {
		t.Fatalf("recovery finalized external compensation: status=%s in_progress=%s", status, inProgressReleaseID)
	}
}

func TestFinalizeReleaseSupersedesFirstAndReturnsPendingAtomicallyIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedCompletedOrchestrationRoute(t, store, pool, orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000032", commitSHA('1'), "live-finalize-route")
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "live")
	seedRelease(t, pool, orgOneID, environmentOneID, releaseTwoID, "deploying")
	if _, err := pool.Exec(context.Background(), `INSERT INTO aliases
		(org_id, environment_id, hostname, release_id) VALUES ($1, $2, 'old.example.test', $3)`,
		orgOneID, environmentOneID, releaseOneID); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireDeployLease(context.Background(), orgOneID, environmentOneID, releaseTwoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPendingCommitSHA(context.Background(), lease, commitSHA('c')); err != nil {
		t.Fatal(err)
	}
	result, err := store.FinalizeReleaseLive(context.Background(), FinalizeReleaseParams{
		Lease: lease, Hostname: "new.example.test",
		Audit: testAudit("00000000-0000-0000-0000-000000000031", "deploy.succeeded", releaseTwoID),
	})
	if err != nil || result.PendingCommitSHA == nil || *result.PendingCommitSHA != commitSHA('c') {
		t.Fatalf("FinalizeReleaseLive() = %+v, %v", result, err)
	}
	var oldStatus, newStatus, aliasRelease, hostname string
	var leaseOwner *string
	var pending *string
	if err := pool.QueryRow(context.Background(), `
		SELECT old.status, current.status, alias.release_id::text, alias.hostname,
			environment.in_progress_release_id::text, environment.pending_commit_sha
		FROM releases old, releases current, aliases alias, environments environment
		WHERE old.id = $1 AND current.id = $2 AND alias.environment_id = $3 AND environment.id = $3`,
		releaseOneID, releaseTwoID, environmentOneID).Scan(&oldStatus, &newStatus,
		&aliasRelease, &hostname, &leaseOwner, &pending); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "superseded" || newStatus != "live" || aliasRelease != releaseTwoID ||
		hostname != "new.example.test" || leaseOwner != nil || pending != nil {
		t.Fatalf("final state = %s/%s alias=%s host=%s lease=%v pending=%v",
			oldStatus, newStatus, aliasRelease, hostname, leaseOwner, pending)
	}
	assertCount(t, pool, "audit_events", 1)
}

func TestFinalizeReleaseLivePromotesPendingSHAAtomicallyIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()
	if _, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID, repositoryOneID, environmentOneID, "00000000-0000-0000-0000-000000000072", commitSHA('1'), "live-route")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET status='failed',terminal_error_code='fixture_terminal',terminal_at=now() WHERE environment_id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	lease, err := dataStore.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.SetPendingCommitSHA(ctx, lease, commitSHA('2')); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.FinalizeReleaseLive(ctx, FinalizeReleaseParams{Lease: lease, Hostname: "planned-one.example.test",
		Audit: testAudit("00000000-0000-0000-0000-000000000074", "deploy.succeeded", releaseOneID)}); err != nil {
		t.Fatal(err)
	}
	var status, commit string
	if err := pool.QueryRow(ctx, `SELECT status,desired_checkout_sha FROM orchestration_jobs WHERE environment_id=$1 AND status='queued'`, environmentOneID).Scan(&status, &commit); err != nil || commit != commitSHA('2') {
		t.Fatalf("promotion=%s/%s %v", status, commit, err)
	}
}

func TestFinalizeReleaseLegacyPendingPreservesWorkflowRevisionAndCanonicalGenerationIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := t.Context()
	params := orchestrationParams(orgOneID, repositoryOneID, environmentOneID,
		"ignored", commitSHA('1'), "legacy-route")
	params.WorkflowRevision = commitSHA('a')
	job, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET status='failed',terminal_error_code='fixture_terminal',terminal_at=now()
		WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := dataStore.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET pending_commit_sha=$2 WHERE id=$1`, environmentOneID, commitSHA('2')); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.FinalizeReleaseLive(ctx, FinalizeReleaseParams{Lease: lease, Hostname: "planned-one.example.test",
		Audit: testAudit("00000000-0000-0000-0000-000000000082", "deploy.succeeded", releaseOneID)}); err != nil {
		t.Fatal(err)
	}
	var checkoutSHA, workflowRevision, key string
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT desired_checkout_sha,workflow_revision,desired_generation,idempotency_key
		FROM orchestration_jobs WHERE environment_id=$1 AND status='queued'`, environmentOneID).Scan(
		&checkoutSHA, &workflowRevision, &generation, &key); err != nil {
		t.Fatal(err)
	}
	if checkoutSHA != commitSHA('2') || workflowRevision != commitSHA('a') || generation != job.DesiredGeneration+1 ||
		key != "recovery:"+releaseOneID+":"+commitSHA('2') {
		t.Fatalf("legacy promotion = checkout:%s workflow:%s generation:%d key:%s", checkoutSHA, workflowRevision, generation, key)
	}
}

func TestFinalizeReleaseFailedPromotesPendingSHAAndRejectsStaleFenceIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()
	if _, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID, repositoryOneID, environmentOneID, "00000000-0000-0000-0000-000000000073", commitSHA('1'), "failed-route")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET status='failed',terminal_error_code='fixture_terminal',terminal_at=now() WHERE environment_id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	lease, err := dataStore.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	stale := lease
	lease, err = dataStore.RenewDeployLease(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.FinalizeReleaseFailed(ctx, FailReleaseParams{Lease: stale, FailureReason: "stale",
		Audit: testAudit("00000000-0000-0000-0000-000000000075", "deploy.failed", releaseOneID)}); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale finalization error=%v", err)
	}
	if err := dataStore.SetPendingCommitSHA(ctx, lease, commitSHA('3')); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.FinalizeReleaseFailed(ctx, FailReleaseParams{Lease: lease, FailureReason: "terminal",
		Audit: testAudit("00000000-0000-0000-0000-000000000076", "deploy.failed", releaseOneID)}); err != nil {
		t.Fatal(err)
	}
	var releaseStatus, commit string
	if err := pool.QueryRow(ctx, `SELECT release.status,job.desired_checkout_sha FROM releases release JOIN orchestration_jobs job ON job.environment_id=release.environment_id AND job.status='queued' WHERE release.id=$1`, releaseOneID).Scan(&releaseStatus, &commit); err != nil || releaseStatus != "failed" || commit != commitSHA('3') {
		t.Fatalf("failed promotion=%s/%s %v", releaseStatus, commit, err)
	}
}

func TestFinalizeReleasePromotionFailureRollsBackReleaseAliasAndLeaseIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()
	lease, err := dataStore.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET pending_commit_sha=$2 WHERE id=$1`, environmentOneID, commitSHA('4')); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.FinalizeReleaseLive(ctx, FinalizeReleaseParams{Lease: lease, Hostname: "planned-one.example.test",
		Audit: testAudit("00000000-0000-0000-0000-000000000085", "deploy.succeeded", releaseOneID)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("promotion failure=%v", err)
	}
	var status, owner, pending string
	var aliases int
	if err := pool.QueryRow(ctx, `SELECT release.status,environment.in_progress_release_id::text,environment.pending_commit_sha,(SELECT count(*) FROM aliases WHERE environment_id=$2) FROM releases release JOIN environments environment ON environment.id=release.environment_id WHERE release.id=$1`, releaseOneID, environmentOneID).Scan(&status, &owner, &pending, &aliases); err != nil {
		t.Fatal(err)
	}
	if status != "deploying" || owner != releaseOneID || pending != commitSHA('4') || aliases != 0 {
		t.Fatalf("rollback=%s/%s/%s aliases=%d", status, owner, pending, aliases)
	}
}

func TestFinalizeReleaseAuditFailureRollsBackEverythingIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "live")
	seedRelease(t, pool, orgOneID, environmentOneID, releaseTwoID, "deploying")
	lease, err := store.AcquireDeployLease(context.Background(), orgOneID, environmentOneID, releaseTwoID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.FinalizeReleaseLive(context.Background(), FinalizeReleaseParams{
		Lease: lease, Hostname: "new.example.test", Audit: testAudit("bad-id", "deploy.succeeded", releaseTwoID),
	})
	if err == nil {
		t.Fatal("FinalizeReleaseLive() succeeded with invalid audit")
	}
	var oldStatus, newStatus string
	var owner string
	if err := pool.QueryRow(context.Background(), `SELECT old.status, current.status, environment.in_progress_release_id::text
		FROM releases old, releases current, environments environment
		WHERE old.id = $1 AND current.id = $2 AND environment.id = $3`, releaseOneID,
		releaseTwoID, environmentOneID).Scan(&oldStatus, &newStatus, &owner); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "live" || newStatus != "deploying" || owner != releaseTwoID {
		t.Fatalf("rollback state = %s/%s owner=%s", oldStatus, newStatus, owner)
	}
}

func TestRecoveredFinalizationGeneratesAuditAndPromotesPendingCommitAtomicallyIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()
	if _, err := store.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID,
		repositoryOneID, environmentOneID, "00000000-0000-0000-0000-000000000071",
		commitSHA('1'), "recovery-route")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET status='failed',terminal_error_code='fixture_terminal',terminal_at=now()
		WHERE org_id = $1 AND environment_id = $2`, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPendingCommitSHA(ctx, lease, commitSHA('2')); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckpointDeployRecovery(ctx, lease, "claimed"); err != nil {
		t.Fatal(err)
	}
	result, err := store.FinalizeRecoveredReleaseFailed(ctx, lease, "expired")
	if err != nil || result.PendingCommitSHA == nil || *result.PendingCommitSHA != commitSHA('2') {
		t.Fatalf("FinalizeRecoveredReleaseFailed() = %+v, %v", result, err)
	}
	var releaseStatus, jobStatus, jobCommit, auditAction string
	var pending *string
	if err := pool.QueryRow(ctx, `SELECT release.status, job.status, job.desired_checkout_sha,
		environment.pending_commit_sha, audit.action
		FROM releases release
		JOIN environments environment ON environment.id = release.environment_id
		JOIN orchestration_jobs job ON job.environment_id = environment.id AND job.status = 'queued'
		JOIN audit_events audit ON audit.target_id = release.id
		WHERE release.id = $1`, releaseOneID).Scan(&releaseStatus, &jobStatus, &jobCommit,
		&pending, &auditAction); err != nil {
		t.Fatal(err)
	}
	if releaseStatus != "failed" || jobStatus != "queued" || jobCommit != commitSHA('2') ||
		pending != nil || auditAction != "release.recovery_finalized_failed" {
		t.Fatalf("recovery finalization = release:%s job:%s/%s pending:%v audit:%s",
			releaseStatus, jobStatus, jobCommit, pending, auditAction)
	}
}

func TestAdoptRecoveredReleaseCreatesAliasFromPlannedHostnameAtomicallyIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()
	lease, err := dataStore.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at = now() - interval '2 seconds'
		WHERE id = $1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	lease, err = dataStore.ClaimExpiredDeployLease(ctx, orgOneID, environmentOneID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.PlannedHostname != "planned-one.example.test" {
		t.Fatalf("planned hostname = %q", lease.PlannedHostname)
	}
	if err := dataStore.CheckpointDeployRecovery(ctx, lease, "late_ready_observed"); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.AdoptRecoveredReleaseLive(ctx, lease); err != nil {
		t.Fatal(err)
	}
	var status, hostname, aliasRelease string
	var owner *string
	if err := pool.QueryRow(ctx, `SELECT release.status, alias.hostname, alias.release_id::text,
		environment.in_progress_release_id::text FROM releases release
		JOIN environments environment ON environment.id = release.environment_id
		JOIN aliases alias ON alias.environment_id = environment.id
		WHERE release.id = $1`, releaseOneID).Scan(&status, &hostname, &aliasRelease, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "live" || hostname != "planned-one.example.test" || aliasRelease != releaseOneID || owner != nil {
		t.Fatalf("adoption = status:%s hostname:%s alias:%s owner:%v", status, hostname, aliasRelease, owner)
	}
}

func TestAdoptRecoveredReleaseUpdatesAliasFromPlannedHostnameAtomicallyIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO aliases (org_id, environment_id, hostname, release_id)
		VALUES ($1, $2, 'old.example.test', $3)`, orgOneID, environmentOneID, releaseOneID); err != nil {
		t.Fatal(err)
	}
	lease, err := dataStore.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at = now() - interval '2 seconds'
		WHERE id = $1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	lease, err = dataStore.ClaimExpiredDeployLease(ctx, orgOneID, environmentOneID)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CheckpointDeployRecovery(ctx, lease, "late_ready_observed"); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.AdoptRecoveredReleaseLive(ctx, lease); err != nil {
		t.Fatal(err)
	}
	var hostname string
	if err := pool.QueryRow(ctx, `SELECT hostname FROM aliases WHERE environment_id = $1`, environmentOneID).Scan(&hostname); err != nil {
		t.Fatal(err)
	}
	if hostname != "planned-one.example.test" {
		t.Fatalf("alias hostname = %q", hostname)
	}
}

func TestQuarantineDeployRecoveryPersistsFailureAndPreservesOwnershipIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedCompletedOrchestrationRoute(t, dataStore, pool, orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000084", commitSHA('1'), "quarantine-route")
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()
	lease, err := dataStore.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.SetPendingCommitSHA(ctx, lease, commitSHA('f')); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.QuarantineDeployRecovery(ctx, lease, "recovery_state_invalid", "snapshot is malformed"); err != nil {
		t.Fatal(err)
	}
	var owner, status, code, message string
	var fence, quarantinedAt time.Time
	var pending string
	if err := pool.QueryRow(ctx, `SELECT environment.in_progress_release_id::text, release.status,
		environment.deploy_recovery_error_code, environment.deploy_recovery_error_message,
		environment.lock_acquired_at, environment.pending_commit_sha,
		environment.deploy_recovery_quarantined_at FROM environments environment
		JOIN releases release ON release.id = environment.in_progress_release_id
		WHERE environment.id = $1`, environmentOneID).Scan(&owner, &status, &code, &message, &fence, &pending, &quarantinedAt); err != nil {
		t.Fatal(err)
	}
	if owner != releaseOneID || status != "deploying" || code != "recovery_state_invalid" || message != "snapshot is malformed" ||
		!fence.Equal(lease.Fence) || pending != commitSHA('f') || quarantinedAt.IsZero() {
		t.Fatalf("quarantine = owner:%s status:%s code:%s message:%s fence:%v pending:%s at:%v",
			owner, status, code, message, fence, pending, quarantinedAt)
	}
	if deploys, err := dataStore.ListExpiredDeploys(ctx); err != nil || len(deploys) != 0 {
		t.Fatalf("ListExpiredDeploys() = %+v, %v", deploys, err)
	}
}

func TestOrchestrationReplayConflictDeadlineReconciliationAndTenantFilteringIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	ctx := context.Background()
	params := orchestrationParams(orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000041", commitSHA('1'), "key-one")
	queued, err := store.EnqueueOrCoalesceOrchestration(ctx, params)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("enqueue = %+v, %v", queued, err)
	}
	replay, err := store.EnqueueOrCoalesceOrchestration(ctx, params)
	if err != nil || replay.ID != queued.ID {
		t.Fatalf("historical replay = %+v, %v", replay, err)
	}
	conflicting := params
	conflicting.CommitSHA = commitSHA('2')
	if _, err := store.EnqueueOrCoalesceOrchestration(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("historical conflict error = %v", err)
	}

	other := orchestrationParams(orgTwoID, repositoryTwoID, environmentTwoID,
		"00000000-0000-0000-0000-000000000042", commitSHA('3'), "key-one")
	if _, err := store.EnqueueOrCoalesceOrchestration(ctx, other); err != nil {
		t.Fatalf("cross-tenant same key enqueue: %v", err)
	}
	claimed, err := store.ClaimOrchestration(ctx, orgOneID, "worker", time.Minute)
	if err != nil || claimed.OrgID != orgOneID {
		t.Fatalf("tenant claim = %+v, %v", claimed, err)
	}
	if err := store.BeginDispatchReconciliation(ctx, orgOneID, claimed.ID, "worker", commitSHA('a'), ".github/workflows/deploy.yml", commitSHA('b'), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	reconcile, err := store.ClaimDispatchReconciliation(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reconcile.InstallationID != 1 || reconcile.RepositoryFullName != "acme/one" {
		t.Fatalf("GitHub recovery target = %d/%q", reconcile.InstallationID, reconcile.RepositoryFullName)
	}
	originalDeadline := *reconcile.DispatchDeadlineAt
	if err := store.ContinueDispatchReconciliation(ctx, orgOneID, reconcile.ID, "reconciler", time.Millisecond); err != nil {
		t.Fatalf("continue dispatch reconciliation error = %v", err)
	}
	var persistedDeadline time.Time
	var persistedStatus string
	if err := pool.QueryRow(ctx, `SELECT dispatch_deadline_at, status FROM orchestration_jobs
		WHERE org_id = $1 AND id = $2`, orgOneID, reconcile.ID).Scan(&persistedDeadline, &persistedStatus); err != nil {
		t.Fatal(err)
	}
	if !persistedDeadline.Equal(originalDeadline) || persistedStatus != "dispatch_reconciling" {
		t.Fatalf("dispatch reconciliation mutated: deadline %v -> %v, status=%s", originalDeadline, persistedDeadline, persistedStatus)
	}
	if got := originalDeadline.Sub(*reconcile.DispatchRequestedAt); got < 5*time.Minute-time.Second || got > 5*time.Minute+time.Second {
		t.Fatalf("dispatch observation deadline interval = %s, want 5m", got)
	}
	if _, err := store.ClaimOrchestration(ctx, orgOneID, "worker-two", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ambiguous dispatch became claimable before deadline: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET reconcile_after = now() - interval '1 second'
		WHERE org_id = $1 AND id = $2`, orgOneID, reconcile.ID); err != nil {
		t.Fatal(err)
	}
	reconcile, err = store.ClaimDispatchReconciliation(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDispatchedRun(ctx, orgOneID, reconcile.ID, "reconciler",
		"cyclopus:"+reconcile.ID, 12345, "queued", commitSHA('a'), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	dispatched, err := store.ClaimDispatchReconciliation(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil || dispatched.Status != "dispatched" || dispatched.GitHubRunID == nil {
		t.Fatalf("dispatched reconciliation = %+v, %v", dispatched, err)
	}
	var runDeadlineSeconds float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM (run_deadline_at - github_status_observed_at))
		FROM orchestration_jobs WHERE org_id = $1 AND id = $2`, orgOneID, dispatched.ID).Scan(&runDeadlineSeconds); err != nil {
		t.Fatal(err)
	}
	if runDeadlineSeconds != orchestrationRunDeadline.Seconds() {
		t.Fatalf("run deadline interval = %v seconds, want 2700", runDeadlineSeconds)
	}
	if err := store.RescheduleDispatchReconciliation(ctx, orgOneID, dispatched.ID, "reconciler", time.Hour); err != nil {
		t.Fatalf("reschedule dispatched reconciliation error = %v", err)
	}
	var reconcileAfter, runDeadline time.Time
	if err := pool.QueryRow(ctx, `SELECT reconcile_after, run_deadline_at FROM orchestration_jobs
		WHERE org_id = $1 AND id = $2`, orgOneID, dispatched.ID).Scan(&reconcileAfter, &runDeadline); err != nil {
		t.Fatal(err)
	}
	if !reconcileAfter.Equal(runDeadline) || !reconcileAfter.After(time.Now()) {
		t.Fatalf("dispatched retry schedule = %v, run deadline = %v", reconcileAfter, runDeadline)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET reconcile_after = now() - interval '1 second'
		WHERE org_id = $1 AND id = $2`, orgOneID, dispatched.ID); err != nil {
		t.Fatal(err)
	}
	dispatched, err = store.ClaimDispatchReconciliation(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ContinueRunReconciliation(ctx, orgOneID, dispatched.ID, "reconciler",
		"in_progress", nil, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	pending := orchestrationParams(orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000043", commitSHA('4'), "key-two")
	coalesced, err := store.EnqueueOrCoalesceOrchestration(ctx, pending)
	if err != nil || coalesced.PendingCommitSHA == nil || *coalesced.PendingCommitSHA != commitSHA('4') {
		t.Fatalf("pending coalesce = %+v, %v", coalesced, err)
	}
	if coalesced.PendingDesiredGeneration == nil || *coalesced.PendingDesiredGeneration != coalesced.DesiredGeneration+1 {
		t.Fatalf("pending generation = current:%d pending:%v", coalesced.DesiredGeneration, coalesced.PendingDesiredGeneration)
	}
	var appGeneration, environmentGeneration int64
	if err := pool.QueryRow(ctx, `SELECT app.desired_generation,environment.desired_generation FROM apps app
		JOIN environments environment ON environment.app_id=app.id WHERE environment.id=$1`, environmentOneID).
		Scan(&appGeneration, &environmentGeneration); err != nil {
		t.Fatal(err)
	}
	if appGeneration != 1 || environmentGeneration != *coalesced.PendingDesiredGeneration {
		t.Fatalf("target generations = app:%d environment:%d pending:%d", appGeneration, environmentGeneration, *coalesced.PendingDesiredGeneration)
	}
	time.Sleep(5 * time.Millisecond)
	dispatched, err = store.ClaimDispatchReconciliation(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := store.ResolveOrchestration(ctx, ResolveOrchestrationParams{
		OrgID: orgOneID, JobID: dispatched.ID, Owner: "reconciler",
		RunStatus: "completed", RunConclusion: "success",
	})
	if err != nil || resolved.Status != "queued" || resolved.CommitSHA != commitSHA('4') || resolved.PendingCommitSHA != nil {
		t.Fatalf("terminal promotion = %+v, %v", resolved, err)
	}
	if resolved.DesiredGeneration != environmentGeneration {
		t.Fatalf("promoted generation = %d, want %d", resolved.DesiredGeneration, environmentGeneration)
	}
	assertCount(t, pool, "orchestration_job_run_evidence", 2)
	if _, err := store.ClaimOrchestration(ctx, orgTwoID, "other-worker", time.Minute); err != nil {
		t.Fatalf("other tenant claim: %v", err)
	}
}

func TestFreshOrchestrationEnqueueReturnsInsertedJobIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	params := orchestrationParams(orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000081", commitSHA('8'), "fresh-enqueue-result")

	job, err := dataStore.EnqueueOrCoalesceOrchestration(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.OrgID != orgOneID || job.AppID != appOneID || job.EnvironmentID != environmentOneID ||
		job.RepositoryID != "2001" || job.WorkflowID != ".github/workflows/deploy.yml" ||
		job.WorkflowRevision != params.CommitSHA || job.GitRef != params.GitRef || job.CommitSHA != params.CommitSHA ||
		job.DesiredGeneration != 2 || job.AcceptedGeneration != 2 || job.IdempotencyKey != params.IdempotencyKey ||
		job.Status != "queued" || job.EnqueueResult != "queued" || job.PendingWorkflowID != nil ||
		job.PendingWorkflowRevision != nil || job.PendingGitRef != nil || job.PendingCommitSHA != nil ||
		job.PendingDesiredGeneration != nil || job.PendingIdempotencyKey != nil {
		t.Fatalf("fresh enqueue returned incomplete job: %+v", job)
	}
}

func TestOrchestrationHistoricalRetryCannotOverwriteNewerQueuedCommitIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	first := orchestrationParams(orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000044", commitSHA('1'), "history-one")
	newer := orchestrationParams(orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000045", commitSHA('2'), "history-two")
	if _, err := store.EnqueueOrCoalesceOrchestration(ctx, first); err != nil {
		t.Fatal(err)
	}
	newerJob, err := store.EnqueueOrCoalesceOrchestration(ctx, newer)
	if err != nil || newerJob.PendingCommitSHA == nil || *newerJob.PendingCommitSHA != newer.CommitSHA {
		t.Fatalf("newer enqueue = %+v, %v", newerJob, err)
	}
	replay, err := store.EnqueueOrCoalesceOrchestration(ctx, first)
	if err != nil || replay.ID != newerJob.ID || replay.CommitSHA != first.CommitSHA {
		t.Fatalf("historical replay = %+v, %v", replay, err)
	}
	var currentSHA, currentKey string
	if err := pool.QueryRow(ctx, `SELECT desired_checkout_sha, idempotency_key FROM orchestration_jobs
		WHERE org_id = $1 AND id = $2`, orgOneID, newerJob.ID).Scan(&currentSHA, &currentKey); err != nil {
		t.Fatal(err)
	}
	if currentSHA != first.CommitSHA || currentKey != first.IdempotencyKey {
		t.Fatalf("current orchestration was overwritten: %s/%s", currentSHA, currentKey)
	}
	assertCount(t, pool, "orchestration_jobs", 1)
}

func TestOrchestrationMissingRunResolvesTerminalAfterDeadlineIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	params := orchestrationParams(orgOneID, repositoryOneID, environmentOneID,
		"00000000-0000-0000-0000-000000000046", commitSHA('3'), "missing-run")
	if _, err := store.EnqueueOrCoalesceOrchestration(ctx, params); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimOrchestration(ctx, orgOneID, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDispatchReconciliation(ctx, orgOneID, job.ID, "worker", commitSHA('a'), ".github/workflows/deploy.yml", commitSHA('b'), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs
		SET reconcile_after = now() - interval '2 seconds', dispatch_deadline_at = now() - interval '1 second'
		WHERE org_id = $1 AND id = $2`, orgOneID, job.ID); err != nil {
		t.Fatal(err)
	}
	reconcile, err := store.ClaimDispatchReconciliation(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ContinueDispatchReconciliation(ctx, orgOneID, job.ID, "reconciler", time.Minute); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("post-deadline observation error = %v", err)
	}
	code := "github_run_missing"
	message := "no correlated run became observable before the dispatch deadline"
	resolved, err := store.ResolveOrchestration(ctx, ResolveOrchestrationParams{
		OrgID: orgOneID, JobID: reconcile.ID, Owner: "reconciler", RunStatus: "completed",
		TerminalCode: &code, TerminalMessage: &message,
	})
	if err != nil || resolved.Status != "failed" {
		t.Fatalf("missing-run resolution = %+v, %v", resolved, err)
	}
	assertCount(t, pool, "orchestration_job_run_evidence", 1)
}

func TestSuccessfulOrchestrationResolutionPreservesDispatchAndRunEvidenceIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	job := prepareDispatchedRecoveryJob(t, store, pool,
		"00000000-0000-0000-0000-000000000121", "successful-evidence")
	resolved, err := store.ResolveOrchestration(context.Background(), ResolveOrchestrationParams{
		OrgID: orgOneID, JobID: job.ID, Owner: "reconciler",
		RunStatus: "completed", RunConclusion: "success",
	})
	if err != nil || resolved.Status != "resolving_image" {
		t.Fatalf("successful resolution = %+v, %v", resolved, err)
	}
	var dispatchRequested time.Time
	var observedRefSHA, workflowPath, workflowRevision, ambiguity, identity, headSHA, status, conclusion string
	var runID int64
	if err := pool.QueryRow(context.Background(), `SELECT dispatch_requested_at,dispatch_ref_observed_sha,
		github_workflow_path,workflow_revision,dispatch_ambiguity_state,github_dispatch_identity,github_run_id,
		github_run_head_sha,github_run_status,github_run_conclusion FROM orchestration_jobs WHERE id=$1`, job.ID).
		Scan(&dispatchRequested, &observedRefSHA, &workflowPath, &workflowRevision, &ambiguity, &identity,
			&runID, &headSHA, &status, &conclusion); err != nil {
		t.Fatal(err)
	}
	if dispatchRequested.IsZero() || observedRefSHA != commitSHA('a') || workflowPath != ".github/workflows/deploy.yml" ||
		workflowRevision != commitSHA('b') || ambiguity != "matched" || identity != "cyclopus:"+job.ID ||
		runID != 12345 || headSHA != commitSHA('a') || status != "completed" || conclusion != "success" {
		t.Fatalf("preserved evidence = requested:%v ref:%s workflow:%s@%s ambiguity:%s identity:%s run:%d/%s/%s/%s",
			dispatchRequested, observedRefSHA, workflowPath, workflowRevision, ambiguity, identity, runID, headSHA, status, conclusion)
	}
}

func TestOrchestrationRetryCannotSchedulePastStateDeadlineIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	job := prepareDispatchedRecoveryJob(t, store, pool,
		"00000000-0000-0000-0000-000000000047", "expired-run-retry")
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET run_deadline_at = now() - interval '1 second'
		WHERE org_id = $1 AND id = $2`, orgOneID, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RescheduleDispatchReconciliation(ctx, orgOneID, job.ID, "reconciler", time.Minute); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("expired retry schedule error = %v", err)
	}
	var reconcileAfter time.Time
	if err := pool.QueryRow(ctx, `SELECT reconcile_after FROM orchestration_jobs
		WHERE org_id = $1 AND id = $2`, orgOneID, job.ID).Scan(&reconcileAfter); err != nil {
		t.Fatal(err)
	}
	if !reconcileAfter.Before(time.Now()) {
		t.Fatalf("expired retry changed schedule to %v", reconcileAfter)
	}
}

func TestOrchestrationConclusionlessRunTimeoutAndTerminalConclusionsIntegration(t *testing.T) {
	terminalConclusions := []string{"success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale"}
	for index, conclusion := range terminalConclusions {
		t.Run(conclusion, func(t *testing.T) {
			store, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			jobID := fmt.Sprintf("00000000-0000-0000-0000-%012d", 100+index)
			job := prepareDispatchedRecoveryJob(t, store, pool, jobID, "terminal-"+conclusion)
			resolved, err := store.ResolveOrchestration(context.Background(), ResolveOrchestrationParams{
				OrgID: orgOneID, JobID: job.ID, Owner: "reconciler",
				RunStatus: "completed", RunConclusion: conclusion,
			})
			wantStatus := "failed"
			if conclusion == "success" {
				wantStatus = "resolving_image"
			}
			if err != nil || resolved.Status != wantStatus {
				t.Fatalf("ResolveOrchestration(%q) = %+v, %v", conclusion, resolved, err)
			}
		})
	}

	t.Run("github_run_timed_out", func(t *testing.T) {
		store, pool := newIntegrationStore(t)
		seedTenant(t, pool, tenantOne())
		ctx := context.Background()
		job := prepareDispatchedRecoveryJob(t, store, pool,
			"00000000-0000-0000-0000-000000000120", "run-timeout")
		code := "github_run_timed_out"
		if _, err := store.ResolveOrchestration(ctx, ResolveOrchestrationParams{
			OrgID: orgOneID, JobID: job.ID, Owner: "reconciler", RunStatus: "in_progress",
			TerminalCode: &code,
		}); !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("pre-deadline run timeout error = %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET run_deadline_at = now() - interval '1 second'
			WHERE org_id = $1 AND id = $2`, orgOneID, job.ID); err != nil {
			t.Fatal(err)
		}
		resolved, err := store.ResolveOrchestration(ctx, ResolveOrchestrationParams{
			OrgID: orgOneID, JobID: job.ID, Owner: "reconciler", RunStatus: "in_progress",
			TerminalCode: &code,
		})
		if err != nil || resolved.Status != "failed" {
			t.Fatalf("conclusionless run timeout = %+v, %v", resolved, err)
		}
		var conclusion *string
		var terminalCode string
		if err := pool.QueryRow(ctx, `SELECT github_run_conclusion, terminal_error_code
			FROM orchestration_job_run_evidence WHERE orchestration_job_id = $1`, job.ID).Scan(
			&conclusion, &terminalCode); err != nil {
			t.Fatal(err)
		}
		if conclusion != nil || terminalCode != code {
			t.Fatalf("timeout evidence conclusion/code = %v/%s", conclusion, terminalCode)
		}
	})
}

func TestReconciliationListOrgIDsIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()

	// No runs → empty.
	orgIDs, err := store.ListReconciliationOrgIDs(ctx)
	if err != nil || len(orgIDs) != 0 {
		t.Fatalf("initial ListReconciliationOrgIDs = %v, %v", orgIDs, err)
	}

	// Queued run → returned.
	runID := "00000000-0000-0000-0000-0000000000e1"
	if _, err := store.CreateReconciliationRun(ctx, CreateReconciliationRunParams{
		ID: runID, OrgID: orgOneID, ScopeKind: "org", ScopeID: orgOneID, DryRun: boolPointer(true),
	}); err != nil {
		t.Fatal(err)
	}
	orgIDs, err = store.ListReconciliationOrgIDs(ctx)
	if err != nil || len(orgIDs) != 1 || orgIDs[0] != orgOneID {
		t.Fatalf("queued ListReconciliationOrgIDs = %v, %v", orgIDs, err)
	}

	// Claim it (leased), then expire the lease → still returned.
	if _, err := store.ClaimReconciliationRun(ctx, orgOneID, "reconciler", time.Minute); err != nil {
		t.Fatal(err)
	}
	orgIDs, err = store.ListReconciliationOrgIDs(ctx)
	if err != nil || len(orgIDs) != 0 {
		t.Fatalf("leased-unexpired ListReconciliationOrgIDs = %v, %v", orgIDs, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE reconciliation_runs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	orgIDs, err = store.ListReconciliationOrgIDs(ctx)
	if err != nil || len(orgIDs) != 1 {
		t.Fatalf("leased-expired ListReconciliationOrgIDs = %v, %v", orgIDs, err)
	}

	// Complete it → absent.
	if _, err := pool.Exec(ctx, `UPDATE reconciliation_runs SET status = 'completed', completed_at = now(), lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	orgIDs, err = store.ListReconciliationOrgIDs(ctx)
	if err != nil || len(orgIDs) != 0 {
		t.Fatalf("completed ListReconciliationOrgIDs = %v, %v", orgIDs, err)
	}
}

func TestReconciliationRunLifecycleOverlapPlanEvidenceAndAuditIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	runID := "00000000-0000-0000-0000-000000000051"
	run, err := store.CreateReconciliationRun(ctx, CreateReconciliationRunParams{
		ID: runID, OrgID: orgOneID, ScopeKind: "environment", ScopeID: environmentOneID,
		ScopeAppID: stringPointer(appOneID), ScopeEnvironmentID: stringPointer(environmentOneID),
		DryRun: boolPointer(true), Prune: true,
	})
	if err != nil || !run.DryRun || !run.Prune || run.Status != "queued" {
		t.Fatalf("create reconciliation run = %+v, %v", run, err)
	}
	assertCount(t, pool, "audit_events", 0)
	_, err = store.CreateReconciliationRun(ctx, CreateReconciliationRunParams{
		ID: "00000000-0000-0000-0000-000000000053", OrgID: orgOneID,
		ScopeKind: "app", ScopeID: appOneID, ScopeAppID: stringPointer(appOneID),
		DryRun: boolPointer(true),
	})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("overlapping reconciliation error = %v", err)
	}
	claimed, err := store.ClaimReconciliationRun(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil || claimed.ID != runID {
		t.Fatalf("claim reconciliation = %+v, %v", claimed, err)
	}
	plan := json.RawMessage(`{"actions":[{"action":"prune","object":"stale-service"}],"dry_run":true}`)
	if err := store.StartReconciliationRun(ctx, orgOneID, runID, "reconciler", plan,
		testAudit("00000000-0000-0000-0000-000000000052", "ignored", runID)); err != nil {
		t.Fatal(err)
	}
	assertCount(t, pool, "audit_events", 1)
	if err := store.AppendReconciliationEvidence(ctx, orgOneID, runID, "reconciler", ReconciliationEvidence{
		Kind: "plan", ObjectKind: stringPointer("Service"), ObjectName: stringPointer("stale-service"),
		Action: stringPointer("prune"), Details: json.RawMessage(`{"applied":false,"reason":"dry_run"}`),
	}); err != nil {
		t.Fatal(err)
	}
	report := json.RawMessage(`{"planned":1,"applied":0,"dry_run":true}`)
	if err := store.CompleteReconciliationRun(ctx, CompleteReconciliationRunParams{
		OrgID: orgOneID, RunID: runID, Owner: "reconciler", Status: "completed", Report: report,
	}); err != nil {
		t.Fatal(err)
	}
	var status, action, targetType string
	var storedPlan, storedReport, auditedPlan json.RawMessage
	if err := pool.QueryRow(ctx, `SELECT run.status, run.plan, run.report, audit.action, audit.target_type, audit.after
		FROM reconciliation_runs run JOIN audit_events audit ON audit.target_id = run.id
		WHERE run.id = $1 AND audit.action = 'reconciliation.invoked'`, runID).Scan(
		&status, &storedPlan, &storedReport, &action, &targetType, &auditedPlan); err != nil {
		t.Fatal(err)
	}
	var expectedPlanValue, auditedPlanValue any
	if err := json.Unmarshal(plan, &expectedPlanValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(auditedPlan, &auditedPlanValue); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || action != "reconciliation.invoked" || targetType != "reconciliation_run" ||
		string(storedPlan) == "" || string(storedReport) == "" || !reflect.DeepEqual(auditedPlanValue, expectedPlanValue) {
		t.Fatalf("reconciliation state = %s/%s/%s plan=%s audited_plan=%s report=%s", status, action, targetType, storedPlan, auditedPlan, storedReport)
	}
	assertCount(t, pool, "reconciliation_evidence", 1)
	assertCount(t, pool, "audit_events", 1)
	evidence, err := store.ListReconciliationEvidence(ctx, orgOneID, runID, 0, 1)
	if err != nil || len(evidence) != 1 || evidence[0].Sequence <= 0 || evidence[0].Kind != "plan" || evidence[0].ObservedAt.IsZero() {
		t.Fatalf("ListReconciliationEvidence() = %+v, %v", evidence, err)
	}
	if next, err := store.ListReconciliationEvidence(ctx, orgOneID, runID, evidence[0].Sequence, 1); err != nil || len(next) != 0 {
		t.Fatalf("ListReconciliationEvidence() after cursor = %+v, %v", next, err)
	}
	if _, err := store.ListReconciliationEvidence(ctx, orgTwoID, runID, 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant reconciliation evidence error = %v", err)
	}
}

func TestReconciliationDefaultsAndAppliedMutationAuditAreAtomicIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	runID := "00000000-0000-0000-0000-000000000056"
	run, err := store.CreateReconciliationRun(ctx, CreateReconciliationRunParams{
		ID: runID, OrgID: orgOneID, ScopeKind: "environment", ScopeID: environmentOneID,
		ScopeAppID: stringPointer(appOneID), ScopeEnvironmentID: stringPointer(environmentOneID),
	})
	if err != nil || !run.DryRun || run.Prune || run.Categories == nil || len(run.Categories) != 0 {
		t.Fatalf("reconciliation defaults = %+v, %v", run, err)
	}
	stored, err := store.GetReconciliationRun(ctx, orgOneID, runID)
	if err != nil || stored.Categories == nil || len(stored.Categories) != 0 {
		t.Fatalf("stored reconciliation defaults = %+v, %v", stored, err)
	}
	listed, err := store.ListReconciliationRuns(ctx, orgOneID, nil, nil)
	if err != nil || len(listed) != 1 || listed[0].Categories == nil || len(listed[0].Categories) != 0 {
		t.Fatalf("listed reconciliation defaults = %+v, %v", listed, err)
	}
	claimed, err := store.ClaimReconciliationRun(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	plan := json.RawMessage(`{"actions":[]}`)
	if err := store.StartReconciliationRun(ctx, orgOneID, claimed.ID, "reconciler", plan,
		testAudit("00000000-0000-0000-0000-000000000057", "ignored", runID)); err != nil {
		t.Fatal(err)
	}
	action := "patch"
	evidence := ReconciliationEvidence{Kind: "apply", ObjectKind: stringPointer("Deployment"),
		ObjectNamespace: stringPointer("app-one-production"), ObjectName: stringPointer("app-one"),
		Action: &action, Details: json.RawMessage(`{"applied":true}`),
		Audit: testAudit("00000000-0000-0000-0000-000000000058", "ignored", runID)}
	if err := store.AppendReconciliationEvidence(ctx, orgOneID, runID, "reconciler", evidence); err != nil {
		t.Fatalf("dry-run evidence error = %v", err)
	}
	assertCount(t, pool, "reconciliation_evidence", 1)
	assertCount(t, pool, "audit_events", 1)

	if _, err := pool.Exec(ctx, `UPDATE reconciliation_runs SET dry_run = false WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendReconciliationEvidence(ctx, orgOneID, runID, "reconciler", evidence); err != nil {
		t.Fatal(err)
	}
	var auditAction, operation string
	if err := pool.QueryRow(ctx, `SELECT action, after->>'operation' FROM audit_events WHERE id = $1`,
		"00000000-0000-0000-0000-000000000058").Scan(&auditAction, &operation); err != nil {
		t.Fatal(err)
	}
	if auditAction != "reconciliation.object_patched" || operation != "patch" {
		t.Fatalf("mutation audit = %s/%s", auditAction, operation)
	}
	assertCount(t, pool, "reconciliation_evidence", 2)
	assertCount(t, pool, "audit_events", 2)
}

func TestDeletionLifecycleRequiresTeardownAndRevocationAndPreservesHistoryIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "failed")
	ctx := context.Background()
	params := TombstoneParams{OrgID: orgOneID, ID: appOneID,
		Audit: testAudit("00000000-0000-0000-0000-000000000059", "ignored", appOneID)}
	if err := store.TombstoneApp(ctx, params); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unconfirmed revocation error = %v", err)
	}
	params.ExternalCredentialsRevoked = true
	if err := store.TombstoneApp(ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("active environment tombstone error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET torn_down_at = now() WHERE id = $1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if err := store.TombstoneApp(ctx, params); err != nil {
		t.Fatal(err)
	}
	var deletedAt, revokedAt *time.Time
	var releases, environments, audits int
	if err := pool.QueryRow(ctx, `SELECT app.deleted_at, app.external_credentials_revoked_at,
		(SELECT count(*) FROM releases WHERE org_id = $1),
		(SELECT count(*) FROM environments WHERE org_id = $1),
		(SELECT count(*) FROM audit_events WHERE id = $3)
		FROM apps app WHERE app.org_id = $1 AND app.id = $2`, orgOneID, appOneID,
		"00000000-0000-0000-0000-000000000059").Scan(&deletedAt, &revokedAt, &releases, &environments, &audits); err != nil {
		t.Fatal(err)
	}
	if deletedAt == nil || revokedAt == nil || releases != 1 || environments != 1 || audits != 1 {
		t.Fatalf("tombstone history = deleted=%v revoked=%v releases=%d environments=%d audits=%d",
			deletedAt, revokedAt, releases, environments, audits)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM apps WHERE id = $1`, appOneID); err == nil {
		t.Fatal("physical app deletion succeeded")
	}
}

func TestTombstoneWaitsForOrganizationBoundaryLockIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `UPDATE environments SET torn_down_at=now() WHERE org_id=$1 AND id=$2`, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }()
	if _, err := lockTx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1380270412))`, orgOneID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- dataStore.TombstoneApp(context.Background(), TombstoneParams{
			OrgID: orgOneID, ID: appOneID, ExternalCredentialsRevoked: true,
			Audit: testAudit("00000000-0000-0000-0000-000000000077", "app.tombstoned", appOneID),
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("tombstone bypassed organization boundary lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tombstone did not proceed after organization boundary lock release")
	}
}

func TestTombstoneTerminalizesWorkAndBlocksAllOperationalPathsIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "deploying")
	ctx := context.Background()

	lease, err := store.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET torn_down_at = now()
		WHERE org_id = $1 AND id = $2`, orgOneID, environmentOneID); err == nil {
		t.Fatal("environment teardown succeeded with an in-flight deploy lease")
	}
	if err := store.ReleaseDeployLease(ctx, lease,
		testAudit("00000000-0000-0000-0000-000000000075", "deploy.lease_released", environmentOneID)); err != nil {
		t.Fatal(err)
	}

	job, err := store.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID,
		repositoryOneID, environmentOneID, "00000000-0000-0000-0000-000000000071",
		commitSHA('7'), "tombstone-job"))
	if err != nil {
		t.Fatal(err)
	}
	runID := "00000000-0000-0000-0000-000000000072"
	if _, err := store.CreateReconciliationRun(ctx, CreateReconciliationRunParams{
		ID: runID, OrgID: orgOneID, ScopeKind: "environment", ScopeID: environmentOneID,
		ScopeAppID: stringPointer(appOneID), ScopeEnvironmentID: stringPointer(environmentOneID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET torn_down_at = now()
		WHERE org_id = $1 AND id = $2`, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if err := store.TombstoneApp(ctx, TombstoneParams{
		OrgID: orgOneID, ID: appOneID, ExternalCredentialsRevoked: true,
		Audit: testAudit("00000000-0000-0000-0000-000000000076", "app.tombstoned", appOneID),
	}); err != nil {
		t.Fatal(err)
	}

	var jobStatus, jobCode, runStatus, runCode string
	var jobLease, reconciliationLease, runLease *string
	if err := pool.QueryRow(ctx, `SELECT job.status, job.terminal_error_code,
		job.lease_owner, job.reconciliation_owner, run.status, run.error_code, run.lease_owner
		FROM orchestration_jobs job, reconciliation_runs run
		WHERE job.id = $1 AND run.id = $2`, job.ID, runID).Scan(&jobStatus, &jobCode,
		&jobLease, &reconciliationLease, &runStatus, &runCode, &runLease); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "cancelled" || jobCode != "ancestor_tombstoned" || jobLease != nil || reconciliationLease != nil ||
		runStatus != "cancelled" || runCode != "ancestor_tombstoned" || runLease != nil {
		t.Fatalf("terminalized state job=%s/%s/%v/%v run=%s/%s/%v", jobStatus, jobCode,
			jobLease, reconciliationLease, runStatus, runCode, runLease)
	}

	if _, err := store.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID,
		repositoryOneID, environmentOneID, "00000000-0000-0000-0000-000000000073",
		commitSHA('8'), "post-tombstone-job")); err == nil {
		t.Fatal("orchestration enqueue succeeded after app tombstone")
	}
	if _, err := store.ClaimOrchestration(ctx, orgOneID, "worker", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orchestration claim after tombstone error = %v", err)
	}
	if _, err := store.CreateReconciliationRun(ctx, CreateReconciliationRunParams{
		ID: "00000000-0000-0000-0000-000000000074", OrgID: orgOneID,
		ScopeKind: "app", ScopeID: appOneID, ScopeAppID: stringPointer(appOneID),
	}); err == nil {
		t.Fatal("reconciliation creation succeeded after app tombstone")
	}
	if _, err := store.AcquireDeployLease(ctx, orgOneID, environmentOneID, releaseOneID); !errors.Is(err, ErrConflict) {
		t.Fatalf("deploy lease acquisition after tombstone error = %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO orchestration_jobs
		(org_id,app_id,environment_id,github_repository_id,github_workflow_path,workflow_revision,dispatch_ref,
		desired_checkout_sha,desired_generation,idempotency_key)
		VALUES ($1,$2,$3,$4,'.github/workflows/deploy.yml',$5,'refs/heads/main',$5,1,'direct-post-tombstone')`,
		orgOneID, appOneID, environmentOneID, installationID("one")+2000, commitSHA('9')); err == nil {
		t.Fatal("direct orchestration insert succeeded after app tombstone")
	}
}

func TestEnvVarMutationCreatesImmutableReleaseLeaseAuditAndCompletesIdempotencyIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "live")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE apps SET health_check_path='/changed', scaling_mode='keda_http', min_replicas=0, max_replicas=9, cpu_request_millicores=777, cpu_limit_millicores=888, memory_request_mib=777, memory_limit_mib=888, runtime_class='runc' WHERE id=$1`, appOneID); err != nil {
		t.Fatal(err)
	}
	hash := requestHash("env-var")
	claim := IdempotencyClaimParams{OrgID: orgOneID, Scope: "environment.env_var.upsert", Key: "env-key",
		RequestHash: hash, Owner: "api", LeaseDuration: time.Minute, Retention: time.Hour}
	if _, err := store.ClaimIdempotency(ctx, claim); err != nil {
		t.Fatal(err)
	}
	result, err := store.MutateEnvVarAndCreateRelease(ctx, MutateEnvVarParams{
		OrgID: orgOneID, EnvironmentID: environmentOneID,
		EnvVarID: "00000000-0000-0000-0000-000000000061", Key: "DATABASE_URL",
		SecretRef: "k8s-secret://app-one-production/runtime#DATABASE_URL",
		ReleaseID: releaseTwoID,
		Idempotency: CompleteIdempotencyParams{Scope: claim.Scope, Key: claim.Key,
			RequestHash: hash, Owner: "api", Status: "completed", StatusCode: 202,
			Body: json.RawMessage(`{"release_id":"00000000-0000-0000-0000-000000000006"}`)},
		Audit: testAudit("00000000-0000-0000-0000-000000000062", "caller.action", releaseOneID),
	})
	if err != nil || result.Lease.ReleaseID != releaseTwoID || result.ConfigHash == "" {
		t.Fatalf("env-var mutation = %+v, %v", result, err)
	}
	var secretRef, kind, releaseStatus, idempotencyStatus, action, targetType, targetID string
	var snapshot json.RawMessage
	if err := pool.QueryRow(ctx, `
		SELECT env.secret_ref, release.kind, release.status, release.config_snapshot,
			idem.status, audit.action, audit.target_type, audit.target_id::text
		FROM env_vars env
		JOIN releases release ON release.id = $3
		JOIN api_idempotency_records idem ON idem.org_id = env.org_id
		  AND idem.scope = $4 AND idem.idempotency_key = $5
		JOIN audit_events audit ON audit.id = $6
		WHERE env.org_id = $1 AND env.environment_id = $2 AND env.key = 'DATABASE_URL'`,
		orgOneID, environmentOneID, releaseTwoID, claim.Scope, claim.Key,
		"00000000-0000-0000-0000-000000000062").Scan(&secretRef, &kind, &releaseStatus,
		&snapshot, &idempotencyStatus, &action, &targetType, &targetID); err != nil {
		t.Fatal(err)
	}
	if secretRef != "k8s-secret://app-one-production/runtime#DATABASE_URL" || kind != "env_change" ||
		releaseStatus != "pending" || idempotencyStatus != "completed" ||
		action != "environment_variable.upserted" || targetType != "environment" || targetID != environmentOneID {
		t.Fatalf("env mutation state = %s/%s/%s/%s audit=%s/%s/%s snapshot=%s",
			secretRef, kind, releaseStatus, idempotencyStatus, action, targetType, targetID, snapshot)
	}
	if _, err := pool.Exec(ctx, `UPDATE releases SET config_snapshot = '{"mutated":true}' WHERE id = $1`, releaseTwoID); err == nil {
		t.Fatal("env-change release snapshot mutation unexpectedly succeeded")
	}
	liveEnvelope, err := DecodeReleaseSnapshot(testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	envEnvelope, err := DecodeReleaseSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if envEnvelope.DesiredState.Deployment.Container.Resources != liveEnvelope.DesiredState.Deployment.Container.Resources || envEnvelope.DesiredState.Deployment.RuntimeClassName != liveEnvelope.DesiredState.Deployment.RuntimeClassName || envEnvelope.DesiredState.Scaling.Kind != liveEnvelope.DesiredState.Scaling.Kind || envEnvelope.EnvVarRevision == nil {
		t.Fatalf("env-change snapshot did not preserve live desired state: %+v", envEnvelope)
	}
}

func TestEnvVarMutationRollsBackProtectedMutationWhenIdempotencyLeaseIsLostIntegration(t *testing.T) {
	store, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "live")
	ctx := context.Background()
	hash := requestHash("env-var-lost-lease")
	claim := IdempotencyClaimParams{OrgID: orgOneID, Scope: "environment.env_var.upsert", Key: "lost-lease",
		RequestHash: hash, Owner: "api", LeaseDuration: time.Minute, Retention: time.Hour}
	if _, err := store.ClaimIdempotency(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE api_idempotency_records SET lease_expires_at = now() - interval '1 second'
		WHERE org_id = $1 AND scope = $2 AND idempotency_key = $3`, orgOneID, claim.Scope, claim.Key); err != nil {
		t.Fatal(err)
	}
	_, err := store.MutateEnvVarAndCreateRelease(ctx, MutateEnvVarParams{
		OrgID: orgOneID, EnvironmentID: environmentOneID,
		EnvVarID: "00000000-0000-0000-0000-000000000063", Key: "API_TOKEN",
		SecretRef: "k8s-secret://app-one-production/runtime#API_TOKEN",
		ReleaseID: releaseTwoID,
		Idempotency: CompleteIdempotencyParams{Scope: claim.Scope, Key: claim.Key,
			RequestHash: hash, Owner: "api", Status: "completed", StatusCode: 202,
			Body: json.RawMessage(`{"ok":true}`)},
		Audit: testAudit("00000000-0000-0000-0000-000000000064", "ignored", environmentOneID),
	})
	if !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("lost idempotency lease mutation error = %v", err)
	}
	var envVars, releases, audits int
	var inProgressRelease *string
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM env_vars WHERE org_id = $1 AND environment_id = $2 AND key = 'API_TOKEN'),
		(SELECT count(*) FROM releases WHERE id = $3),
		(SELECT count(*) FROM audit_events WHERE id = $4),
		(SELECT in_progress_release_id::text FROM environments WHERE org_id = $1 AND id = $2)`,
		orgOneID, environmentOneID, releaseTwoID,
		"00000000-0000-0000-0000-000000000064").Scan(&envVars, &releases, &audits, &inProgressRelease); err != nil {
		t.Fatal(err)
	}
	if envVars != 0 || releases != 0 || audits != 0 || inProgressRelease != nil {
		t.Fatalf("protected mutation was not rolled back: env_vars=%d releases=%d audits=%d lease=%v",
			envVars, releases, audits, inProgressRelease)
	}
}

func TestEnvVarMutationRejectsInvalidSecretReferenceBeforeMutationIntegration(t *testing.T) {
	for _, test := range []struct {
		name      string
		orgID     string
		envID     string
		secretRef string
		tearDown  bool
		want      error
	}{
		{name: "malformed", orgID: orgOneID, envID: environmentOneID, secretRef: "runtime", want: ErrInvalidArgument},
		{name: "foreign namespace", orgID: orgOneID, envID: environmentOneID, secretRef: "k8s-secret://other/runtime#DATABASE_URL", want: ErrInvalidArgument},
		{name: "cross organization", orgID: orgTwoID, envID: environmentOneID, secretRef: "k8s-secret://app-one-production/runtime#DATABASE_URL", want: ErrConflict},
		{name: "not found", orgID: orgOneID, envID: "00000000-0000-0000-0000-000000000099", secretRef: "k8s-secret://app-one-production/runtime#DATABASE_URL", want: ErrConflict},
		{name: "torn down", orgID: orgOneID, envID: environmentOneID, secretRef: "k8s-secret://app-one-production/runtime#DATABASE_URL", tearDown: true, want: ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "live")
			if test.tearDown {
				if _, err := pool.Exec(t.Context(), `UPDATE environments SET torn_down_at=now() WHERE id=$1`, environmentOneID); err != nil {
					t.Fatal(err)
				}
			}
			_, err := dataStore.ReplaceEnvVarsAndCreateRelease(t.Context(), ReplaceEnvVarsParams{
				OrgID: test.orgID, EnvironmentID: test.envID, ReleaseID: releaseTwoID,
				Variables: []EnvironmentVariableValue{{Key: "DATABASE_URL", SecretRef: test.secretRef, IsSecret: true}},
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("ReplaceEnvVarsAndCreateRelease() error = %v, want %v", err, test.want)
			}
			assertCount(t, pool, "env_vars", 0)
		})
	}
}

func TestRollbackTargetsMustHavePreviouslyServedIntegration(t *testing.T) {
	for _, status := range []string{"failed", "pending", "deploying", "live", "superseded"} {
		t.Run(status, func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, status)
			hash := requestHash("rollback-" + status)
			claim := IdempotencyClaimParams{OrgID: orgOneID, Scope: "rollback.accept", Key: "rollback-" + status,
				RequestHash: hash, Owner: "user", LeaseDuration: time.Minute, Retention: time.Hour}
			if _, err := dataStore.ClaimIdempotency(t.Context(), claim); err != nil {
				t.Fatal(err)
			}
			_, err := dataStore.AcceptRelease(t.Context(), AcceptReleaseParams{
				ID: releaseTwoID, OrgID: orgOneID, EnvironmentID: environmentOneID, Kind: "rollback",
				RollbackTargetID: stringPointer(releaseOneID),
				Idempotency: CompleteIdempotencyParams{Scope: claim.Scope, Key: claim.Key, RequestHash: hash,
					Owner: claim.Owner, Status: "completed", StatusCode: 202, Body: json.RawMessage(`{"status":"queued"}`)},
				Audit: testAudit("00000000-0000-0000-0000-000000000098", "ignored", releaseTwoID),
			})
			valid := status == "live" || status == "superseded"
			if valid && err != nil {
				t.Fatalf("valid %s rollback target rejected: %v", status, err)
			}
			if !valid && !errors.Is(err, ErrNotFound) {
				t.Fatalf("invalid %s rollback target error = %v, want ErrNotFound", status, err)
			}
		})
	}
}

func TestTenantScopedResourceQueriesAndAcceptedDeployIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	ctx := context.Background()

	if _, err := dataStore.GetProject(ctx, orgTwoID, projectOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant project read error = %v", err)
	}
	if _, err := dataStore.GetApp(ctx, orgTwoID, appOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant app read error = %v", err)
	}
	if _, err := dataStore.GetEnvironment(ctx, orgTwoID, environmentOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant environment read error = %v", err)
	}

	hash := requestHash("accepted-deploy")
	claim := IdempotencyClaimParams{OrgID: orgOneID, Scope: "deploy.accept", Key: "deploy-one",
		RequestHash: hash, Owner: "user", LeaseDuration: time.Minute, Retention: time.Hour}
	if _, err := dataStore.ClaimIdempotency(ctx, claim); err != nil {
		t.Fatal(err)
	}
	snapshot := testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("a", 64))
	body := json.RawMessage(`{"operation_id":"00000000-0000-0000-0000-000000000005","resource_id":"00000000-0000-0000-0000-000000000005","status":"queued"}`)
	lease, err := dataStore.AcceptRelease(ctx, AcceptReleaseParams{
		ID: releaseOneID, OrgID: orgOneID, EnvironmentID: environmentOneID,
		ImageRef: "ghcr.io/acme/test@sha256:" + strings.Repeat("a", 64), Kind: "deploy",
		ConfigSnapshot: snapshot, ConfigHash: strings.Repeat("b", 64),
		Idempotency: CompleteIdempotencyParams{OrgID: orgOneID, Scope: claim.Scope,
			Key: claim.Key, RequestHash: hash, Owner: claim.Owner, Status: "completed",
			StatusCode: 202, Body: body},
		Audit: testAudit("00000000-0000-0000-0000-000000000091", "ignored", releaseOneID),
	})
	if err != nil || lease.ReleaseID != releaseOneID {
		t.Fatalf("AcceptRelease() = %+v, %v", lease, err)
	}
	var releaseStatus, idempotencyStatus, inProgressID string
	if err := pool.QueryRow(ctx, `SELECT release.status, record.status, environment.in_progress_release_id::text
		FROM releases release JOIN environments environment ON environment.id=release.environment_id
		JOIN api_idempotency_records record ON record.org_id=release.org_id
		AND record.scope=$2 AND record.idempotency_key=$3 WHERE release.id=$1`, releaseOneID,
		claim.Scope, claim.Key).Scan(&releaseStatus, &idempotencyStatus, &inProgressID); err != nil {
		t.Fatal(err)
	}
	if releaseStatus != "pending" || idempotencyStatus != "completed" || inProgressID != releaseOneID {
		t.Fatalf("accepted state = release:%s idempotency:%s lease:%s", releaseStatus, idempotencyStatus, inProgressID)
	}
	replay, err := dataStore.ClaimIdempotency(ctx, claim)
	if err != nil || replay.Disposition != IdempotencyReplay || replay.StatusCode != 202 {
		t.Fatalf("accepted replay = %+v, %v", replay, err)
	}
}

func TestAcceptDeployInheritsCurrentEnvRevisionAtomicallyIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	revisionID := "00000000-0000-0000-0000-0000000000a1"
	liveReleaseID := "00000000-0000-0000-0000-0000000000a2"
	deployReleaseID := "00000000-0000-0000-0000-0000000000a3"
	variables := []ReleaseEnvironment{{Key: "DATABASE_URL", SecretRef: "k8s-secret://app-one-production/runtime#DATABASE_URL", IsSecret: true}}
	liveEnvelope, err := DecodeReleaseSnapshot(testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	liveEnvelope.EnvVarRevision = &EnvVarRevision{Version: 1, ID: revisionID, Revision: 1, Hash: ComputeEnvVarRevisionHash(variables), Variables: variables}
	destinationName := EnvDestinationSecretName(liveEnvelope.DesiredState.Deployment.Identity.Name, revisionID)
	liveEnvelope.DesiredState.Secret = &SecretIntent{Version: 1, Identity: ObjectIdentity{Name: destinationName, Namespace: "app-one-production", Labels: liveEnvelope.DesiredState.Deployment.Identity.Labels}, Immutable: true, Entries: []SecretEntryIntent{{Key: variables[0].Key, SecretRef: variables[0].SecretRef}}}
	liveEnvelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{destinationName}
	liveEnvelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-revision-id"] = revisionID
	liveEnvelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = liveEnvelope.EnvVarRevision.Hash
	liveSnapshot, liveHash, err := EncodeReleaseSnapshot(liveEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO env_var_revisions(id,org_id,environment_id,revision) VALUES($1,$2,$3,1)`, revisionID, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO env_var_revision_entries(org_id,environment_id,env_var_revision_id,key,secret_ref,is_secret) VALUES($1,$2,$3,$4,$5,true)`, orgOneID, environmentOneID, revisionID, variables[0].Key, variables[0].SecretRef); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO releases(id,org_id,environment_id,image_ref,kind,env_var_revision_id,config_snapshot,config_hash,status) VALUES($1,$2,$3,$4,'env_change',$5,$6,$7,'live')`, liveReleaseID, orgOneID, environmentOneID, liveEnvelope.DesiredState.Deployment.Container.Image, revisionID, liveSnapshot, liveHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET current_env_var_revision_id=$2 WHERE id=$1`, environmentOneID, revisionID); err != nil {
		t.Fatal(err)
	}

	newEnvelope, err := DecodeReleaseSnapshot(testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("c", 64)))
	if err != nil {
		t.Fatal(err)
	}
	newEnvelope.DesiredState.Deployment.Container.Port = 9090
	newEnvelope.DesiredState.Service.TargetPort = 9090
	newEnvelope.DesiredState.Deployment.Container.ReadinessProbe.Port = 9090
	newSnapshot, newHash, err := EncodeReleaseSnapshot(newEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	claim := IdempotencyClaimParams{OrgID: orgOneID, Scope: "deploy.accept", Key: "inherit-env", RequestHash: requestHash("inherit-env"), Owner: "user", LeaseDuration: time.Minute, Retention: time.Hour}
	if _, err := dataStore.ClaimIdempotency(ctx, claim); err != nil {
		t.Fatal(err)
	}
	lease, err := dataStore.AcceptRelease(ctx, AcceptReleaseParams{ID: deployReleaseID, OrgID: orgOneID, EnvironmentID: environmentOneID, ImageRef: newEnvelope.DesiredState.Deployment.Container.Image, Kind: "deploy", ConfigSnapshot: newSnapshot, ConfigHash: newHash, InheritCurrentEnv: true, Idempotency: CompleteIdempotencyParams{Scope: claim.Scope, Key: claim.Key, RequestHash: claim.RequestHash, Owner: claim.Owner, Status: "completed", StatusCode: 202, Body: json.RawMessage(`{"status":"queued"}`)}, Audit: testAudit("00000000-0000-0000-0000-0000000000a4", "ignored", deployReleaseID)})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := dataStore.GetRelease(ctx, orgOneID, deployReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	acceptedEnvelope, err := DecodeReleaseSnapshot(accepted.ConfigSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.EnvVarRevisionID == nil || *accepted.EnvVarRevisionID != revisionID || acceptedEnvelope.EnvVarRevision == nil || acceptedEnvelope.EnvVarRevision.ID != revisionID || acceptedEnvelope.DesiredState.Secret == nil || acceptedEnvelope.DesiredState.Secret.Identity.Name != destinationName || !reflect.DeepEqual(acceptedEnvelope.DesiredState.Deployment.Container.EnvFromSecrets, []string{destinationName}) || acceptedEnvelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-revision-id"] != revisionID || acceptedEnvelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] != liveEnvelope.EnvVarRevision.Hash {
		t.Fatalf("accepted deploy did not inherit env state: release=%+v envelope=%+v", accepted, acceptedEnvelope)
	}
	if acceptedEnvelope.DesiredState.Deployment.Container.Image != newEnvelope.DesiredState.Deployment.Container.Image || acceptedEnvelope.DesiredState.Service.TargetPort != 9090 || acceptedEnvelope.DesiredState.Deployment.Container.Port != 9090 {
		t.Fatalf("accepted deploy lost new mutable basis: %+v", acceptedEnvelope.DesiredState)
	}
	seedCompletedOrchestrationRoute(t, dataStore, pool, orgOneID, repositoryOneID, environmentOneID, "00000000-0000-0000-0000-0000000000a5", commitSHA('a'), "inherit-finalize-route")
	if _, err := dataStore.FinalizeReleaseLive(ctx, FinalizeReleaseParams{Lease: lease, Hostname: "planned-one.example.test", Audit: testAudit("00000000-0000-0000-0000-0000000000a6", "ignored", deployReleaseID)}); err != nil {
		t.Fatal(err)
	}
	var currentRevision string
	if err := pool.QueryRow(ctx, `SELECT current_env_var_revision_id::text FROM environments WHERE id=$1`, environmentOneID).Scan(&currentRevision); err != nil {
		t.Fatal(err)
	}
	if currentRevision != revisionID {
		t.Fatalf("current revision=%s want=%s", currentRevision, revisionID)
	}
}

func TestAcceptDeployEnvInheritanceSerializesAgainstRevisionChangeIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	baseRevision := "00000000-0000-0000-0000-0000000000b1"
	newRevision := "00000000-0000-0000-0000-0000000000b2"
	liveID := "00000000-0000-0000-0000-0000000000b3"
	deployID := "00000000-0000-0000-0000-0000000000b4"
	seedLiveEnvRevision(t, pool, baseRevision, liveID, "ONE")
	claim := IdempotencyClaimParams{OrgID: orgOneID, Scope: "deploy.accept", Key: "serialized-env", RequestHash: requestHash("serialized-env"), Owner: "user", LeaseDuration: time.Minute, Retention: time.Hour}
	if _, err := dataStore.ClaimIdempotency(ctx, claim); err != nil {
		t.Fatal(err)
	}
	basis := testReleaseSnapshot("ghcr.io/acme/test@sha256:" + strings.Repeat("d", 64))
	_, basisHash, err := EncodeReleaseSnapshot(mustDecodeSnapshot(t, basis))
	if err != nil {
		t.Fatal(err)
	}
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `SELECT id FROM environments WHERE id=$1 FOR UPDATE`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := dataStore.AcceptRelease(context.Background(), AcceptReleaseParams{ID: deployID, OrgID: orgOneID, EnvironmentID: environmentOneID, ImageRef: "ghcr.io/acme/test@sha256:" + strings.Repeat("d", 64), Kind: "deploy", ConfigSnapshot: basis, ConfigHash: basisHash, InheritCurrentEnv: true, Idempotency: CompleteIdempotencyParams{Scope: claim.Scope, Key: claim.Key, RequestHash: claim.RequestHash, Owner: claim.Owner, Status: "completed", StatusCode: 202, Body: json.RawMessage(`{"status":"queued"}`)}, Audit: testAudit("00000000-0000-0000-0000-0000000000b5", "ignored", deployID)})
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("accept completed while environment lock held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	newSnapshot, newHash := envRevisionSnapshot(t, newRevision, "TWO")
	if _, err := lockTx.Exec(ctx, `UPDATE releases SET status='superseded' WHERE id=$1`, liveID); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `INSERT INTO env_var_revisions(id,org_id,environment_id,revision) VALUES($1,$2,$3,2)`, newRevision, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `INSERT INTO env_var_revision_entries(org_id,environment_id,env_var_revision_id,key,secret_ref,is_secret) VALUES($1,$2,$3,'TOKEN',$4,true)`, orgOneID, environmentOneID, newRevision, "k8s-secret://app-one-production/runtime#TWO"); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `INSERT INTO releases(id,org_id,environment_id,image_ref,kind,env_var_revision_id,config_snapshot,config_hash,status) VALUES('00000000-0000-0000-0000-0000000000b6',$1,$2,$3,'env_change',$4,$5,$6,'live')`, orgOneID, environmentOneID, "ghcr.io/acme/test@sha256:"+strings.Repeat("a", 64), newRevision, newSnapshot, newHash); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `UPDATE environments SET current_env_var_revision_id=$2 WHERE id=$1`, environmentOneID, newRevision); err != nil {
		t.Fatal(err)
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	accepted, err := dataStore.GetRelease(ctx, orgOneID, deployID)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := DecodeReleaseSnapshot(accepted.ConfigSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.EnvVarRevisionID == nil || *accepted.EnvVarRevisionID != newRevision || envelope.EnvVarRevision == nil || envelope.EnvVarRevision.ID != newRevision || envelope.DesiredState.Secret.Identity.Name != EnvDestinationSecretName("app", newRevision) || envelope.EnvVarRevision.Variables[0].SecretRef != "k8s-secret://app-one-production/runtime#TWO" {
		t.Fatalf("mixed inherited snapshot: release=%+v envelope=%+v", accepted, envelope)
	}
}

func TestProjectAndAppCreateUpdateAreTenantScopedIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	projectID := "00000000-0000-0000-0000-000000000092"
	claim := IdempotencyClaimParams{OrgID: orgOneID, Scope: "project.create", Key: "project-create",
		RequestHash: requestHash("project-create"), Owner: "user", LeaseDuration: time.Minute, Retention: time.Hour}
	if _, err := dataStore.ClaimIdempotency(ctx, claim); err != nil {
		t.Fatal(err)
	}
	project, err := dataStore.CreateProject(ctx, SaveProjectParams{
		Project: Project{ID: projectID, OrgID: orgOneID, Name: "Created", Slug: "created"},
		Idempotency: CompleteIdempotencyParams{Scope: claim.Scope, Key: claim.Key,
			RequestHash: claim.RequestHash, Owner: claim.Owner, Status: "completed", StatusCode: 201,
			Body: json.RawMessage(`{"id":"00000000-0000-0000-0000-000000000092"}`)},
		Audit: testAudit("00000000-0000-0000-0000-000000000093", "ignored", projectID),
	})
	if err != nil || project.OrgID != orgOneID {
		t.Fatalf("CreateProject() = %+v, %v", project, err)
	}
	appID := "00000000-0000-0000-0000-000000000094"
	claim = IdempotencyClaimParams{OrgID: orgOneID, Scope: "app.create", Key: "app-create",
		RequestHash: requestHash("app-create"), Owner: "user", LeaseDuration: time.Minute, Retention: time.Hour}
	if _, err := dataStore.ClaimIdempotency(ctx, claim); err != nil {
		t.Fatal(err)
	}
	app, err := dataStore.CreateApp(ctx, SaveAppParams{App: App{ID: appID, OrgID: orgOneID,
		ProjectID: projectID, Name: "Created App", Slug: "created-app", HostnameLabel: "created-app",
		ImageRepository: stringPointer("ghcr.io/acme/created-app"), GitHubWorkflowPath: ".github/workflows/deploy.yml",
		ProductionBranch: "main", AnnotationPolicy: "validate_if_present", ImageVisibility: "public",
		ContainerPort: 8080, HealthCheckPath: "/", ScalingMode: "hpa", MinReplicas: 1, MaxReplicas: 3,
		HPATargetCPUPercent: 70, CPURequestMillicores: 100, CPULimitMillicores: 1000,
		MemoryRequestMiB: 128, MemoryLimitMiB: 512, PreviewsEnabled: true,
		MaxPreviewEnvironments: 10, PreviewIdleTTLHours: 168, DeployTimeoutSeconds: 600,
		ReleaseRetentionCount: 5, RuntimeClass: "gvisor"},
		Idempotency: CompleteIdempotencyParams{Scope: claim.Scope, Key: claim.Key,
			RequestHash: claim.RequestHash, Owner: claim.Owner, Status: "completed", StatusCode: 201,
			Body: json.RawMessage(`{"id":"00000000-0000-0000-0000-000000000094"}`)},
		Audit: testAudit("00000000-0000-0000-0000-000000000095", "ignored", appID)})
	if err != nil || app.ProjectID != projectID || app.RuntimeClass != "gvisor" || app.KEDAHTTPTargetPendingRequests != 100 {
		t.Fatalf("CreateApp() = %+v, %v", app, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE org_id=$1 AND project_id=$2`, orgOneID, projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("created app count = %d", count)
	}
}

func TestCreateProductionEnvironmentReservesAndQueuesProvisionIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE environments SET torn_down_at=now() WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	environmentID := "00000000-0000-0000-0000-000000000041"
	environment, err := dataStore.CreateProductionEnvironment(ctx, CreateProductionEnvironmentParams{
		ID: environmentID, OrgID: orgOneID, AppID: appOneID, Branch: "release",
		Namespace: "app-one-release-production", Hostname: "release-one.example.test",
		Audit: testAudit("00000000-0000-0000-0000-000000000042", "ignored", environmentID),
	})
	if err != nil {
		t.Fatalf("CreateProductionEnvironment() error = %v", err)
	}
	if environment.LifecycleStatus != "reserved" || environment.Kind != "production" ||
		environment.PlannedHostname == nil || *environment.PlannedHostname != "release-one.example.test" {
		t.Fatalf("CreateProductionEnvironment() = %+v", environment)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM environment_lifecycle_jobs
		WHERE org_id=$1 AND environment_id=$2`, orgOneID, environmentID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("lifecycle job count = %d, want exactly 1", jobs)
	}
	var kind, status, reason string
	if err := pool.QueryRow(ctx, `SELECT kind,status,reason FROM environment_lifecycle_jobs
		WHERE org_id=$1 AND environment_id=$2`, orgOneID, environmentID).Scan(&kind, &status, &reason); err != nil {
		t.Fatal(err)
	}
	if kind != "provision" || status != "queued" || reason != "production_create" {
		t.Fatalf("lifecycle job kind=%q status=%q reason=%q, want provision/queued/production_create", kind, status, reason)
	}
}

func TestClaimEnvironmentLifecycleJobReturnsProductionKindAndPlannedHostnameIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE environments SET torn_down_at=now() WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	environmentID := "00000000-0000-0000-0000-000000000041"
	if _, err := dataStore.CreateProductionEnvironment(ctx, CreateProductionEnvironmentParams{
		ID: environmentID, OrgID: orgOneID, AppID: appOneID, Branch: "release",
		Namespace: "app-one-release-production", Hostname: "release-one.example.test",
		Audit: testAudit("00000000-0000-0000-0000-000000000042", "ignored", environmentID),
	}); err != nil {
		t.Fatalf("CreateProductionEnvironment() error = %v", err)
	}
	job, err := dataStore.ClaimEnvironmentLifecycleJob(ctx, orgOneID, "worker", time.Minute)
	if err != nil {
		t.Fatalf("ClaimEnvironmentLifecycleJob() error = %v", err)
	}
	if job.EnvironmentID != environmentID || job.Kind != "provision" || job.Status != "leased" {
		t.Fatalf("claimed job = %+v", job)
	}
	if job.EnvironmentKind != "production" {
		t.Fatalf("claimed job EnvironmentKind = %q, want production", job.EnvironmentKind)
	}
	if job.PlannedHostname == nil || *job.PlannedHostname != "release-one.example.test" {
		t.Fatalf("claimed job PlannedHostname = %v, want %q", job.PlannedHostname, "release-one.example.test")
	}
}

func TestRequestPreviewTeardownByIDCoversProductionIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	job, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID,
		repositoryOneID, environmentOneID, "00000000-0000-0000-0000-000000000043",
		commitSHA('4'), "production-teardown-active"))
	if err != nil {
		t.Fatal(err)
	}
	var generationBefore int64
	if err := pool.QueryRow(ctx, `SELECT desired_generation FROM environments WHERE id=$1`, environmentOneID).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}
	environment, err := dataStore.RequestPreviewTeardown(ctx, PreviewTeardownParams{
		OrgID: orgOneID, AppID: appOneID, Branch: "main", EnvironmentID: environmentOneID,
		IdempotencyKey: "production-teardown", EventKind: "teardown", Reason: "app_decommission",
		Audit: testAudit("00000000-0000-0000-0000-000000000047", "ignored", environmentOneID),
	})
	if err != nil {
		t.Fatalf("RequestPreviewTeardown() production environment error = %v", err)
	}
	if environment.ID != environmentOneID || environment.LifecycleStatus != "teardown_requested" {
		t.Fatalf("RequestPreviewTeardown() = %+v", environment)
	}
	var kind, status, reason, jobStatus, terminalCode string
	var terminalAt *time.Time
	var generationAfter int64
	if err := pool.QueryRow(ctx, `SELECT lifecycle.kind,lifecycle.status,lifecycle.reason,
		job.status,job.terminal_error_code,job.terminal_at,environment.desired_generation
		FROM environment_lifecycle_jobs lifecycle
		JOIN environments environment ON environment.id=lifecycle.environment_id
		JOIN orchestration_jobs job ON job.id=$3
		WHERE lifecycle.org_id=$1 AND lifecycle.environment_id=$2 AND lifecycle.kind='teardown'`,
		orgOneID, environmentOneID, job.ID).Scan(&kind, &status, &reason, &jobStatus, &terminalCode, &terminalAt, &generationAfter); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || reason != "app_decommission" || generationAfter != generationBefore+1 ||
		jobStatus != "cancelled" || terminalCode != "environment_teardown_requested" || terminalAt == nil {
		t.Fatalf("teardown state lifecycle=%s/%s generation=%d->%d orchestration=%s/%s/%v",
			status, reason, generationBefore, generationAfter, jobStatus, terminalCode, terminalAt)
	}
}

func TestRequestPreviewTeardownFencesEveryActiveOrchestrationStateIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	activeStates := []string{"queued", "leased", "dispatch_reconciling", "dispatched", "resolving_image", "deploying"}
	for index, activeState := range activeStates {
		branch := "feature/teardown-" + activeState
		push, err := dataStore.AcceptPreviewPush(ctx, previewPushParams("teardown-"+activeState, branch, commitSHA(byte('1'+index))))
		if err != nil {
			t.Fatal(err)
		}
		jobID := ""
		if err := pool.QueryRow(ctx, `SELECT id::text FROM orchestration_jobs WHERE environment_id=$1`, push.Environment.ID).Scan(&jobID); err != nil {
			t.Fatal(err)
		}
		switch activeState {
		case "leased":
			_, err = pool.Exec(ctx, `UPDATE orchestration_jobs SET status='leased',lease_owner='teardown-worker',
				lease_expires_at=now()+interval '5 minutes' WHERE id=$1`, jobID)
		case "dispatch_reconciling":
			_, err = pool.Exec(ctx, `UPDATE orchestration_jobs SET status='dispatch_reconciling',attempt=1,
				dispatch_requested_at=now(),dispatch_ref_observed_sha=desired_checkout_sha,
				dispatch_ambiguity_state='possible',reconcile_after=now()+interval '30 seconds',
				dispatch_deadline_at=now()+interval '5 minutes' WHERE id=$1`, jobID)
		case "dispatched":
			_, err = pool.Exec(ctx, `UPDATE orchestration_jobs SET status='dispatched',attempt=1,
				dispatch_requested_at=now(),dispatch_ref_observed_sha=desired_checkout_sha,
				dispatch_ambiguity_state='matched',github_dispatch_identity='cyclopus:'||id,
				github_run_id=$2,github_run_status='in_progress',run_deadline_at=now()+interval '45 minutes' WHERE id=$1`, jobID, 9000+index)
		case "resolving_image":
			_, err = pool.Exec(ctx, `UPDATE orchestration_jobs SET status='resolving_image',attempt=1,
				github_run_status='completed',github_run_conclusion='success',github_run_head_sha=desired_checkout_sha WHERE id=$1`, jobID)
		case "deploying":
			_, err = pool.Exec(ctx, `UPDATE orchestration_jobs job SET status='deploying',attempt=1,
				github_run_status='completed',github_run_conclusion='success',github_run_head_sha=desired_checkout_sha,
				image_tag_ref=app.image_repository||':'||desired_checkout_sha,resolved_image_digest='sha256:'||repeat('9',64),
				immutable_image_ref=app.image_repository||'@sha256:'||repeat('9',64),
				oci_media_type='application/vnd.oci.image.manifest.v1+json',oci_resolution_state='resolved',
				oci_resolved_at=now(),deployment_state='ready' FROM apps app WHERE job.id=$1 AND app.id=job.app_id`, jobID)
		}
		if err != nil {
			t.Fatal(err)
		}
		environment, err := dataStore.RequestPreviewTeardown(ctx, PreviewTeardownParams{
			OrgID: orgOneID, AppID: appOneID, EnvironmentID: push.Environment.ID,
			IdempotencyKey: "teardown-request-" + activeState, EventKind: "teardown", Reason: "state_coverage",
			Audit: testAudit(fmt.Sprintf("00000000-0000-0000-0000-%012d", 900+index), "ignored", push.Environment.ID),
		})
		if err != nil || environment.LifecycleStatus != "teardown_requested" {
			t.Fatalf("teardown %s = %+v, %v", activeState, environment, err)
		}
		var generation int64
		var status, code string
		if err := pool.QueryRow(ctx, `SELECT environment.desired_generation,job.status,job.terminal_error_code
			FROM environments environment JOIN orchestration_jobs job ON job.environment_id=environment.id
			WHERE environment.id=$1`, push.Environment.ID).Scan(&generation, &status, &code); err != nil {
			t.Fatal(err)
		}
		if generation != 3 || status != "cancelled" || code != "environment_teardown_requested" {
			t.Fatalf("teardown %s state = generation:%d status:%s code:%s", activeState, generation, status, code)
		}
	}
}

func TestRequestPreviewTeardownByBranchStaysPreviewOnlyIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	// Branch-delete against a branch whose only environment is production is
	// rejected: branch teardown semantics are preview-only (§4.4).
	if _, err := dataStore.RequestPreviewTeardown(ctx, PreviewTeardownParams{
		OrgID: orgOneID, AppID: appOneID, Branch: "main",
		IdempotencyKey: "production-branch-delete", EventKind: "delete", Reason: "branch_deleted",
		Audit: testAudit("00000000-0000-0000-0000-000000000044", "ignored", appOneID),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("production branch-delete error = %v, want ErrNotFound", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT lifecycle_status FROM environments WHERE org_id=$1 AND id=$2`,
		orgOneID, environmentOneID).Scan(&status); err != nil || status != "ready" {
		t.Fatalf("production environment lifecycle_status = %q, %v; want untouched ready", status, err)
	}
	// Branch-delete against a preview environment is unchanged.
	previewID := "00000000-0000-0000-0000-000000000045"
	if _, err := pool.Exec(ctx, `INSERT INTO environments (id,org_id,app_id,branch,kind,preview_slug,namespace,planned_hostname,lifecycle_status)
		VALUES ($1,$2,$3,'feature/x','preview','calm-otter-0001','preview-calm-otter-0001','calm-otter-0001.previews.example.test','ready')`,
		previewID, orgOneID, appOneID); err != nil {
		t.Fatal(err)
	}
	environment, err := dataStore.RequestPreviewTeardown(ctx, PreviewTeardownParams{
		OrgID: orgOneID, AppID: appOneID, Branch: "feature/x",
		IdempotencyKey: "preview-branch-delete", EventKind: "delete", Reason: "branch_deleted",
		Audit: testAudit("00000000-0000-0000-0000-000000000046", "ignored", previewID),
	})
	if err != nil || environment.ID != previewID || environment.LifecycleStatus != "teardown_requested" {
		t.Fatalf("preview branch-delete = %+v, %v", environment, err)
	}
}

func newIntegrationStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("POSTGRES_TEST_URL")
	}
	if databaseURL == "" {
		t.Skip("CYCLOPUS_TEST_DATABASE_URL or POSTGRES_TEST_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	if version < 180000 || version >= 190000 {
		admin.Close(ctx)
		t.Skipf("PostgreSQL 18 required, server_version_num=%d", version)
	}
	schema := fmt.Sprintf("store_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close(context.Background())
	})

	testURL := withSearchPath(databaseURL, schema)
	if err := Migrate(ctx, MigrationConfig{
		URL: testURL, LockTimeout: 5 * time.Second, Migrations: Migrations,
	}); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(testURL)
	if err != nil {
		t.Fatal(err)
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		var searchPath string
		if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
			return fmt.Errorf("read repository test search_path: %w", err)
		}
		if searchPath != (pgx.Identifier{schema}).Sanitize() {
			return fmt.Errorf("repository test search_path = %q, want %q", searchPath, schema)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return New(pool), pool
}

type tenantFixture struct {
	orgID, projectID, appID, environmentID, repositoryID string
	slug, namespace, repo                                string
}

func tenantOne() tenantFixture {
	return tenantFixture{orgOneID, projectOneID, appOneID, environmentOneID, repositoryOneID,
		"one", "app-one-production", "acme/one"}
}

func tenantTwo() tenantFixture {
	return tenantFixture{orgTwoID, projectTwoID, appTwoID, environmentTwoID, repositoryTwoID,
		"two", "app-two-production", "acme/two"}
}

func seedTenant(t *testing.T, pool *pgxpool.Pool, fixture tenantFixture) {
	t.Helper()
	ctx := context.Background()
	queries := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO orgs (id, name, slug) VALUES ($1, $2, $3)`, []any{fixture.orgID, "Org " + fixture.slug, "org-" + fixture.slug}},
		{`INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,
			access_state,reconciliation_generation,last_api_confirmed_generation) VALUES($1,$2,$3,$4,'Organization','selected','active',1,1)`, []any{installationID(fixture.slug), fixture.orgID, installationID(fixture.slug) + 1000, "owner-" + fixture.slug}},
		{`INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,
			visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation)
			VALUES($1,$2,$3,$4,$5,$6,$7,'private','main','active',1,1)`, []any{installationID(fixture.slug) + 2000, fixture.orgID, installationID(fixture.slug), "node-" + fixture.slug, strings.Split(fixture.repo, "/")[0], strings.Split(fixture.repo, "/")[1], fixture.repo}},
		{`INSERT INTO git_connections (id,org_id,github_repository_id) VALUES($1,$2,$3)`, []any{fixture.repositoryID, fixture.orgID, installationID(fixture.slug) + 2000}},
		{`INSERT INTO projects (id, org_id, name, slug, git_connection_id)
			VALUES ($1, $2, $3, $4, $5)`, []any{fixture.projectID, fixture.orgID, "Project " + fixture.slug, "project-" + fixture.slug, fixture.repositoryID}},
		{`INSERT INTO apps (id, org_id, project_id, name, slug, hostname_label, image_repository,github_workflow_path,production_branch,deploy_timeout_seconds, previews_enabled)
			VALUES ($1, $2, $3, $4, $5, $6, $7,'.github/workflows/deploy.yml','main',1,true)`, []any{fixture.appID, fixture.orgID, fixture.projectID, "App " + fixture.slug, "app-" + fixture.slug, "host-" + fixture.slug, "ghcr.io/acme/app-" + fixture.slug}},
		{`INSERT INTO environments (id, org_id, app_id, branch, kind, namespace, planned_hostname)
			VALUES ($1, $2, $3, 'main', 'production', $4, $5)`, []any{fixture.environmentID, fixture.orgID, fixture.appID, fixture.namespace, "planned-" + fixture.slug + ".example.test"}},
	}
	for _, item := range queries {
		if _, err := pool.Exec(ctx, item.query, item.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func seedRelease(t *testing.T, pool *pgxpool.Pool, orgID, environmentID, releaseID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO releases (id, org_id, environment_id, image_ref, kind, config_snapshot, config_hash, status)
		VALUES ($1, $2, $3, 'ghcr.io/acme/test@sha256:' || repeat('a', 64),
			'deploy', $5, repeat('b', 64), $4)`, releaseID, orgID, environmentID, status,
		testReleaseSnapshot("ghcr.io/acme/test@sha256:"+strings.Repeat("a", 64))); err != nil {
		t.Fatal(err)
	}
}

func testReleaseSnapshot(image string) json.RawMessage {
	envelope := testSnapshotEnvelope()
	envelope.DesiredState.Deployment.Identity.Namespace = "app-one-production"
	envelope.DesiredState.Service.Identity.Namespace = "app-one-production"
	envelope.DesiredState.Ingress.Identity.Namespace = "app-one-production"
	envelope.DesiredState.Deployment.Container.Image = image
	encoded, _, err := EncodeReleaseSnapshot(envelope)
	if err != nil {
		panic(err)
	}
	return encoded
}

func seedReleaseSnapshot(t *testing.T, pool *pgxpool.Pool, orgID, environmentID, releaseID, status, commitSHA string, snapshot json.RawMessage) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO releases (id, org_id, environment_id, image_ref, git_commit_sha, kind, config_snapshot, config_hash, status)
		VALUES ($1, $2, $3, 'ghcr.io/acme/test@sha256:' || repeat('a', 64), $4,
			'deploy', $5, repeat('b', 64), $6)`, releaseID, orgID, environmentID, commitSHA, snapshot, status); err != nil {
		t.Fatal(err)
	}
}

func idempotencyClaim(orgID, owner, hash string) IdempotencyClaimParams {
	return IdempotencyClaimParams{OrgID: orgID, Scope: "deploy", Key: "same-key",
		RequestHash: hash, Owner: owner, LeaseDuration: time.Minute, Retention: time.Hour}
}

func orchestrationParams(orgID, repositoryID, environmentID, id, sha, key string) EnqueueOrchestrationParams {
	_ = id
	return EnqueueOrchestrationParams{OrgID: orgID, RepositoryID: repositoryID,
		WorkflowID: ".github/workflows/deploy.yml", GitRef: "refs/heads/main", EnvironmentID: environmentID,
		CommitSHA: sha, IdempotencyKey: key}
}

func seedCompletedOrchestrationRoute(t *testing.T, dataStore *Store, pool *pgxpool.Pool,
	orgID, repositoryID, environmentID, id, sha, key string) {
	t.Helper()
	if _, err := dataStore.EnqueueOrCoalesceOrchestration(t.Context(), orchestrationParams(
		orgID, repositoryID, environmentID, id, sha, key)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE orchestration_jobs SET status='failed',terminal_error_code='fixture_terminal',terminal_at=now()
		WHERE org_id=$1 AND environment_id=$2 AND idempotency_key=$3`, orgID, environmentID, key); err != nil {
		t.Fatal(err)
	}
}

func mustDecodeSnapshot(t *testing.T, snapshot json.RawMessage) ReleaseSnapshotEnvelope {
	t.Helper()
	envelope, err := DecodeReleaseSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func envRevisionSnapshot(t *testing.T, revisionID, sourceKey string) (json.RawMessage, string) {
	t.Helper()
	envelope := mustDecodeSnapshot(t, testReleaseSnapshot("ghcr.io/acme/test@sha256:"+strings.Repeat("a", 64)))
	variables := []ReleaseEnvironment{{Key: "TOKEN", SecretRef: "k8s-secret://app-one-production/runtime#" + sourceKey, IsSecret: true}}
	envelope.EnvVarRevision = &EnvVarRevision{Version: 1, ID: revisionID, Revision: 1, Hash: ComputeEnvVarRevisionHash(variables), Variables: variables}
	name := EnvDestinationSecretName(envelope.DesiredState.Deployment.Identity.Name, revisionID)
	envelope.DesiredState.Secret = &SecretIntent{Version: 1, Identity: ObjectIdentity{Name: name, Namespace: "app-one-production", Labels: envelope.DesiredState.Deployment.Identity.Labels}, Immutable: true, Entries: []SecretEntryIntent{{Key: "TOKEN", SecretRef: variables[0].SecretRef}}}
	envelope.DesiredState.Deployment.Container.EnvFromSecrets = []string{name}
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-revision-id"] = revisionID
	envelope.DesiredState.Deployment.PodAnnotations["cyclopus.dev/env-hash"] = envelope.EnvVarRevision.Hash
	snapshot, hash, err := EncodeReleaseSnapshot(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, hash
}

func seedLiveEnvRevision(t *testing.T, pool *pgxpool.Pool, revisionID, releaseID, sourceKey string) {
	t.Helper()
	snapshot, hash := envRevisionSnapshot(t, revisionID, sourceKey)
	if _, err := pool.Exec(t.Context(), `INSERT INTO env_var_revisions(id,org_id,environment_id,revision) VALUES($1,$2,$3,1)`, revisionID, orgOneID, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO env_var_revision_entries(org_id,environment_id,env_var_revision_id,key,secret_ref,is_secret) VALUES($1,$2,$3,'TOKEN',$4,true)`, orgOneID, environmentOneID, revisionID, "k8s-secret://app-one-production/runtime#"+sourceKey); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO releases(id,org_id,environment_id,image_ref,kind,env_var_revision_id,config_snapshot,config_hash,status) VALUES($1,$2,$3,$4,'env_change',$5,$6,$7,'live')`, releaseID, orgOneID, environmentOneID, "ghcr.io/acme/test@sha256:"+strings.Repeat("a", 64), revisionID, snapshot, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE environments SET current_env_var_revision_id=$2 WHERE id=$1`, environmentOneID, revisionID); err != nil {
		t.Fatal(err)
	}
}

func prepareDispatchedRecoveryJob(t *testing.T, store *Store, pool *pgxpool.Pool, id, key string) OrchestrationJob {
	t.Helper()
	ctx := context.Background()
	if _, err := store.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(
		orgOneID, repositoryOneID, environmentOneID, id, commitSHA('a'), key)); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimOrchestration(ctx, orgOneID, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDispatchReconciliation(ctx, orgOneID, job.ID, "worker", commitSHA('a'), ".github/workflows/deploy.yml", commitSHA('b'), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET reconcile_after = now() - interval '1 second'
		WHERE org_id = $1 AND id = $2`, orgOneID, job.ID); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.ClaimDispatchReconciliation(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDispatchedRun(ctx, orgOneID, job.ID, "reconciler",
		"cyclopus:"+job.ID, 12345, "in_progress", commitSHA('a'), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET reconcile_after = now() - interval '1 second'
		WHERE org_id = $1 AND id = $2`, orgOneID, job.ID); err != nil {
		t.Fatal(err)
	}
	recovery, err = store.ClaimDispatchReconciliation(ctx, orgOneID, "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return *recovery
}

func testAudit(id, action, targetID string) *AuditEvent {
	return &AuditEvent{ID: id}
}

func requestHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func commitSHA(value byte) string { return strings.Repeat(string(value), 40) }

func stringPointer(value string) *string { return &value }

func boolPointer(value bool) *bool { return &value }

func installationID(slug string) int64 {
	if slug == "one" {
		return 1
	}
	return 2
}

func assertCount(t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}

func assertJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}
