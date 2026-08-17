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

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
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

	t.Run("greenfield reaches migration 000009 and preserves epoch", func(t *testing.T) {
		var version uint
		var dirty bool
		if err := db.QueryRow(context.Background(), "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
			t.Fatal(err)
		}
		if version != 9 || dirty {
			t.Fatalf("schema migration state = version:%d dirty:%t, want version:9 dirty:false", version, dirty)
		}
		var nullable string
		if err := db.QueryRow(context.Background(), `SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='github_setup_sessions' AND column_name='state_generation'`).Scan(&nullable); err != nil {
			t.Fatal(err)
		}
		if nullable != "YES" {
			t.Fatalf("state_generation is_nullable = %q, want YES for terminal legacy rows", nullable)
		}
		epochDB, err := sql.Open("pgx", testURL)
		if err != nil {
			t.Fatal(err)
		}
		defer epochDB.Close()
		if err := ValidateSchemaEpoch(context.Background(), epochDB); err != nil {
			t.Fatalf("ValidateSchemaEpoch() after 000009: %v", err)
		}
	})

	t.Run("registry identity constraints", func(t *testing.T) {
		var firstOrg, secondOrg string
		if err := db.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Registry One','registry-one') RETURNING id::text`).Scan(&firstOrg); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Registry Two','registry-two') RETURNING id::text`).Scan(&secondOrg); err != nil {
			t.Fatal(err)
		}
		insert := `INSERT INTO registries(org_id,provider,registry_host) VALUES($1,$2,$3)`
		for _, values := range [][]any{{firstOrg, "ghcr", "ghcr.io"}, {secondOrg, "ghcr", "ghcr.io"}, {firstOrg, "gcr", "gcr.io"}} {
			if _, err := db.Exec(t.Context(), insert, values...); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(t.Context(), insert, firstOrg, "ghcr", "ghcr.io"); err == nil {
			t.Fatal("duplicate canonical registry succeeded")
		}
		for _, identity := range [][2]string{{"other", "registry.example.test"}, {"ghcr", "gcr.io"}, {"GHCR", "ghcr.io"}, {"ghcr", "https://ghcr.io"}} {
			if _, err := db.Exec(t.Context(), insert, secondOrg, identity[0], identity[1]); err == nil {
				t.Fatalf("unsupported/noncanonical identity %q/%q succeeded", identity[0], identity[1])
			}
		}
	})

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

	t.Run("keda scaling constraints", func(t *testing.T) {
		insertApp := func(scalingMode string, minReplicas int, schedules string, pendingRequests int) error {
			_, err := db.Exec(context.Background(), `
				INSERT INTO apps (org_id, project_id, name, slug, hostname_label, image_repository, github_workflow_path, production_branch, scaling_mode, min_replicas, keda_cron_schedules, keda_http_target_pending_requests)
				VALUES ($1, $2, 'KEDA App', $3, $3, 'ghcr.io/acme/keda', '.github/workflows/deploy.yml', 'main', $4, $5, $6::jsonb, $7)`,
				ids.org, ids.project, "keda-"+strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", ""), scalingMode, minReplicas, schedules, pendingRequests)
			return err
		}
		validSchedules := `[{"timezone":"Europe/Berlin","start":"0 8 * * 1-5","end":"0 18 * * 1-5","desired_replicas":3}]`
		if err := insertApp("keda_cron", 0, validSchedules, 100); err != nil {
			t.Fatalf("valid keda_cron app rejected: %v", err)
		}
		if err := insertApp("keda_http", 0, `[]`, 100); err != nil {
			t.Fatalf("valid keda_http scale-to-zero app rejected: %v", err)
		}
		if err := insertApp("keda_http", 0, validSchedules, 100); err == nil {
			t.Fatal("keda_http carrying cron schedules succeeded")
		}
		if err := insertApp("hpa", 0, `[]`, 100); err == nil {
			t.Fatal("hpa scale-to-zero succeeded")
		}
		if err := insertApp("keda_cron", 0, `[]`, 100); err == nil {
			t.Fatal("keda_cron without schedules succeeded")
		}
		if err := insertApp("keda_cron", 0, `[{"timezone":"Europe/Berlin","start":"0 8 * * * *","end":"0 18 * * 1-5","desired_replicas":3}]`, 100); err == nil {
			t.Fatal("6-field cron start succeeded")
		}
		if err := insertApp("keda_cron", 0, `[{"timezone":"","start":"0 8 * * 1-5","end":"0 18 * * 1-5","desired_replicas":3}]`, 100); err == nil {
			t.Fatal("empty timezone succeeded")
		}
		if err := insertApp("keda_cron", 0, `[{"timezone":"`+strings.Repeat("a", 65)+`","start":"0 8 * * 1-5","end":"0 18 * * 1-5","desired_replicas":3}]`, 100); err == nil {
			t.Fatal("oversized timezone succeeded")
		}
		if err := insertApp("keda_cron", 0, `[{"timezone":"UTC","start":"0 8 * * 1-5","end":"0 18 * * 1-5","desired_replicas":-1}]`, 100); err == nil {
			t.Fatal("negative desired_replicas succeeded")
		}
		if err := insertApp("keda_cron", 0, `[{"timezone":"UTC","start":"0 8 * * 1-5","end":"0 18 * * 1-5","desired_replicas":1.5}]`, 100); err == nil {
			t.Fatal("fractional desired_replicas succeeded")
		}
		if err := insertApp("keda_cron", 0, `[{"timezone":"UTC","start":"0 8 * * 1-5","end":"0 18 * * 1-5","desired_replicas":3,"extra":true}]`, 100); err == nil {
			t.Fatal("unexpected schedule key succeeded")
		}
		if err := insertApp("keda_cron", 0, `[{"timezone":"UTC","start":"0 8 * * 1-5","end":"0 18 * * 1-5"}]`, 100); err == nil {
			t.Fatal("missing desired_replicas succeeded")
		}
		if err := insertApp("keda_cron", 0, `{"timezone":"UTC"}`, 100); err == nil {
			t.Fatal("non-array schedules succeeded")
		}
		oversized := strings.Repeat(`{"timezone":"UTC","start":"0 8 * * 1-5","end":"0 18 * * 1-5","desired_replicas":3},`, 16)
		if err := insertApp("keda_cron", 0, `[`+oversized+`{"timezone":"UTC","start":"0 8 * * 1-5","end":"0 18 * * 1-5","desired_replicas":3}]`, 100); err == nil {
			t.Fatal("17 cron schedules succeeded")
		}
		if err := insertApp("keda_http", 0, `[]`, 0); err == nil {
			t.Fatal("zero pending-requests target succeeded")
		}
		var defaultTarget int
		if err := db.QueryRow(context.Background(), `
			INSERT INTO apps (org_id, project_id, name, slug, hostname_label, image_repository, github_workflow_path, production_branch, scaling_mode, min_replicas)
			VALUES ($1, $2, 'Default KEDA Target', $3, $3, 'ghcr.io/acme/keda-default', '.github/workflows/deploy.yml', 'main', 'keda_http', 0)
			RETURNING keda_http_target_pending_requests`, ids.org, ids.project, "keda-default-"+strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")).Scan(&defaultTarget); err != nil {
			t.Fatalf("database KEDA HTTP default rejected: %v", err)
		}
		if defaultTarget != 100 {
			t.Fatalf("database KEDA HTTP default = %d, want 100", defaultTarget)
		}
		if err := insertApp("keda_http", 0, `[]`, -1); err == nil {
			t.Fatal("negative pending-requests target succeeded")
		}
	})

	t.Run("cross organization links are forbidden", func(t *testing.T) {
		other := seedTenantGraph(t, db, "other", "feature/other", "other-preview", "other-preview-ns")
		if _, err := db.Exec(context.Background(), `
			UPDATE projects SET git_connection_id = $1 WHERE id = $2`, other.repository, ids.project); err == nil {
			t.Fatal("cross-org project git connection succeeded")
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO orchestration_jobs (org_id,app_id,environment_id,github_repository_id,github_workflow_path,
				workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key)
			SELECT $1,$2,$3,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('a',40),
				'refs/heads/main',repeat('a',40),1,'cross-org' FROM git_connections connection WHERE connection.id=$4`,
			ids.org, ids.app, ids.environment, other.repository); err == nil {
			t.Fatal("cross-org orchestration job succeeded")
		}
	})

	t.Run("preview branch can be recreated after teardown", func(t *testing.T) {
		preview := seedTenantGraph(t, db, "previewx", "feature/recreate", "recreate", "recreate-ns")
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
			INSERT INTO orchestration_jobs (org_id,app_id,environment_id,github_repository_id,github_workflow_path,
				workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key)
			SELECT $1,$2,$3,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('b',40),
				'refs/heads/main',repeat('b',40),1,'same-key' FROM git_connections connection WHERE connection.id=$4
			RETURNING status`, ids.org, ids.app, ids.environment, ids.repository).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "queued" {
			t.Fatalf("orchestration status = %q, want queued", status)
		}
		if _, err := db.Exec(context.Background(), `
			INSERT INTO orchestration_jobs (org_id,app_id,environment_id,github_repository_id,github_workflow_path,
				workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key)
			SELECT $1,$2,$3,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('c',40),
				'refs/heads/main',repeat('c',40),1,'same-key' FROM git_connections connection WHERE connection.id=$4`,
			other.org, other.app, other.environment, other.repository); err != nil {
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

func TestMigration000005ExpiresProtocolIncompatibleSetupSessions(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration5_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(4); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var orgID, userID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Legacy','legacy') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO users(org_id,email,password_hash,display_name,operator) VALUES($1,'legacy@example.test','hash','Legacy',true) RETURNING id::text`, orgID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO github_setup_sessions(state_hash,flow_kind,initiating_user_id,initiating_session_hash,status,checkpoint,expires_at) VALUES(repeat('a',64),'app_setup',$1,repeat('b',64),'active','created',now()+interval '10 minutes')`, userID); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(5); err != nil {
		t.Fatal(err)
	}
	var status, checkpoint, code string
	var generation *string
	if err := conn.QueryRow(t.Context(), `SELECT status,checkpoint,failure_code,state_generation FROM github_setup_sessions`).Scan(&status, &checkpoint, &code, &generation); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || checkpoint != "completed" || code != "github_setup_protocol_incompatible" || generation != nil {
		t.Fatalf("legacy session = %s %s %s %v", status, checkpoint, code, generation)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO github_setup_sessions(state_hash,state_generation,flow_kind,initiating_user_id,initiating_session_hash,status,checkpoint,expires_at) VALUES(repeat('c',64),repeat('d',64),'app_setup',$1,repeat('e',64),'active','created',now()+interval '10 minutes')`, userID); err != nil {
		t.Fatalf("immediate replacement setup: %v", err)
	}
}

