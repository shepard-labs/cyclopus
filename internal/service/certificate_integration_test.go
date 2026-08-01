//go:build integration

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/store"
)

// These integration tests reuse the two-tenant fixture from the telemetry
// integration suite (seedTelemetryTenants): telOrg owns telEnv (namespace
// tel-app-production); otherOrg owns otherEnv. They prove cert reads are
// org-scoped: a caller can only read certificates for their own environments'
// managed namespaces, and the reader is only reached with that namespace.

func TestListCertificatesUnavailableWithoutReader(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	svc := New(dataStore) // no CertReader wired
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}
	if _, err := svc.ListCertificates(context.Background(), actor, telEnvID); !errors.Is(err, ErrCertReaderUnavailable) {
		t.Fatalf("ListCertificates without reader = %v, want ErrCertReaderUnavailable", err)
	}
}

func TestListCertificatesReturnsReaderStatusesForOwnEnvironment(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	fake := &fakeCertReader{statuses: []CertificateStatus{{Hostname: "app.example.test", SecretName: "app-tls", Ready: true, Reason: "Certificate is up to date", Issuer: "letsencrypt"}}}
	svc := New(dataStore, WithCertReader(fake))
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}

	statuses, err := svc.ListCertificates(context.Background(), actor, telEnvID)
	if err != nil {
		t.Fatalf("ListCertificates error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].SecretName != "app-tls" || !statuses[0].Ready {
		t.Fatalf("statuses = %+v, want the canned Ready certificate", statuses)
	}
	// The reader must be reached only with the environment's own managed namespace.
	if !fake.called || fake.lastScope.Namespace != "tel-app-production" || fake.lastScope.EnvironmentID != telEnvID {
		t.Fatalf("reader scope = %+v, want namespace tel-app-production for telEnv", fake.lastScope)
	}
}

func TestListCertificatesCrossOrgEnvironmentNotFoundBeforeReader(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	fake := &fakeCertReader{}
	svc := New(dataStore, WithCertReader(fake))
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}

	// otherEnv belongs to a different org: not-found, and the reader must never
	// be reached (no cross-tenant namespace can leak into a cert read).
	if _, err := svc.ListCertificates(context.Background(), actor, otherEnvID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-org environment error = %v, want not found", err)
	}
	if fake.called {
		t.Fatal("cert reader was invoked for a cross-org environment; org-scoping must precede the read")
	}
}
