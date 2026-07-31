//go:build integration

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/cyclopus-dev/cyclopus/internal/store"
)

// TestDomainServiceVerifyFlowIntegration drives CreateDomain then VerifyDomain
// against a real store with a fake resolver, covering the apex dns_txt success
// path, the failure path (verified_at stays null), and org-scoping.
func TestDomainServiceVerifyFlowIntegration(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedCapTenant(t, pool)
	ctx := context.Background()

	resolver := &mutableResolver{txt: map[string][]string{}}
	svc := New(dataStore, WithDNSResolver(resolver), WithIngressTarget("ingress.platform.test"))
	actor := ActorContext{UserID: capUserID, OrgID: capOrgID}

	created, instructions, err := svc.CreateDomain(ctx, actor, "create-apex", DomainInput{AppID: capAppID, Hostname: "customer.com"})
	if err != nil || created.VerificationMethod != verificationMethodDNSTXT || created.VerificationToken == nil {
		t.Fatalf("CreateDomain(apex) = %+v, %v", created, err)
	}
	if instructions.RecordType != "TXT" || instructions.RecordName != "_cyclopus-challenge.customer.com" {
		t.Fatalf("apex instructions = %+v", instructions)
	}

	// Verification fails before the customer publishes the TXT record.
	if _, _, err := svc.VerifyDomain(ctx, actor, "verify-fail", created.ID); !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("VerifyDomain(before TXT) error = %v, want ErrFailedPrecondition", err)
	}
	if fetched, _, _ := svc.GetDomain(ctx, actor, created.ID); fetched.VerifiedAt != nil {
		t.Fatalf("verified_at set after failed verification: %v", fetched.VerifiedAt)
	}

	// Publish the challenge and verify succeeds, setting verified_at.
	resolver.txt["_cyclopus-challenge.customer.com"] = []string{*created.VerificationToken}
	verified, _, err := svc.VerifyDomain(ctx, actor, "verify-ok", created.ID)
	if err != nil || verified.VerifiedAt == nil {
		t.Fatalf("VerifyDomain(after TXT) = %+v, %v", verified, err)
	}

	// A different org cannot verify this domain.
	otherActor := ActorContext{UserID: capUserID, OrgID: "00000000-0000-0000-0000-0000000000ff"}
	if _, _, err := svc.VerifyDomain(ctx, otherActor, "verify-cross-org", created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("VerifyDomain(cross-org) error = %v, want ErrNotFound", err)
	}
}

// TestDomainServiceCreateSubdomainIntegration covers the subdomain cname branch:
// method is cname, token is null, and CNAME verification honors the ingress
// target.
func TestDomainServiceCreateSubdomainIntegration(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedCapTenant(t, pool)
	ctx := context.Background()

	resolver := &mutableResolver{cname: map[string]string{"app.customer.com": "ingress.platform.test."}}
	svc := New(dataStore, WithDNSResolver(resolver), WithIngressTarget("ingress.platform.test"))
	actor := ActorContext{UserID: capUserID, OrgID: capOrgID}

	created, _, err := svc.CreateDomain(ctx, actor, "create-sub", DomainInput{AppID: capAppID, Hostname: "app.customer.com"})
	if err != nil || created.VerificationMethod != verificationMethodCNAME || created.VerificationToken != nil {
		t.Fatalf("CreateDomain(subdomain) = %+v, %v", created, err)
	}
	verified, _, err := svc.VerifyDomain(ctx, actor, "verify-sub", created.ID)
	if err != nil || verified.VerifiedAt == nil {
		t.Fatalf("VerifyDomain(cname) = %+v, %v", verified, err)
	}
}

type mutableResolver struct {
	txt   map[string][]string
	cname map[string]string
}

func (m *mutableResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if records, ok := m.txt[name]; ok {
		return records, nil
	}
	return nil, errors.New("no such host")
}

func (m *mutableResolver) LookupCNAME(_ context.Context, name string) (string, error) {
	if target, ok := m.cname[name]; ok {
		return target, nil
	}
	return "", errors.New("no such host")
}
