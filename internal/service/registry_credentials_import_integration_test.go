//go:build integration

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/oci"
)

type fakeImportSourceWriter struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeImportSourceWriter) DeleteCredential(_ context.Context, _, _, _ string, _ int64, _ bool) error {
	return nil
}

func (f *fakeImportSourceWriter) WriteCredential(_ context.Context, orgID, registryID, operationID string, generation int64, credential *oci.Credential) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s/%s/g%d", orgID, registryID, generation))
	if f.err != nil {
		return "", "", "", f.err
	}
	return fmt.Sprintf("k8s-secret://cyclopus-system/credential-%s-g%d#dockerconfigjson", registryID[:13], generation), "uid", "1", nil
}

func (f *fakeImportSourceWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func privateImportFixture(t *testing.T) (githubDiscoveryFixture, *fakeImportSourceWriter) {
	t.Helper()
	fixture := newGitHubDiscoveryFixture(t)
	source := &fakeImportSourceWriter{}
	credentials, err := NewRegistryCredentialServiceFromStore(fixture.service.store, source, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.registryCredentialService = credentials
	return fixture, source
}

func privateImportInput() ImportGitRepositoryInput {
	return ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml",
		ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api",
		HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "private-import",
		GhcrCredential: &GhcrCredentialInput{Username: []byte("octocat"), PersonalAccessToken: []byte("pat-value")}}
}

func TestGitHubImportPrivateFirstCredentialCreatesPendingCandidateIntegration(t *testing.T) {
	fixture, source := privateImportFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", private: true, defaultBranch: "main"})
	fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	operation, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, privateImportInput())
	if err != nil || operation.State != "completed" {
		t.Fatalf("first credential import = %+v, %v", operation, err)
	}
	if operation.OrchestrationJobID == nil {
		t.Fatal("first credential import orchestration job is nil")
	}
	if source.count() != 1 {
		t.Fatalf("source writes = %d, want 1", source.count())
	}
	var visibility, registryID, credentialState string
	var required *int64
	if err := fixture.pool.QueryRow(t.Context(), `SELECT app.image_visibility,app.registry_id::text,environment.registry_credential_state,environment.required_registry_credential_generation
		FROM apps app JOIN environments environment ON environment.app_id=app.id WHERE app.id=$1`, *operation.AppID).Scan(&visibility, &registryID, &credentialState, &required); err != nil {
		t.Fatal(err)
	}
	if visibility != "private" || registryID == "" || credentialState != "pending" || required == nil || *required != 1 {
		t.Fatalf("private app/environment = %q %q %q %v", visibility, registryID, credentialState, required)
	}
	var operationKind, operationStatus, target, reference string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT kind,status,target_repository,target_reference FROM registry_credential_operations WHERE org_id=$1`, fixture.actor.OrgID).Scan(&operationKind, &operationStatus, &target, &reference); err != nil {
		t.Fatal(err)
	}
	if operationKind != "import_create" || operationStatus != "pending" || target != "acme/api" || reference != strings.Repeat("a", 40) {
		t.Fatalf("credential operation = %q %q %q %q", operationKind, operationStatus, target, reference)
	}
	// No raw credential material reaches the database: audits, idempotency,
	// operations, and generations carry only non-secret references.
	var leaks int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM audit_events WHERE before::text LIKE '%pat-value%' OR before::text LIKE '%octocat%' OR after::text LIKE '%pat-value%' OR after::text LIKE '%octocat%') +
		(SELECT count(*) FROM api_idempotency_records WHERE request_hash='pat-value' OR response_body::text LIKE '%pat-value%') +
		(SELECT count(*) FROM registry_credential_generations WHERE secret_ref LIKE '%pat-value%')`).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("secret material rows = %d, %v", leaks, err)
	}
	// The imported job's environment holds the pending candidate binding; the
	// claim predicate's registry readiness gate is exercised in the store
	// integration suite.
	var claimable int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM orchestration_jobs job
		JOIN apps app ON app.org_id=job.org_id AND app.id=job.app_id
		LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
		LEFT JOIN registry_credential_generations generation ON generation.org_id=registry.org_id AND generation.registry_id=registry.id AND generation.generation=registry.active_generation AND generation.state='active'
		WHERE job.org_id=$1 AND job.status='resolving_image' AND app.image_visibility='private'
		AND registry.state='ready' AND registry.active_generation>0 AND generation.generation=registry.active_generation`, fixture.actor.OrgID).Scan(&claimable); err != nil || claimable != 0 {
		t.Fatalf("claimable pending-credential jobs = %d, want 0", claimable)
	}
}

func TestGitHubImportPrivateReadyCredentialReuseWithoutPATWriteIntegration(t *testing.T) {
	fixture, source := privateImportFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", private: true, defaultBranch: "main"})
	fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	// Pre-establish a ready active credential for the org.
	var registryID string
	if err := fixture.pool.QueryRow(t.Context(), `INSERT INTO registries(org_id,provider,registry_host,state,checkpoint,active_generation,active_credential_secret_ref,activated_at)
		VALUES($1,'ghcr','ghcr.io','ready','activated',1,'k8s-secret://cyclopus-system/established#dockerconfigjson',clock_timestamp()) RETURNING id::text`, fixture.actor.OrgID).Scan(&registryID); err != nil {
		t.Fatal(err)
	}
	var credentialOperationID string
	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(t.Context(), `INSERT INTO registry_credential_operations(org_id,registry_id,kind,status,checkpoint,candidate_generation,idempotency_key,terminal_at)
		VALUES($1,$2,'create','succeeded','activated',1,'ready-import-fixture',clock_timestamp()) RETURNING id::text`, fixture.actor.OrgID, registryID).Scan(&credentialOperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id,validated_at,activated_at)
		VALUES($1,$2,1,'k8s-secret://cyclopus-system/established#dockerconfigjson','active',$3,clock_timestamp(),clock_timestamp())`, fixture.actor.OrgID, registryID, credentialOperationID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	input := privateImportInput()
	input.GhcrCredential = nil
	input.PrivateCredentialRequested = false
	operation, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if err != nil || operation.State != "completed" {
		t.Fatalf("reuse import = %+v, %v", operation, err)
	}
	if operation.OrchestrationJobID == nil {
		t.Fatal("reuse import orchestration job is nil")
	}
	if source.count() != 0 {
		t.Fatalf("source writes = %d, want 0", source.count())
	}
	var bound, credentialState string
	var required *int64
	if err := fixture.pool.QueryRow(t.Context(), `SELECT app.registry_id::text,environment.registry_credential_state,environment.required_registry_credential_generation
		FROM apps app JOIN environments environment ON environment.app_id=app.id WHERE app.id=$1`, *operation.AppID).Scan(&bound, &credentialState, &required); err != nil {
		t.Fatal(err)
	}
	if bound != registryID || credentialState != "ready" || required == nil || *required != 1 {
		t.Fatalf("reuse binding = %q %q %v", bound, credentialState, required)
	}
	var operations int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM registry_credential_operations WHERE org_id=$1 AND status NOT IN ('succeeded','failed','cancelled')`, fixture.actor.OrgID).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("credential operations = %d, want 0", operations)
	}
}

func TestGitHubImportPrivateReadyCredentialRejectsSuppliedReplacementIntegration(t *testing.T) {
	fixture, source := privateImportFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", private: true, defaultBranch: "main"})
	fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	var registryID string
	if err := fixture.pool.QueryRow(t.Context(), `INSERT INTO registries(org_id,provider,registry_host,state,checkpoint,active_generation,active_credential_secret_ref,activated_at)
		VALUES($1,'ghcr','ghcr.io','ready','activated',1,'k8s-secret://cyclopus-system/established#dockerconfigjson',clock_timestamp()) RETURNING id::text`, fixture.actor.OrgID).Scan(&registryID); err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	var operationID string
	if err := tx.QueryRow(t.Context(), `INSERT INTO registry_credential_operations(org_id,registry_id,kind,status,checkpoint,candidate_generation,idempotency_key,terminal_at)
		VALUES($1,$2,'create','succeeded','activated',1,'replacement-fixture',clock_timestamp()) RETURNING id::text`, fixture.actor.OrgID, registryID).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id,validated_at,activated_at)
		VALUES($1,$2,1,'k8s-secret://cyclopus-system/established#dockerconfigjson','active',$3,clock_timestamp(),clock_timestamp())`, fixture.actor.OrgID, registryID, operationID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, privateImportInput())
	if !errors.Is(err, ErrRegistryCredentialExists) || failed.State != "failed" || failed.FailureCode == nil || *failed.FailureCode != "registry_credential_exists" {
		t.Fatalf("supplied replacement = %+v, %v", failed, err)
	}
	if source.count() != 0 {
		t.Fatalf("source writes = %d, want 0", source.count())
	}
	replay, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, privateImportInput())
	if !errors.Is(err, ErrRegistryCredentialExists) || replay.ID != failed.ID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
}

