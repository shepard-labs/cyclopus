//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	aliasOneID = "00000000-0000-0000-0000-0000000000a1"
	aliasTwoID = "00000000-0000-0000-0000-0000000000a2"
)

func seedAlias(t *testing.T, pool *pgxpool.Pool, orgID, environmentID, releaseID, id, hostname string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO aliases (id, org_id, environment_id, hostname, release_id) VALUES ($1, $2, $3, $4, $5)`,
		id, orgID, environmentID, hostname, releaseID); err != nil {
		t.Fatal(err)
	}
}

func TestAliasStoreOrgScopingIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	seedRelease(t, pool, orgOneID, environmentOneID, releaseOneID, "live")
	seedRelease(t, pool, orgTwoID, environmentTwoID, releaseThreeID, "live")
	seedAlias(t, pool, orgOneID, environmentOneID, releaseOneID, aliasOneID, "one.example.test")
	seedAlias(t, pool, orgTwoID, environmentTwoID, releaseThreeID, aliasTwoID, "two.example.test")
	ctx := context.Background()

	got, err := dataStore.GetAlias(ctx, orgOneID, aliasOneID)
	if err != nil || got.Hostname != "one.example.test" || got.EnvironmentID != environmentOneID || got.ReleaseID != releaseOneID {
		t.Fatalf("GetAlias() = %+v, %v", got, err)
	}

	// Org scoping: org two cannot read org one's alias.
	if _, err := dataStore.GetAlias(ctx, orgTwoID, aliasOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAlias(cross-org) error = %v, want ErrNotFound", err)
	}

	listed, err := dataStore.ListAliases(ctx, orgOneID, environmentOneID)
	if err != nil || len(listed) != 1 || listed[0].ID != aliasOneID {
		t.Fatalf("ListAliases() = %+v, %v", listed, err)
	}

	// Cross-org: listing org two's environment as org one yields nothing.
	crossOrg, err := dataStore.ListAliases(ctx, orgOneID, environmentTwoID)
	if err != nil || len(crossOrg) != 0 {
		t.Fatalf("ListAliases(cross-org env) = %+v, %v, want empty", crossOrg, err)
	}
}