func TestMigration000009NormalizationDownAndReUp(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration9_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(8); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var firstOrg string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Legacy One','legacy-one') RETURNING id::text`).Scan(&firstOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `ALTER TABLE registries DROP CONSTRAINT registries_singleton_key, DROP CONSTRAINT registries_provider_check, DROP CONSTRAINT registries_registry_host_check, DROP CONSTRAINT registries_singleton_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO registries(org_id,provider,registry_host) VALUES
		($1,' GHCR ',' HTTPS://GHCR.IO/ ')`, firstOrg); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(9); err != nil {
		t.Fatalf("up: %v", err)
	}
	var normalized int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM registries WHERE provider='ghcr' AND registry_host='ghcr.io'`).Scan(&normalized); err != nil || normalized != 1 {
		t.Fatalf("normalized rows = %d, %v", normalized, err)
	}
	if err := migrator.Migrate(8); err != nil {
		t.Fatalf("compatibility down: %v", err)
	}
	if err := migrator.Migrate(9); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	var count int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM registries`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows after down/re-up = %d, %v", count, err)
	}
}

func TestMigrations000007And000008DownAndReUp(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration7_8_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(6); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	parents := seedTenantGraph(t, conn, "migration-seven-eight", "main", "", "migration-seven-eight-production")

	if err := migrator.Migrate(7); err != nil {
		t.Fatalf("000007 up: %v", err)
	}
	var revision string
	if err := conn.QueryRow(t.Context(), `SELECT github_workflow_revision FROM apps WHERE id=$1`, parents.app).Scan(&revision); err != nil || revision != strings.Repeat("0", 40) {
		t.Fatalf("000007 backfilled revision = %q, %v", revision, err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE apps SET github_workflow_revision=$2 WHERE id=$1`, parents.app, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE apps SET github_workflow_revision='main' WHERE id=$1`, parents.app); err == nil {
		t.Fatal("000007 accepted a mutable workflow revision")
	}
	if err := migrator.Migrate(6); err != nil {
		t.Fatalf("000007 down: %v", err)
	}
	if err := migrator.Migrate(7); err != nil {
		t.Fatalf("000007 re-up: %v", err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT github_workflow_revision FROM apps WHERE id=$1`, parents.app).Scan(&revision); err != nil || revision != strings.Repeat("0", 40) {
		t.Fatalf("000007 revision after down/re-up = %q, %v", revision, err)
	}

	if err := migrator.Migrate(8); err != nil {
		t.Fatalf("000008 up: %v", err)
	}
	var generation int64
	if err := conn.QueryRow(t.Context(), `SELECT repository_reconciliation_generation FROM github_installations WHERE org_id=$1`, parents.org).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("000008 backfilled generation = %d, %v", generation, err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE github_installations SET repository_reconciliation_generation=0 WHERE org_id=$1`, parents.org); err == nil {
		t.Fatal("000008 accepted a non-positive repository generation")
	}
	if _, err := conn.Exec(t.Context(), `UPDATE github_installations SET repository_reconciliation_generation=3 WHERE org_id=$1`, parents.org); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(7); err != nil {
		t.Fatalf("000008 down: %v", err)
	}
	if err := migrator.Migrate(8); err != nil {
		t.Fatalf("000008 re-up: %v", err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT repository_reconciliation_generation FROM github_installations WHERE org_id=$1`, parents.org).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("000008 generation after down/re-up = %d, %v", generation, err)
	}
}

