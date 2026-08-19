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

	t.Run("greenfield reaches migration 000016 and preserves epoch", func(t *testing.T) {
		var version uint
		var dirty bool
		if err := db.QueryRow(context.Background(), "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
			t.Fatal(err)
		}
		if version != 16 || dirty {
			t.Fatalf("schema migration state = version:%d dirty:%t, want version:16 dirty:false", version, dirty)
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
			t.Fatalf("ValidateSchemaEpoch() after 000010: %v", err)
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

func TestMigration000011FailureRestoresV10RollbackGuard(t *testing.T) {
	url := integrationURL(t)
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := "cyclopus_migration11_restore_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	testURL := withSearchPath(url, schema)
	db, err := pgx.Connect(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	migrator := githubWebhookMigrator(t, testURL)
	if err := migrator.Migrate(9); err != nil {
		t.Fatal(err)
	}
	if sourceErr, databaseErr := migrator.Close(); sourceErr != nil || databaseErr != nil {
		t.Fatalf("close baseline migrator: source=%v database=%v", sourceErr, databaseErr)
	}
	committedV10, err := os.ReadFile("testdata/000010_github_webhook_routing.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), string(committedV10)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE schema_migrations SET version=10,dirty=false WHERE version=9 AND dirty=false`); err != nil {
		t.Fatal(err)
	}
	seedGitHubWebhookFixtureConn(t, db)
	if _, err := db.Exec(t.Context(), `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO github_repository_routing_state(repository_id,org_id,repository_node_id,repository_generation,lifecycle_generation) VALUES(999999,$1,'missing-node',1,1)`, githubWebhookOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `SET session_replication_role=origin`); err != nil {
		t.Fatal(err)
	}
	err = Migrate(t.Context(), integrationConfig(testURL))
	if err == nil || !strings.Contains(err.Error(), "installation backfill blocked") {
		t.Fatalf("failed 000011 migration error = %v", err)
	}
	var version uint
	var dirty bool
	if err := db.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 10 || dirty {
		t.Fatalf("migration state after restored failure = %d/%t, %v", version, dirty, err)
	}
	var guardCount int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM pg_trigger WHERE tgrelid='schema_migrations'::regclass AND tgname='guard_github_webhook_v10_down' AND NOT tgisinternal`).Scan(&guardCount); err != nil || guardCount != 1 {
		t.Fatalf("restored v10 guard count = %d, %v", guardCount, err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO github_webhook_deliveries(delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,verified_payload,payload_nonce,key_version) VALUES('30000000-0000-0000-0000-000000000099','push',90001,$1,'R_node',$2,repeat('a',64),decode(repeat('00',18),'hex'),decode(repeat('00',12),'hex'),'k1')`, githubWebhookRepo, githubWebhookOrg); err != nil {
		t.Fatal(err)
	}
	down := githubWebhookMigrator(t, testURL)
	if err := down.Steps(-1); err == nil || !strings.Contains(err.Error(), "000010 rollback blocked") {
		t.Fatalf("unsafe rollback after failed 000011 error = %v", err)
	}
	if sourceErr, databaseErr := down.Close(); sourceErr != nil || databaseErr != nil {
		t.Fatalf("close rollback migrator: source=%v database=%v", sourceErr, databaseErr)
	}
	if err := db.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 10 || dirty {
		t.Fatalf("migration state after blocked rollback = %d/%t, %v", version, dirty, err)
	}
	if _, err := db.Exec(t.Context(), `DELETE FROM github_repository_routing_state WHERE repository_id=999999;
		UPDATE github_webhook_deliveries SET status='completed',result_code='processed',terminal_at=clock_timestamp(),verified_payload=NULL,payload_nonce=NULL,payload_redacted_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), integrationConfig(testURL)); err != nil {
		t.Fatalf("repaired 000011 migration: %v", err)
	}
	if err := db.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 15 || dirty {
		t.Fatalf("migration state after repaired up = %d/%t, %v", version, dirty, err)
	}
	if err := Migrate(t.Context(), integrationConfig(testURL)); err != nil {
		t.Fatalf("idempotent repaired migration: %v", err)
	}
}

func TestMigration000013ConflictGuardDownAndReUp(t *testing.T) {
	url, schema, admin := newGitHubWebhookSchema(t)
	defer admin.Close(context.Background())
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	migrator := githubWebhookMigrator(t, url)
	defer migrator.Close()
	if err := migrator.Migrate(12); err != nil {
		t.Fatalf("migrate to 000012: %v", err)
	}
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	seedGitHubWebhookFixtureConn(t, conn)
	var deliveryID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO github_webhook_deliveries(delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,key_version,signature_key_version,status,result_code,terminal_at,retain_until,payload_redacted_at)
		VALUES('30000000-0000-0000-0000-000000000013','push',90001,$1,'R_node',$2,repeat('d',64),'k1','legacy','completed','processed',clock_timestamp(),clock_timestamp()+interval '1 hour',clock_timestamp()) RETURNING id::text`, githubWebhookRepo, githubWebhookOrg).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(13); err != nil {
		t.Fatalf("000012 to 000013: %v", err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE github_webhook_deliveries SET status='failed',result_code=NULL,error_code='github_conflict',error_detail='persistent conflict' WHERE id=$1`, deliveryID); err != nil {
		t.Fatalf("000013 github_conflict row: %v", err)
	}
	if err := migrator.Migrate(12); err == nil || !strings.Contains(err.Error(), "000013 rollback blocked") {
		t.Fatalf("000013 guarded down error = %v", err)
	}
	var version uint
	var dirty bool
	if err := conn.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 13 || dirty {
		t.Fatalf("guarded 000013 state = %d/%t, %v", version, dirty, err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE github_webhook_deliveries SET error_code='invalid_event' WHERE id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(12); err != nil {
		t.Fatalf("clean 000013 down: %v", err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE github_webhook_deliveries SET error_code='github_conflict' WHERE id=$1`, deliveryID); err == nil {
		t.Fatal("000012 constraint accepted github_conflict")
	}
	if err := migrator.Migrate(13); err != nil {
		t.Fatalf("000013 re-up: %v", err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE github_webhook_deliveries SET error_code='github_conflict' WHERE id=$1`, deliveryID); err != nil {
		t.Fatalf("000013 re-up rejected github_conflict: %v", err)
	}
}

func TestMigration000014FanoutGuardShapeDownAndReUp(t *testing.T) {
	url, schema, admin := newGitHubWebhookSchema(t)
	defer admin.Close(context.Background())
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	migrator := githubWebhookMigrator(t, url)
	defer migrator.Close()
	if err := migrator.Migrate(13); err != nil {
		t.Fatalf("migrate to 000013: %v", err)
	}
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for _, table := range []string{"github_routing_fanout_manifests", "github_routing_fanout_candidates"} {
		var count int
		if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=$1`, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("000013 table %s count = %d, %v", table, count, err)
		}
	}
	if err := migrator.Migrate(14); err != nil {
		t.Fatalf("000013 to 000014: %v", err)
	}
	var tables, indexes, checks, guard int
	if err := conn.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ('github_routing_fanout_manifests','github_routing_fanout_candidates')),
		(SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname IN ('github_routing_fanout_manifests_ref_generation','github_routing_fanout_manifests_pending','github_routing_fanout_manifests_leased','github_routing_fanout_manifests_terminal','github_routing_fanout_candidates_app_identity')),
		(SELECT count(*) FROM information_schema.table_constraints WHERE constraint_schema=current_schema() AND table_name IN ('github_routing_fanout_manifests','github_routing_fanout_candidates') AND constraint_type IN ('CHECK','UNIQUE','FOREIGN KEY')),
		(SELECT count(*) FROM pg_trigger WHERE tgrelid='schema_migrations'::regclass AND tgname='guard_github_routing_fanout_v14_down' AND NOT tgisinternal)`).Scan(&tables, &indexes, &checks, &guard); err != nil {
		t.Fatal(err)
	}
	if tables != 2 || indexes != 5 || checks < 15 || guard != 1 {
		t.Fatalf("000014 shape tables/indexes/constraints/guard = %d/%d/%d/%d", tables, indexes, checks, guard)
	}
	seedGitHubWebhookFixtureConn(t, conn)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO github_webhook_deliveries(id,delivery_id,event,installation_id,repository_id,repository_node_id,org_id,payload_digest,key_version,signature_key_version,status,result_code,terminal_at,payload_redacted_at) VALUES
			('54000000-0000-0000-0000-000000000001','54000000-0000-0000-0000-000000000011','push',90001,$1,'R_node',$2,repeat('a',64),'k1','legacy','completed','fanout_staged',clock_timestamp(),clock_timestamp()),
			('54000000-0000-0000-0000-000000000002','54000000-0000-0000-0000-000000000012','push',90001,$1,'R_node',$2,repeat('b',64),'k1','legacy','completed','processed',clock_timestamp(),clock_timestamp()),
			('54000000-0000-0000-0000-000000000003','54000000-0000-0000-0000-000000000013','push',90001,$1,'R_node',$2,repeat('c',64),'k1','legacy','completed','processed',clock_timestamp(),clock_timestamp())`, []any{githubWebhookRepo, githubWebhookOrg}},
		{`INSERT INTO github_repository_routing_state(repository_id,org_id,repository_node_id,installation_id,repository_generation,lifecycle_generation) VALUES($1,$2,'R_node',90001,1,1)`, []any{githubWebhookRepo, githubWebhookOrg}},
		{`INSERT INTO github_ref_routing_state(repository_id,ref,routing_generation,lifecycle_generation,validated_sha,ref_exists) VALUES($1,'refs/heads/main',1,0,repeat('a',40),true)`, []any{githubWebhookRepo}},
		{`INSERT INTO github_routing_fanout_manifests(id,delivery_id,org_id,repository_id,repository_node_id,installation_id,ref,event_kind,commit_sha,routing_generation,repository_generation,ref_lifecycle_generation,status,total_count,inserted_count,batch_size,max_fanout,error_code,terminal_at) VALUES
			('54000000-0000-0000-0000-000000000021','54000000-0000-0000-0000-000000000001',$2,$1,'R_node',90001,'refs/heads/main','push',repeat('a',40),1,1,0,'completed',0,0,1,1,NULL,clock_timestamp()),
			('54000000-0000-0000-0000-000000000022','54000000-0000-0000-0000-000000000002',$2,$1,'R_node',90001,'refs/heads/main','push',repeat('a',40),2,1,0,'superseded',0,0,1,1,NULL,clock_timestamp()),
			('54000000-0000-0000-0000-000000000023','54000000-0000-0000-0000-000000000003',$2,$1,'R_node',90001,'refs/heads/main','push',repeat('a',40),3,1,0,'failed',0,0,1,1,'attempts_exhausted',clock_timestamp())`, []any{githubWebhookRepo, githubWebhookOrg}},
	}
	for _, statement := range statements {
		if _, err := conn.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrator.Migrate(13); err == nil || !strings.Contains(err.Error(), "result_code=fanout_staged") {
		t.Fatalf("fanout_staged 000014 down error = %v", err)
	}
	var version uint
	var dirty bool
	if err := conn.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 14 || dirty {
		t.Fatalf("guarded 000014 state = %d/%t, %v", version, dirty, err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE github_webhook_deliveries SET result_code='processed' WHERE result_code='fanout_staged'`); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(13); err != nil {
		t.Fatalf("clean 000014 down: %v", err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ('github_routing_fanout_manifests','github_routing_fanout_candidates')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("000014 down fanout table count = %d, %v", tables, err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE github_webhook_deliveries SET result_code='fanout_staged' WHERE id='54000000-0000-0000-0000-000000000001'`); err == nil {
		t.Fatal("000013 constraint accepted fanout_staged")
	}
	if err := migrator.Migrate(14); err != nil {
		t.Fatalf("000014 re-up: %v", err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 14 || dirty {
		t.Fatalf("000014 re-up state = %d/%t, %v", version, dirty, err)
	}
}

func TestMigration000015UpgradeGuardsConstraintsAndReUp(t *testing.T) {
	url, schema, admin := newGitHubWebhookSchema(t)
	defer admin.Close(context.Background())
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	migrator := githubWebhookMigrator(t, url)
	defer migrator.Close()
	if err := migrator.Migrate(14); err != nil {
		t.Fatalf("literal latest-14 migrate: %v", err)
	}
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	legacy := seedTenantGraph(t, conn, "migration-fifteen", "main", "", "migration-fifteen-production")
	var legacyResolvedJob string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orchestration_jobs(org_id,app_id,environment_id,github_repository_id,
		github_workflow_path,workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key,status,
		oci_resolution_state,image_tag_ref,resolved_image_digest,immutable_image_ref,oci_media_type,oci_resolved_at,deployment_state)
		SELECT $1,$2,$3,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('a',40),'refs/heads/main',repeat('b',40),1,
		'legacy-resolved-v14','deploying','resolved','ghcr.io/acme/migration-fifteen:'||repeat('b',40),'sha256:'||repeat('c',64),
		'ghcr.io/acme/migration-fifteen@sha256:'||repeat('c',64),'application/vnd.oci.image.manifest.v1+json',now(),'ready'
		FROM git_connections connection WHERE connection.id=$4 RETURNING id::text`, legacy.org, legacy.app, legacy.environment, legacy.repository).Scan(&legacyResolvedJob); err != nil {
		t.Fatal(err)
	}
	var legacyRegistry string
	if err := conn.QueryRow(t.Context(), `INSERT INTO registries(org_id,provider,registry_host,state,checkpoint,pending_generation,pending_credential_secret_ref)
		VALUES($1,'ghcr','ghcr.io','rotating','secret_written',1,'k8s-secret://cyclopus-system/legacy-ghcr#dockerconfig') RETURNING id::text`, legacy.org).Scan(&legacyRegistry); err != nil {
		t.Fatal(err)
	}
	v14Catalog := schemaCatalogSnapshot(t, conn)
	var v14LegacyJob string
	if err := conn.QueryRow(t.Context(), `SELECT to_jsonb(job)::text FROM orchestration_jobs job WHERE id=$1`, legacyResolvedJob).Scan(&v14LegacyJob); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(15); err != nil {
		t.Fatalf("000014 to 000015: %v", err)
	}
	var version uint
	var dirty, enabled bool
	if err := conn.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 15 || dirty {
		t.Fatalf("000015 state = %d/%t, %v", version, dirty, err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT phase6_enabled FROM schema_epoch WHERE singleton`).Scan(&enabled); err != nil || enabled {
		t.Fatalf("phase6 enabled after migration = %t, %v", enabled, err)
	}
	var legacyAnnotation string
	var legacyDeadline *time.Time
	if err := conn.QueryRow(t.Context(), `SELECT oci_annotation_status,oci_deadline_at FROM orchestration_jobs WHERE id=$1`, legacyResolvedJob).Scan(&legacyAnnotation, &legacyDeadline); err != nil || legacyAnnotation != "legacy" || legacyDeadline != nil {
		t.Fatalf("legacy resolved evidence after additive migration = %q/%v, %v", legacyAnnotation, legacyDeadline, err)
	}
	var previous *int64
	var attempts int
	if err := conn.QueryRow(t.Context(), `SELECT previous_generation,attempts FROM registries WHERE id=$1`, legacyRegistry).Scan(&previous, &attempts); err != nil || previous != nil || attempts != 0 {
		t.Fatalf("registry mixed defaults = %v/%d, %v", previous, attempts, err)
	}
	var checkpoint string
	if err := conn.QueryRow(t.Context(), `SELECT checkpoint FROM registries WHERE id=$1`, legacyRegistry).Scan(&checkpoint); err != nil || checkpoint != "secret_written" {
		t.Fatalf("v14 registry checkpoint after migration = %q, %v, want secret_written", checkpoint, err)
	}
	var ociAttempts int
	var annotation string
	if err := conn.QueryRow(t.Context(), `SELECT 0,'pending' WHERE NOT EXISTS (SELECT 1 FROM orchestration_jobs WHERE oci_attempts<>0 OR oci_annotation_status NOT IN ('pending','legacy'))`).Scan(&ociAttempts, &annotation); err != nil || ociAttempts != 0 || annotation != "pending" {
		t.Fatalf("OCI mixed defaults = %d/%q, %v", ociAttempts, annotation, err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE schema_epoch SET phase6_enabled=true`); err == nil {
		t.Fatal("direct Phase 6 enable succeeded")
	}
	if _, err := conn.Exec(t.Context(), `SELECT set_config('cyclopus.phase6_enable','on',true); UPDATE schema_epoch SET phase6_enabled=true`); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(14); err == nil || !strings.Contains(err.Error(), "Phase 6 is enabled") {
		t.Fatalf("enabled guarded down error = %v", err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 15 || dirty {
		t.Fatalf("blocked down state = %d/%t, %v", version, dirty, err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE schema_epoch SET phase6_enabled=false`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE orchestration_jobs SET oci_attempts=1 WHERE id=$1`, legacyResolvedJob); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(14); err == nil || !strings.Contains(err.Error(), "Phase 6 credential, reconciliation, OCI, or release evidence remains") {
		t.Fatalf("legacy job with changed v15 evidence down error = %v", err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE orchestration_jobs SET oci_attempts=0 WHERE id=$1`, legacyResolvedJob); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(14); err != nil {
		t.Fatalf("clean 000015 down: %v", err)
	}
	if got := schemaCatalogSnapshot(t, conn); got != v14Catalog {
		t.Fatalf("000014 catalog after 000015 down differs from seeded v14 catalog\nwant:\n%s\n\ngot:\n%s", v14Catalog, got)
	}
	var downLegacyJob string
	if err := conn.QueryRow(t.Context(), `SELECT to_jsonb(job)::text FROM orchestration_jobs job WHERE id=$1`, legacyResolvedJob).Scan(&downLegacyJob); err != nil || downLegacyJob != v14LegacyJob {
		t.Fatalf("legacy resolved job after clean down differs\nwant: %s\n got: %s\nerr: %v", v14LegacyJob, downLegacyJob, err)
	}
	if err := migrator.Migrate(15); err != nil {
		t.Fatalf("000015 re-up: %v", err)
	}
	first := seedTenantGraph(t, conn, "migration-fifteen-one", "main", "", "migration-fifteen-one-production")
	second := seedTenantGraph(t, conn, "migration-fifteen-second", "main", "", "migration-fifteen-second-production")
	var firstRegistry, secondRegistry string
	for _, item := range []struct {
		org string
		id  *string
	}{{first.org, &firstRegistry}, {second.org, &secondRegistry}} {
		if err := conn.QueryRow(t.Context(), `INSERT INTO registries(org_id,provider,registry_host) VALUES($1,'ghcr','ghcr.io') RETURNING id::text`, item.org).Scan(item.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,candidate_generation,idempotency_key)
		VALUES('00000000-0000-0000-0000-000000000151',$1,$2,'create',1,'tenant-fk')`, first.org, secondRegistry); err == nil {
		t.Fatal("cross-tenant credential operation succeeded")
	}
	var jobID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orchestration_jobs(org_id,app_id,environment_id,github_repository_id,github_workflow_path,workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key)
		SELECT $1,$2,$3,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('a',40),'refs/heads/main',repeat('a',40),environment.desired_generation,'migration-fifteen-job'
		FROM git_connections connection,environments environment WHERE connection.id=$4 AND environment.id=$3 RETURNING id::text`, first.org, first.app, first.environment, first.repository).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE orchestration_jobs SET oci_attempts=21 WHERE id=$1`, jobID); err == nil {
		t.Fatal("OCI attempt cap accepted 21")
	}
	if _, err := conn.Exec(t.Context(), `UPDATE orchestration_jobs SET oci_lease_owner='worker' WHERE id=$1`, jobID); err == nil {
		t.Fatal("partial OCI lease succeeded")
	}
	_ = firstRegistry
}

func TestMigration000016CanonicalRepositoryTriggerCatalogDownAndReUp(t *testing.T) {
	url, schema, admin := newGitHubWebhookSchema(t)
	defer admin.Close(context.Background())
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	migrator := githubWebhookMigrator(t, url)
	defer migrator.Close()
	if err := migrator.Migrate(15); err != nil {
		t.Fatalf("migrate to 000015: %v", err)
	}
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	fixture := seedTenantGraph(t, conn, "migration-sixteen", "main", "", "migration-sixteen-production")
	var jobID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO orchestration_jobs(org_id,app_id,environment_id,github_repository_id,
		github_workflow_path,workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key)
		SELECT $1,$2,$3,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('a',40),'refs/heads/main',repeat('b',40),environment.desired_generation,'migration-sixteen-job'
		FROM git_connections connection,environments environment WHERE connection.id=$4 AND environment.id=$3 RETURNING id::text`,
		fixture.org, fixture.app, fixture.environment, fixture.repository).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	v15Catalog := schemaCatalogSnapshot(t, conn)
	if err := migrator.Migrate(16); err != nil {
		t.Fatalf("000015 to 000016: %v", err)
	}
	var repository string
	if err := conn.QueryRow(t.Context(), `SELECT image_repository FROM apps WHERE id=$1`, fixture.app).Scan(&repository); err != nil || repository != "acme/migration-sixteen" {
		t.Fatalf("v16 repository = %q, %v", repository, err)
	}
	for _, invalid := range []string{"ghcr.io/acme/migration-sixteen", "acme/migration-sixteen:latest", "acme/migration-sixteen@sha256:abc"} {
		if _, err := conn.Exec(t.Context(), `UPDATE apps SET image_repository=$2 WHERE id=$1`, fixture.app, invalid); err == nil {
			t.Fatalf("v16 invalid repository %q accepted", invalid)
		}
	}
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE orchestration_jobs SET
		image_tag_ref='ghcr.io/acme/migration-sixteen:'||desired_checkout_sha,
		resolved_image_digest='sha256:'||repeat('c',64),
		immutable_image_ref='ghcr.io/acme/migration-sixteen@sha256:'||repeat('c',64)
		WHERE id=$1`, jobID); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatalf("v16 canonical references rejected: %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(15); err != nil {
		t.Fatalf("clean 000016 down: %v", err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT image_repository FROM apps WHERE id=$1`, fixture.app).Scan(&repository); err != nil || repository != "ghcr.io/acme/migration-sixteen" {
		t.Fatalf("v15 repository after down = %q, %v", repository, err)
	}
	if got := schemaCatalogSnapshot(t, conn); got != v15Catalog {
		t.Fatalf("000015 catalog after 000016 down differs from seeded v15 catalog\nwant:\n%s\n\ngot:\n%s", v15Catalog, got)
	}
	if err := migrator.Migrate(16); err != nil {
		t.Fatalf("000016 re-up: %v", err)
	}
	var registryID string
	if err := conn.QueryRow(t.Context(), `INSERT INTO registries(org_id,provider,registry_host) VALUES($1,'ghcr','ghcr.io') RETURNING id::text`, fixture.org).Scan(&registryID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE apps SET registry_id=$2,image_visibility='private' WHERE id=$1`, fixture.app, registryID); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(15); err == nil || !strings.Contains(err.Error(), "000016 rollback blocked: combined GitHub and registry applications remain") {
		t.Fatalf("combined app down error = %v", err)
	}
}

// schemaCatalogSnapshot covers every schema object whose shape migration 000015
// can change.  Keeping it catalog-derived makes v14, rather than a copied SQL
// fixture, the authority for an exact upgrade/down round trip.
func schemaCatalogSnapshot(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	queries := []string{
		`SELECT coalesce(string_agg(format('%s.%s|%s|%s|%s', table_name,column_name,udt_name,is_nullable,coalesce(column_default,'')), E'\n' ORDER BY table_name,ordinal_position),'')
		 FROM information_schema.columns WHERE table_schema=current_schema()`,
		`SELECT coalesce(string_agg(format('%s|%s|%s', rel.relname,con.conname,pg_get_constraintdef(con.oid,true)), E'\n' ORDER BY rel.relname,con.conname),'')
		 FROM pg_constraint con JOIN pg_class rel ON rel.oid=con.conrelid WHERE rel.relnamespace=current_schema()::regnamespace`,
		`SELECT coalesce(string_agg(format('%s|%s', indexrel.relname,pg_get_indexdef(indexrel.oid)), E'\n' ORDER BY indexrel.relname),'')
		 FROM pg_index idx JOIN pg_class table_rel ON table_rel.oid=idx.indrelid JOIN pg_class indexrel ON indexrel.oid=idx.indexrelid WHERE table_rel.relnamespace=current_schema()::regnamespace`,
		`SELECT coalesce(string_agg(format('%s|%s|%s', rel.relname,trg.tgname,pg_get_triggerdef(trg.oid,true)), E'\n' ORDER BY rel.relname,trg.tgname),'')
		 FROM pg_trigger trg JOIN pg_class rel ON rel.oid=trg.tgrelid WHERE rel.relnamespace=current_schema()::regnamespace AND NOT trg.tgisinternal`,
		`SELECT coalesce(string_agg(format('%s|%s', p.oid::regprocedure,pg_get_functiondef(p.oid)), E'\n' ORDER BY p.oid::regprocedure::text),'')
		 FROM pg_proc p WHERE p.pronamespace=current_schema()::regnamespace`,
	}
	parts := make([]string, len(queries))
	for i, query := range queries {
		if err := conn.QueryRow(t.Context(), query).Scan(&parts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return strings.Join(parts, "\n---\n")
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
