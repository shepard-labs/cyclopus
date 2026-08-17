//go:build integration

package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	githubadapter "github.com/shepard-labs/cyclopus/internal/adapters/github"
	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestGitHubDiscoveryUsesInstallationClientPaginationAndAuthoritativeRepositoryFieldsIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.api.repositories = []fakeRepository{
		{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", private: true, archived: false, defaultBranch: "main"},
		{id: 100, nodeID: "R_100", owner: "acme", name: "old", fullName: "acme/old", archived: true, defaultBranch: "master"},
	}
	page, err := fixture.service.ListGitHubRepositories(t.Context(), fixture.actor, fixture.installation.ID, 1, "")
	if err != nil || len(page.Repositories) != 1 || page.NextPageToken == "" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	first := page.Repositories[0]
	if first.GitHubRepositoryID != 99 || first.NodeID != "R_99" || first.Visibility != "private" || first.Archived || first.DefaultBranch != "main" || first.FullName != "acme/api" {
		t.Fatalf("authoritative repository = %+v", first)
	}
	second, err := fixture.service.ListGitHubRepositories(t.Context(), fixture.actor, fixture.installation.ID, 1, page.NextPageToken)
	if err != nil || len(second.Repositories) != 1 || second.Repositories[0].GitHubRepositoryID != 100 || !second.Repositories[0].Archived || second.NextPageToken != "" {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	fixture.api.mu.Lock()
	fixture.api.repositories[1].name, fixture.api.repositories[1].fullName, fixture.api.repositories[1].defaultBranch = "renamed", "acme/renamed", "release"
	fixture.api.mu.Unlock()
	if _, err := fixture.service.ListGitHubRepositories(t.Context(), fixture.actor, fixture.installation.ID, 1, page.NextPageToken); err != ErrInvalidArgument {
		t.Fatalf("stale cursor after new repository error = %v", err)
	}
	for name, token := range map[string]string{
		"tampered":           page.NextPageToken[:len(page.NextPageToken)-1] + "A",
		"wrong installation": fixture.service.newGitHubCursor("repositories", fixture.actor.OrgID, fixture.installation.ID, "other", githubInstallationSnapshot(fixture.installation), 2, 50),
		"page size changed":  page.NextPageToken,
	} {
		t.Run(name, func(t *testing.T) {
			size := 50
			if name != "page size changed" {
				size = 1
			}
			if _, err := fixture.service.ListGitHubRepositories(t.Context(), fixture.actor, fixture.installation.ID, size, token); err != ErrInvalidArgument {
				t.Fatalf("cursor error = %v", err)
			}
		})
	}
	fixture.api.mu.Lock()
	wantSizes := append([]int(nil), fixture.api.repositoryPageSizes...)
	fixture.api.mu.Unlock()
	if fmt.Sprint(wantSizes) != "[1 1]" {
		t.Fatalf("GitHub per_page values = %v", wantSizes)
	}
}

func TestGitHubDiscoveryInstallationAndAPIErrorClassificationIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.api.repositories = []fakeRepository{{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"}}
	for _, state := range []struct {
		name, access string
		want         error
	}{{"suspended", "suspended", ErrGitHubInstallationSuspended}, {"deleted", "deleted", ErrGitHubInstallationRemoved}} {
		t.Run(state.name, func(t *testing.T) {
			local := newGitHubDiscoveryFixture(t)
			local.api.repositories = []fakeRepository{{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"}}
			if _, err := local.pool.Exec(t.Context(), `UPDATE github_installations SET access_state=$1,suspended_at=CASE WHEN $1='suspended' THEN now() ELSE NULL END,deleted_at=CASE WHEN $1='deleted' THEN now() ELSE NULL END WHERE id=$2`, state.access, local.installation.ID); err != nil {
				t.Fatal(err)
			}
			_, err := local.service.ListGitHubRepositories(t.Context(), local.actor, local.installation.ID, 50, "")
			if err != state.want {
				t.Fatalf("error = %v, want %v", err, state.want)
			}
		})
	}
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(statusCode), func(t *testing.T) {
			fixture.api.repositoryStatus = statusCode
			defer func() { fixture.api.repositoryStatus = 0 }()
			_, err := fixture.service.ListGitHubRepositories(t.Context(), fixture.actor, fixture.installation.ID, 50, "")
			var apiError *GitHubAPIError
			if !errors.As(err, &apiError) || apiError.Status != statusCode {
				t.Fatalf("error = %#v", err)
			}
		})
	}
	fixture.api.repositoryStatus = http.StatusForbidden
	fixture.api.rateRemaining = "0"
	fixture.api.rateReset = strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10)
	_, err := fixture.service.ListGitHubRepositories(t.Context(), fixture.actor, fixture.installation.ID, 50, "")
	var rateError *GitHubAPIError
	if !errors.As(err, &rateError) || rateError.Status != http.StatusForbidden || rateError.RetryAfter <= 0 {
		t.Fatalf("primary rate limit = %#v", err)
	}
}

func TestGetGhcrCredentialStatusUsesCanonicalRegistryIdentityIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	value, err := fixture.service.GetGhcrCredentialStatus(t.Context(), fixture.actor)
	if err != nil || value.State != "absent" || value.ID != "" || value.HasActiveCredential {
		t.Fatalf("missing canonical status = %+v, %v", value, err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO registries(id,org_id,provider,registry_host,state,active_generation,active_credential_secret_ref)
		VALUES('00000000-0000-7000-8000-000000000213',$1,'ghcr','ghcr.io','ready',1,'k8s-secret://cyclopus-system/ghcr#token')`, fixture.actor.OrgID); err != nil {
		t.Fatal(err)
	}
	value, err = fixture.service.GetGhcrCredentialStatus(t.Context(), fixture.actor)
	if err != nil || value.ID != "00000000-0000-7000-8000-000000000213" || !value.HasActiveCredential || value.RegistryHost != "ghcr.io" {
		t.Fatalf("canonical status = %+v, %v", value, err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `DROP INDEX registries_canonical_identity_key`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO registries(id,org_id,provider,registry_host,state)
		VALUES('00000000-0000-7000-8000-000000000214',$1,'ghcr','ghcr.io','absent')`, fixture.actor.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.GetGhcrCredentialStatus(t.Context(), fixture.actor); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("duplicate canonical status error = %v", err)
	}
}

func TestCreateAppRejectsUnsupportedRegistryIdentityIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	var projectID string
	if err := fixture.pool.QueryRow(t.Context(), `INSERT INTO projects(org_id,name,slug) VALUES($1,'Manual','manual') RETURNING id::text`, fixture.actor.OrgID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	var registryID string
	if err := fixture.pool.QueryRow(t.Context(), `INSERT INTO registries(org_id,provider,registry_host,state,active_generation,active_credential_secret_ref) VALUES($1,'gcr','gcr.io','ready',1,'k8s-secret://cyclopus-system/gcr#token') RETURNING id::text`, fixture.actor.OrgID).Scan(&registryID); err != nil {
		t.Fatal(err)
	}
	repository := "acme/api"
	input := AppInput{ProjectID: projectID, Name: "Manual API", Slug: "manual-api", HostnameLabel: "manual-api", RegistryID: &registryID, ImageRepository: &repository}
	prepared := applyAppCreateDefaults(normalizeAppInput(input))
	if err := fixture.service.validateRegistryApp(t.Context(), fixture.actor.OrgID, prepared); err != nil {
		t.Fatalf("validateRegistryApp(canonical GCR): %v", err)
	}
	if err := validateApp(prepared, "gvisor"); err != nil {
		t.Fatalf("validateApp(canonical GCR): %v", err)
	}
	if _, err := fixture.service.CreateApp(t.Context(), fixture.actor, "manual-gcr", input); err != nil {
		t.Fatalf("CreateApp(canonical GCR): %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `ALTER TABLE registries DROP CONSTRAINT registries_canonical_identity_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE registries SET registry_host='ghcr.io' WHERE id=$1`, registryID); err != nil {
		t.Fatal(err)
	}
	input.Slug = "manual-invalid"
	input.HostnameLabel = "manual-invalid"
	if _, err := fixture.service.CreateApp(t.Context(), fixture.actor, "manual-invalid", input); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CreateApp(provider/host mismatch) error = %v", err)
	}
}

func TestGitHubWorkflowCursorSurvivesNoOpRepositoryRefreshAndRejectsMutationIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	repository := fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"}
	fixture.seedRepository(t, repository)
	persisted, err := fixture.service.store.GetGitHubRepository(t.Context(), fixture.actor.OrgID, repository.id)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := fixture.service.store.GetGitHubInstallation(t.Context(), fixture.actor.OrgID, fixture.installation.GitHubInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	token := fixture.service.newGitHubCursor("workflows", fixture.actor.OrgID, strconv.FormatInt(repository.id, 10), fixture.installation.ID, githubInstallationSnapshot(installation)+":"+githubRepositorySnapshot(persisted), 1, 50)
	if _, err := fixture.service.ListGitHubRepositories(t.Context(), fixture.actor, fixture.installation.ID, 50, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ListGitHubRepositoryWorkflows(t.Context(), fixture.actor, repository.id, 50, token); err != nil {
		t.Fatalf("workflow cursor after no-op refresh = %v", err)
	}
	fixture.api.mu.Lock()
	fixture.api.repositories[0].defaultBranch = "release"
	fixture.api.mu.Unlock()
	if _, err := fixture.service.ListGitHubRepositories(t.Context(), fixture.actor, fixture.installation.ID, 50, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ListGitHubRepositoryWorkflows(t.Context(), fixture.actor, repository.id, 50, token); err != ErrInvalidArgument {
		t.Fatalf("workflow cursor after semantic mutation = %v", err)
	}
}

func TestGitHubWorkflowDiscoverySelectedRepositoryStateContentAndDeterminismIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	fixture.api.workflows = []fakeWorkflow{{path: ".github/workflows/z-disabled.yml", name: "disabled", state: "disabled_manually"}, {path: ".github/workflows/a-deploy.yml", name: "deploy", state: "active"}}
	fixture.api.contents = map[string]fakeContent{".github/workflows/a-deploy.yml@main": {sha: strings.Repeat("c", 40), content: compatibleWorkflow}}
	page, err := fixture.service.ListGitHubRepositoryWorkflows(t.Context(), fixture.actor, 99, 50, "")
	if err != nil || len(page.Workflows) != 2 {
		t.Fatalf("workflows = %+v, %v", page, err)
	}
	if page.Workflows[0].Path != ".github/workflows/a-deploy.yml" || !page.Workflows[0].Analysis.Compatible || page.Workflows[1].Analysis.Reasons[0] != "workflow_not_active" {
		t.Fatalf("sorted workflow analyses = %+v", page.Workflows)
	}
	fixture.api.contents[".github/workflows/a-deploy.yml@main"] = fakeContent{sha: strings.Repeat("d", 40), content: strings.Replace(compatibleWorkflow, `ref: "${{ inputs.commit_sha }}"`, "ref: main", 1)}
	first, err := fixture.service.ListGitHubRepositoryWorkflows(t.Context(), fixture.actor, 99, 50, "")
	second, secondErr := fixture.service.ListGitHubRepositoryWorkflows(t.Context(), fixture.actor, 99, 50, "")
	if err != nil || secondErr != nil || fmt.Sprint(first.Workflows[0].Analysis.Reasons) != fmt.Sprint(second.Workflows[0].Analysis.Reasons) || !contains(first.Workflows[0].Analysis.Reasons, "checkout_commit_sha_missing") {
		t.Fatalf("deterministic incompatible workflow = %+v / %+v / %v / %v", first, second, err, secondErr)
	}
	fixture.api.contentStatus = http.StatusNotFound
	unavailable, err := fixture.service.ListGitHubRepositoryWorkflows(t.Context(), fixture.actor, 99, 50, "")
	if err != nil || unavailable.Workflows[0].Analysis.Reasons[0] != "workflow_content_unavailable" {
		t.Fatalf("unavailable workflow = %+v, %v", unavailable, err)
	}
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		fixture.api.contentStatus = statusCode
		_, err := fixture.service.ListGitHubRepositoryWorkflows(t.Context(), fixture.actor, 99, 50, "")
		var apiError *GitHubAPIError
		if !errors.As(err, &apiError) || apiError.Status != statusCode {
			t.Fatalf("content status %d error = %#v", statusCode, err)
		}
	}
}

func TestGitHubImportRevalidatesRepositoryWorkflowBranchAndImageIntegration(t *testing.T) {
	newInput := func() ImportGitRepositoryInput {
		return ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "import"}
	}
	for name, mutate := range map[string]func(*githubDiscoveryFixture){
		"workflow changed": func(f *githubDiscoveryFixture) {
			f.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("e", 40), content: strings.Replace(compatibleWorkflow, "push: true", "push: false", 1)}
		},
		"branch removed":          func(f *githubDiscoveryFixture) { delete(f.api.branches, "main") },
		"repository inaccessible": func(f *githubDiscoveryFixture) { f.api.repositoryGetStatus = http.StatusNotFound },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newGitHubDiscoveryFixture(t)
			fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
			fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
			mutate(&fixture)
			_, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, newInput())
			if name == "workflow changed" {
				if !errors.Is(err, ErrWorkflowIncompatible) {
					t.Fatalf("error = %v", err)
				}
			} else if !errors.Is(err, ErrGitHubRepositoryRemoved) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	t.Run("repository renamed", func(t *testing.T) {
		fixture := newGitHubDiscoveryFixture(t)
		fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
		fixture.api.repositories[0].owner, fixture.api.repositories[0].name, fixture.api.repositories[0].fullName = "new-acme", "renamed", "new-acme/renamed"
		fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
		operation, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, newInput())
		if err != nil || operation.State != "completed" || operation.GitHubRepositoryID != 99 {
			t.Fatalf("renamed import = %+v, %v", operation, err)
		}
		persisted, err := fixture.service.store.GetGitHubRepository(t.Context(), fixture.actor.OrgID, 99)
		if err != nil || persisted.NodeID != "R_99" || persisted.FullName != "new-acme/renamed" || persisted.OwnerLogin != "new-acme" || persisted.Name != "renamed" {
			t.Fatalf("renamed persisted repository = %+v, %v", persisted, err)
		}
	})
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	fixture.api.contents[".github/workflows/deploy.yml@"+strings.Repeat("a", 40)] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	input := newInput()
	input.ImageRepository = "ghcr.io/acme/wrong"
	if _, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input); !errors.Is(err, ErrWorkflowIncompatible) {
		t.Fatalf("wrong image error = %v", err)
	}
}