func TestMigration000009RejectsUnsupportedLegacyRegistry(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration9_invalid_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(8); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var orgID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Unsupported','unsupported') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `ALTER TABLE registries DROP CONSTRAINT registries_provider_check, DROP CONSTRAINT registries_registry_host_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO registries(org_id,provider,registry_host) VALUES($1,'ecr','public.ecr.aws')`, orgID); err != nil {
		t.Fatal(err)
	}
	err = migrator.Migrate(9)
	if err == nil || !strings.Contains(err.Error(), "supported identities are ghcr/ghcr.io and gcr/gcr.io") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestMigration000009RejectsInvalidLegacyRepositoryAtomically(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration9_repository_invalid_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(8); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var orgID, projectID, appID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Invalid Repository','invalid-repository') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO projects(org_id,name,slug) VALUES($1,'Invalid Repository','invalid-repository') RETURNING id::text`, orgID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `ALTER TABLE apps DROP CONSTRAINT apps_image_repository_ghcr`); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO apps(org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch)
		VALUES($1,$2,'Invalid Repository','invalid-repository','invalid-repository','ghcr.io/acme/api___v2','.github/workflows/deploy.yml','main') RETURNING id::text`, orgID, projectID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	err = migrator.Migrate(9)
	if err == nil || !strings.Contains(err.Error(), "not a canonical public GHCR repository") {
		t.Fatalf("migration error = %v", err)
	}
	var version uint
	var dirty bool
	if err := conn.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 9 || !dirty {
		t.Fatalf("migration state after rejected up = %d/%t, %v", version, dirty, err)
	}
	var repository string
	if err := conn.QueryRow(t.Context(), `SELECT image_repository FROM apps WHERE id=$1`, appID).Scan(&repository); err != nil || repository != "ghcr.io/acme/api___v2" {
		t.Fatalf("app after rejected up = %q, %v", repository, err)
	}
	var functionCount int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM pg_proc WHERE pronamespace=current_schema()::regnamespace AND proname='valid_repository_path'`).Scan(&functionCount); err != nil || functionCount != 0 {
		t.Fatalf("validation functions after rejected up = %d, %v", functionCount, err)
	}
}

