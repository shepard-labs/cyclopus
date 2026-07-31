package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

const (
	domainOneID = "00000000-0000-0000-0000-0000000000d1"
	domainTwoID = "00000000-0000-0000-0000-0000000000d2"
)

func TestDomainStoreCRUDIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()

	claim := func(scope, key string) CompleteIdempotencyParams {
		t.Helper()
		hash := requestHash(key)
		if _, err := dataStore.ClaimIdempotency(ctx, IdempotencyClaimParams{
			OrgID: orgOneID, Scope: scope, Key: key, RequestHash: hash,
			Owner: "owner", LeaseDuration: time.Minute, Retention: time.Hour,
		}); err != nil {
			t.Fatal(err)
		}
		return CompleteIdempotencyParams{OrgID: orgOneID, Scope: scope, Key: key, RequestHash: hash, Owner: "owner",
			Status: "completed", StatusCode: 200, Body: json.RawMessage(`{"ok":true}`)}
	}

	token := "challenge-token-value"
	created, err := dataStore.CreateDomain(ctx, SaveDomainParams{
		Domain:      Domain{ID: domainOneID, OrgID: orgOneID, AppID: appOneID, Hostname: "customer.com", VerificationMethod: "dns_txt", VerificationToken: &token},
		Idempotency: claim("domain.create", "create-1"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000e1", "domain.created", domainOneID),
	})
	if err != nil || created.ID != domainOneID || created.VerifiedAt != nil || created.VerificationToken == nil || *created.VerificationToken != token {
		t.Fatalf("CreateDomain(dns_txt) = %+v, %v", created, err)
	}

	// cname domain must carry a nil token; the DB CHECK enforces the invariant.
	cnameCreated, err := dataStore.CreateDomain(ctx, SaveDomainParams{
		Domain:      Domain{ID: domainTwoID, OrgID: orgOneID, AppID: appOneID, Hostname: "app.customer.com", VerificationMethod: "cname"},
		Idempotency: claim("domain.create", "create-2"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000e2", "domain.created", domainTwoID),
	})
	if err != nil || cnameCreated.VerificationToken != nil {
		t.Fatalf("CreateDomain(cname) = %+v, %v", cnameCreated, err)
	}

	// The method/token invariant is rejected by the DB CHECK (dns_txt requires a token).
	if _, err := dataStore.CreateDomain(ctx, SaveDomainParams{
		Domain:      Domain{ID: "00000000-0000-0000-0000-0000000000d3", OrgID: orgOneID, AppID: appOneID, Hostname: "bad.customer.com", VerificationMethod: "dns_txt"},
		Idempotency: claim("domain.create", "create-3"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000e3", "domain.created", "00000000-0000-0000-0000-0000000000d3"),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CreateDomain(dns_txt without token) error = %v, want ErrInvalidArgument", err)
	}

	fetched, err := dataStore.GetDomain(ctx, orgOneID, domainOneID)
	if err != nil || fetched.Hostname != "customer.com" || fetched.VerificationMethod != "dns_txt" {
		t.Fatalf("GetDomain() = %+v, %v", fetched, err)
	}
	// Org scoping: another org cannot read this domain.
	if _, err := dataStore.GetDomain(ctx, orgTwoID, domainOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDomain(other org) error = %v, want ErrNotFound", err)
	}

	listed, err := dataStore.ListDomains(ctx, orgOneID, appOneID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListDomains() = %+v, %v", listed, err)
	}

	verified, err := dataStore.VerifyDomain(ctx, orgOneID, domainOneID, claim("domain.verify", "verify-1"),
		testAudit("00000000-0000-0000-0000-0000000000e4", "domain.verified", domainOneID))
	if err != nil || verified.VerifiedAt == nil {
		t.Fatalf("VerifyDomain() = %+v, %v", verified, err)
	}
	firstVerifiedAt := *verified.VerifiedAt
	// Re-verifying keeps the original timestamp (idempotent).
	reVerified, err := dataStore.VerifyDomain(ctx, orgOneID, domainOneID, claim("domain.verify", "verify-2"),
		testAudit("00000000-0000-0000-0000-0000000000e5", "domain.verified", domainOneID))
	if err != nil || reVerified.VerifiedAt == nil || !reVerified.VerifiedAt.Equal(firstVerifiedAt) {
		t.Fatalf("VerifyDomain(re-verify) = %+v, %v, want unchanged verified_at %s", reVerified, err, firstVerifiedAt)
	}

	if err := dataStore.DeleteDomain(ctx, orgOneID, domainOneID, claim("domain.delete", "delete-1"),
		testAudit("00000000-0000-0000-0000-0000000000e6", "domain.deleted", domainOneID)); err != nil {
		t.Fatalf("DeleteDomain() error = %v", err)
	}
	if _, err := dataStore.GetDomain(ctx, orgOneID, domainOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDomain() after delete error = %v, want ErrNotFound", err)
	}
}

func TestDomainStoreCreateMissingAppIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()
	hash := requestHash("create-missing")
	if _, err := dataStore.ClaimIdempotency(ctx, IdempotencyClaimParams{OrgID: orgOneID, Scope: "domain.create",
		Key: "create-missing", RequestHash: hash, Owner: "owner", LeaseDuration: time.Minute, Retention: time.Hour}); err != nil {
		t.Fatal(err)
	}
	idem := CompleteIdempotencyParams{OrgID: orgOneID, Scope: "domain.create", Key: "create-missing", RequestHash: hash,
		Owner: "owner", Status: "completed", StatusCode: 201, Body: json.RawMessage(`{}`)}
	_, err := dataStore.CreateDomain(ctx, SaveDomainParams{
		Domain:      Domain{ID: domainOneID, OrgID: orgOneID, AppID: "00000000-0000-0000-0000-00000000ffff", Hostname: "orphan.com", VerificationMethod: "dns_txt", VerificationToken: stringPointer("t")},
		Idempotency: idem,
		Audit:       testAudit("00000000-0000-0000-0000-0000000000e7", "domain.created", domainOneID),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateDomain(missing app) error = %v, want ErrNotFound", err)
	}
}
