//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestImportGitRepositoryTransactionalIdempotentIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	input := integrationImportParams(actorID)
	claimImport(t, dataStore, input)
	operation, disposition, err := dataStore.ImportGitRepository(t.Context(), input)
	if err != nil || disposition.Disposition != IdempotencyClaimed || operation.State != "completed" || operation.OrchestrationJobID == nil {
		t.Fatalf("import = %+v, %+v, %v", operation, disposition, err)
	}
	var jobCount, envCount int
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM orchestration_jobs WHERE org_id=$1`, orgOneID).Scan(&jobCount)
	_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM environments WHERE org_id=$1 AND app_id=$2 AND kind='production'`, orgOneID, *operation.AppID).Scan(&envCount)
	if jobCount != 1 || envCount != 1 {
		t.Fatalf("jobs=%d envs=%d", jobCount, envCount)
	}
	assertImportedRepositoryRows(t, pool, input, operation)
	claimResult, err := dataStore.ClaimIdempotency(t.Context(), input.Idempotency)
	if err != nil || claimResult.Disposition != IdempotencyReplay {
		t.Fatalf("replay claim = %+v, %v", claimResult, err)
	}
	conflict := input
	conflict.Idempotency.RequestHash, conflict.RequestDigest, conflict.Completion.RequestHash = strings.Repeat("b", 64), strings.Repeat("b", 64), strings.Repeat("b", 64)
	claimResult, err = dataStore.ClaimIdempotency(t.Context(), conflict.Idempotency)
	if err != nil || claimResult.Disposition != IdempotencyConflict {
		t.Fatalf("conflict = %+v, %v", claimResult, err)
	}
}

func TestImportGitRepositoryConcurrentDuplicateReturnsOneResultIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	baseline := tableCounts(t, pool, []string{"git_connections", "projects", "apps", "environments", "environment_lifecycle_jobs", "orchestration_jobs", "import_operations"})
	input := integrationImportParams(actorID)
	claimImport(t, dataStore, input)
	operation, disposition, err := dataStore.ImportGitRepository(context.Background(), input)
	if err != nil || operation.State != "completed" || disposition.Disposition != IdempotencyClaimed {
		t.Fatalf("import result = %+v, %+v, %v", operation, disposition, err)
	}
	replay, err := dataStore.ClaimIdempotency(t.Context(), input.Idempotency)
	if err != nil || replay.Disposition != IdempotencyReplay {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	for table, want := range baseline {
		want++
		if table == "environment_lifecycle_jobs" || table == "orchestration_jobs" || table == "import_operations" {
			want = baseline[table] + 1
		}
		assertTableCount(t, pool, table, want)
	}
}

func TestImportGitRepositoryInjectedFailureLeavesNoPartialsIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	baseline := tableCounts(t, pool, []string{"git_connections", "projects", "apps", "environments", "orchestration_jobs", "import_operations"})
	importBeforeCommitHook = func() error { return errors.New("injected") }
	t.Cleanup(func() { importBeforeCommitHook = nil })
	params := integrationImportParams(actorID)
	claimImport(t, dataStore, params)
	if _, _, err := dataStore.ImportGitRepository(t.Context(), params); err == nil {
		t.Fatal("expected injected error")
	}
	for table, want := range baseline {
		assertTableCount(t, pool, table, want)
	}
	if err := dataStore.AbandonIdempotency(t.Context(), params.Idempotency); err != nil {
		t.Fatalf("abandon transient import claim: %v", err)
	}
	importBeforeCommitHook = nil
	claimImport(t, dataStore, params)
	if operation, _, err := dataStore.ImportGitRepository(t.Context(), params); err != nil || operation.State != "completed" {
		t.Fatalf("immediate transient retry = %+v, %v", operation, err)
	}
}

