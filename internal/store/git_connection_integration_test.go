//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	gitConnectionOneID = "00000000-0000-0000-0000-0000000000fa"
	gitConnectionTwoID = "00000000-0000-0000-0000-0000000000fb"
)

func TestGitConnectionStoreCRUDIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	ctx := context.Background()

	fetched, err := dataStore.GetGitConnection(ctx, orgOneID, repositoryOneID)
	if err != nil || fetched.RepositoryFullName != "acme/one" || fetched.RepositoryDefaultBranch != "main" || fetched.GitHubInstallationID != 1 {
		t.Fatalf("GetGitConnection() = %+v, %v", fetched, err)
	}
	// Org scoping: another org cannot read this connection.
	if _, err := dataStore.GetGitConnection(ctx, orgTwoID, repositoryOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGitConnection(other org) error = %v, want ErrNotFound", err)
	}

	listed, err := dataStore.ListGitConnections(ctx, orgOneID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListGitConnections() = %+v, %v, want 1", listed, err)
	}
}

func TestDisconnectAndDispatchAuthorizationAreSerializedIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	if _, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID, repositoryOneID, environmentOneID, "", commitSHA('a'), "disconnect-race")); err != nil {
		t.Fatal(err)
	}
	var connectionID, jobID string
	if err := pool.QueryRow(ctx, `SELECT connection.id::text,job.id::text FROM git_connections connection
		JOIN projects project ON project.git_connection_id=connection.id JOIN apps app ON app.project_id=project.id
		JOIN orchestration_jobs job ON job.app_id=app.id WHERE connection.org_id=$1 LIMIT 1`, orgOneID).Scan(&connectionID, &jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET status='dispatch_reconciling',dispatch_requested_at=now(),
		dispatch_ref_observed_sha=repeat('b',40),dispatch_ambiguity_state='possible',reconcile_after=now()+interval '1 minute',
		dispatch_deadline_at=now()+interval '5 minutes',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	params := DisconnectGitConnectionParams{OrgID: orgOneID, ID: connectionID,
		Idempotency: CompleteIdempotencyParams{OrgID: orgOneID, Scope: "git_connection.disconnect", Key: "disconnect-race", RequestHash: strings.Repeat("a", 64), Owner: "owner", Status: "completed", StatusCode: 200, Body: json.RawMessage(`{}`)},
		Audit:       &AuditEvent{ID: "00000000-0000-0000-0000-0000000000f1"}}
	if _, err := pool.Exec(ctx, `INSERT INTO api_idempotency_records(org_id,scope,idempotency_key,request_hash,status,lease_owner,lease_expires_at,expires_at)
		VALUES($1,$2,$3,$4,'in_progress',$5,now()+interval '1 minute',now()+interval '1 day')`, orgOneID, params.Idempotency.Scope, params.Idempotency.Key, params.Idempotency.RequestHash, params.Idempotency.Owner); err != nil {
		t.Fatal(err)
	}
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx)
	if _, err := lockTx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1380270412))`, orgOneID); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(2)
	authorizeErr := make(chan error, 1)
	disconnectErr := make(chan error, 1)
	go func() {
		defer wait.Done()
		authorizeErr <- dataStore.AuthorizeWorkflowDispatch(context.Background(), orgOneID, jobID, "worker", time.Minute)
	}()
	time.Sleep(25 * time.Millisecond)
	go func() {
		defer wait.Done()
		_, err := dataStore.DisconnectGitConnection(context.Background(), params)
		disconnectErr <- err
	}()
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	authErr, discErr := <-authorizeErr, <-disconnectErr
	switch {
	case discErr == nil:
		if !errors.Is(authErr, ErrLeaseNotOwned) {
			t.Fatalf("authorize after disconnect error = %v, want ErrLeaseNotOwned", authErr)
		}
	case errors.Is(discErr, ErrConflict):
		if authErr != nil {
			t.Fatalf("dispatch authorization won serialization but failed: %v", authErr)
		}
		if err := dataStore.FinalizeWorkflowDispatch(ctx, orgOneID, jobID, "worker"); err != nil {
			t.Fatal(err)
		}
		if _, err := dataStore.DisconnectGitConnection(ctx, params); err != nil {
			t.Fatalf("disconnect after dispatch authorization finalized: %v", err)
		}
	default:
		t.Fatalf("disconnect error = %v", discErr)
	}
	var state, cancellation string
	if err := pool.QueryRow(ctx, `SELECT connection.state,job.cancellation_state FROM git_connections connection
		JOIN projects project ON project.git_connection_id=connection.id JOIN apps app ON app.project_id=project.id
		JOIN orchestration_jobs job ON job.app_id=app.id WHERE connection.id=$1 AND job.id=$2`, connectionID, jobID).Scan(&state, &cancellation); err != nil {
		t.Fatal(err)
	}
	if state != "disconnected" || cancellation != "requested" {
		t.Fatalf("final state = %s/%s, want disconnected/requested durable cancellation", state, cancellation)
	}
}

func TestDisconnectWaitsForDispatchPinFinalizationAndToleratesExpiryIntegration(t *testing.T) {
	for _, test := range []struct {
		name    string
		expired bool
	}{
		{name: "live pin blocks until finalized"},
		{name: "expired crash pin does not block", expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			ctx := t.Context()
			if _, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID, repositoryOneID, environmentOneID, "", commitSHA('a'), "pin-job")); err != nil {
				t.Fatal(err)
			}
			var connectionID, jobID string
			if err := pool.QueryRow(ctx, `SELECT connection.id::text,job.id::text FROM git_connections connection
				JOIN projects project ON project.git_connection_id=connection.id JOIN apps app ON app.project_id=project.id
				JOIN orchestration_jobs job ON job.app_id=app.id WHERE connection.org_id=$1 LIMIT 1`, orgOneID).Scan(&connectionID, &jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET status='dispatch_reconciling',dispatch_requested_at=now(),
				dispatch_ref_observed_sha=repeat('b',40),dispatch_ambiguity_state='possible',reconcile_after=now()+interval '1 minute',
				dispatch_deadline_at=now()+interval '5 minutes',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, jobID); err != nil {
				t.Fatal(err)
			}
			if err := dataStore.AuthorizeWorkflowDispatch(ctx, orgOneID, jobID, "worker", time.Minute); err != nil {
				t.Fatal(err)
			}
			if test.expired {
				if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET dispatch_call_expires_at=clock_timestamp()-interval '1 microsecond' WHERE id=$1`, jobID); err != nil {
					t.Fatal(err)
				}
			}
			params := disconnectIntegrationParams(t, pool, connectionID, "pin-"+strings.ReplaceAll(test.name, " ", "-"))
			_, err := dataStore.DisconnectGitConnection(ctx, params)
			if !test.expired {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("disconnect with live pin error = %v", err)
				}
				if err := dataStore.FinalizeWorkflowDispatch(ctx, orgOneID, jobID, "worker"); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `DELETE FROM api_idempotency_records WHERE org_id=$1 AND scope=$2 AND idempotency_key=$3`, orgOneID, params.Idempotency.Scope, params.Idempotency.Key); err != nil {
					t.Fatal(err)
				}
				params = disconnectIntegrationParams(t, pool, connectionID, "pin-finalized")
				_, err = dataStore.DisconnectGitConnection(ctx, params)
			}
			if err != nil {
				t.Fatalf("disconnect after pin completion/expiry: %v", err)
			}
		})
	}
}