func TestGitHubImportOversizedWorkflowFailsDeterministicallyAndReplaysIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	head := strings.Repeat("a", 40)
	fixture.api.contents[".github/workflows/deploy.yml@"+head] = fakeContent{sha: strings.Repeat("c", 40), content: strings.Repeat("x", maxWorkflowBytes+1)}
	input := ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "oversized"}
	failed, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrWorkflowIncompatible) || failed.State != "failed" || failed.FailureCode == nil || *failed.FailureCode != "workflow_incompatible" || failed.FailureMessage == nil || !strings.Contains(*failed.FailureMessage, "workflow_too_large") {
		t.Fatalf("oversized failure = %+v, %v", failed, err)
	}
	fixture.api.mu.Lock()
	calls := fixture.api.callCount
	fixture.api.mu.Unlock()
	replay, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrWorkflowIncompatible) || replay.ID != failed.ID {
		t.Fatalf("oversized replay = %+v, %v", replay, err)
	}
	fixture.api.mu.Lock()
	defer fixture.api.mu.Unlock()
	if fixture.api.callCount != calls {
		t.Fatalf("oversized replay GitHub calls = %d, want %d", fixture.api.callCount, calls)
	}
}

func TestGitHubImportInvalidBuildContextFailsDeterministicallyAndReplaysIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	head := strings.Repeat("a", 40)
	workflow := strings.Replace(compatibleWorkflow, "          context: .\n", "", 1)
	fixture.api.contents[".github/workflows/deploy.yml@"+head] = fakeContent{sha: strings.Repeat("c", 40), content: workflow}
	input := ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "execution-control-bypass"}
	failed, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrWorkflowIncompatible) || failed.State != "failed" || failed.FailureCode == nil || *failed.FailureCode != "workflow_incompatible" || failed.FailureMessage == nil || !strings.Contains(*failed.FailureMessage, "build_push_tag_invalid") {
		t.Fatalf("build-context failure = %+v, %v", failed, err)
	}
	fixture.api.mu.Lock()
	calls := fixture.api.callCount
	fixture.api.mu.Unlock()
	replay, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrWorkflowIncompatible) || replay.ID != failed.ID {
		t.Fatalf("build-context replay = %+v, %v", replay, err)
	}
	fixture.api.mu.Lock()
	defer fixture.api.mu.Unlock()
	if fixture.api.callCount != calls {
		t.Fatalf("build-context replay GitHub calls = %d, want %d", fixture.api.callCount, calls)
	}
}

