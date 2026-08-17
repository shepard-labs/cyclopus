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

func TestGitHubRepositoryActiveInstallationTransferAndCrossOrgRejectIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	install := func(orgID string, id, accountID int64, login string) GitHubInstallation {
		value, err := dataStore.UpsertGitHubInstallation(t.Context(), orgID, GitHubInstallation{GitHubInstallationID: id, AccountID: accountID, AccountLogin: login, AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"},
			GitHubAuditContext{OrgID: orgID, Event: &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-8000-%012d", id)}, Source: "test"}, orgID)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	oldInstallation := install(orgOneID, 401, 1401, "acme")
	newInstallation := install(orgOneID, 402, 1402, "acme")
	otherInstallation := install(orgTwoID, 403, 1403, "other")
	repository := GitHubRepository{GitHubRepositoryID: 9001, NodeID: "R_9001", OwnerLogin: "acme", Name: "api", FullName: "acme/api", Visibility: "private", DefaultBranch: "main"}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, oldInstallation, []GitHubRepository{repository}, nil); err != nil {
		t.Fatal(err)
	}
	audit := &AuditEvent{ID: "00000000-0000-7000-8000-000000009001"}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, newInstallation, []GitHubRepository{repository}, audit); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("active installation transfer error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE github_repositories SET access_state='inaccessible' WHERE github_repository_id=$1`, repository.GitHubRepositoryID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, newInstallation, []GitHubRepository{repository}, audit); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("active installation inaccessible repository transfer error = %v", err)
	}
	var installationID int64
	if err := pool.QueryRow(t.Context(), `SELECT github_installation_id FROM github_repositories WHERE github_repository_id=$1`, repository.GitHubRepositoryID).Scan(&installationID); err != nil || installationID != oldInstallation.GitHubInstallationID {
		t.Fatalf("installation after rejected transfer = %d, %v", installationID, err)
	}
	var auditCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE id=$1`, audit.ID).Scan(&auditCount); err != nil || auditCount != 0 {
		t.Fatalf("rejected transfer audit count = %d, %v", auditCount, err)
	}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgTwoID, otherInstallation, []GitHubRepository{repository}, nil); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("cross-org rebind error = %v", err)
	}
}

func TestGitHubRepositoryGenerationIdentityAndInactiveOldRebindIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	install := func(id int64) GitHubInstallation {
		value, err := dataStore.UpsertGitHubInstallation(t.Context(), orgOneID, GitHubInstallation{GitHubInstallationID: id, AccountID: id + 1000, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"},
			GitHubAuditContext{OrgID: orgOneID, Event: &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-8000-%012d", id)}, Source: "test"}, orgOneID)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	oldInstallation, newInstallation := install(501), install(502)
	repository := GitHubRepository{GitHubRepositoryID: 9101, NodeID: "R_9101", OwnerLogin: "acme", Name: "api", FullName: "acme/api", Visibility: "private", DefaultBranch: "main", AccessState: "active"}
	_, insertedGeneration, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, oldInstallation, []GitHubRepository{repository}, nil)
	if err != nil || insertedGeneration <= oldInstallation.RepositoryReconciliationGeneration {
		t.Fatalf("insert generation = %d, %v", insertedGeneration, err)
	}
	_, unchangedGeneration, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, oldInstallation, []GitHubRepository{repository}, nil)
	if err != nil || unchangedGeneration != insertedGeneration {
		t.Fatalf("unchanged generation = %d, want %d, %v", unchangedGeneration, insertedGeneration, err)
	}
	repository.FullName, repository.Name = "acme/renamed", "renamed"
	_, renamedGeneration, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, oldInstallation, []GitHubRepository{repository}, &AuditEvent{ID: "00000000-0000-7000-8000-000000009101"})
	if err != nil || renamedGeneration <= unchangedGeneration {
		t.Fatalf("renamed generation = %d, %v", renamedGeneration, err)
	}
	for _, accessState := range []string{"suspended", "deleted"} {
		t.Run(accessState+" old installation", func(t *testing.T) {
			repositoryID := int64(9200)
			if accessState == "deleted" {
				repositoryID++
			}
			old := install(repositoryID)
			destination := install(repositoryID + 10)
			candidate := repository
			candidate.GitHubRepositoryID = repositoryID
			candidate.NodeID = fmt.Sprintf("R_%d", repositoryID)
			if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, old, []GitHubRepository{candidate}, nil); err != nil {
				t.Fatal(err)
			}
			if accessState == "suspended" {
				_, err = pool.Exec(t.Context(), `UPDATE github_installations SET access_state='suspended',suspended_at=now() WHERE github_installation_id=$1`, old.GitHubInstallationID)
			} else {
				_, err = pool.Exec(t.Context(), `UPDATE github_installations SET access_state='deleted',deleted_at=now() WHERE github_installation_id=$1`, old.GitHubInstallationID)
			}
			if err != nil {
				t.Fatal(err)
			}
			values, reboundGeneration, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, destination, []GitHubRepository{candidate}, &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-9000-%012d", repositoryID)})
			if err != nil || len(values) != 1 || values[0].GitHubInstallationID != destination.GitHubInstallationID || reboundGeneration <= destination.RepositoryReconciliationGeneration {
				t.Fatalf("inactive-old rebind = %+v, generation %d, %v", values, reboundGeneration, err)
			}
		})
	}
	if _, err := pool.Exec(t.Context(), `UPDATE github_installations SET access_state='suspended',suspended_at=now() WHERE github_installation_id=$1`, oldInstallation.GitHubInstallationID); err != nil {
		t.Fatal(err)
	}
	_, reboundGeneration, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, newInstallation, []GitHubRepository{repository}, &AuditEvent{ID: "00000000-0000-7000-8000-000000009102"})
	if err != nil || reboundGeneration <= newInstallation.RepositoryReconciliationGeneration {
		t.Fatalf("suspended-old rebind generation = %d, %v", reboundGeneration, err)
	}
	repository.NodeID = "R_DIFFERENT"
	audit := &AuditEvent{ID: "00000000-0000-7000-8000-000000009103"}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, newInstallation, []GitHubRepository{repository}, audit); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("node identity mismatch error = %v", err)
	}
	var nodeID, action string
	if err := pool.QueryRow(t.Context(), `SELECT node_id FROM github_repositories WHERE github_repository_id=$1`, repository.GitHubRepositoryID).Scan(&nodeID); err != nil || nodeID != "R_9101" {
		t.Fatalf("persisted node ID = %q, %v", nodeID, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT action FROM audit_events WHERE id=$1`, audit.ID).Scan(&action); err != nil || action != "github_repository.identity_mismatch" {
		t.Fatalf("identity audit = %q, %v", action, err)
	}
	repository.GitHubRepositoryID, repository.NodeID = 9102, "R_9101"
	audit.ID = "00000000-0000-7000-8000-000000009104"
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, newInstallation, []GitHubRepository{repository}, audit); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("numeric identity mismatch error = %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT action FROM audit_events WHERE id=$1`, audit.ID).Scan(&action); err != nil || action != "github_repository.identity_mismatch" {
		t.Fatalf("numeric identity audit = %q, %v", action, err)
	}
}