func TestImportGitRepositoryReusedConnectionRollbackAndUniqueCollisionsIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	var connectionID string
	if err := pool.QueryRow(t.Context(), `INSERT INTO git_connections(org_id,github_repository_id,state,access_state) VALUES($1,99,'active','active') RETURNING id::text`, orgOneID).Scan(&connectionID); err != nil {
		t.Fatal(err)
	}
	baseline := tableCounts(t, pool, []string{"git_connections", "projects", "apps", "environments", "environment_lifecycle_jobs", "orchestration_jobs", "import_operations"})
	params := integrationImportParams(actorID)
	claimImport(t, dataStore, params)
	params.BeforeCommit = func() error { return errors.New("injected") }
	if _, _, err := dataStore.ImportGitRepository(t.Context(), params); err == nil {
		t.Fatal("expected reused-connection rollback error")
	}
	for table, want := range baseline {
		assertTableCount(t, pool, table, want)
	}
	params = integrationImportParams(actorID)
	params.IdempotencyKey = "first"
	params.Idempotency.Key = "first"
	params.Completion.Key = "first"
	claimImport(t, dataStore, params)
	if _, _, err := dataStore.ImportGitRepository(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	baseline = tableCounts(t, pool, []string{"git_connections", "projects", "apps", "environments", "environment_lifecycle_jobs", "orchestration_jobs", "import_operations"})
	for name, mutate := range map[string]func(*ImportRepositoryParams){
		"project slug": func(value *ImportRepositoryParams) {
			value.Project.ID = "00000000-0000-7000-8000-000000000402"
			value.App.ID = "00000000-0000-7000-8000-000000000403"
			value.Environment.ID = "00000000-0000-7000-8000-000000000404"
		},
		"app slug": func(value *ImportRepositoryParams) {
			value.Project.ID = "00000000-0000-7000-8000-000000000412"
			value.Project.Slug = "platform-two"
			value.App.ID = "00000000-0000-7000-8000-000000000413"
			value.Environment.ID = "00000000-0000-7000-8000-000000000414"
		},
	} {
		t.Run(name, func(t *testing.T) {
			collision := integrationImportParams(actorID)
			collision.OperationID = "00000000-0000-7000-8000-000000000401"
			collision.IdempotencyKey, collision.Idempotency.Key, collision.Completion.Key = "collision-"+strings.ReplaceAll(name, " ", "-"), "collision-"+strings.ReplaceAll(name, " ", "-"), "collision-"+strings.ReplaceAll(name, " ", "-")
			mutate(&collision)
			claimImport(t, dataStore, collision)
			if _, _, err := dataStore.ImportGitRepository(t.Context(), collision); err == nil {
				t.Fatal("expected unique collision")
			}
			for table, want := range baseline {
				assertTableCount(t, pool, table, want)
			}
		})
	}
}

func TestImportGitRepositoryPreviewFieldDefaultsFalseAndPersistsTrueIntegration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			actorID := "00000000-0000-0000-0000-000000000091"
			_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
			seedImportRepository(t, dataStore, actorID)
			params := integrationImportParams(actorID)
			params.App.PreviewsEnabled = enabled
			claimImport(t, dataStore, params)
			if _, _, err := dataStore.ImportGitRepository(t.Context(), params); err != nil {
				t.Fatal(err)
			}
			var persisted bool
			if err := pool.QueryRow(t.Context(), `SELECT previews_enabled FROM apps WHERE id=$1`, params.App.ID).Scan(&persisted); err != nil || persisted != enabled {
				t.Fatalf("previews_enabled = %t, %v", persisted, err)
			}
		})
	}
}

func TestImportGitRepositoryLeaseTakeoverFencesStaleOwnerIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	stale := integrationImportParams(actorID)
	claimImport(t, dataStore, stale)
	if _, err := pool.Exec(t.Context(), `UPDATE api_idempotency_records SET lease_expires_at=now()-interval '1 second' WHERE org_id=$1 AND scope=$2 AND idempotency_key=$3`, stale.Idempotency.OrgID, stale.Idempotency.Scope, stale.Idempotency.Key); err != nil {
		t.Fatal(err)
	}
	fresh := stale
	fresh.Idempotency.Owner = "fresh-owner"
	result, err := dataStore.ClaimIdempotency(t.Context(), fresh.Idempotency)
	if err != nil || result.Disposition != IdempotencyClaimed {
		t.Fatalf("takeover = %+v, %v", result, err)
	}
	if _, _, err := dataStore.ImportGitRepository(t.Context(), stale); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale commit error = %v", err)
	}
	fresh.Completion.Owner = fresh.Idempotency.Owner
	operation, _, err := dataStore.ImportGitRepository(t.Context(), fresh)
	if err != nil || operation.State != "completed" {
		t.Fatalf("fresh commit = %+v, %v", operation, err)
	}
}