func TestCancellationDiscoveryIncludesOldRowsAndTornDownEnvironmentsIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	if _, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID, repositoryOneID, environmentOneID, "", commitSHA('a'), "old-cancel-job")); err != nil {
		t.Fatal(err)
	}
	var jobID string
	if err := pool.QueryRow(ctx, `UPDATE orchestration_jobs SET status='cancelled',terminal_at=clock_timestamp(),
		dispatch_requested_at=clock_timestamp()-interval '1 minute',dispatch_ref_observed_sha=repeat('b',40),
		cancellation_state='requested',cancellation_requested_at=clock_timestamp()-interval '1 minute',
		cancellation_next_attempt_at=clock_timestamp(),dispatch_call_authorized_at=NULL
		WHERE org_id=$1 RETURNING id::text`, orgOneID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lifecycle_status='torn_down',torn_down_at=clock_timestamp() WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	orgIDs, err := dataStore.ListOrchestrationCancellationOrgIDs(ctx)
	if err != nil || len(orgIDs) != 1 || orgIDs[0] != orgOneID {
		t.Fatalf("cancellation orgs = %v, %v", orgIDs, err)
	}
	job, err := dataStore.ClaimOrchestrationCancellation(ctx, orgOneID, "cancel-worker", time.Minute)
	if err != nil || job.ID != jobID || job.DispatchRequestedAt == nil || job.DispatchCallAuthorizedAt != nil {
		t.Fatalf("old-row cancellation claim = %+v, %v", job, err)
	}
}

