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

	listed, err := dataStore.ListRegistries(ctx, orgOneID)
	if err != nil || len(listed) != 0 {
		t.Fatalf("ListRegistries() = %+v, %v, want empty", listed, err)
	}
	// Org scoping on List: org two sees none of org one's registries.
	if other, err := dataStore.ListRegistries(ctx, orgTwoID); err != nil || len(other) != 0 {
		t.Fatalf("ListRegistries(other org) = %+v, %v, want empty", other, err)
	}

	if _, err := dataStore.CreateRegistry(ctx, SaveRegistryParams{}); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("CreateRegistry() error = %v, want ErrFailedPrecondition", err)
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

	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host,active_generation,
		active_credential_secret_ref,state,reconciliation_generation) VALUES($1,$2,'ghcr','ghcr.io',1,$3,'ready',1)`,
		registryOneID, orgOneID, "k8s-secret://cyclopus/registry-creds#dockerconfigjson"); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// Point an app at the registry so the FK RESTRICT applies on delete.
	if _, err := pool.Exec(ctx, `UPDATE apps SET registry_id=$1, image_repository='ghcr.io/acme/app',image_visibility='private' WHERE org_id=$2 AND id=$3`,
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
