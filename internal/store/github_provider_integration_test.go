//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestGitHubSetupSessionBindingReplayExpiryAndConcurrencyIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	userID := "00000000-0000-0000-0000-000000000091"
	if _, err := pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, userID, orgOneID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stateHash := requestHash("github-state")
	sessionHash := requestHash("browser-session")
	generation := requestHash("generation")
	audit := GitHubAuditContext{OrgID: orgOneID, Event: &AuditEvent{ID: "00000000-0000-7000-8000-000000000001", ActorUserID: &userID}, Source: "test"}
	created, err := dataStore.BeginGitHubSetupSession(t.Context(), stateHash, generation, "app_setup", userID, sessionHash, nil, nil, now.Add(14*time.Minute), audit)
	if err != nil || created.StateHash != stateHash || created.StateGeneration != generation || created.UserID != userID || created.SessionHash != sessionHash || created.FlowKind != "app_setup" || created.Checkpoint != "created" || created.Status != "active" {
		t.Fatalf("BeginGitHubSetupSession() = %+v, %v", created, err)
	}
	if err := dataStore.ValidateGitHubSetupSession(t.Context(), stateHash, userID, sessionHash, "app_setup", now); err != nil {
		t.Fatal(err)
	}
	audit.Event.ID = "00000000-0000-7000-8000-000000000002"
	redirected, err := dataStore.RedirectGitHubSetupSession(t.Context(), stateHash, userID, sessionHash, "app_setup", now, audit)
	if err != nil || redirected.Checkpoint != "redirected" || redirected.StateGeneration != generation {
		t.Fatalf("RedirectGitHubSetupSession() = %+v, %v", redirected, err)
	}
	audit.Event.ID = "00000000-0000-7000-8000-000000000003"
	if _, err := dataStore.RedirectGitHubSetupSession(t.Context(), stateHash, userID, sessionHash, "app_setup", now, audit); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("second redirect error = %v", err)
	}
	checkpointed, err := dataStore.GitHubSetupSessionByState(t.Context(), stateHash, "app_setup", now)
	if err != nil || checkpointed.Checkpoint != "redirected" || checkpointed.UserID != userID || checkpointed.SessionHash != sessionHash {
		t.Fatalf("GitHubSetupSessionByState() = %+v, %v", checkpointed, err)
	}
	if err := dataStore.ValidateGitHubSetupSession(t.Context(), stateHash, userID, sessionHash, "app_setup", now); err != nil {
		t.Fatal(err)
	}
	for _, mismatch := range []struct{ user, session, kind string }{
		{orgTwoID, sessionHash, "app_setup"}, {userID, requestHash("other-session"), "app_setup"}, {userID, sessionHash, "installation"},
	} {
		if err := dataStore.ValidateGitHubSetupSession(t.Context(), stateHash, mismatch.user, mismatch.session, mismatch.kind, now); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("mismatch validation error = %v", err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			localAudit := audit
			localAudit.Event = &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-8000-%012d", time.Now().UnixNano()%1000000000000), ActorUserID: &userID}
			_, err := dataStore.ConsumeGitHubSetupSession(context.Background(), stateHash, userID, sessionHash, "app_setup", now, localAudit)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	successes, replays := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrFailedPrecondition) {
			replays++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("concurrent consume = success:%d replay:%d", successes, replays)
	}
	expiredHash := requestHash("expired-state")
	audit.Event.ID = "00000000-0000-7000-8000-000000000004"
	if _, err := dataStore.BeginGitHubSetupSession(t.Context(), expiredHash, requestHash("expired-generation"), "app_setup", userID, sessionHash, nil, nil, now.Add(time.Minute), audit); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.ConsumeGitHubSetupSession(t.Context(), expiredHash, userID, sessionHash, "app_setup", now.Add(2*time.Minute), audit); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("expired consume error = %v", err)
	}
}

func TestGitHubInstallationClaimUniquenessReplayAndTombstoneIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	installation := GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}
	audit := GitHubAuditContext{OrgID: orgOneID, Event: &AuditEvent{ID: "00000000-0000-7000-8000-000000000011"}, Source: "test"}
	created, err := dataStore.UpsertGitHubInstallation(t.Context(), orgOneID, installation, audit, orgOneID)
	if err != nil || created.OrgID != orgOneID || created.RepositorySelection != "selected" {
		t.Fatalf("UpsertGitHubInstallation() = %+v, %v", created, err)
	}
	installation.AccountLogin = "acme-renamed"
	audit.Event.ID = "00000000-0000-7000-8000-000000000012"
	if replay, err := dataStore.UpsertGitHubInstallation(t.Context(), orgOneID, installation, audit, orgOneID); err != nil || replay.AccountLogin != "acme-renamed" {
		t.Fatalf("replay update = %+v, %v", replay, err)
	}
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), orgTwoID, installation, audit, orgTwoID); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("cross-org claim error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE github_installations SET access_state='deleted',deleted_at=now() WHERE github_installation_id=42`); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), orgOneID, installation, audit, orgOneID); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("deleted tombstone replay error = %v", err)
	}
	second := GitHubInstallation{GitHubInstallationID: 43, AccountID: 8, AccountLogin: "other", AccountType: "User", RepositorySelection: "all", AccessState: "active"}
	audit.OrgID, audit.Event.ID = orgTwoID, "00000000-0000-7000-8000-000000000013"
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), orgTwoID, second, audit, orgTwoID); err != nil {
		t.Fatalf("unrelated owner installation error = %v", err)
	}
}

func TestBeginGitHubAppSetupExpiresStaleSessionIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	userID := "00000000-0000-0000-0000-000000000091"
	if _, err := pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, userID, orgOneID); err != nil {
		t.Fatal(err)
	}
	sessionHash := requestHash("browser-session")
	if _, err := pool.Exec(t.Context(), `INSERT INTO github_setup_sessions(state_hash,state_generation,flow_kind,initiating_user_id,initiating_session_hash,status,checkpoint,expires_at,created_at,updated_at)
		VALUES($1,$2,'app_setup',$3,$4,'active','created',now()-interval '1 second',now()-interval '1 minute',now()-interval '1 minute')`, requestHash("stale"), requestHash("stale-generation"), userID, sessionHash); err != nil {
		t.Fatal(err)
	}
	audit := GitHubAuditContext{OrgID: orgOneID, Event: &AuditEvent{ID: "00000000-0000-7000-8000-000000000021", ActorUserID: &userID}, Source: "test"}
	if _, err := dataStore.BeginGitHubSetupSession(t.Context(), requestHash("fresh"), requestHash("fresh-generation"), "app_setup", userID, sessionHash, nil, nil, time.Now().UTC().Add(10*time.Minute), audit); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM github_setup_sessions WHERE state_hash=$1`, requestHash("stale")).Scan(&status); err != nil || status != "expired" {
		t.Fatalf("stale status = %q, %v", status, err)
	}
}
