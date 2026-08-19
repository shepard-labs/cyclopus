//go:build integration

package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedPrivateGHCRRegistry inserts a ready GHCR registry with an active
// generation 1 credential, mirroring a completed first-credential saga: a
// succeeded operation plus a validated/activated generation row inside one
// deferred-constraint transaction (the operation/generation FK pair is
// circular).
func seedPrivateGHCRRegistry(t *testing.T, pool *pgxpool.Pool, orgID, registryID string) {
	t.Helper()
	secretRef := "k8s-secret://cyclopus-system/credential-" + registryID[24:] + "#dockerconfigjson"
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO registries(id,org_id,provider,registry_host,state,checkpoint,active_generation,active_credential_secret_ref,activated_at)
		VALUES($1,$2,'ghcr','ghcr.io','ready','activated',1,$3,clock_timestamp())`, registryID, orgID, secretRef); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,candidate_generation,idempotency_key,terminal_at)
		VALUES(uuidv7(),$1,$2,'create','succeeded','activated',1,'seed-`+registryID[24:]+`',clock_timestamp())`, orgID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id,validated_at,activated_at)
		SELECT $1,$2,1,$3,'active',operation.id,clock_timestamp(),clock_timestamp() FROM registry_credential_operations operation
		WHERE operation.org_id=$1 AND operation.registry_id=$2 AND operation.candidate_generation=1`, orgID, registryID, secretRef); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func privateImportParams(base ImportRepositoryParams) ImportRepositoryParams {
	base.App.ImageVisibility = "private"
	base.RegistryOperationID = ""
	base.RegistryCandidateGeneration = 1
	base.Environment.RegistryCredentialState = "pending"
	required := int64(1)
	base.Environment.RequiredRegistryCredentialGeneration = &required
	return base
}

func TestImportGitRepositoryPrivateReuseReadyCredentialIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	registryID := "00000000-0000-7000-8000-000000000421"
	seedPrivateGHCRRegistry(t, pool, orgOneID, registryID)
	params := privateImportParams(integrationImportParams(actorID))
	params.Environment.RegistryCredentialState = "ready"
	params.Environment.ReadyRegistryCredentialGeneration = params.Environment.RequiredRegistryCredentialGeneration
	params.Environment.RegistryCredentialCheckpoint = "activated"
	claimImport(t, dataStore, params)
	operation, _, err := dataStore.ImportGitRepository(t.Context(), params)
	if err != nil || operation.State != "completed" {
		t.Fatalf("private reuse import = %+v, %v", operation, err)
	}
	if operation.OrchestrationJobID == nil {
		t.Fatal("private reuse import orchestration job is nil")
	}
	var visibility, registry string
	var required *int64
	var credentialState string
	if err := pool.QueryRow(t.Context(), `SELECT app.image_visibility,app.registry_id::text,environment.required_registry_credential_generation,environment.registry_credential_state
		FROM apps app JOIN environments environment ON environment.app_id=app.id WHERE app.id=$1`, *operation.AppID).Scan(&visibility, &registry, &required, &credentialState); err != nil {
		t.Fatal(err)
	}
	if visibility != "private" || registry != registryID || required == nil || *required != 1 || credentialState != "ready" {
		t.Fatalf("private reuse binding visibility=%q registry=%q required=%v state=%q", visibility, registry, required, credentialState)
	}
	// Reusing the active credential never creates an operation or generation.
	var newOperations int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM registry_credential_operations WHERE kind='rotate' AND status NOT IN ('succeeded','failed','cancelled')`).Scan(&newOperations); err != nil || newOperations != 0 {
		t.Fatalf("new active operations = %d, want 0", newOperations)
	}
	var generations int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM registry_credential_generations`).Scan(&generations); err != nil || generations != 1 {
		t.Fatalf("generations = %d, want 1", generations)
	}
}

func TestImportGitRepositoryPrivateFirstCredentialPendingBindingIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	registryID, err := dataStore.EnsureGHCRRegistryID(t.Context(), orgOneID)
	if err != nil {
		t.Fatal(err)
	}
	secretRef := "k8s-secret://cyclopus-system/credential-import#dockerconfigjson"
	reserved, err := dataStore.ReserveRegistryCredentialOperation(t.Context(), credentialReserveParams(
		orgOneID, registryID, "import_create", "ghcr.import.import-first", "acme/api", commitSHA('d')))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.AttachRegistryCredentialSource(t.Context(), AttachRegistryCredentialSourceParams{
		OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: reserved.IdempotencyKey, SecretRef: secretRef}); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimRegistryCredentialOperations(t.Context(), orgOneID, "worker-before-import", time.Minute, 10)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("awaiting_import claimed before import = %+v, %v", claimed, err)
	}
	params := privateImportParams(integrationImportParams(actorID))
	params.RegistryOperationID, params.RegistryCandidateGeneration = reserved.ID, reserved.CandidateGeneration
	claimImport(t, dataStore, params)
	operation, _, err := dataStore.ImportGitRepository(t.Context(), params)
	if err != nil || operation.State != "completed" {
		t.Fatalf("first credential import = %+v, %v", operation, err)
	}
	if operation.OrchestrationJobID == nil {
		t.Fatal("first credential import orchestration job is nil")
	}
	var visibility, registry string
	var required *int64
	var credentialState string
	if err := pool.QueryRow(t.Context(), `SELECT app.image_visibility,app.registry_id::text,environment.required_registry_credential_generation,environment.registry_credential_state
		FROM apps app JOIN environments environment ON environment.app_id=app.id WHERE app.id=$1`, *operation.AppID).Scan(&visibility, &registry, &required, &credentialState); err != nil {
		t.Fatal(err)
	}
	if visibility != "private" || registry != registryID || required == nil || *required != reserved.CandidateGeneration || credentialState != "pending" {
		t.Fatalf("first credential binding visibility=%q registry=%q required=%v state=%q", visibility, registry, required, credentialState)
	}
	// The operation/generation carry only non-secret references.
	var operationTarget, operationStatus, generationState, generationSecretRef string
	if err := pool.QueryRow(t.Context(), `SELECT operation.target_repository,operation.status,generation.state,generation.secret_ref
		FROM registry_credential_operations operation JOIN registry_credential_generations generation ON generation.operation_id=operation.id
		WHERE operation.id=$1`, reserved.ID).Scan(&operationTarget, &operationStatus, &generationState, &generationSecretRef); err != nil {
		t.Fatal(err)
	}
	if operationTarget != "acme/api" || operationStatus != "pending" || generationState != "pending" || generationSecretRef != secretRef {
		t.Fatalf("operation/generation = %q %q %q %q", operationTarget, operationStatus, generationState, generationSecretRef)
	}
	for _, column := range []string{"username", "personal_access_token", "pat", "dockerconfigjson"} {
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_name LIKE 'registry_credential%' AND column_name=$1`, column).Scan(&count); err != nil || count != 0 {
			t.Fatalf("secret-bearing column %q count = %d", column, count)
		}
	}
}