func TestGitHubImportPrivateWithoutCredentialFailsIntegration(t *testing.T) {
	fixture, source := privateImportFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", private: true, defaultBranch: "main"})
	fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	input := privateImportInput()
	input.GhcrCredential = nil
	input.PrivateCredentialRequested = false
	failed, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrRegistryAuthRequired) || failed.State != "failed" || failed.FailureCode == nil || *failed.FailureCode != "registry_auth_required" {
		t.Fatalf("private without credential = %+v, %v", failed, err)
	}
	if source.count() != 0 {
		t.Fatalf("source writes = %d, want 0", source.count())
	}
	for _, table := range []string{"git_connections", "projects", "apps", "environments", "registry_credential_operations"} {
		var count int
		if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s writes = %d, %v", table, count, err)
		}
	}
}

func TestGitHubImportPublicRejectsSuppliedCredentialIntegration(t *testing.T) {
	fixture, source := privateImportFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	failed, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, privateImportInput())
	if !errors.Is(err, ErrInvalidArgument) || failed.State != "failed" || failed.FailureCode == nil || *failed.FailureCode != "invalid_argument" {
		t.Fatalf("public with credential = %+v, %v", failed, err)
	}
	if source.count() != 0 {
		t.Fatalf("source writes = %d, want 0", source.count())
	}
}

func TestGitHubImportPrivateRetryAfterTransientSourceFailureIntegration(t *testing.T) {
	fixture, source := privateImportFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", private: true, defaultBranch: "main"})
	fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	source.err = errors.New("kubernetes unavailable")
	if _, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, privateImportInput()); !errors.Is(err, ErrRegistryCredentialRetry) {
		t.Fatalf("transient source error = %v, want retry", err)
	}
	source.err = nil
	operation, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, privateImportInput())
	if !errors.Is(err, ErrInProgress) || operation.ID != "" {
		t.Fatalf("retry while durable source operation is pending = %+v, %v", operation, err)
	}
	if source.count() != 1 {
		t.Fatalf("source writes = %d, want 1; retries must not accept new secret bytes", source.count())
	}
	var operations, generations int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM registry_credential_operations), (SELECT count(*) FROM registry_credential_generations)`).Scan(&operations, &generations); err != nil || operations != 1 || generations != 1 {
		t.Fatalf("operations=%d generations=%d, want 1/1", operations, generations)
	}
}
