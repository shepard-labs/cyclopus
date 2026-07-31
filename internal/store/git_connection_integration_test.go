package store

import (
	"context"
	"errors"
	"testing"
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

	secretRef := "k8s-secret://cyclopus/github#webhook"
	created, err := dataStore.CreateGitConnection(ctx, SaveGitConnectionParams{
		GitConnection: GitConnection{ID: gitConnectionOneID, OrgID: orgOneID, Provider: "github", InstallationID: 9001,
			RepoFullName: "acme/widgets", DefaultBranch: "main", WebhookSecretRef: secretRef},
		Idempotency: idempotencyClaimHelper(t, dataStore, orgOneID, "git_connection.create", "create-1"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000fc", "git_connection.created", gitConnectionOneID),
	})
	if err != nil || created.ID != gitConnectionOneID || created.InstallationID != 9001 || created.WebhookSecretRef != secretRef {
		t.Fatalf("CreateGitConnection() = %+v, %v", created, err)
	}

	// A duplicate (org_id, provider, installation_id, repo_full_name) violates UNIQUE.
	if _, err := dataStore.CreateGitConnection(ctx, SaveGitConnectionParams{
		GitConnection: GitConnection{ID: gitConnectionTwoID, OrgID: orgOneID, Provider: "github", InstallationID: 9001,
			RepoFullName: "acme/widgets", DefaultBranch: "dev", WebhookSecretRef: secretRef},
		Idempotency: idempotencyClaimHelper(t, dataStore, orgOneID, "git_connection.create", "create-dup"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000fd", "git_connection.created", gitConnectionTwoID),
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateGitConnection(duplicate) error = %v, want ErrAlreadyExists", err)
	}

	fetched, err := dataStore.GetGitConnection(ctx, orgOneID, gitConnectionOneID)
	if err != nil || fetched.RepoFullName != "acme/widgets" || fetched.DefaultBranch != "main" {
		t.Fatalf("GetGitConnection() = %+v, %v", fetched, err)
	}
	// Org scoping: another org cannot read this connection.
	if _, err := dataStore.GetGitConnection(ctx, orgTwoID, gitConnectionOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGitConnection(other org) error = %v, want ErrNotFound", err)
	}

	// tenantOne's seed inserted one git_connection (repositoryOneID); with the new
	// row the org now lists two.
	listed, err := dataStore.ListGitConnections(ctx, orgOneID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListGitConnections() = %+v, %v, want 2", listed, err)
	}

	if err := dataStore.DeleteGitConnection(ctx, orgOneID, gitConnectionOneID,
		idempotencyClaimHelper(t, dataStore, orgOneID, "git_connection.delete", "delete-1"),
		testAudit("00000000-0000-0000-0000-0000000000fe", "git_connection.deleted", gitConnectionOneID)); err != nil {
		t.Fatalf("DeleteGitConnection() error = %v", err)
	}
	if _, err := dataStore.GetGitConnection(ctx, orgOneID, gitConnectionOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGitConnection() after delete error = %v, want ErrNotFound", err)
	}
}

// TestGitConnectionStoreDeleteSetNullIntegration proves that deleting a git
// connection still referenced by a project succeeds and nulls the project's
// git_connection_id: projects.git_connection_id references git_connections
// ON DELETE SET NULL (distinct from registries' RESTRICT).
func TestGitConnectionStoreDeleteSetNullIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()

	// The seeded project references the seeded git_connection (repositoryOneID).
	if err := dataStore.DeleteGitConnection(ctx, orgOneID, repositoryOneID,
		idempotencyClaimHelper(t, dataStore, orgOneID, "git_connection.delete", "delete-setnull"),
		testAudit("00000000-0000-0000-0000-0000000000ff", "git_connection.deleted", repositoryOneID)); err != nil {
		t.Fatalf("DeleteGitConnection(referenced by project) error = %v, want nil (SET NULL)", err)
	}
	var connectionID *string
	if err := pool.QueryRow(ctx, `SELECT git_connection_id::text FROM projects WHERE org_id=$1 AND id=$2`, orgOneID, projectOneID).Scan(&connectionID); err != nil {
		t.Fatalf("read project git_connection_id: %v", err)
	}
	if connectionID != nil {
		t.Fatalf("project git_connection_id after delete = %v, want NULL", *connectionID)
	}
}
