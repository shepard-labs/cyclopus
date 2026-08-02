//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

const (
	registryOneID = "00000000-0000-0000-0000-0000000000f1"
	registryTwoID = "00000000-0000-0000-0000-0000000000f2"
)

// idempotencyClaimHelper claims an idempotency record and returns the matching
// completion params, mirroring the domain integration test's claim helper.
func idempotencyClaimHelper(t *testing.T, dataStore *Store, orgID, scope, key string) CompleteIdempotencyParams {
	t.Helper()
	ctx := context.Background()
	hash := requestHash(scope + ":" + key)
	if _, err := dataStore.ClaimIdempotency(ctx, IdempotencyClaimParams{
		OrgID: orgID, Scope: scope, Key: key, RequestHash: hash,
		Owner: "owner", LeaseDuration: time.Minute, Retention: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	return CompleteIdempotencyParams{OrgID: orgID, Scope: scope, Key: key, RequestHash: hash, Owner: "owner",
		Status: "completed", StatusCode: 200, Body: json.RawMessage(`{"ok":true}`)}
}

func TestRegistryStoreCRUDIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	ctx := context.Background()

	secretRef := "k8s-secret://cyclopus/registry-creds#dockerconfigjson"
	created, err := dataStore.CreateRegistry(ctx, SaveRegistryParams{
		Registry:    Registry{ID: registryOneID, OrgID: orgOneID, Provider: "ghcr", RegistryURL: "ghcr.io/acme", CredentialSecretRef: secretRef},
		Idempotency: idempotencyClaimHelper(t, dataStore, orgOneID, "registry.create", "create-1"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000f5", "registry.created", registryOneID),
	})
	if err != nil || created.ID != registryOneID || created.CredentialSecretRef != secretRef {
		t.Fatalf("CreateRegistry() = %+v, %v", created, err)
	}

	// A duplicate (org_id, registry_url) violates the UNIQUE constraint.
	if _, err := dataStore.CreateRegistry(ctx, SaveRegistryParams{
		Registry:    Registry{ID: registryTwoID, OrgID: orgOneID, Provider: "gcr", RegistryURL: "ghcr.io/acme", CredentialSecretRef: secretRef},
		Idempotency: idempotencyClaimHelper(t, dataStore, orgOneID, "registry.create", "create-dup"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000f6", "registry.created", registryTwoID),
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateRegistry(duplicate url) error = %v, want ErrAlreadyExists", err)
	}

	fetched, err := dataStore.GetRegistry(ctx, orgOneID, registryOneID)
	if err != nil || fetched.Provider != "ghcr" || fetched.RegistryURL != "ghcr.io/acme" {
		t.Fatalf("GetRegistry() = %+v, %v", fetched, err)
	}
	// Org scoping: another org cannot read this registry.
	if _, err := dataStore.GetRegistry(ctx, orgTwoID, registryOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRegistry(other org) error = %v, want ErrNotFound", err)
	}

	listed, err := dataStore.ListRegistries(ctx, orgOneID)
	if err != nil || len(listed) != 1 || listed[0].ID != registryOneID {
		t.Fatalf("ListRegistries() = %+v, %v", listed, err)
	}
	// Org scoping on List: org two sees none of org one's registries.
	if other, err := dataStore.ListRegistries(ctx, orgTwoID); err != nil || len(other) != 0 {
		t.Fatalf("ListRegistries(other org) = %+v, %v, want empty", other, err)
	}

	if err := dataStore.DeleteRegistry(ctx, orgOneID, registryOneID,
		idempotencyClaimHelper(t, dataStore, orgOneID, "registry.delete", "delete-1"),
		testAudit("00000000-0000-0000-0000-0000000000f7", "registry.deleted", registryOneID)); err != nil {
		t.Fatalf("DeleteRegistry() error = %v", err)
	}
	if _, err := dataStore.GetRegistry(ctx, orgOneID, registryOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRegistry() after delete error = %v, want ErrNotFound", err)
	}
}

// TestRegistryStoreDeleteRestrictedIntegration proves that deleting a registry
// still referenced by an app fails: apps.registry_id references registries
// ON DELETE RESTRICT, so the FK violation surfaces as ErrFailedPrecondition
// rather than a bare 500.
func TestRegistryStoreDeleteRestrictedIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()

	secretRef := "k8s-secret://cyclopus/registry-creds#dockerconfigjson"
	if _, err := dataStore.CreateRegistry(ctx, SaveRegistryParams{
		Registry:    Registry{ID: registryOneID, OrgID: orgOneID, Provider: "ghcr", RegistryURL: "ghcr.io/acme", CredentialSecretRef: secretRef},
		Idempotency: idempotencyClaimHelper(t, dataStore, orgOneID, "registry.create", "create-restrict"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000f8", "registry.created", registryOneID),
	}); err != nil {
		t.Fatalf("CreateRegistry() error = %v", err)
	}

	// Point an app at the registry so the FK RESTRICT applies on delete.
	if _, err := pool.Exec(ctx, `UPDATE apps SET registry_id=$1, image_repository='acme/app' WHERE org_id=$2 AND id=$3`,
		registryOneID, orgOneID, appOneID); err != nil {
		t.Fatalf("attach registry to app: %v", err)
	}

	err := dataStore.DeleteRegistry(ctx, orgOneID, registryOneID,
		idempotencyClaimHelper(t, dataStore, orgOneID, "registry.delete", "delete-restrict"),
		testAudit("00000000-0000-0000-0000-0000000000f9", "registry.deleted", registryOneID))
	if !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("DeleteRegistry(referenced by app) error = %v, want ErrFailedPrecondition", err)
	}
	// The registry is still present after the rejected delete.
	if _, err := dataStore.GetRegistry(ctx, orgOneID, registryOneID); err != nil {
		t.Fatalf("GetRegistry() after rejected delete error = %v, want nil", err)
	}
}
