//go:build integration

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/cyclopus-dev/cyclopus/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	telOrgID       = "00000000-0000-0000-0000-0000000000b1"
	telProjectID   = "00000000-0000-0000-0000-0000000000b2"
	telAppID       = "00000000-0000-0000-0000-0000000000b3"
	telUserID      = "00000000-0000-0000-0000-0000000000b5"
	telEnvID       = "00000000-0000-0000-0000-0000000000b6"
	otherOrgID     = "00000000-0000-0000-0000-0000000000c1"
	otherProjectID = "00000000-0000-0000-0000-0000000000c2"
	otherAppID     = "00000000-0000-0000-0000-0000000000c3"
	otherEnvID     = "00000000-0000-0000-0000-0000000000c6"
)

func seedTelemetryTenants(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO orgs (id, name, slug) VALUES ($1, 'Tel Org', 'tel-org')`, []any{telOrgID}},
		{`INSERT INTO users (id, org_id, email, github_user_id) VALUES ($1, $2, 'tel@example.com', 515151)`, []any{telUserID, telOrgID}},
		{`INSERT INTO projects (id, org_id, name, slug) VALUES ($1, $2, 'Tel Project', 'tel-project')`, []any{telProjectID, telOrgID}},
		{`INSERT INTO apps (id, org_id, project_id, name, slug, hostname_label, ci_workflow_id)
			VALUES ($1, $2, $3, 'Tel App', 'tel-app', 'tel-host', 'deploy.yml')`, []any{telAppID, telOrgID, telProjectID}},
		{`INSERT INTO environments (id, org_id, app_id, branch, kind, namespace, lifecycle_status)
			VALUES ($1, $2, $3, 'main', 'production', 'tel-app-production', 'ready')`, []any{telEnvID, telOrgID, telAppID}},
		// A second org with its own app/environment used for cross-tenant probes.
		{`INSERT INTO orgs (id, name, slug) VALUES ($1, 'Other Org', 'other-org')`, []any{otherOrgID}},
		{`INSERT INTO projects (id, org_id, name, slug) VALUES ($1, $2, 'Other Project', 'other-project')`, []any{otherProjectID, otherOrgID}},
		{`INSERT INTO apps (id, org_id, project_id, name, slug, hostname_label, ci_workflow_id)
			VALUES ($1, $2, $3, 'Other App', 'other-app', 'other-host', 'deploy.yml')`, []any{otherAppID, otherOrgID, otherProjectID}},
		{`INSERT INTO environments (id, org_id, app_id, branch, kind, namespace, lifecycle_status)
			VALUES ($1, $2, $3, 'main', 'production', 'other-app-production', 'ready')`, []any{otherEnvID, otherOrgID, otherAppID}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.query, s.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTelemetryCrossTenantAppReturnsNotFoundBeforeUpstream(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	fake := &recordingTelemetry{}
	svc := New(dataStore, WithTelemetry(fake))
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}

	// The other org's app is not visible to this actor: not-found, and crucially
	// the upstream adapter must never be reached.
	_, err := svc.QueryApplicationLogs(context.Background(), actor, LogQueryInput{AppID: otherAppID})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant app_id error = %v, want not found", err)
	}
	if fake.called {
		t.Fatal("adapter was invoked for a cross-tenant app; scope must be validated first")
	}
}

func TestTelemetryEnvironmentMismatchRejected(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	fake := &recordingTelemetry{}
	svc := New(dataStore, WithTelemetry(fake))
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}

	// An environment belonging to a different app (even the same org would fail;
	// here it is another org's env) must be rejected before any upstream call.
	_, err := svc.QueryApplicationLogs(context.Background(), actor, LogQueryInput{AppID: telAppID, EnvironmentID: otherEnvID})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("env mismatch error = %v, want not found", err)
	}
	if fake.called {
		t.Fatal("adapter was invoked despite an environment/app mismatch")
	}
}

func TestTelemetryValidScopeReachesAdapterWithStampedTenant(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	fake := &recordingTelemetry{}
	svc := New(dataStore, WithTelemetry(fake))
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}

	if _, err := svc.QueryApplicationLogs(context.Background(), actor, LogQueryInput{AppID: telAppID, EnvironmentID: telEnvID}); err != nil {
		t.Fatalf("valid query error = %v", err)
	}
	if !fake.called {
		t.Fatal("adapter should be invoked for an authorized scope")
	}
	if fake.lastScope.OrgID != telOrgID || fake.lastScope.AppID != telAppID || fake.lastScope.EnvironmentID != telEnvID {
		t.Fatalf("tenant scope not stamped from validated IDs: %+v", fake.lastScope)
	}
}

func TestTelemetryLimitValidation(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedTelemetryTenants(t, pool)
	fake := &recordingTelemetry{}
	svc := New(dataStore, WithTelemetry(fake))
	actor := ActorContext{UserID: telUserID, OrgID: telOrgID}

	if _, err := svc.QueryApplicationLogs(context.Background(), actor, LogQueryInput{AppID: telAppID, Limit: maxTelemetryLimit + 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("over-limit error = %v", err)
	}
}
