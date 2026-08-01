//go:build integration

package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/store"
)

// fakeCustomDomainProvisioner records the provision/deprovision calls so tests
// can assert VerifyDomain/DeleteDomain drive routing with the correct,
// service-resolved request. It optionally returns a canned error to exercise the
// provision-failure-does-not-roll-back-verified_at path.
type fakeCustomDomainProvisioner struct {
	mu               sync.Mutex
	provisionErr     error
	provisioned      []CustomDomainRequest
	deprovisioned    []CustomDomainRequest
	provisionCalls   int
	deprovisionCalls int
}

func (f *fakeCustomDomainProvisioner) ProvisionCustomDomain(_ context.Context, req CustomDomainRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisionCalls++
	f.provisioned = append(f.provisioned, req)
	return f.provisionErr
}

func (f *fakeCustomDomainProvisioner) DeprovisionCustomDomain(_ context.Context, req CustomDomainRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deprovisionCalls++
	f.deprovisioned = append(f.deprovisioned, req)
	return nil
}

// createVerifiableDomain inserts a cname domain whose challenge already resolves
// to the ingress target so VerifyDomain's DNS check passes.
func createVerifiableDomain(t *testing.T, svc *Service, actor ActorContext, hostname string) store.Domain {
	t.Helper()
	domain, _, err := svc.CreateDomain(context.Background(), actor, "create-"+hostname, DomainInput{AppID: telAppID, Hostname: hostname, VerificationMethod: verificationMethodCNAME})
	if err != nil {
		t.Fatalf("CreateDomain(%q) error = %v", hostname, err)
	}
	return domain
}

func customDomainService(dataStore *store.Store, provisioner CustomDomainProvisioner, hostname string) *Service {
	resolver := fakeDNSResolver{cname: map[string]string{hostname: ingressTarget + "."}}
	opts := []Option{WithDNSResolver(resolver), WithIngressTarget(ingressTarget), WithCertManagerClusterIssuer("letsencrypt")}
	if provisioner != nil {
		opts = append(opts, WithCustomDomainProvisioner(provisioner))
	}
	return New(dataStore, opts...)
}

func TestVerifyDomainProvisionsCustomDomainRouting(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}
	hostname := "app.customer.test"
	fake := &fakeCustomDomainProvisioner{}
	svc := customDomainService(dataStore, fake, hostname)

	domain := createVerifiableDomain(t, svc, actor, hostname)
	verified, _, err := svc.VerifyDomain(context.Background(), actor, "verify-1", domain.ID)
	if err != nil {
		t.Fatalf("VerifyDomain error = %v", err)
	}
	if verified.VerifiedAt == nil {
		t.Fatal("verified_at must be set on success")
	}
	if fake.provisionCalls != 1 {
		t.Fatalf("provision calls = %d, want 1", fake.provisionCalls)
	}
	req := fake.provisioned[0]
	if req.Namespace != "tel-app-production" || req.EnvironmentID != telEnvID {
		t.Fatalf("request namespace/env = %q/%q, want tel-app-production/%s", req.Namespace, req.EnvironmentID, telEnvID)
	}
	if req.Hostname != hostname || req.ServiceName != "tel-app" || req.ServicePort != 80 {
		t.Fatalf("request routing = %+v, want host %q -> tel-app:80", req, hostname)
	}
	if req.TLSSecretName != "app-customer-test-tls" || req.ClusterIssuer != "letsencrypt" || req.DomainID != domain.ID {
		t.Fatalf("request tls/issuer = %+v", req)
	}
}

func TestVerifyDomainSucceedsWithoutProvisioner(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}
	hostname := "noprov.customer.test"
	svc := customDomainService(dataStore, nil, hostname) // no provisioner wired

	domain := createVerifiableDomain(t, svc, actor, hostname)
	verified, _, err := svc.VerifyDomain(context.Background(), actor, "verify-1", domain.ID)
	if err != nil {
		t.Fatalf("VerifyDomain without provisioner error = %v", err)
	}
	if verified.VerifiedAt == nil {
		t.Fatal("verified_at must be set even without a provisioner")
	}
}

func TestVerifyDomainProvisionErrorKeepsVerifiedAt(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}
	hostname := "provfail.customer.test"
	fake := &fakeCustomDomainProvisioner{provisionErr: errors.New("cluster unreachable")}
	svc := customDomainService(dataStore, fake, hostname)

	domain := createVerifiableDomain(t, svc, actor, hostname)
	verified, _, err := svc.VerifyDomain(context.Background(), actor, "verify-1", domain.ID)
	if err == nil {
		t.Fatal("VerifyDomain should surface the provision error")
	}
	// The error is surfaced BUT verified_at is committed (control was proven).
	if verified.VerifiedAt == nil {
		t.Fatal("verified_at must remain committed when only provisioning fails")
	}
	// Re-loading confirms verified_at persisted (no rollback).
	reloaded, err := dataStore.GetDomain(context.Background(), telOrgID, domain.ID)
	if err != nil {
		t.Fatalf("GetDomain error = %v", err)
	}
	if reloaded.VerifiedAt == nil {
		t.Fatal("persisted verified_at must not be rolled back on provision failure")
	}
}

func TestDeleteDomainDeprovisionsCustomDomainRouting(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}
	hostname := "delete.customer.test"
	fake := &fakeCustomDomainProvisioner{}
	svc := customDomainService(dataStore, fake, hostname)

	domain := createVerifiableDomain(t, svc, actor, hostname)
	if err := svc.DeleteDomain(context.Background(), actor, "delete-1", domain.ID); err != nil {
		t.Fatalf("DeleteDomain error = %v", err)
	}
	if fake.deprovisionCalls != 1 {
		t.Fatalf("deprovision calls = %d, want 1", fake.deprovisionCalls)
	}
	if fake.deprovisioned[0].DomainID != domain.ID || fake.deprovisioned[0].Namespace != "tel-app-production" {
		t.Fatalf("deprovision request = %+v", fake.deprovisioned[0])
	}
}