func TestMigration000009RejectsInvalidLegacyRegistryRepositoryAtomically(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration9_registry_repository_invalid_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(8); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var orgID, projectID, registryID, appID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Invalid Registry Repository','invalid-registry-repository') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO projects(org_id,name,slug) VALUES($1,'Invalid Registry Repository','invalid-registry-repository') RETURNING id::text`, orgID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO registries(org_id,provider,registry_host) VALUES($1,'ghcr','ghcr.io') RETURNING id::text`, orgID).Scan(&registryID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `ALTER TABLE apps DROP CONSTRAINT apps_image_repository_ghcr`); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO apps(org_id,project_id,name,slug,hostname_label,registry_id,image_repository,github_workflow_path,production_branch,image_visibility)
		VALUES($1,$2,'Invalid Registry Repository','invalid-registry-repository','invalid-registry-repository',$3,'ghcr.io/acme/api','.github/workflows/deploy.yml','main','private') RETURNING id::text`, orgID, projectID, registryID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	err = migrator.Migrate(9)
	if err == nil || !strings.Contains(err.Error(), "must be a canonical hostless repository path") {
		t.Fatalf("migration error = %v", err)
	}
	var version uint
	var dirty bool
	if err := conn.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 9 || !dirty {
		t.Fatalf("migration state after rejected up = %d/%t, %v", version, dirty, err)
	}
	var repository string
	if err := conn.QueryRow(t.Context(), `SELECT image_repository FROM apps WHERE id=$1`, appID).Scan(&repository); err != nil || repository != "ghcr.io/acme/api" {
		t.Fatalf("app after rejected up = %q, %v", repository, err)
	}
	var functionCount int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM pg_proc WHERE pronamespace=current_schema()::regnamespace AND proname='valid_repository_path'`).Scan(&functionCount); err != nil || functionCount != 0 {
		t.Fatalf("validation functions after rejected up = %d, %v", functionCount, err)
	}
}

