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
