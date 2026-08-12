//go:build integration

package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const integrationPepper = "0123456789abcdef0123456789abcdef"
const integrationBootstrap = "bootstrap-0123456789abcdef0123456789abcdef"

func TestAuthenticationPostgresIntegration(t *testing.T) {
	service, dataStore, pool := newIntegrationService(t)
	service.passwordParams = PasswordParams{MemoryKiB: minMemoryKiB, Iterations: 1, Parallelism: 1, SaltBytes: 16}
	ctx := context.Background()

	t.Run("first-user race and password verification", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for i := range 2 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := service.ClaimFirstUser(ctx, fmt.Sprintf("user%d@example.com", i), "local password", integrationBootstrap)
				results <- err
			}(i)
		}
		wg.Wait()
		close(results)
		successes, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, store.ErrConflict):
				conflicts++
			default:
				t.Fatalf("unexpected first-user error: %v", err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("first-user results successes=%d conflicts=%d", successes, conflicts)
		}

		var email, passwordHash string
		var operator bool
		if err := pool.QueryRow(ctx, "SELECT email, password_hash, operator FROM users").Scan(&email, &passwordHash, &operator); err != nil {
			t.Fatal(err)
		}
		if !operator {
			t.Fatal("bootstrap user is not persisted as operator")
		}
		if strings.Contains(passwordHash, "local password") {
			t.Fatal("database contains plaintext password")
		}
		principal, err := service.AuthenticatePassword(ctx, email, "local password")
		if err != nil || principal.OrgID == "" || !principal.Operator {
			t.Fatalf("AuthenticatePassword() = %+v, %v", principal, err)
		}
		if _, err := service.AuthenticatePassword(ctx, email, "wrong password"); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("wrong password error = %v", err)
		}
	})

	user, err := dataStore.UserByEmail(ctx, currentUserEmail(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	principal := principalFromUser(user)
	appOne := insertAppGraph(t, pool, user.OrgID, "one")
	otherOrg, otherUser := insertOtherTenant(t, pool)
	appTwo := insertAppGraph(t, pool, otherOrg, "two")
	scopeOne, _ := DeployAppScope(appOne)
	scopeTwo, _ := DeployAppScope(appTwo)
	if _, err := pool.Exec(ctx, `INSERT INTO api_tokens
		(user_id, name, token_prefix, token_hash, scopes) VALUES
		($1, 'invalid', 'AAAAAAAAAAAA', repeat('a', 64), ARRAY['reconcile:org'])`, user.ID); err == nil {
		t.Fatal("database accepted a non-v1 scope")
	}

	t.Run("token scope tenant expiry revocation and throttling", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		service.now = func() time.Time { return now }
		created, err := service.CreateToken(ctx, principal, CreateTokenParams{Name: "deploy one", Scopes: []string{scopeOne}})
		if err != nil {
			t.Fatal(err)
		}
		var storedPrefix, storedHash string
		if err := pool.QueryRow(ctx, `SELECT token_prefix, token_hash FROM api_tokens WHERE id = $1`, created.ID).
			Scan(&storedPrefix, &storedHash); err != nil {
			t.Fatal(err)
		}
		if storedPrefix != created.Prefix || storedHash == created.Token || strings.Contains(storedHash, created.Token) {
			t.Fatalf("stored token material prefix=%q hash=%q", storedPrefix, storedHash)
		}
		tokenPrincipal, err := service.AuthenticateToken(ctx, created.Token)
		if err != nil || tokenPrincipal.OrgID != user.OrgID || !tokenPrincipal.Operator {
			t.Fatalf("AuthenticateToken() = %+v, %v", tokenPrincipal, err)
		}
		if err := service.AuthorizeDeployApp(ctx, tokenPrincipal, appOne); err != nil {
			t.Fatalf("own app authorization: %v", err)
		}
		if err := service.AuthorizeDeployApp(ctx, tokenPrincipal, appTwo); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant authorization error = %v", err)
		}
		if _, err := service.CreateToken(ctx, principal, CreateTokenParams{Name: "cross tenant", Scopes: []string{scopeTwo}}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant token creation error = %v", err)
		}

		var firstUsed *time.Time
		if err := pool.QueryRow(ctx, `SELECT last_used_at FROM api_tokens WHERE id = $1`, created.ID).Scan(&firstUsed); err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return now.Add(time.Minute) }
		if _, err := service.AuthenticateToken(ctx, created.Token); err != nil {
			t.Fatal(err)
		}
		var throttledUsed *time.Time
		if err := pool.QueryRow(ctx, `SELECT last_used_at FROM api_tokens WHERE id = $1`, created.ID).Scan(&throttledUsed); err != nil {
			t.Fatal(err)
		}
		if firstUsed == nil || throttledUsed == nil || !throttledUsed.Equal(*firstUsed) {
			t.Fatalf("last_used_at changed inside throttle window: %v -> %v", firstUsed, throttledUsed)
		}

		if err := service.RevokeToken(ctx, Principal{UserID: otherUser, OrgID: otherOrg}, created.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant revocation error = %v", err)
		}
		if err := service.RevokeToken(ctx, principal, created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.AuthenticateToken(ctx, created.Token); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("revoked token error = %v", err)
		}

		expires := now.Add(time.Hour)
		service.now = func() time.Time { return now }
		expiring, err := service.CreateToken(ctx, principal, CreateTokenParams{Name: "expiring", Scopes: []string{scopeOne}, ExpiresAt: &expires})
		if err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return expires }
		if _, err := service.AuthenticateToken(ctx, expiring.Token); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("expired token error = %v", err)
		}
	})
}

