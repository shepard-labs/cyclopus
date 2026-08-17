//go:build integration

package store

import (
	"context"
	"errors"
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

func TestRegistryCanonicalIdentityIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	for _, value := range []struct{ org, provider, host string }{
		{orgOneID, RegistryProviderGHCR, RegistryHostGHCR},
		{orgTwoID, RegistryProviderGHCR, RegistryHostGHCR},
		{orgOneID, RegistryProviderGCR, RegistryHostGCR},
	} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO registries(org_id,provider,registry_host) VALUES($1,$2,$3)`, value.org, value.provider, value.host); err != nil {
			t.Fatalf("insert supported registry %s/%s: %v", value.provider, value.host, err)
		}
	}
	value, err := dataStore.GetGHCRRegistry(t.Context(), orgOneID)
	if err != nil || value.Provider != RegistryProviderGHCR || value.RegistryHost != RegistryHostGHCR {
		t.Fatalf("GetGHCRRegistry() = %+v, %v", value, err)
	}
	if _, err := dataStore.GetGHCRRegistry(t.Context(), "00000000-0000-0000-0000-000000000099"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGHCRRegistry(unknown org) error = %v", err)
	}
	for _, identity := range [][2]string{{"other", "registry.example.test"}, {RegistryProviderGHCR, RegistryHostGCR}, {"GHCR", RegistryHostGHCR}, {RegistryProviderGHCR, "https://ghcr.io"}} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO registries(org_id,provider,registry_host) VALUES($1,$2,$3)`, orgTwoID, identity[0], identity[1]); err == nil {
			t.Fatalf("unsupported/noncanonical identity %q/%q succeeded", identity[0], identity[1])
		}
	}
}
