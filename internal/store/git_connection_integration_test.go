//go:build integration

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

	fetched, err := dataStore.GetGitConnection(ctx, orgOneID, repositoryOneID)
	if err != nil || fetched.RepoFullName != "acme/one" || fetched.DefaultBranch != "main" || fetched.InstallationID != 1 {
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
	if _, err := dataStore.CreateGitConnection(ctx, SaveGitConnectionParams{}); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("CreateGitConnection() error = %v, want ErrFailedPrecondition", err)
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
		testAudit("00000000-0000-0000-0000-0000000000ff", "git_connection.deleted", repositoryOneID)); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("DeleteGitConnection(referenced by project) error = %v, want ErrFailedPrecondition", err)
	}
	var connectionID *string
	if err := pool.QueryRow(ctx, `SELECT git_connection_id::text FROM projects WHERE org_id=$1 AND id=$2`, orgOneID, projectOneID).Scan(&connectionID); err != nil {
		t.Fatalf("read project git_connection_id: %v", err)
	}
	if connectionID == nil || *connectionID != repositoryOneID {
		t.Fatalf("project git_connection_id after rejected delete = %v, want %s", connectionID, repositoryOneID)
	}
}
