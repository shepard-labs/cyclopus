//go:build integration

package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	githubadapter "github.com/shepard-labs/cyclopus/internal/adapters/github"
	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestGitHubProviderHandlersBeginGetListAndRefreshIntegration(t *testing.T) {
	dataStore, pool := newGitHubServerStore(t)
	actor := seedGitHubServerActor(t, pool)
	privateKey := serverPrivateKey(t)
	secret := &serverManagedSecret{value: secretref.ManagedSecret{ResourceVersion: "1", Data: map[string][]byte{secretref.GitHubPrivateKeyKey: privateKey, secretref.GitHubWebhookKey: []byte("01234567890123456789012345678901")}}}
	onboarding, err := service.NewGitHubOnboarding(dataStore, secret, service.GitHubOnboardingConfig{ExternalURL: "https://cyclopus.example", APIBaseURL: "https://api.github.test/", UploadURL: "https://uploads.github.test/", APIVersion: "2022-11-28", CredentialSecretRef: "k8s-secret://cyclopus/cyclopus-github-app#private-key.pem", RequestTimeout: time.Second, SealingKey: []byte(strings.Repeat("k", 32))}, &http.Client{},
		service.WithGitHubInstallationVerifier(func(_ context.Context, _ githubadapter.AppCredential, id int64) (store.GitHubInstallation, error) {
			return store.GitHubInstallation{GitHubInstallationID: id, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "selected", AccessState: "active"}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	seedReadyGitHubServerApp(t, dataStore, actor)
	if _, err := dataStore.UpsertGitHubInstallation(t.Context(), actor.OrgID, store.GitHubInstallation{GitHubInstallationID: 42, AccountID: 7, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "all", AccessState: "active"}, store.GitHubAuditContext{OrgID: actor.OrgID, Event: &store.AuditEvent{ID: "00000000-0000-7000-8000-000000000101"}, Source: "test"}, actor.OrgID); err != nil {
		t.Fatal(err)
	}
	domain := service.New(dataStore, service.WithGitHubOnboarding(onboarding))
	handler := &apiHandlers{service: domain}
	ctx := authenticatedServerContext(true)
	ctx = context.WithValue(ctx, actorContextKey, actor)

	begin, err := handler.BeginGitHubInstallation(ctx, &cyclopusv1.BeginGitHubInstallationRequest{IdempotencyKey: "begin-install"})
	if err != nil || begin.GetFlow().GetStartUrl() == "" || begin.GetFlow().GetExpiresAt() == nil {
		t.Fatalf("begin = %+v, %v", begin, err)
	}
	get, err := handler.GetGitHubInstallation(ctx, &cyclopusv1.GetGitHubInstallationRequest{InstallationId: "42"})
	if err != nil || get.GetInstallation().GetGithubInstallationId() != 42 {
		t.Fatalf("get = %+v, %v", get, err)
	}
	list, err := handler.ListGitHubInstallations(ctx, &cyclopusv1.ListGitHubInstallationsRequest{PageSize: 10})
	if err != nil || len(list.GetInstallations()) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	refresh, err := handler.RefreshGitHubInstallation(ctx, &cyclopusv1.RefreshGitHubInstallationRequest{InstallationId: "42", IdempotencyKey: "refresh"})
	if err != nil || refresh.GetInstallation().GetRepositorySelection() != cyclopusv1.GitHubInstallationRepositorySelection_GITHUB_INSTALLATION_REPOSITORY_SELECTION_SELECTED {
		t.Fatalf("refresh = %+v, %v", refresh, err)
	}
	status, err := handler.GetGitHubAppStatus(ctx, &cyclopusv1.GetGitHubAppStatusRequest{})
	if err != nil || status.GetGithubApp().GetSetupState() != cyclopusv1.GitHubAppSetupState_GITHUB_APP_SETUP_STATE_READY || status.GetGithubApp().GetCredentialReadiness() != cyclopusv1.GitHubCredentialReadiness_GITHUB_CREDENTIAL_READINESS_READY {
		t.Fatalf("status = %+v, %v", status, err)
	}
}

type serverManagedSecret struct{ value secretref.ManagedSecret }

func (s *serverManagedSecret) ReadGitHubApp(context.Context) (secretref.ManagedSecret, error) {
	return s.value, nil
}
func (s *serverManagedSecret) WriteGitHubApp(_ context.Context, value secretref.ManagedSecret) (string, error) {
	s.value = value
	return "1", nil
}

func newGitHubServerStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("POSTGRES_TEST_URL")
	}
	if databaseURL == "" {
		t.Skip("PostgreSQL 18 integration URL is not set")
	}
	admin, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := admin.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 180000 || version >= 190000 {
		t.Skipf("PostgreSQL 18 required, got %d", version)
	}
	schema := fmt.Sprintf("github_server_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(context.Background())
	})
	separator := "?"
	if strings.Contains(databaseURL, "?") {
		separator = "&"
	}
	testURL := databaseURL + separator + "search_path=" + url.QueryEscape(schema)
	if err := store.Migrate(t.Context(), store.MigrationConfig{URL: testURL, LockTimeout: 5 * time.Second, Migrations: store.Migrations}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool), pool
}

func seedGitHubServerActor(t *testing.T, pool *pgxpool.Pool) service.ActorContext {
	orgID, userID := "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000091"
	if _, err := pool.Exec(t.Context(), `INSERT INTO orgs(id,name,slug) VALUES($1,'Acme','acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($2,$1,'admin@example.test','hash','Admin',true)`, orgID, userID); err != nil {
		t.Fatal(err)
	}
	return service.ActorContext{UserID: userID, OrgID: orgID, SessionID: "session", Operator: true}
}

func seedReadyGitHubServerApp(t *testing.T, dataStore *store.Store, actor service.ActorContext) {
	session, err := dataStore.BeginGitHubSetupSession(t.Context(), strings.Repeat("a", 64), strings.Repeat("c", 64), "app_setup", actor.UserID, strings.Repeat("b", 64), nil, nil, time.Now().Add(10*time.Minute), store.GitHubAuditContext{OrgID: actor.OrgID, Event: &store.AuditEvent{ID: "00000000-0000-7000-8000-000000000106", ActorUserID: &actor.UserID}, Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	audit := store.GitHubAuditContext{OrgID: actor.OrgID, Event: &store.AuditEvent{ID: "00000000-0000-7000-8000-000000000102", ActorUserID: &actor.UserID}, Source: "test"}
	if err := dataStore.StartGitHubAppExchange(t.Context(), session.ID, audit); err != nil {
		t.Fatal(err)
	}
	audit.Event.ID = "00000000-0000-7000-8000-000000000103"
	if err := dataStore.RecordGitHubAppMetadata(t.Context(), session.ID, store.GitHubAppMetadata{AppID: 42, OwnerID: 7, Slug: "cyclopus", Name: "Cyclopus", HTMLURL: "https://github.com/apps/cyclopus", OwnerLogin: "acme", OwnerType: "Organization"}, audit); err != nil {
		t.Fatal(err)
	}
	audit.Event.ID = "00000000-0000-7000-8000-000000000104"
	if err := dataStore.MarkGitHubAppSecretWrite(t.Context(), session.ID, true, audit); err != nil {
		t.Fatal(err)
	}
	audit.Event.ID = "00000000-0000-7000-8000-000000000105"
	if err := dataStore.ActivateGitHubApp(t.Context(), session.ID, "k8s-secret://cyclopus/cyclopus-github-app#private-key.pem", audit); err != nil {
		t.Fatal(err)
	}
}

func serverPrivateKey(t *testing.T) []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