func TestRenewIdempotencyRequiresCurrentUnexpiredOwnerIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	claim := integrationImportParams("00000000-0000-0000-0000-000000000091").Idempotency
	claimImport(t, dataStore, ImportRepositoryParams{Idempotency: claim})
	if err := dataStore.RenewIdempotency(t.Context(), claim); err != nil {
		t.Fatalf("renew current owner: %v", err)
	}
	stale := claim
	stale.Owner = "other-owner"
	if err := dataStore.RenewIdempotency(t.Context(), stale); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("renew stale owner error = %v", err)
	}
}

func TestImportGitRepositoryFinalLeaseCheckUsesWallClockIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	params := integrationImportParams(actorID)
	claimImport(t, dataStore, params)
	if _, err := pool.Exec(t.Context(), `UPDATE api_idempotency_records SET lease_expires_at=clock_timestamp()+interval '150 milliseconds'
		WHERE org_id=$1 AND scope=$2 AND idempotency_key=$3`, params.Idempotency.OrgID, params.Idempotency.Scope, params.Idempotency.Key); err != nil {
		t.Fatal(err)
	}
	params.BeforeCommit = func() error {
		_, err := pool.Exec(t.Context(), `SELECT pg_sleep(0.25)`)
		return err
	}
	if _, _, err := dataStore.ImportGitRepository(t.Context(), params); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("completion after wall-clock expiry error = %v", err)
	}
}