func TestGitHubImportPinsWorkflowContentToResolvedBranchHeadIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	oldHead, newHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	fixture.api.contents[".github/workflows/deploy.yml@"+oldHead] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	fixture.api.contents[".github/workflows/deploy.yml@"+newHead] = fakeContent{sha: strings.Repeat("d", 40), content: strings.Replace(compatibleWorkflow, "push: true", "push: false", 1)}
	fixture.api.branchMoveAfterResolve = newHead
	input := ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "immutable"}
	operation, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if err != nil || operation.State != "completed" || operation.OrchestrationJobID == nil {
		t.Fatalf("import = %+v, %v", operation, err)
	}
	var ref, checkoutSHA, revision string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT dispatch_ref,desired_checkout_sha,workflow_revision FROM orchestration_jobs WHERE id=$1`, *operation.OrchestrationJobID).Scan(&ref, &checkoutSHA, &revision); err != nil {
		t.Fatal(err)
	}
	if ref != "main" || checkoutSHA != oldHead || revision != strings.Repeat("c", 40) {
		t.Fatalf("immutable import ref=%q checkout=%q revision=%q", ref, checkoutSHA, revision)
	}
}

func TestGitHubImportReplayAndConflictMakeNoGitHubCallsIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	head := strings.Repeat("a", 40)
	fixture.api.contents[".github/workflows/deploy.yml@"+head] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	input := ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "replay"}
	first, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if err != nil {
		t.Fatal(err)
	}
	fixture.api.mu.Lock()
	calls := fixture.api.callCount
	fixture.api.repositoryGetStatus = http.StatusServiceUnavailable
	fixture.api.mu.Unlock()
	replayActor := fixture.actor
	replayActor.SessionID = "renewed-session"
	replay, err := fixture.service.ImportGitRepository(t.Context(), replayActor, input)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	fixture.api.mu.Lock()
	if fixture.api.callCount != calls {
		t.Fatalf("replay GitHub calls = %d, want %d", fixture.api.callCount, calls)
	}
	fixture.api.mu.Unlock()
	other := fixture.actor
	other.UserID = "00000000-0000-0000-0000-000000000099"
	if _, err := fixture.service.ImportGitRepository(t.Context(), other, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("other actor error = %v", err)
	}
	fixture.api.mu.Lock()
	defer fixture.api.mu.Unlock()
	if fixture.api.callCount != calls {
		t.Fatalf("conflict GitHub calls = %d, want %d", fixture.api.callCount, calls)
	}
}

func TestGitHubImportPrivateCredentialFailureIsAtomicAndReplayableIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	fixture.api.mu.Lock()
	calls := fixture.api.callCount
	fixture.api.mu.Unlock()
	input := ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "private", PrivateCredentialRequested: true}
	failed, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrRegistryAuthRequired) || failed.State != "failed" || failed.FailureCode == nil || *failed.FailureCode != "registry_auth_required" {
		t.Fatalf("private failure = %+v, %v", failed, err)
	}
	for _, table := range []string{"git_connections", "projects", "apps", "environments", "orchestration_jobs"} {
		var count int
		if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s writes = %d, %v", table, count, err)
		}
	}
	replay, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrRegistryAuthRequired) || replay.ID != failed.ID {
		t.Fatalf("private replay = %+v, %v", replay, err)
	}
	fixture.api.mu.Lock()
	defer fixture.api.mu.Unlock()
	if fixture.api.callCount != calls {
		t.Fatalf("private failure GitHub calls = %d, want %d", fixture.api.callCount, calls)
	}
}

func TestGitHubImportInProgressReturnsBeforeGitHubIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	input := ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "concurrent"}
	request := struct {
		ActorUserID string
		Operator    bool
		Input       ImportGitRepositoryInput
	}{fixture.actor.UserID, fixture.actor.Operator, input}
	claim := store.IdempotencyClaimParams{OrgID: fixture.actor.OrgID, Scope: "github.repository.import", Key: input.IdempotencyKey, RequestHash: requestHash(request), Owner: "existing-owner", LeaseDuration: importLeaseDuration, Retention: idempotencyRetention}
	result, err := fixture.service.store.ClaimIdempotency(t.Context(), claim)
	if err != nil || result.Disposition != store.IdempotencyClaimed {
		t.Fatalf("initial claim = %+v, %v", result, err)
	}
	fixture.api.mu.Lock()
	calls := fixture.api.callCount
	fixture.api.mu.Unlock()
	if _, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input); !errors.Is(err, ErrInProgress) {
		t.Fatalf("concurrent import error = %v", err)
	}
	fixture.api.mu.Lock()
	defer fixture.api.mu.Unlock()
	if fixture.api.callCount != calls {
		t.Fatalf("in-progress GitHub calls = %d, want %d", fixture.api.callCount, calls)
	}
}

func TestGitHubImportDeterministicStoreFailureCompletesAndReplaysIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	head := strings.Repeat("a", 40)
	fixture.api.contents[".github/workflows/deploy.yml@"+head] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO projects(org_id,name,slug) VALUES($1,'Existing','imported')`, fixture.actor.OrgID); err != nil {
		t.Fatal(err)
	}
	input := ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "store-conflict"}
	failed, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrConflict) || failed.State != "failed" || failed.FailureCode == nil || *failed.FailureCode != "conflict" {
		t.Fatalf("deterministic store failure = %+v, %v", failed, err)
	}
	fixture.api.mu.Lock()
	calls := fixture.api.callCount
	fixture.api.mu.Unlock()
	replay, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if !errors.Is(err, ErrConflict) || replay.ID != failed.ID {
		t.Fatalf("deterministic store replay = %+v, %v", replay, err)
	}
	fixture.api.mu.Lock()
	defer fixture.api.mu.Unlock()
	if fixture.api.callCount != calls {
		t.Fatalf("deterministic store replay GitHub calls = %d, want %d", fixture.api.callCount, calls)
	}
}

