//go:build integration

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const (
	capOrgID     = "00000000-0000-0000-0000-0000000000a1"
	capProjectID = "00000000-0000-0000-0000-0000000000a2"
	capAppID     = "00000000-0000-0000-0000-0000000000a3"
	capRepoID    = "00000000-0000-0000-0000-0000000000a4"
	capUserID    = "00000000-0000-0000-0000-0000000000a5"
)

func newServiceIntegrationStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("POSTGRES_TEST_URL")
	}
	if databaseURL == "" {
		t.Skip("CYCLOPUS_TEST_DATABASE_URL or POSTGRES_TEST_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	if version < 180000 || version >= 190000 {
		admin.Close(ctx)
		t.Skipf("PostgreSQL 18 required, server_version_num=%d", version)
	}
	schema := fmt.Sprintf("service_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close(context.Background())
	})

	testURL := databaseURL
	if strings.Contains(testURL, "?") {
		testURL += "&"
	} else {
		testURL += "?"
	}
	testURL += "search_path=" + schema
	if err := store.Migrate(ctx, store.MigrationConfig{URL: testURL, LockTimeout: 5 * time.Second, Migrations: store.Migrations}); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(testURL)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool), pool
}

func seedCapTenant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO orgs (id, name, slug) VALUES ($1, 'Cap Org', 'cap-org')`, []any{capOrgID}},
		{`INSERT INTO users (id, org_id, email, github_user_id) VALUES ($1, $2, 'cap@example.com', 424242)`, []any{capUserID, capOrgID}},
		{`INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(4242,$1,4243,'acme','Organization','selected','active',1,1)`, []any{capOrgID}},
		{`INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(4244,$1,4242,'cap-node','acme','cap','acme/cap','private','main','active',1,1)`, []any{capOrgID}},
		{`INSERT INTO git_connections (id,org_id,github_repository_id) VALUES($1,$2,4244)`, []any{capRepoID, capOrgID}},
		{`INSERT INTO projects (id, org_id, name, slug, git_connection_id)
			VALUES ($1, $2, 'Cap Project', 'cap-project', $3)`, []any{capProjectID, capOrgID, capRepoID}},
		{`INSERT INTO apps (id, org_id, project_id, name, slug, hostname_label,image_repository,github_workflow_path,production_branch,previews_enabled,max_preview_environments,deploy_timeout_seconds)
			VALUES ($1, $2, $3, 'Cap App', 'cap-app', 'cap-host','acme/cap','.github/workflows/deploy.yml','main',true,1,60)`, []any{capAppID, capOrgID, capProjectID}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.query, s.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func capCommit(value byte) string { return strings.Repeat(string(value), 40) }

func TestStableEventPreviewCapReached(t *testing.T) {
	dataStore, pool := newServiceIntegrationStore(t)
	seedCapTenant(t, pool)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	svc := New(dataStore, WithLogger(logger))

	actor := ActorContext{UserID: capUserID, OrgID: capOrgID}

	// First preview push fills the cap of 1.
	if _, err := svc.AcceptPreviewPush(context.Background(), actor, "cap-one",
		PreviewPushInput{AppID: capAppID, Branch: "feature/one", CommitSHA: capCommit('1')}); err != nil {
		t.Fatalf("first preview push error = %v", err)
	}
	// Second push on a different branch is rejected at the cap.
	if _, err := svc.AcceptPreviewPush(context.Background(), actor, "cap-two",
		PreviewPushInput{AppID: capAppID, Branch: "feature/two", CommitSHA: capCommit('2')}); err == nil {
		t.Fatal("second preview push at cap did not error")
	}

	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode slog record: %v", err)
		}
		records = append(records, record)
	}

	var event map[string]any
	for _, record := range records {
		if record["event"] == "preview.cap_reached" {
			event = record
		}
	}
	if event == nil {
		t.Fatalf("preview.cap_reached not emitted; records = %+v", records)
	}
	if event["msg"] != "preview.cap_reached" || event["org_id"] != capOrgID || event["app_id"] != capAppID {
		t.Fatalf("preview.cap_reached fields = %+v", event)
	}
}