func TestCancellationExhaustionPreservesConcurrentWorkersActiveFinalAttemptIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	if _, err := dataStore.EnqueueOrCoalesceOrchestration(ctx, orchestrationParams(orgOneID, repositoryOneID, environmentOneID, "", commitSHA('a'), "final-cancel-attempt")); err != nil {
		t.Fatal(err)
	}
	var jobID string
	if err := pool.QueryRow(ctx, `UPDATE orchestration_jobs SET status='cancelled',terminal_at=clock_timestamp(),
		dispatch_requested_at=clock_timestamp()-interval '1 minute',dispatch_ref_observed_sha=repeat('b',40),
		cancellation_state='requested',cancellation_requested_at=clock_timestamp()-interval '1 minute',
		cancellation_attempts=$2-1,cancellation_next_attempt_at=clock_timestamp(),dispatch_call_authorized_at=NULL
		WHERE org_id=$1 RETURNING id::text`, orgOneID, OrchestrationCancellationMaxAttempts).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimOrchestrationCancellation(ctx, orgOneID, "final-attempt-worker", time.Minute)
	if err != nil || claimed.ID != jobID || claimed.CancellationAttempts != OrchestrationCancellationMaxAttempts {
		t.Fatalf("final cancellation claim = %+v, %v", claimed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET cancellation_requested_at=clock_timestamp()-make_interval(secs=>$2)-interval '1 microsecond'
		WHERE id=$1`, jobID, OrchestrationCancellationDeadline.Seconds()); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.ClaimOrchestrationCancellation(ctx, orgOneID, "exhaustion-worker", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("concurrent claim across deadline = %v, want ErrNotFound", err)
	}
	var state, owner string
	var leaseExpiresAt time.Time
	if err := pool.QueryRow(ctx, `SELECT cancellation_state,reconciliation_owner,reconciliation_lease_expires_at
		FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&state, &owner, &leaseExpiresAt); err != nil {
		t.Fatal(err)
	}
	if state != "requested" || owner != "final-attempt-worker" || !leaseExpiresAt.After(time.Now()) {
		t.Fatalf("active final attempt was terminalized: state=%q owner=%q lease=%v", state, owner, leaseExpiresAt)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET reconciliation_lease_expires_at=clock_timestamp()-interval '1 microsecond' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.ClaimOrchestrationCancellation(ctx, orgOneID, "exhaustion-worker", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim after lease expiry = %v, want exhausted ErrNotFound", err)
	}
	if err := pool.QueryRow(ctx, `SELECT cancellation_state FROM orchestration_jobs WHERE id=$1`, jobID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("cancellation state after lease expiry = %q, %v", state, err)
	}
	orgIDs, err := dataStore.ListOrchestrationCancellationOrgIDs(ctx)
	if err != nil || len(orgIDs) != 1 || orgIDs[0] != orgOneID {
		t.Fatalf("known residual terminal cancellation rediscovery = %v, %v", orgIDs, err)
	}
}

func disconnectIntegrationParams(t *testing.T, pool DB, connectionID, key string) DisconnectGitConnectionParams {
	t.Helper()
	auditID := fmt.Sprintf("00000000-0000-7000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000)
	params := DisconnectGitConnectionParams{OrgID: orgOneID, ID: connectionID,
		Idempotency: CompleteIdempotencyParams{OrgID: orgOneID, Scope: "git_connection.disconnect", Key: key, RequestHash: strings.Repeat("a", 64), Owner: "owner", Status: "completed", StatusCode: 200, Body: json.RawMessage(`{}`)},
		Audit:       &AuditEvent{ID: auditID}}
	if _, err := pool.Exec(t.Context(), `INSERT INTO api_idempotency_records(org_id,scope,idempotency_key,request_hash,status,lease_owner,lease_expires_at,expires_at)
		VALUES($1,$2,$3,$4,'in_progress',$5,now()+interval '1 minute',now()+interval '1 day')`, orgOneID, params.Idempotency.Scope, key, params.Idempotency.RequestHash, params.Idempotency.Owner); err != nil {
		t.Fatal(err)
	}
	return params
}

func TestDisconnectGitConnectionIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()

	var connectionID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM git_connections WHERE org_id=$1 LIMIT 1`, orgOneID).Scan(&connectionID); err != nil {
		t.Fatal(err)
	}
	params := DisconnectGitConnectionParams{OrgID: orgOneID, ID: connectionID,
		Idempotency: CompleteIdempotencyParams{OrgID: orgOneID, Scope: "git_connection.disconnect", Key: "disconnect-1", RequestHash: strings.Repeat("a", 64), Owner: "owner", Status: "completed", StatusCode: 200, Body: json.RawMessage(`{}`)},
		Audit:       &AuditEvent{ID: "00000000-0000-0000-0000-0000000000fc"}}
	if _, err := pool.Exec(ctx, `INSERT INTO api_idempotency_records(org_id,scope,idempotency_key,request_hash,status,lease_owner,lease_expires_at,expires_at)
		VALUES($1,$2,$3,$4,'in_progress',$5,now()+interval '1 minute',now()+interval '1 day')`, orgOneID, params.Idempotency.Scope, params.Idempotency.Key, params.Idempotency.RequestHash, params.Idempotency.Owner); err != nil {
		t.Fatal(err)
	}
	releaseID := "00000000-0000-0000-0000-0000000000fd"
	seedRelease(t, pool, orgOneID, environmentOneID, releaseID, "live")
	beforeApps, beforeEnvironments := generationSums(t, pool, connectionID)
	value, err := dataStore.DisconnectGitConnection(ctx, params)
	if err != nil || value.State != "disconnected" || value.DisconnectedAt == nil {
		t.Fatalf("DisconnectGitConnection() = %+v, %v", value, err)
	}
	afterApps, afterEnvironments := generationSums(t, pool, connectionID)
	if afterApps <= beforeApps || afterEnvironments <= beforeEnvironments {
		t.Fatalf("generation sums apps %d→%d environments %d→%d", beforeApps, afterApps, beforeEnvironments, afterEnvironments)
	}
	firstDisconnectedAt := *value.DisconnectedAt
	if _, err := pool.Exec(ctx, `INSERT INTO api_idempotency_records(org_id,scope,idempotency_key,request_hash,status,lease_owner,lease_expires_at,expires_at)
		VALUES($1,$2,$3,$4,'in_progress',$5,now()+interval '1 minute',now()+interval '1 day')`, orgOneID, params.Idempotency.Scope, "disconnect-2", params.Idempotency.RequestHash, params.Idempotency.Owner); err != nil {
		t.Fatal(err)
	}
	params.Idempotency.Key = "disconnect-2"
	params.Audit = &AuditEvent{ID: "00000000-0000-0000-0000-0000000000fe"}
	replayed, err := dataStore.DisconnectGitConnection(ctx, params)
	if err != nil || replayed.DisconnectedAt == nil || !replayed.DisconnectedAt.Equal(firstDisconnectedAt) {
		t.Fatalf("second disconnect = %+v, %v", replayed, err)
	}
	replayApps, replayEnvironments := generationSums(t, pool, connectionID)
	if replayApps != afterApps || replayEnvironments != afterEnvironments {
		t.Fatalf("idempotent disconnect advanced generations again: apps %d→%d environments %d→%d", afterApps, replayApps, afterEnvironments, replayEnvironments)
	}
	var nonterminal int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orchestration_jobs job JOIN apps app ON app.id=job.app_id JOIN projects project ON project.id=app.project_id
		WHERE project.git_connection_id=$1 AND job.status IN ('queued','leased','dispatch_reconciling','dispatched','resolving_image','deploying')`, connectionID).Scan(&nonterminal); err != nil || nonterminal != 0 {
		t.Fatalf("nonterminal jobs = %d, %v", nonterminal, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM github_routing_intents intent JOIN apps app ON app.id=intent.app_id JOIN projects project ON project.id=app.project_id
		WHERE project.git_connection_id=$1 AND intent.status IN ('pending','leased')`, connectionID).Scan(&nonterminal); err != nil || nonterminal != 0 {
		t.Fatalf("nonterminal routing intents = %d, %v", nonterminal, err)
	}
	var releaseCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM releases WHERE id=$1`, releaseID).Scan(&releaseCount); err != nil {
		t.Fatal(err)
	}
	if releaseCount != 1 {
		t.Fatalf("release was not preserved")
	}
	if _, err := dataStore.GetGitConnection(ctx, orgTwoID, connectionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other org read = %v", err)
	}
}

func generationSums(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, connectionID string) (int64, int64) {
	t.Helper()
	var apps, environments int64
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(sum(app.desired_generation),0),COALESCE(sum(environment.desired_generation),0)
		FROM projects project JOIN apps app ON app.project_id=project.id JOIN environments environment ON environment.app_id=app.id WHERE project.git_connection_id=$1`, connectionID).Scan(&apps, &environments); err != nil {
		t.Fatal(err)
	}
	return apps, environments
}