func TestGetGHCRRegistryUsesCanonicalProviderAndHostIdentityIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	value, err := dataStore.GetGHCRRegistry(t.Context(), orgOneID)
	if !errors.Is(err, ErrNotFound) || value.ID != "" {
		t.Fatalf("missing canonical registry status = %+v, %v", value, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO registries(id,org_id,provider,registry_host,state,active_generation,active_credential_secret_ref)
		VALUES('00000000-0000-7000-8000-000000000203',$1,'ghcr','ghcr.io','ready',1,'k8s-secret://cyclopus-system/ghcr#token')`, orgOneID); err != nil {
		t.Fatal(err)
	}
	value, err = dataStore.GetGHCRRegistry(t.Context(), orgOneID)
	if err != nil || value.ID != "00000000-0000-7000-8000-000000000203" || value.Provider != "ghcr" || value.RegistryHost != "ghcr.io" || !value.HasActiveCredential {
		t.Fatalf("canonical GHCR status = %+v, %v", value, err)
	}
	if _, err := pool.Exec(t.Context(), `DROP INDEX registries_canonical_identity_key`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO registries(id,org_id,provider,registry_host,state)
		VALUES('00000000-0000-7000-8000-000000000204',$1,'ghcr','ghcr.io','absent')`, orgOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.GetGHCRRegistry(t.Context(), orgOneID); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("duplicate canonical GHCR error = %v", err)
	}
}

