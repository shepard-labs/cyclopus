//go:build integration

package store

import (
	"context"
	"testing"
)

const (
	registryOneID = "00000000-0000-0000-0000-0000000000f1"
	registryTwoID = "00000000-0000-0000-0000-0000000000f2"
)

func TestRegistryStoreCRUDIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	ctx := context.Background()

	listed, err := dataStore.ListRegistries(ctx, orgOneID)
	if err != nil || len(listed) != 0 {
		t.Fatalf("ListRegistries() = %+v, %v, want empty", listed, err)
	}
	// Org scoping on List: org two sees none of org one's registries.
	if other, err := dataStore.ListRegistries(ctx, orgTwoID); err != nil || len(other) != 0 {
		t.Fatalf("ListRegistries(other org) = %+v, %v, want empty", other, err)
	}

}