func TestImportGitRepositoryFencesPreflightRepositorySnapshotIntegration(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}){
		"installation suspended": func(t *testing.T, pool interface {
			Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
		}) {
			if _, err := pool.Exec(t.Context(), `UPDATE github_installations SET access_state='suspended',suspended_at=now(),repository_reconciliation_generation=repository_reconciliation_generation+1 WHERE org_id=$1 AND github_installation_id=42`, orgOneID); err != nil {
				t.Fatal(err)
			}
		},
		"repository access removed": func(t *testing.T, pool interface {
			Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
		}) {
			if _, err := pool.Exec(t.Context(), `UPDATE github_repositories SET access_state='inaccessible',reconciliation_generation=reconciliation_generation+1,updated_at=now() WHERE org_id=$1 AND github_repository_id=99`, orgOneID); err != nil {
				t.Fatal(err)
			}
		},
		"repository renamed after preflight": func(t *testing.T, pool interface {
			Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
		}) {
			if _, err := pool.Exec(t.Context(), `UPDATE github_repositories SET name='renamed',full_name='acme/renamed',reconciliation_generation=reconciliation_generation+1,updated_at=now() WHERE org_id=$1 AND github_repository_id=99`, orgOneID); err != nil {
				t.Fatal(err)
			}
		},
		"repository rebound after preflight": func(t *testing.T, pool interface {
			Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
		}) {
			if _, err := pool.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,repository_reconciliation_generation)
				VALUES(43,$1,8,'acme-secondary','Organization','all','active',1)`, orgOneID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE github_repositories SET github_installation_id=43,reconciliation_generation=reconciliation_generation+1,updated_at=now() WHERE org_id=$1 AND github_repository_id=99`, orgOneID); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dataStore, pool := newIntegrationStore(t)
			seedTenant(t, pool, tenantOne())
			actorID := "00000000-0000-0000-0000-000000000091"
			_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
			seedImportRepository(t, dataStore, actorID)
			params := integrationImportParams(actorID)
			claimImport(t, dataStore, params)
			mutate(t, pool)
			baseline := tableCounts(t, pool, []string{"git_connections", "projects", "apps", "environments", "environment_lifecycle_jobs", "orchestration_jobs", "import_operations"})
			connectionBefore := importFenceConnectionSnapshot(t, pool)
			installationBefore := importFenceInstallationSnapshot(t, pool)
			repositoryBefore := importFenceRepositorySnapshot(t, pool)
			if _, _, err := dataStore.ImportGitRepository(t.Context(), params); !errors.Is(err, ErrFailedPrecondition) {
				t.Fatalf("fenced import error = %v", err)
			}
			for table, want := range baseline {
				assertTableCount(t, pool, table, want)
			}
			if connectionAfter := importFenceConnectionSnapshot(t, pool); connectionAfter != connectionBefore {
				t.Fatalf("existing connection changed after fenced import: before=%+v after=%+v", connectionBefore, connectionAfter)
			}
			if installationAfter := importFenceInstallationSnapshot(t, pool); installationAfter != installationBefore {
				t.Fatalf("authoritative installation metadata changed inside fenced import: before=%+v after=%+v", installationBefore, installationAfter)
			}
			if repositoryAfter := importFenceRepositorySnapshot(t, pool); repositoryAfter != repositoryBefore {
				t.Fatalf("authoritative repository metadata changed inside fenced import: before=%+v after=%+v", repositoryBefore, repositoryAfter)
			}
			var status, owner string
			var responseCode *int
			var resourceType, resourceID *string
			if err := pool.QueryRow(t.Context(), `SELECT status,lease_owner,response_code,resource_type,resource_id::text FROM api_idempotency_records
					WHERE org_id=$1 AND scope=$2 AND idempotency_key=$3`, params.OrgID, params.Idempotency.Scope, params.Idempotency.Key).Scan(&status, &owner, &responseCode, &resourceType, &resourceID); err != nil {
				t.Fatal(err)
			}
			if status != "in_progress" || owner != params.Idempotency.Owner || responseCode != nil || resourceType != nil || resourceID != nil {
				t.Fatalf("fenced idempotency record = status:%q owner:%q response:%v resource:%v/%v", status, owner, responseCode, resourceType, resourceID)
			}
			var successAudits int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='github_import.completed' OR id=ANY($1::uuid[])`, params.AuditIDs).Scan(&successAudits); err != nil || successAudits != 0 {
				t.Fatalf("fenced import success audits = %d, %v", successAudits, err)
			}
		})
	}
}

type importFenceConnectionState struct {
	Count              int
	ID                 string
	GitHubRepositoryID int64
	State, AccessState string
}

func importFenceConnectionSnapshot(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) importFenceConnectionState {
	t.Helper()
	var value importFenceConnectionState
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM git_connections`).Scan(&value.Count); err != nil {
		t.Fatal(err)
	}
	if value.Count != 1 {
		t.Fatalf("fixture git_connections count=%d want=1", value.Count)
	}
	if err := pool.QueryRow(t.Context(), `SELECT id::text,github_repository_id,state,access_state FROM git_connections`).Scan(
		&value.ID, &value.GitHubRepositoryID, &value.State, &value.AccessState); err != nil {
		t.Fatal(err)
	}
	return value
}

type importFenceRepositoryState struct {
	InstallationID, ReconciliationGeneration int64
	NodeID, Name, FullName, AccessState      string
	Archived                                 bool
}

type importFenceInstallationState struct {
	RepositoryGeneration int64
	AccessState          string
	Suspended            bool
}

func importFenceInstallationSnapshot(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) importFenceInstallationState {
	t.Helper()
	var value importFenceInstallationState
	if err := pool.QueryRow(t.Context(), `SELECT repository_reconciliation_generation,access_state,suspended_at IS NOT NULL
		FROM github_installations WHERE org_id=$1 AND github_installation_id=42`, orgOneID).Scan(
		&value.RepositoryGeneration, &value.AccessState, &value.Suspended); err != nil {
		t.Fatal(err)
	}
	return value
}