func TestGitHubImportTransientStoreFailureAbandonsClaimImmediatelyIntegration(t *testing.T) {
	fixture := newGitHubDiscoveryFixture(t)
	fixture.seedRepository(t, fakeRepository{id: 99, nodeID: "R_99", owner: "acme", name: "api", fullName: "acme/api", defaultBranch: "main"})
	head := strings.Repeat("a", 40)
	fixture.api.contents[".github/workflows/deploy.yml@"+head] = fakeContent{sha: strings.Repeat("c", 40), content: compatibleWorkflow}
	if _, err := fixture.pool.Exec(t.Context(), `CREATE FUNCTION fail_import_transient() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected serialization failure' USING ERRCODE='40001'; END $$;
		CREATE TRIGGER fail_import_transient BEFORE INSERT ON projects FOR EACH ROW EXECUTE FUNCTION fail_import_transient()`); err != nil {
		t.Fatal(err)
	}
	input := ImportGitRepositoryInput{RepositoryID: 99, WorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Imported", ProjectSlug: "imported", ApplicationName: "Imported API", ApplicationSlug: "imported-api", HostnameLabel: "imported-api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "store-transient"}
	if _, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input); err == nil || errors.Is(err, ErrInProgress) {
		t.Fatalf("transient store failure = %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `DROP TRIGGER fail_import_transient ON projects; DROP FUNCTION fail_import_transient()`); err != nil {
		t.Fatal(err)
	}
	operation, err := fixture.service.ImportGitRepository(t.Context(), fixture.actor, input)
	if err != nil || operation.State != "completed" {
		t.Fatalf("immediate transient retry = %+v, %v", operation, err)
	}
}