func TestImportGitRepositoryPrivateFenceRejectsStaleCredentialStateIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	actorID := "00000000-0000-0000-0000-000000000091"
	_, _ = pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'admin@example.test','hash','Admin',true)`, actorID, orgOneID)
	seedImportRepository(t, dataStore, actorID)
	t.Run("absent registry", func(t *testing.T) {
		params := privateImportParams(integrationImportParams(actorID))
		params.IdempotencyKey, params.Idempotency.Key, params.Completion.Key = "import-absent", "import-absent", "import-absent"
		params.Project.Slug, params.App.Slug, params.App.HostnameLabel = "platform-absent", "api-absent", "api-absent"
		claimImport(t, dataStore, params)
		if _, _, err := dataStore.ImportGitRepository(t.Context(), params); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("absent registry error = %v, want failed precondition", err)
		}
	})
	t.Run("stale operation", func(t *testing.T) {
		params := privateImportParams(integrationImportParams(actorID))
		params.IdempotencyKey, params.Idempotency.Key, params.Completion.Key = "import-stale", "import-stale", "import-stale"
		params.Project.Slug, params.App.Slug, params.App.HostnameLabel = "platform-stale", "api-stale", "api-stale"
		params.RegistryOperationID, params.RegistryCandidateGeneration = "00000000-0000-7000-8000-000000000422", 1
		claimImport(t, dataStore, params)
		if _, _, err := dataStore.ImportGitRepository(t.Context(), params); !errors.Is(err, ErrFailedPrecondition) {
			t.Fatalf("stale operation error = %v, want failed precondition", err)
		}
	})
}

func TestReserveRegistryCredentialOperationConcurrentRotationOrgIsolationIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	actorOneID := "00000000-0000-0000-0000-000000000711"
	if _, err := pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'instance-admin@example.test','hash','Admin',true)`, actorOneID, orgOneID); err != nil {
		t.Fatal(err)
	}
	seedPrivateGHCRRegistry(t, pool, orgOneID, "00000000-0000-7000-8000-000000000431")
	seedPrivateGHCRRegistry(t, pool, orgTwoID, "00000000-0000-7000-8000-000000000432")
	const rotations = 4
	var wait sync.WaitGroup
	results := make(chan error, rotations)
	for index := 0; index < rotations; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			params := credentialReserveParams(orgOneID, "00000000-0000-7000-8000-000000000431", "rotate", "rotate-concurrent-"+strings.Repeat("k", index+1), "", "")
			params.ActorUserID = actorOneID
			_, err := dataStore.ReserveRegistryCredentialOperation(t.Context(), params)
			results <- err
		}(index)
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("concurrent rotation error = %v, want conflict", err)
		}
	}
	var orgOneOps int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM registry_credential_operations WHERE org_id=$1 AND kind='rotate' AND status NOT IN ('succeeded','failed','cancelled')`, orgOneID).Scan(&orgOneOps); err != nil {
		t.Fatal(err)
	}
	if orgOneOps != 1 {
		t.Fatalf("operations org1=%d, want one", orgOneOps)
	}
	params := credentialReserveParams(orgTwoID, "00000000-0000-7000-8000-000000000432", "rotate", "cross-org-actor", "", "")
	params.ActorUserID = actorOneID
	if _, err := dataStore.ReserveRegistryCredentialOperation(t.Context(), params); err == nil {
		t.Fatal("cross-org actor reservation succeeded")
	}
	var cross int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM registry_credential_operations WHERE org_id=$1 AND actor_user_id=$2`, orgTwoID, actorOneID).Scan(&cross); err != nil || cross != 0 {
		t.Fatalf("cross-org attributed operation rows = %d, %v", cross, err)
	}
}

func TestEnsureGHCRRegistryIDConcurrentCreateIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	const creators = 4
	var wait sync.WaitGroup
	ids := make(chan string, creators)
	for index := 0; index < creators; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			id, err := dataStore.EnsureGHCRRegistryID(t.Context(), orgOneID)
			if err != nil {
				t.Errorf("ensure = %v", err)
				return
			}
			ids <- id
		}()
	}
	wait.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("ensure ids diverged: %q vs %q", id, first)
		}
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM registries WHERE org_id=$1 AND provider='ghcr' AND registry_host='ghcr.io'`, orgOneID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ghcr registries = %d, want 1", count)
	}
}