func TestGitHubRepositoryConcurrentTransferAllowsSingleActiveDestinationIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	install := func(id int64) GitHubInstallation {
		value, err := dataStore.UpsertGitHubInstallation(t.Context(), orgOneID, GitHubInstallation{GitHubInstallationID: id, AccountID: id + 1000, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"},
			GitHubAuditContext{OrgID: orgOneID, Event: &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-8000-%012d", id)}, Source: "test"}, orgOneID)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	old, first, second := install(601), install(602), install(603)
	repository := GitHubRepository{GitHubRepositoryID: 9301, NodeID: "R_9301", OwnerLogin: "acme", Name: "api", FullName: "acme/api", Visibility: "private", DefaultBranch: "main"}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, old, []GitHubRepository{repository}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE github_installations SET access_state='suspended',suspended_at=now() WHERE github_installation_id=$1`, old.GitHubInstallationID); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index, destination := range []GitHubInstallation{first, second} {
		wait.Add(1)
		go func(index int, destination GitHubInstallation) {
			defer wait.Done()
			<-start
			_, _, err := dataStore.UpsertGitHubRepositories(context.Background(), orgOneID, destination, []GitHubRepository{repository}, &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-8000-00000000931%d", index)})
			results <- err
		}(index, destination)
	}
	close(start)
	wait.Wait()
	close(results)
	successes, rejected := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrFailedPrecondition) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("concurrent transfer = success:%d rejected:%d", successes, rejected)
	}
	var transferAudits int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='github_repository.transferred_rebound' AND target_id IN ($1,$2)`, first.ID, second.ID).Scan(&transferAudits); err != nil || transferAudits != 1 {
		t.Fatalf("successful transfer audit count = %d, %v", transferAudits, err)
	}
}

func TestGitHubRepositoryBatchValidationAndDeterministicLockingIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	install := func(id int64) GitHubInstallation {
		value, err := dataStore.UpsertGitHubInstallation(t.Context(), orgOneID, GitHubInstallation{GitHubInstallationID: id, AccountID: id + 1000, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"},
			GitHubAuditContext{OrgID: orgOneID, Event: &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-8000-%012d", id)}, Source: "test"}, orgOneID)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := install(701)
	repositories := []GitHubRepository{
		{GitHubRepositoryID: 9401, NodeID: "R_9401", OwnerLogin: "acme", Name: "one", FullName: "acme/one", Visibility: "private", DefaultBranch: "main"},
		{GitHubRepositoryID: 9402, NodeID: "R_9402", OwnerLogin: "acme", Name: "two", FullName: "acme/two", Visibility: "private", DefaultBranch: "main"},
	}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, first, repositories, nil); err != nil {
		t.Fatal(err)
	}
	duplicate := []GitHubRepository{repositories[0], repositories[0]}
	duplicate[1].Name = "ambiguous"
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, first, duplicate, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate repository ID error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, batch := range [][]GitHubRepository{repositories, {repositories[1], repositories[0]}} {
		wait.Add(1)
		go func(batch []GitHubRepository) {
			defer wait.Done()
			<-start
			_, _, err := dataStore.UpsertGitHubRepositories(ctx, orgOneID, first, batch, nil)
			results <- err
		}(batch)
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("reversed batch failed: %v", err)
		}
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("reversed batch timed out: %v", err)
	}
	var rebound int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_repositories WHERE github_repository_id=ANY($1) AND github_installation_id=$2`, []int64{9401, 9402}, first.GitHubInstallationID).Scan(&rebound); err != nil || rebound != 2 {
		t.Fatalf("rebound repository count = %d, %v", rebound, err)
	}
}

func TestGitHubRepositoryCrossedInstallationBatchTransferDoesNotDeadlockIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	install := func(id int64) GitHubInstallation {
		value, err := dataStore.UpsertGitHubInstallation(t.Context(), orgOneID, GitHubInstallation{GitHubInstallationID: id, AccountID: id + 1000, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"},
			GitHubAuditContext{OrgID: orgOneID, Event: &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-8000-%012d", id)}, Source: "test"}, orgOneID)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	oldFirst, oldSecond, destinationFirst, destinationSecond := install(801), install(802), install(803), install(804)
	first := GitHubRepository{GitHubRepositoryID: 9501, NodeID: "R_9501", OwnerLogin: "acme", Name: "one", FullName: "acme/one", Visibility: "private", DefaultBranch: "main"}
	second := GitHubRepository{GitHubRepositoryID: 9502, NodeID: "R_9502", OwnerLogin: "acme", Name: "two", FullName: "acme/two", Visibility: "private", DefaultBranch: "main"}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, oldFirst, []GitHubRepository{first}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, oldSecond, []GitHubRepository{second}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE github_installations SET access_state='suspended',suspended_at=now() WHERE github_installation_id=ANY($1)`, []int64{oldFirst.GitHubInstallationID, oldSecond.GitHubInstallationID}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index, attempt := range []struct {
		installation GitHubInstallation
		repositories []GitHubRepository
	}{
		{destinationFirst, []GitHubRepository{second}},
		{destinationSecond, []GitHubRepository{first}},
	} {
		wait.Add(1)
		go func(index int, attempt struct {
			installation GitHubInstallation
			repositories []GitHubRepository
		}) {
			defer wait.Done()
			<-start
			_, _, err := dataStore.UpsertGitHubRepositories(ctx, orgOneID, attempt.installation, attempt.repositories, &AuditEvent{ID: fmt.Sprintf("00000000-0000-7000-8000-0000000095%02d", index)})
			results <- err
		}(index, attempt)
	}
	close(start)
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			t.Fatalf("crossed transfer failed: %v", err)
		}
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("crossed transfer timed out: %v", err)
	}
	if successes != 2 {
		t.Fatalf("crossed transfer successes = %d, want 2", successes)
	}
	var firstInstallationID, secondInstallationID int64
	if err := pool.QueryRow(t.Context(), `SELECT github_installation_id FROM github_repositories WHERE github_repository_id=$1`, first.GitHubRepositoryID).Scan(&firstInstallationID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT github_installation_id FROM github_repositories WHERE github_repository_id=$1`, second.GitHubRepositoryID).Scan(&secondInstallationID); err != nil {
		t.Fatal(err)
	}
	if firstInstallationID != destinationSecond.GitHubInstallationID || secondInstallationID != destinationFirst.GitHubInstallationID {
		t.Fatalf("crossed transfer destinations = %d,%d", firstInstallationID, secondInstallationID)
	}
}