func importFenceRepositorySnapshot(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) importFenceRepositoryState {
	t.Helper()
	var value importFenceRepositoryState
	if err := pool.QueryRow(t.Context(), `SELECT github_installation_id,reconciliation_generation,node_id,name,full_name,access_state,archived
		FROM github_repositories WHERE org_id=$1 AND github_repository_id=99`, orgOneID).Scan(&value.InstallationID,
		&value.ReconciliationGeneration, &value.NodeID, &value.Name, &value.FullName, &value.AccessState, &value.Archived); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertImportedRepositoryRows(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, params ImportRepositoryParams, operation ImportOperation) {
	t.Helper()
	var connectionState, projectName, projectSlug string
	if err := pool.QueryRow(t.Context(), `SELECT connection.state,project.name,project.slug FROM git_connections connection JOIN projects project ON project.git_connection_id=connection.id WHERE connection.id=$1`, *operation.GitConnectionID).Scan(&connectionState, &projectName, &projectSlug); err != nil || connectionState != "active" || projectName != params.Project.Name || projectSlug != params.Project.Slug {
		t.Fatalf("connection/project = %q %q %q, %v", connectionState, projectName, projectSlug, err)
	}
	var appName, appSlug, image, workflow, revision, branch string
	var previews bool
	if err := pool.QueryRow(t.Context(), `SELECT name,slug,image_repository,github_workflow_path,github_workflow_revision,production_branch,previews_enabled FROM apps WHERE id=$1`, *operation.AppID).Scan(&appName, &appSlug, &image, &workflow, &revision, &branch, &previews); err != nil || appName != params.App.Name || appSlug != params.App.Slug || image != *params.App.ImageRepository || workflow != params.App.GitHubWorkflowPath || revision != params.WorkflowRevision || branch != params.App.ProductionBranch || previews {
		t.Fatalf("canonical app = %q %q %q %q %q %q previews=%t err=%v", appName, appSlug, image, workflow, revision, branch, previews, err)
	}
	var kind, lifecycle, lifecycleKind, lifecycleStatus, lifecycleReason string
	if err := pool.QueryRow(t.Context(), `SELECT environment.kind,environment.lifecycle_status,lifecycle.kind,lifecycle.status,lifecycle.reason FROM environments environment JOIN environment_lifecycle_jobs lifecycle ON lifecycle.environment_id=environment.id WHERE environment.id=$1`, *operation.EnvironmentID).Scan(&kind, &lifecycle, &lifecycleKind, &lifecycleStatus, &lifecycleReason); err != nil || kind != "production" || lifecycle != "reserved" || lifecycleKind != "provision" || lifecycleStatus != "queued" || lifecycleReason != "repository_import" {
		t.Fatalf("environment/lifecycle = %q %q %q %q %q, %v", kind, lifecycle, lifecycleKind, lifecycleStatus, lifecycleReason, err)
	}
	var repositoryID int64
	var jobWorkflow, jobRevision, ref, sha, key, status string
	var generation int64
	if err := pool.QueryRow(t.Context(), `SELECT github_repository_id,github_workflow_path,workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key,status FROM orchestration_jobs WHERE id=$1`, *operation.OrchestrationJobID).Scan(&repositoryID, &jobWorkflow, &jobRevision, &ref, &sha, &generation, &key, &status); err != nil || repositoryID != params.RepositoryID || jobWorkflow != params.App.GitHubWorkflowPath || jobRevision != params.WorkflowRevision || ref != params.App.ProductionBranch || sha != params.BranchHeadSHA || generation != 2 || key != "import:"+params.OperationID || status != "queued" {
		t.Fatalf("orchestration = %d %q %q %q %q %d %q %q, %v", repositoryID, jobWorkflow, jobRevision, ref, sha, generation, key, status, err)
	}
	var operationState, idempotencyStatus string
	if err := pool.QueryRow(t.Context(), `SELECT operation.state,idempotency.status FROM import_operations operation JOIN api_idempotency_records idempotency ON idempotency.org_id=operation.org_id AND idempotency.idempotency_key=operation.idempotency_key AND idempotency.scope='github.repository.import' WHERE operation.id=$1`, operation.ID).Scan(&operationState, &idempotencyStatus); err != nil || operationState != "completed" || idempotencyStatus != "completed" {
		t.Fatalf("operation/idempotency = %q %q, %v", operationState, idempotencyStatus, err)
	}
	var audits int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE id=ANY($1::uuid[])`, params.AuditIDs).Scan(&audits); err != nil || audits != 5 {
		t.Fatalf("explicit audits = %d, %v", audits, err)
	}
}

func assertTableCount(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != want {
		t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
	}
}

func tableCounts(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tables []string) map[string]int {
	t.Helper()
	values := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		values[table] = count
	}
	return values
}

func seedImportRepository(t *testing.T, dataStore *Store, actorID string) {
	t.Helper()
	installation, err := dataStore.UpsertGitHubInstallation(t.Context(), orgOneID, GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "all", AccessState: "active"}, GitHubAuditContext{OrgID: orgOneID, Event: &AuditEvent{ID: "00000000-0000-7000-8000-000000000101"}, Source: "test"}, actorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dataStore.UpsertGitHubRepositories(t.Context(), orgOneID, installation, []GitHubRepository{{GitHubRepositoryID: 99, NodeID: "R_99", OwnerLogin: "acme", Name: "api", FullName: "acme/api", Visibility: "public", DefaultBranch: "main"}}, nil); err != nil {
		t.Fatal(err)
	}
}

func integrationImportParams(actorID string) ImportRepositoryParams {
	hash, workflowRevision, branchSHA := strings.Repeat("a", 64), strings.Repeat("c", 40), strings.Repeat("d", 40)
	image, host := "ghcr.io/acme/api", "api.example.test"
	projectID, appID := "00000000-0000-7000-8000-000000000302", "00000000-0000-7000-8000-000000000303"
	app := App{ID: appID, OrgID: orgOneID, ProjectID: projectID, Name: "API", Slug: "api", HostnameLabel: "api", ImageRepository: &image, GitHubWorkflowPath: ".github/workflows/deploy.yml", GitHubWorkflowRevision: workflowRevision, ProductionBranch: "main", AnnotationPolicy: "validate_if_present", ImageVisibility: "public", ContainerPort: 8080, HealthCheckPath: "/", ScalingMode: "hpa", MinReplicas: 1, MaxReplicas: 3, HPATargetCPUPercent: 70, KEDAHTTPTargetPendingRequests: 100, CPURequestMillicores: 100, CPULimitMillicores: 1000, MemoryRequestMiB: 128, MemoryLimitMiB: 512, MaxPreviewEnvironments: 10, PreviewIdleTTLHours: 168, DeployTimeoutSeconds: 600, ReleaseRetentionCount: 5, RuntimeClass: "gvisor"}
	return ImportRepositoryParams{OperationID: "00000000-0000-7000-8000-000000000301", OrgID: orgOneID, ActorUserID: actorID, IdempotencyKey: "import", RequestDigest: hash, RepositoryID: 99, ExpectedGitHubInstallationID: 42, ExpectedInstallationGeneration: 2, ExpectedRepositoryNodeID: "R_99", ExpectedRepositoryGeneration: 1, Project: Project{ID: projectID, OrgID: orgOneID, Name: "Platform", Slug: "platform"}, App: app, Environment: Environment{ID: "00000000-0000-7000-8000-000000000304", OrgID: orgOneID, AppID: appID, Branch: "main", Kind: "production", Namespace: "api-production", PlannedHostname: &host, LifecycleStatus: "reserved"}, WorkflowRevision: workflowRevision, BranchHeadSHA: branchSHA, Idempotency: IdempotencyClaimParams{OrgID: orgOneID, Scope: "github.repository.import", Key: "import", RequestHash: hash, Owner: "owner", LeaseDuration: time.Minute, Retention: time.Hour}, Completion: CompleteIdempotencyParams{OrgID: orgOneID, Scope: "github.repository.import", Key: "import", RequestHash: hash, Status: "completed", StatusCode: 200}, AuditIDs: []string{"00000000-0000-7000-8000-000000000311", "00000000-0000-7000-8000-000000000312", "00000000-0000-7000-8000-000000000313", "00000000-0000-7000-8000-000000000314", "00000000-0000-7000-8000-000000000315"}}
}

func claimImport(t *testing.T, dataStore *Store, params ImportRepositoryParams) {
	t.Helper()
	result, err := dataStore.ClaimIdempotency(t.Context(), params.Idempotency)
	if err != nil || result.Disposition != IdempotencyClaimed {
		t.Fatalf("claim import = %+v, %v", result, err)
	}
}
