//go:build integration

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/shepard-labs/cyclopus/internal/store"
)

const capEnvironmentID = "00000000-0000-0000-0000-0000000000a6"

func TestEnvironmentTeardownCoversProductionButBranchDeleteStaysPreview(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedCapTenant(t, pool)
	svc := New(dataStore)
	actor := ActorContext{UserID: capUserID, OrgID: capOrgID}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO environments (id,org_id,app_id,branch,kind,namespace,planned_hostname,lifecycle_status)
		VALUES ($1,$2,$3,'main','production','cap-host-production','cap-host.example.test','ready')`,
		capEnvironmentID, capOrgID, capAppID); err != nil {
		t.Fatal(err)
	}

	// Branch-delete against a branch whose only environment is production must
	// stay rejected: branch-delete semantics are preview-only (§4.4).
	if _, err := svc.RequestPreviewBranchDelete(ctx, actor, "cap-production-branch-delete", capAppID, "main"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("production branch-delete error = %v, want ErrNotFound", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT lifecycle_status FROM environments WHERE org_id=$1 AND id=$2`,
		capOrgID, capEnvironmentID).Scan(&status); err != nil || status != "ready" {
		t.Fatalf("production environment lifecycle_status = %q, %v; want untouched ready", status, err)
	}

	// The API teardown path accepts a production environment ID.
	environment, err := svc.RequestEnvironmentTeardown(ctx, actor, "cap-production-teardown", capEnvironmentID, "app_decommission")
	if err != nil {
		t.Fatalf("RequestEnvironmentTeardown() production error = %v", err)
	}
	if environment.ID != capEnvironmentID || environment.LifecycleStatus != "teardown_requested" {
		t.Fatalf("RequestEnvironmentTeardown() = %+v", environment)
	}
	var jobStatus, reason string
	if err := pool.QueryRow(ctx, `SELECT status,reason FROM environment_lifecycle_jobs
		WHERE org_id=$1 AND environment_id=$2 AND kind='teardown'`, capOrgID, capEnvironmentID).Scan(&jobStatus, &reason); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "queued" || reason != "app_decommission" {
		t.Fatalf("teardown job status=%q reason=%q, want queued/app_decommission", jobStatus, reason)
	}
}
