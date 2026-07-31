//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedAuditEvent(t *testing.T, pool *pgxpool.Pool, id, orgID, action, targetType, targetID, after string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO audit_events (id, org_id, action, target_type, target_id, after) VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		id, orgID, action, targetType, targetID, after); err != nil {
		t.Fatal(err)
	}
}

func TestAuditEventsOrgScopingAndOrderingIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	ctx := context.Background()

	orgOneTarget := "00000000-0000-0000-0000-0000000000f1"
	orgTwoTarget := "00000000-0000-0000-0000-0000000000f2"
	// Two events for org one (created_at DESC ordering by insertion order), one for org two.
	seedAuditEvent(t, pool, "00000000-0000-0000-0000-0000000000b1", orgOneID, "registry.created", "registry", orgOneTarget, `{"provider":"ghcr"}`)
	seedAuditEvent(t, pool, "00000000-0000-0000-0000-0000000000b2", orgOneID, "domain.created", "domain", orgOneTarget, `{"hostname":"a.example.test"}`)
	seedAuditEvent(t, pool, "00000000-0000-0000-0000-0000000000b3", orgTwoID, "registry.created", "registry", orgTwoTarget, `{"provider":"gcr"}`)

	// Org scoping: org one sees only its two events, never org two's.
	events, err := dataStore.ListAuditEvents(ctx, orgOneID, AuditEventFilter{}, 100)
	if err != nil || len(events) != 2 {
		t.Fatalf("ListAuditEvents(orgOne) = %d events, %v, want 2", len(events), err)
	}
	for _, event := range events {
		if event.OrgID != orgOneID {
			t.Fatalf("leaked event from org %s", event.OrgID)
		}
	}
	// Most-recent first: the domain event (inserted last) precedes the registry event.
	if events[0].Action != "domain.created" || events[1].Action != "registry.created" {
		t.Fatalf("ordering = [%s, %s], want most-recent first", events[0].Action, events[1].Action)
	}

	// target_type filter narrows within the org.
	filtered, err := dataStore.ListAuditEvents(ctx, orgOneID, AuditEventFilter{TargetType: "registry"}, 100)
	if err != nil || len(filtered) != 1 || filtered[0].Action != "registry.created" {
		t.Fatalf("ListAuditEvents(registry filter) = %+v, %v", filtered, err)
	}

	// target_type + target_id filter.
	byTarget, err := dataStore.ListAuditEvents(ctx, orgOneID, AuditEventFilter{TargetType: "domain", TargetID: orgOneTarget}, 100)
	if err != nil || len(byTarget) != 1 || byTarget[0].Action != "domain.created" {
		t.Fatalf("ListAuditEvents(target filter) = %+v, %v", byTarget, err)
	}

	// Cross-org: org two's target_id supplied under org one returns nothing.
	crossOrg, err := dataStore.ListAuditEvents(ctx, orgOneID, AuditEventFilter{TargetType: "registry", TargetID: orgTwoTarget}, 100)
	if err != nil || len(crossOrg) != 0 {
		t.Fatalf("ListAuditEvents(cross-org target) = %+v, %v, want empty", crossOrg, err)
	}
}