func TestMigration000009IncompatibleDownIsAtomic(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration9_down_invalid_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(9); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var orgID, projectID, registryID, appID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Down Invalid','down-invalid') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO projects(org_id,name,slug) VALUES($1,'Down Invalid','down-invalid') RETURNING id::text`, orgID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO registries(org_id,provider,registry_host,state) VALUES($1,'ghcr','ghcr.io','absent') RETURNING id::text`, orgID).Scan(&registryID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `INSERT INTO apps(org_id,project_id,name,slug,hostname_label,registry_id,image_repository,image_visibility,github_workflow_path,production_branch)
		VALUES($1,$2,'Down Invalid','down-invalid','down-invalid',$3,'foo--bar','private',NULL,NULL) RETURNING id::text`, orgID, projectID, registryID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	err = migrator.Migrate(8)
	if err == nil || !strings.Contains(err.Error(), "rollback blocked by registry-backed app") {
		t.Fatalf("incompatible down error = %v", err)
	}
	var version uint
	var dirty bool
	if err := conn.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 9 || dirty {
		t.Fatalf("migration state after rejected down = %d/%t, %v", version, dirty, err)
	}
	var workflowNullable, branchNullable string
	if err := conn.QueryRow(t.Context(), `SELECT max(CASE WHEN column_name='github_workflow_path' THEN is_nullable END),max(CASE WHEN column_name='production_branch' THEN is_nullable END)
		FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='apps'`).Scan(&workflowNullable, &branchNullable); err != nil || workflowNullable != "YES" || branchNullable != "YES" {
		t.Fatalf("app nullability after rejected down = %s/%s, %v", workflowNullable, branchNullable, err)
	}
	var repository string
	if err := conn.QueryRow(t.Context(), `SELECT image_repository FROM apps WHERE id=$1`, appID).Scan(&repository); err != nil || repository != "foo--bar" {
		t.Fatalf("app data after rejected down = %q, %v", repository, err)
	}
}

func TestMigration000006BackfillsGitHubInstallationAuditIDs(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration6_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(5); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var orgID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Legacy','legacy') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	for installationID, accountID := range map[int64]int64{101: 1001, 102: 1002} {
		if _, err := conn.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES($1,$2,$3,'acme','Organization','selected','active',1,1)`, installationID, orgID, accountID); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrator.Migrate(6); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(t.Context(), `SELECT id::text FROM github_installations ORDER BY github_installation_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("backfilled GitHub installation IDs = %v, want two non-empty unique IDs", ids)
	}
	if ids[0][14] != '7' || ids[1][14] != '7' {
		t.Fatalf("backfilled GitHub installation IDs = %v, want UUIDv7 IDs", ids)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO github_installations(id,github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(NULL,103,$1,1003,'acme','Organization','selected','active',1,1)`, orgID); err == nil {
		t.Fatal("GitHub installation with null internal ID succeeded")
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO github_installations(id,github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES($1,103,$2,1003,'acme','Organization','selected','active',1,1)`, ids[0], orgID); err == nil {
		t.Fatal("GitHub installation with duplicate internal ID succeeded")
	}
	if err := ValidateSchemaEpoch(t.Context(), db); err != nil {
		t.Fatalf("ValidateSchemaEpoch() after 000006: %v", err)
	}
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
		{"INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES($1,$2,$3,'acme','Organization','selected','active',1,1) RETURNING github_installation_id::text", []any{int64(len(prefix) + 100), &org, int64(len(prefix) + 1000)}, new(string)},
		{"INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES($1,$2,$3,$4,'acme',$5,$6,'private','main','active',1,1) RETURNING github_repository_id::text", []any{int64(len(prefix) + 10000), &org, int64(len(prefix) + 100), "node-" + prefix, prefix, "acme/" + prefix}, new(string)},
		{"INSERT INTO git_connections(org_id,github_repository_id) VALUES($1,$2) RETURNING id", []any{&org, int64(len(prefix) + 10000)}, &repository},
		{"INSERT INTO projects(org_id, name, slug, git_connection_id) VALUES ($1, $2, $3, $4) RETURNING id", []any{&org, prefix + " Project", prefix + "-project", &repository}, &project},
		{"INSERT INTO apps(org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch) VALUES($1,$2,$3,$4,$5,$6,'.github/workflows/deploy.yml','main') RETURNING id", []any{&org, &project, prefix + " App", prefix + "-app", prefix + "-app", "ghcr.io/acme/" + prefix}, &app},
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