func newIntegrationService(t *testing.T) (*Service, *store.Store, *pgxpool.Pool) {
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
		t.Fatal(err)
	}
	if version < 180000 || version >= 190000 {
		t.Skipf("PostgreSQL 18 required, server_version_num=%d", version)
	}
	schema := fmt.Sprintf("auth_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(context.Background())
	})
	testURL := authSearchPathURL(t, databaseURL, schema)
	if err := store.Migrate(ctx, store.MigrationConfig{URL: testURL, LockTimeout: 5 * time.Second, Migrations: store.Migrations}); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(testURL)
	if err != nil {
		t.Fatal(err)
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		var searchPath string
		if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
			return fmt.Errorf("read auth test search_path: %w", err)
		}
		if searchPath != (pgx.Identifier{schema}).Sanitize() {
			return fmt.Errorf("auth test search_path = %q, want %q", searchPath, schema)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dataStore := store.New(pool)
	service, err := NewService(dataStore, []byte(integrationPepper), []byte(integrationBootstrap))
	if err != nil {
		t.Fatal(err)
	}
	return service, dataStore, pool
}
func currentUserEmail(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var email string
	if err := pool.QueryRow(context.Background(), "SELECT email FROM users").Scan(&email); err != nil {
		t.Fatal(err)
	}
	return email
}

func insertAppGraph(t *testing.T, pool *pgxpool.Pool, orgID, suffix string) string {
	t.Helper()
	ctx := context.Background()
	var projectID, appID string
	if err := pool.QueryRow(ctx, `INSERT INTO projects (org_id, name, slug)
		VALUES ($1, $2, $3) RETURNING id::text`, orgID, "Project "+suffix, "project-"+suffix).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps
		(org_id, project_id, name, slug, hostname_label) VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text`, orgID, projectID, "App "+suffix, "app-"+suffix, "auth-"+suffix).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	return appID
}

func insertOtherTenant(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP INDEX users_singleton"); err != nil {
		t.Fatal(err)
	}
	var orgID, userID string
	if err := pool.QueryRow(ctx, `INSERT INTO orgs (name, slug) VALUES ('Other Org', 'other-org') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (org_id, email, password_hash)
		VALUES ($1, 'other@example.com', 'test-only') RETURNING id::text`, orgID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	return orgID, userID
}