func TestParsePublicGHCRRepository(t *testing.T) {
	valid := []string{"ghcr.io/acme/api", "ghcr.io/a/b.c", "ghcr.io/a/b_c", "ghcr.io/a/b__c", "ghcr.io/a/b---c", "ghcr.io/a/b/c"}
	invalid := []string{"", "ghcr.io/a", "ghcr.io//a/b", "ghcr.io/a//b", "ghcr.io/a/b/", "ghcr.io/a/.", "ghcr.io/a/..", "ghcr.io/A/b", "ghcr.io/a/B", "ghcr.io/a/b-", "ghcr.io/a/_b", "ghcr.io/a/b___c", "ghcr.io/a/b:c", "ghcr.io/a/b@sha256:abc", "https://ghcr.io/a/b", "ghcr.io/a/b?q=1", "ghcr.io/a/b#x", "ghcr.io/a/bé"}
	for _, value := range valid {
		if got, err := ParsePublicGHCRRepository(value); err != nil || got != value {
			t.Errorf("valid %q = %q, %v", value, got, err)
		}
	}
	for _, value := range invalid {
		if _, err := ParsePublicGHCRRepository(value); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("invalid %q error = %v", value, err)
		}
	}
}

type githubDiscoveryFixture struct {
	service      *Service
	actor        ActorContext
	installation store.GitHubInstallation
	pool         *pgxpool.Pool
	api          *fakeGitHubAPI
}

