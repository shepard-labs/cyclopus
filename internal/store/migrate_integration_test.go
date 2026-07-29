//go:build integration

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgres18Migrations(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var versionText string
	if err := admin.QueryRow(context.Background(), "SHOW server_version_num").Scan(&versionText); err != nil {
		t.Fatal(err)
	}
	version, err := strconv.Atoi(versionText)
	if err != nil {
		t.Fatalf("parse PostgreSQL server_version_num %q: %v", versionText, err)
	}
	if version < 180000 || version >= 190000 {
		t.Fatalf("PostgreSQL server_version_num = %d, want 18.x", version)
	}

	schema := "cyclopus_test_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)

	if err := Migrate(context.Background(), integrationConfig(testURL)); err != nil {
		t.Fatalf("clean migrate: %v", err)
	}
	if err := Migrate(context.Background(), integrationConfig(testURL)); err != nil {
		t.Fatalf("rerun migrate: %v", err)
	}

	db, err := pgx.Connect(context.Background(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())

	t.Run("uuidv7", func(t *testing.T) {
		var id string
		if err := db.QueryRow(context.Background(), "INSERT INTO orgs(name, slug) VALUES ('UUID', 'uuid') RETURNING id::text").Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id[14] != '7' {
			t.Fatalf("UUID %q is not version 7", id)
		}
	})

	t.Run("first user claim concurrency", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := range 2 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn, err := pgx.Connect(context.Background(), testURL)
				if err != nil {
					errs <- err
					return
				}
				defer conn.Close(context.Background())
				_, err = conn.Exec(context.Background(), "SELECT claim_first_user($1, $2, NULL)", fmt.Sprintf("user%d@example.com", i), "hash")
				errs <- err
			}(i)
		}
		wg.Wait()
		close(errs)
		successes := 0
		for err := range errs {
			if err == nil {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("successful claims = %d, want 1", successes)
		}
	})

	ids := seedReleaseParents(t, db)
	t.Run("constraints and unique live release", func(t *testing.T) {
		insert := `INSERT INTO releases(org_id, environment_id, image_ref, kind, config_snapshot, config_hash, status)
			VALUES ($1, $2, $3, 'deploy', $5, repeat('a', 64), $4) RETURNING id`
		snapshot := testReleaseSnapshot("ghcr.io/acme/app@sha256:" + strings.Repeat("b", 64))
		var liveID string
		if err := db.QueryRow(context.Background(), insert, ids.org, ids.environment, "ghcr.io/acme/app@sha256:"+strings.Repeat("b", 64), "live", snapshot).Scan(&liveID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(context.Background(), insert, ids.org, ids.environment, "ghcr.io/acme/app@sha256:"+strings.Repeat("c", 64), "live", snapshot); err == nil {
			t.Fatal("second live release succeeded")
		}
		if _, err := db.Exec(context.Background(), "UPDATE releases SET config_hash = repeat('d', 64) WHERE id = $1", liveID); err == nil {
			t.Fatal("immutable release update succeeded")
		}
		if _, err := db.Exec(context.Background(), "INSERT INTO aliases(org_id, environment_id, hostname, release_id) VALUES ($1, $2, 'one.example.com', $3)", ids.org, ids.environment, liveID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(context.Background(), "INSERT INTO aliases(org_id, environment_id, hostname, release_id) VALUES ($1, $2, 'two.example.com', $3)", ids.org, ids.environment, liveID); err == nil {
			t.Fatal("second environment alias succeeded")
		}
	})

	t.Run("cross organization links are forbidden", func(t *testing.T) {
		other := seedTenantGraph(t, db, "other", "feature/other", "other-preview", "other-preview-ns")
		if _, err := db.Exec(context.Background(), `
			UPDATE projects SET git_connection_id = $1 WHERE id = $2`, other.repository, ids.project); err == nil {
			t.Fatal("cross-org project git connection succeeded")
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO orchestration_jobs (org_id, repository_id, workflow_id, git_ref, environment_id, commit_sha, idempotency_key)
			VALUES ($1, $2, 'deploy.yml', 'refs/heads/main', $3, repeat('a', 40), 'cross-org')`,
			ids.org, other.repository, ids.environment); err == nil {
			t.Fatal("cross-org orchestration job succeeded")
		}
	})

	t.Run("preview branch can be recreated after teardown", func(t *testing.T) {
		preview := seedTenantGraph(t, db, "preview", "feature/recreate", "recreate", "recreate-ns")
		if _, err := db.Exec(context.Background(), "UPDATE environments SET torn_down_at = now() WHERE id = $1", preview.environment); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
			VALUES ($1, $2, 'feature/recreate', 'preview', 'recreate', 'recreate-ns', 'recreate.example.test')`, preview.org, preview.app); err == nil {
			t.Fatal("reused preview slug and namespace succeeded")
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO environments (org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname)
			VALUES ($1, $2, 'feature/recreate', 'preview', 'recreate-fresh', 'recreate-fresh-ns', 'recreate-fresh.example.test')`, preview.org, preview.app); err != nil {
			t.Fatalf("recreate torn-down preview: %v", err)
		}
	})

	t.Run("secret references are canonical", func(t *testing.T) {
		if _, err := db.Exec(context.Background(), `
			INSERT INTO env_vars (org_id, environment_id, key, secret_ref)
			VALUES ($1, $2, 'DATABASE_URL', 'k8s-secret://cyclopus/runtime-secrets#database-url')`, ids.org, ids.environment); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO env_vars (org_id, environment_id, key, secret_ref)
			VALUES ($1, $2, 'BAD_SECRET', 'runtime-secrets/database-url')`, ids.org, ids.environment); err == nil {
			t.Fatal("non-canonical secret reference succeeded")
		}
	})

	t.Run("tenant uniqueness and defaults", func(t *testing.T) {
		other := seedTenantGraph(t, db, "unique", "feature/unique", "unique-preview", "unique-preview-ns")
		insertIdempotency := `INSERT INTO api_idempotency_records
			(org_id, scope, idempotency_key, request_hash, lease_owner, lease_expires_at, expires_at)
			VALUES ($1, 'deploy', 'same-key', repeat('a', 64), 'worker', now() + interval '1 minute', now() + interval '1 hour')`
		if _, err := db.Exec(context.Background(), insertIdempotency, ids.org); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(context.Background(), insertIdempotency, other.org); err != nil {
			t.Fatalf("same idempotency key in another tenant failed: %v", err)
		}
		if _, err := db.Exec(context.Background(), insertIdempotency, ids.org); err == nil {
			t.Fatal("duplicate tenant idempotency key succeeded")
		}

		var status string
		if err := db.QueryRow(context.Background(), `
			INSERT INTO orchestration_jobs (org_id, repository_id, workflow_id, git_ref, environment_id, commit_sha, idempotency_key)
			VALUES ($1, $2, 'deploy.yml', 'refs/heads/main', $3, repeat('b', 40), 'same-key')
			RETURNING status`, ids.org, ids.repository, ids.environment).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "queued" {
			t.Fatalf("orchestration status = %q, want queued", status)
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO orchestration_jobs (org_id, repository_id, workflow_id, git_ref, environment_id, commit_sha, idempotency_key)
			VALUES ($1, $2, 'deploy.yml', 'refs/heads/main', $3, repeat('c', 40), 'same-key')`,
			other.org, other.repository, other.environment); err != nil {
			t.Fatalf("same orchestration key in another tenant failed: %v", err)
		}
	})

	t.Run("overlapping reconcile scopes are rejected", func(t *testing.T) {
		if _, err := db.Exec(context.Background(), `
			INSERT INTO reconciliation_runs (org_id, scope_kind, scope_id, scope_app_id, scope_environment_id)
			VALUES ($1, 'environment', $2, $3, $2)`, ids.org, ids.environment, ids.app); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO reconciliation_runs (org_id, scope_kind, scope_id, scope_app_id)
			VALUES ($1, 'app', $2, $2)`, ids.org, ids.app); err == nil {
			t.Fatal("overlapping app reconcile succeeded")
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO reconciliation_runs (org_id, scope_kind, scope_id)
			VALUES ($1, 'org', $1)`, ids.org); err == nil {
			t.Fatal("overlapping org reconcile succeeded")
		}
	})

	t.Run("attacker controlled inputs are bounded", func(t *testing.T) {
		if _, err := db.Exec(context.Background(), `
			INSERT INTO webhooks (org_id, target_url, events, secret_ref)
			VALUES ($1, 'https://example.com/hook', array_fill('event', ARRAY[65]), 'k8s-secret://cyclopus/webhook#token')`, ids.org); err == nil {
			t.Fatal("oversized webhook event list succeeded")
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO api_idempotency_records
			(org_id, scope, idempotency_key, request_hash, lease_owner, lease_expires_at, expires_at)
			VALUES ($1, 'deploy', repeat('k', 256), repeat('a', 64), 'worker', now() + interval '1 minute', now() + interval '1 hour')`, ids.org); err == nil {
			t.Fatal("oversized indexed idempotency key succeeded")
		}
	})

	t.Run("audit and release history require tombstones", func(t *testing.T) {
		var auditID string
		if err := db.QueryRow(context.Background(), `
			INSERT INTO audit_events (org_id, action, target_type, target_id)
			VALUES ($1, 'release.created', 'release', $2) RETURNING id`, ids.org, ids.environment).Scan(&auditID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(context.Background(), "UPDATE audit_events SET action = 'changed' WHERE id = $1", auditID); err == nil {
			t.Fatal("audit update succeeded")
		}
		if _, err := db.Exec(context.Background(), "DELETE FROM audit_events WHERE id = $1", auditID); err == nil {
			t.Fatal("audit delete succeeded")
		}
		if _, err := db.Exec(context.Background(), "DELETE FROM orgs WHERE id = $1", ids.org); err == nil {
			t.Fatal("org deletion cascaded audit or release history")
		}
		if _, err := db.Exec(context.Background(), "UPDATE environments SET torn_down_at = now() WHERE id = $1", ids.environment); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(context.Background(), "UPDATE projects SET external_credentials_revoked_at = now(), deleted_at = now() WHERE id = $1", ids.project); err != nil {
			t.Fatalf("project tombstone failed: %v", err)
		}
		if _, err := db.Exec(context.Background(), "UPDATE apps SET external_credentials_revoked_at = now(), deleted_at = now() WHERE id = $1", ids.app); err != nil {
			t.Fatalf("app tombstone failed: %v", err)
		}
	})
}

func TestMigrationAdvisoryLock(t *testing.T) {
	url := integrationURL(t)
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)

	cfg := integrationConfig(url)
	cfg.LockTimeout = 250 * time.Millisecond
	err = Migrate(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("Migrate() error = %v, want advisory lock timeout", err)
	}
}

type releaseParents struct {
	org         string
	project     string
	app         string
	repository  string
	environment string
}

func seedReleaseParents(t *testing.T, db *pgx.Conn) releaseParents {
	t.Helper()
	return seedTenantGraph(t, db, "release", "main", "", "release-app-production")
}

func seedTenantGraph(t *testing.T, db *pgx.Conn, prefix, branch, previewSlug, namespace string) releaseParents {
	t.Helper()
	var org, project, app, repository, environment string
	kind := "production"
	var preview any
	if previewSlug != "" {
		kind = "preview"
		preview = previewSlug
	}
	queries := []struct {
		query string
		args  []any
		dest  *string
	}{
		{"INSERT INTO orgs(name, slug) VALUES ($1, $2) RETURNING id", []any{prefix + " Org", prefix + "-org"}, &org},
		{"INSERT INTO git_connections(org_id, installation_id, repo_full_name, webhook_secret_ref) VALUES ($1, $2, $3, 'k8s-secret://cyclopus/github#webhook') RETURNING id", []any{&org, int64(len(prefix) + 100), "acme/" + prefix}, &repository},
		{"INSERT INTO projects(org_id, name, slug, git_connection_id) VALUES ($1, $2, $3, $4) RETURNING id", []any{&org, prefix + " Project", prefix + "-project", &repository}, &project},
		{"INSERT INTO apps(org_id, project_id, name, slug, hostname_label) VALUES ($1, $2, $3, $4, $5) RETURNING id", []any{&org, &project, prefix + " App", prefix + "-app", prefix + "-app"}, &app},
		{"INSERT INTO environments(org_id, app_id, branch, kind, preview_slug, namespace, planned_hostname) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id", []any{&org, &app, branch, kind, preview, namespace, prefix + ".example.test"}, &environment},
	}
	for _, item := range queries {
		args := make([]any, len(item.args))
		for i, arg := range item.args {
			switch value := arg.(type) {
			case *string:
				args[i] = *value
			default:
				args[i] = value
			}
		}
		if err := db.QueryRow(context.Background(), item.query, args...).Scan(item.dest); err != nil {
			t.Fatal(err)
		}
	}
	return releaseParents{org: org, project: project, app: app, repository: repository, environment: environment}
}

func integrationURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("POSTGRES_TEST_URL is not set")
	}
	return url
}

func integrationConfig(url string) MigrationConfig {
	return MigrationConfig{URL: url, LockTimeout: 5 * time.Second, Migrations: Migrations}
}