func newGitHubDiscoveryFixture(t *testing.T) githubDiscoveryFixture {
	t.Helper()
	dataStore, pool := newGitHubIntegrationStore(t)
	actor := seedGitHubActor(t, pool)
	api := &fakeGitHubAPI{contents: map[string]fakeContent{}, branches: map[string]string{"main": strings.Repeat("a", 40)}}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credential := githubadapter.AppCredential{AppID: 42, PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), ResourceVersion: "1"}
	factory, err := githubadapter.NewInstallationClientFactory(staticCredentialProvider{credential}, server.URL+"/", server.URL+"/", "2022-11-28", http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, testGitHubAudit(actor), actor.UserID)
	if err != nil {
		t.Fatal(err)
	}
	return githubDiscoveryFixture{service: New(dataStore, WithGitHubDiscovery(factory, []byte(strings.Repeat("k", 32))), WithHostnameSuffix("example.test")), actor: actor, installation: installation, pool: pool, api: api}
}

func (f githubDiscoveryFixture) seedRepository(t *testing.T, repository fakeRepository) {
	t.Helper()
	f.api.repositories = []fakeRepository{repository}
	if _, err := f.service.ListGitHubRepositories(t.Context(), f.actor, f.installation.ID, 50, ""); err != nil {
		t.Fatal(err)
	}
}

type staticCredentialProvider struct{ credential githubadapter.AppCredential }

func (p staticCredentialProvider) AppCredential(context.Context) (githubadapter.AppCredential, error) {
	return p.credential, nil
}

type fakeRepository struct {
	id                      int64
	nodeID, owner, name     string
	fullName, defaultBranch string
	private, archived       bool
}
type fakeWorkflow struct{ path, name, state string }
type fakeContent struct{ sha, content string }
type fakeGitHubAPI struct {
	mu                                                   sync.Mutex
	repositories                                         []fakeRepository
	workflows                                            []fakeWorkflow
	contents                                             map[string]fakeContent
	branches                                             map[string]string
	repositoryPageSizes                                  []int
	repositoryStatus, repositoryGetStatus, contentStatus int
	rateRemaining, rateReset                             string
	branchMoveAfterResolve                               string
	callCount                                            int
}

func (f *fakeGitHubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
		_, _ = fmt.Fprintf(w, `{"token":"installation-token","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	case r.URL.Path == "/installation/repositories":
		if f.repositoryStatus != 0 {
			if f.rateRemaining != "" {
				w.Header().Set("X-RateLimit-Remaining", f.rateRemaining)
			}
			if f.rateReset != "" {
				w.Header().Set("X-RateLimit-Reset", f.rateReset)
			}
			w.WriteHeader(f.repositoryStatus)
			_, _ = w.Write([]byte(`{"message":"unavailable"}`))
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		f.repositoryPageSizes = append(f.repositoryPageSizes, perPage)
		start := (page - 1) * perPage
		end := start + perPage
		if end > len(f.repositories) {
			end = len(f.repositories)
		}
		values := []map[string]any{}
		if start < len(f.repositories) {
			for _, repository := range f.repositories[start:end] {
				values = append(values, repositoryJSON(repository))
			}
		}
		if end < len(f.repositories) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/installation/repositories?page=%d&per_page=%d>; rel="next"`, absoluteURL(r), page+1, perPage))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(f.repositories), "repositories": values})
	case strings.HasPrefix(r.URL.Path, "/repos/acme/") && strings.HasSuffix(r.URL.Path, "/actions/workflows"):
		values := []map[string]any{}
		for index, workflow := range f.workflows {
			values = append(values, map[string]any{"id": index + 1, "name": workflow.name, "path": workflow.path, "state": workflow.state})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(values), "workflows": values})
	case strings.Contains(r.URL.Path, "/contents/"):
		if f.contentStatus != 0 {
			w.WriteHeader(f.contentStatus)
			_, _ = w.Write([]byte(`{"message":"missing"}`))
			return
		}
		path := r.URL.Path[strings.Index(r.URL.Path, "/contents/")+len("/contents/"):]
		value, ok := f.contents[path+"@"+r.URL.Query().Get("ref")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "name": path, "path": path, "sha": value.sha, "size": len(value.content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(value.content))})
	case strings.Contains(r.URL.Path, "/branches/"):
		branch := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		sha, ok := f.branches[branch]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": branch, "commit": map[string]any{"sha": sha}})
		if f.branchMoveAfterResolve != "" {
			f.branches[branch] = f.branchMoveAfterResolve
			f.branchMoveAfterResolve = ""
		}
	case strings.HasPrefix(r.URL.Path, "/repositories/"):
		if f.repositoryGetStatus != 0 {
			w.WriteHeader(f.repositoryGetStatus)
			_, _ = w.Write([]byte(`{"message":"missing"}`))
			return
		}
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repositories/"), 10, 64)
		for _, repository := range f.repositories {
			if repository.id == id {
				_ = json.NewEncoder(w).Encode(repositoryJSON(repository))
				return
			}
		}
		http.NotFound(w, r)
	case strings.HasPrefix(r.URL.Path, "/repos/"):
		if f.repositoryGetStatus != 0 {
			w.WriteHeader(f.repositoryGetStatus)
			_, _ = w.Write([]byte(`{"message":"missing"}`))
			return
		}
		fullName := strings.TrimPrefix(r.URL.Path, "/repos/")
		for _, repository := range f.repositories {
			if repository.fullName == fullName {
				_ = json.NewEncoder(w).Encode(repositoryJSON(repository))
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func repositoryJSON(repository fakeRepository) map[string]any {
	visibility := "public"
	if repository.private {
		visibility = "private"
	}
	return map[string]any{"id": repository.id, "node_id": repository.nodeID, "name": repository.name, "full_name": repository.fullName, "private": repository.private, "archived": repository.archived, "visibility": visibility, "default_branch": repository.defaultBranch, "owner": map[string]any{"login": repository.owner}}
}
func absoluteURL(r *http.Request) string { return "http://" + r.Host }
