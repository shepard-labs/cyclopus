//go:build integration

package server

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	internalauth "github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/store"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type e2eClients struct {
	auth           cyclopusv1.AuthenticationServiceClient
	organization   cyclopusv1.OrganizationServiceClient
	project        cyclopusv1.ProjectServiceClient
	application    cyclopusv1.ApplicationServiceClient
	environment    cyclopusv1.EnvironmentServiceClient
	release        cyclopusv1.ReleaseServiceClient
	reconciliation cyclopusv1.ReconciliationServiceClient
	webhook        cyclopusv1.WebhookServiceClient
	token          string
	orgID          string
	userID         string
}

func (c e2eClients) sessionCtx(ctx context.Context) context.Context {
	return bearerContext(ctx, c.token)
}

func newE2EConn(t *testing.T, fixture apiFixture) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := newGRPCServer(fixture.services)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func newE2EClients(t *testing.T, fixture apiFixture, email string) e2eClients {
	t.Helper()
	conn := newE2EConn(t, fixture)
	clients := e2eClients{
		auth:           cyclopusv1.NewAuthenticationServiceClient(conn),
		organization:   cyclopusv1.NewOrganizationServiceClient(conn),
		project:        cyclopusv1.NewProjectServiceClient(conn),
		application:    cyclopusv1.NewApplicationServiceClient(conn),
		environment:    cyclopusv1.NewEnvironmentServiceClient(conn),
		release:        cyclopusv1.NewReleaseServiceClient(conn),
		reconciliation: cyclopusv1.NewReconciliationServiceClient(conn),
		webhook:        cyclopusv1.NewWebhookServiceClient(conn),
	}
	if _, err := clients.auth.Bootstrap(t.Context(), &cyclopusv1.BootstrapRequest{
		Email: email, Password: "e2e password", BootstrapToken: serverIntegrationBootstrap,
	}); err != nil {
		t.Fatalf("Bootstrap() = %v; %s", err, fixture.bootstrapFailureState(t))
	}
	login, err := clients.auth.Login(t.Context(), &cyclopusv1.LoginRequest{Email: email, Password: "e2e password"})
	if err != nil {
		t.Fatal(err)
	}
	clients.token = login.GetSession().GetAccessToken()
	clients.orgID = login.GetSession().GetUser().GetOrganizationId()
	clients.userID = login.GetSession().GetUser().GetId()
	return clients
}

func queryString(t *testing.T, pool *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var value string
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func queryInt(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var value int
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func queryBool(t *testing.T, pool *pgxpool.Pool, query string, args ...any) bool {
	t.Helper()
	var value bool
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func mustExec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func requireCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func requireErrorReason(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	requireCode(t, err, code)
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == reason {
			return
		}
	}
	t.Fatalf("error %v lacks ErrorInfo reason %q", err, reason)
}

func finalizeReleaseLive(t *testing.T, fixture apiFixture, dataStore *store.Store, orgID, envID, releaseID, userID, hostname string) {
	t.Helper()
	var fence time.Time
	if err := fixture.pool.QueryRow(t.Context(), `SELECT lock_acquired_at FROM environments WHERE id=$1`, envID).Scan(&fence); err != nil {
		t.Fatal(err)
	}
	_, err := dataStore.FinalizeReleaseLive(t.Context(), store.FinalizeReleaseParams{
		Lease: store.DeployLease{OrgID: orgID, EnvironmentID: envID, ReleaseID: releaseID,
			Fence: fence, ExpiresAt: fence.Add(10 * time.Minute)},
		Hostname: hostname,
		Audit:    &store.AuditEvent{ID: uuid.NewString(), ActorUserID: &userID},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedGitIntegration(t *testing.T, fixture apiFixture, orgID, projectID string, appIDs ...string) {
	t.Helper()
	var gitID string
	mustExec(t, fixture.pool, `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(12345,$1,12346,'acme','Organization','selected','active',1,1)`, orgID)
	mustExec(t, fixture.pool, `INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(12347,$1,12345,'api-node','acme','api','acme/api','private','main','active',1,1)`, orgID)
	if err := fixture.pool.QueryRow(t.Context(), `INSERT INTO git_connections(org_id,github_repository_id) VALUES($1,12347) RETURNING id::text`, orgID).Scan(&gitID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, fixture.pool, `UPDATE projects SET git_connection_id=$2 WHERE id=$1`, projectID, gitID)
	for _, appID := range appIDs {
		mustExec(t, fixture.pool, `UPDATE apps SET github_workflow_path='.github/workflows/build.yml',production_branch='main' WHERE id=$1`, appID)
	}
}

func claimLifecycleJob(t *testing.T, dataStore *store.Store, orgID string) store.EnvironmentLifecycleJob {
	t.Helper()
	job, err := dataStore.ClaimEnvironmentLifecycleJob(t.Context(), orgID, "e2e-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func completeTeardown(t *testing.T, dataStore *store.Store, orgID, environmentID string) {
	t.Helper()
	job := claimLifecycleJob(t, dataStore, orgID)
	if job.EnvironmentID != environmentID || job.Kind != "teardown" {
		t.Fatalf("claimed lifecycle job kind=%s environment=%s, want teardown/%s", job.Kind, job.EnvironmentID, environmentID)
	}
	if err := dataStore.CompleteEnvironmentTeardown(t.Context(), job); err != nil {
		t.Fatal(err)
	}
}

func TestE2EAuthIdentity(t *testing.T) {
	fixture := newAPIFixture(t, "e2eauth")
	conn := newE2EConn(t, fixture)
	authClient := cyclopusv1.NewAuthenticationServiceClient(conn)
	ctx := t.Context()

	var session *cyclopusv1.Session
	t.Run("Bootstrap", func(t *testing.T) {
		if _, err := authClient.Bootstrap(ctx, &cyclopusv1.BootstrapRequest{
			Email: "Operator@Example.com", Password: "e2e password", BootstrapToken: "wrong-token",
		}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Bootstrap() wrong token error = %v", err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM users`); count != 0 {
			t.Fatalf("users after failed bootstrap = %d", count)
		}
		response, err := authClient.Bootstrap(ctx, &cyclopusv1.BootstrapRequest{
			Email: "Operator@Example.com", Password: "e2e password", BootstrapToken: serverIntegrationBootstrap,
		})
		if err != nil {
			t.Fatalf("Bootstrap() = %v; %s", err, fixture.bootstrapFailureState(t))
		}
		session = response.GetSession()
		if session.GetAccessToken() == "" || session.GetExpiresAt() == nil || session.GetUser().GetId() == "" {
			t.Fatalf("Bootstrap() session = %+v", session)
		}
		if name, slug := queryString(t, fixture.pool, `SELECT name FROM orgs`), queryString(t, fixture.pool, `SELECT slug FROM orgs`); name != "My Organization" || slug != "my-organization" {
			t.Fatalf("orgs row = %q/%q", name, slug)
		}
		if !queryBool(t, fixture.pool, `SELECT operator AND email='operator@example.com' AND org_id::text=$1 FROM users WHERE id=$2`,
			session.GetUser().GetOrganizationId(), session.GetUser().GetId()) {
			t.Fatal("users row not lowercased operator")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM api_tokens WHERE kind='session' AND scopes='{}'::text[] AND expires_at IS NOT NULL`); count != 1 {
			t.Fatalf("session token rows = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='auth.bootstrap'`); count != 1 {
			t.Fatalf("auth.bootstrap audit rows = %d", count)
		}
	})

	t.Run("BootstrapOnlyOnce", func(t *testing.T) {
		// internal/auth/service.go:111-119 returns ErrUnauthenticated once the installation is claimed
		if _, err := authClient.Bootstrap(ctx, &cyclopusv1.BootstrapRequest{
			Email: "second@example.com", Password: "e2e password", BootstrapToken: serverIntegrationBootstrap,
		}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("second Bootstrap() error = %v", err)
		}
	})

	var loginToken string
	var otherLoginToken string
	t.Run("Login", func(t *testing.T) {
		tokensBefore := queryInt(t, fixture.pool, `SELECT count(*) FROM api_tokens`)
		if _, err := authClient.Login(ctx, &cyclopusv1.LoginRequest{Email: "operator@example.com", Password: "wrong password"}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Login() wrong password error = %v", err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM api_tokens`); count != tokensBefore {
			t.Fatalf("api_tokens after failed login = %d, want %d", count, tokensBefore)
		}
		login, err := authClient.Login(ctx, &cyclopusv1.LoginRequest{Email: "operator@example.com", Password: "e2e password"})
		if err != nil {
			t.Fatal(err)
		}
		loginToken = login.GetSession().GetAccessToken()
		otherLogin, err := authClient.Login(ctx, &cyclopusv1.LoginRequest{Email: "operator@example.com", Password: "e2e password"})
		if err != nil {
			t.Fatal(err)
		}
		otherLoginToken = otherLogin.GetSession().GetAccessToken()
		current, err := authClient.GetCurrentUser(bearerContext(ctx, loginToken), &cyclopusv1.GetCurrentUserRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var id, orgID, email string
		if err := fixture.pool.QueryRow(ctx, `SELECT id::text, org_id::text, email FROM users`).Scan(&id, &orgID, &email); err != nil {
			t.Fatal(err)
		}
		if current.GetUser().GetId() != id || current.GetUser().GetOrganizationId() != orgID || current.GetUser().GetEmail() != email {
			t.Fatalf("GetCurrentUser() = %+v, want %s/%s/%s", current.GetUser(), id, orgID, email)
		}
		prefix := loginToken[len("cyp_v1_") : len("cyp_v1_")+12]
		if !queryBool(t, fixture.pool, `SELECT last_used_at IS NOT NULL AND last_used_at >= created_at FROM api_tokens WHERE token_prefix=$1`, prefix) {
			t.Fatal("session token last_used_at not touched after authenticated call")
		}
	})

	t.Run("UpdateProfile", func(t *testing.T) {
		updated, err := authClient.UpdateCurrentUser(bearerContext(ctx, loginToken), &cyclopusv1.UpdateCurrentUserRequest{
			Email: "updated@example.com", FirstName: "Octo", LastName: "Pod", DisplayName: "Captain Critter",
			AvatarKey: "21d7a04043", CurrentPassword: "wrong password",
		})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("UpdateCurrentUser() wrong password error = %v", err)
		}
		updated, err = authClient.UpdateCurrentUser(bearerContext(ctx, loginToken), &cyclopusv1.UpdateCurrentUserRequest{
			Email: "updated@example.com", FirstName: "Octo", LastName: "Pod", DisplayName: "Captain Critter",
			AvatarKey: "21d7a04043", CurrentPassword: "e2e password",
		})
		if err != nil {
			t.Fatal(err)
		}
		user := updated.GetUser()
		if user.GetEmail() != "updated@example.com" || user.GetFirstName() != "Octo" || user.GetLastName() != "Pod" ||
			user.GetDisplayName() != "Captain Critter" || user.GetAvatarKey() != "21d7a04043" {
			t.Fatalf("UpdateCurrentUser() = %+v", user)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='user.profile_updated'`); count != 1 {
			t.Fatalf("user.profile_updated audit rows = %d", count)
		}
	})

	t.Run("ChangePassword", func(t *testing.T) {
		if _, err := authClient.ChangePassword(bearerContext(ctx, loginToken), &cyclopusv1.ChangePasswordRequest{
			CurrentPassword: "wrong password", NewPassword: "new e2e password",
		}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("ChangePassword() wrong password error = %v", err)
		}
		if _, err := authClient.ChangePassword(bearerContext(ctx, loginToken), &cyclopusv1.ChangePasswordRequest{
			CurrentPassword: "e2e password", NewPassword: "new e2e password",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := authClient.GetCurrentUser(bearerContext(ctx, loginToken), &cyclopusv1.GetCurrentUserRequest{}); err != nil {
			t.Fatalf("current session after password change = %v", err)
		}
		if _, err := authClient.GetCurrentUser(bearerContext(ctx, otherLoginToken), &cyclopusv1.GetCurrentUserRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("other session after password change error = %v", err)
		}
		if _, err := authClient.Login(ctx, &cyclopusv1.LoginRequest{Email: "updated@example.com", Password: "e2e password"}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("old password login error = %v", err)
		}
		if _, err := authClient.Login(ctx, &cyclopusv1.LoginRequest{Email: "updated@example.com", Password: "new e2e password"}); err != nil {
			t.Fatalf("new password login = %v", err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='user.password_changed'`); count != 1 {
			t.Fatalf("user.password_changed audit rows = %d", count)
		}
	})

	t.Run("Logout", func(t *testing.T) {
		prefix := loginToken[len("cyp_v1_") : len("cyp_v1_")+12]
		if _, err := authClient.Logout(bearerContext(ctx, loginToken), &cyclopusv1.LogoutRequest{}); err != nil {
			t.Fatal(err)
		}
		if !queryBool(t, fixture.pool, `SELECT revoked_at IS NOT NULL FROM api_tokens WHERE token_prefix=$1`, prefix) {
			t.Fatal("logout did not revoke the caller token")
		}
		if _, err := authClient.GetCurrentUser(bearerContext(ctx, loginToken), &cyclopusv1.GetCurrentUserRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("GetCurrentUser() after logout error = %v", err)
		}
	})
	_ = session
}

func TestE2EApiTokens(t *testing.T) {
	fixture := newAPIFixture(t, "e2etokens")
	clients := newE2EClients(t, fixture, "tokens@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)

	project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "project-1",
		Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryID := fixture.insertRegistry(t, clients.orgID)
	appA, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-a", Application: validApplication(registryID),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := validApplication(registryID)
	worker.Name, worker.Slug, worker.HostnameLabel = "Worker", "worker", "worker"
	appB, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-b", Application: worker,
	})
	if err != nil {
		t.Fatal(err)
	}
	environmentID := fixture.insertEnvironment(t, clients.orgID, appA.GetApplication().GetId())
	scopeA, err := internalauth.DeployAppScope(appA.GetApplication().GetId())
	if err != nil {
		t.Fatal(err)
	}
	scopeB, err := internalauth.DeployAppScope(appB.GetApplication().GetId())
	if err != nil {
		t.Fatal(err)
	}

	var tokenAID, tokenA string
	t.Run("CreateReplayListAndScopeEnforcement", func(t *testing.T) {
		created, err := clients.auth.CreateApiToken(sessionCtx, &cyclopusv1.CreateApiTokenRequest{
			Name: "CI", Scopes: []string{scopeA}, IdempotencyKey: "token-1",
		})
		if err != nil || created.GetResult() != cyclopusv1.CreateApiTokenResult_CREATE_API_TOKEN_RESULT_CREATED {
			t.Fatalf("CreateApiToken() = %+v, %v", created, err)
		}
		if !strings.HasPrefix(created.GetToken(), "cyp_v1_") || created.GetApiToken().GetId() == "" {
			t.Fatalf("CreateApiToken() token = %+v", created)
		}
		tokenAID, tokenA = created.GetApiToken().GetId(), created.GetToken()
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM api_tokens WHERE id=$1 AND kind='api' AND name='CI'
			AND scopes=ARRAY[$2]::text[] AND octet_length(token_prefix)=12 AND token_hash ~ '^[0-9a-f]{64}$' AND revoked_at IS NULL`, tokenAID, scopeA); count != 1 {
			t.Fatalf("persisted api token rows = %d", count)
		}
		if queryBool(t, fixture.pool, `SELECT EXISTS (
			SELECT 1 FROM api_idempotency_records WHERE response_body::text LIKE '%cyp_v1_%' OR response_body::text LIKE '%'||$1||'%'
			UNION ALL SELECT 1 FROM audit_events WHERE coalesce(before::text,'') LIKE '%cyp_v1_%' OR coalesce(after::text,'') LIKE '%cyp_v1_%')`, tokenA) {
			t.Fatal("plaintext bearer secret persisted in idempotency or audit JSON")
		}
		replayed, err := clients.auth.CreateApiToken(sessionCtx, &cyclopusv1.CreateApiTokenRequest{
			Name: "CI", Scopes: []string{scopeA}, IdempotencyKey: "token-1",
		})
		if err != nil || replayed.GetToken() != "" || replayed.GetApiToken().GetId() != tokenAID ||
			replayed.GetResult() != cyclopusv1.CreateApiTokenResult_CREATE_API_TOKEN_RESULT_ALREADY_CREATED_SECRET_NOT_REPLAYABLE {
			t.Fatalf("CreateApiToken() replay = %+v, %v", replayed, err)
		}
		listed, err := clients.auth.ListApiTokens(sessionCtx, &cyclopusv1.ListApiTokensRequest{})
		if err != nil || len(listed.GetApiTokens()) != 1 {
			t.Fatalf("ListApiTokens() = %+v, %v", listed, err)
		}
		listedToken := listed.GetApiTokens()[0]
		if listedToken.GetId() != tokenAID || listedToken.GetName() != "CI" || len(listedToken.GetScopes()) != 1 || listedToken.GetScopes()[0] != scopeA {
			t.Fatalf("ListApiTokens() token = %+v", listedToken)
		}
		if _, err := clients.project.ListProjects(bearerContext(ctx, tokenA), &cyclopusv1.ListProjectsRequest{OrganizationId: clients.orgID}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("ListProjects() with deploy token error = %v", err)
		}
	})

	t.Run("ScopedDeployAndRevocation", func(t *testing.T) {
		tokenB, err := clients.auth.CreateApiToken(sessionCtx, &cyclopusv1.CreateApiTokenRequest{
			Name: "CI B", Scopes: []string{scopeB}, IdempotencyKey: "token-2",
		})
		if err != nil {
			t.Fatal(err)
		}
		digest := strings.Repeat("a", 64)
		imageRef := "ghcr.io/acme/api@sha256:" + digest
		if _, err := clients.release.Deploy(bearerContext(ctx, tokenB.GetToken()), &cyclopusv1.DeployRequest{
			EnvironmentId: environmentID, ImageRef: imageRef, GitCommitSha: strings.Repeat("b", 40), IdempotencyKey: "deploy-wrong-scope",
		}); status.Code(err) != codes.NotFound {
			t.Fatalf("Deploy() with wrong app scope error = %v", err)
		}
		deploy, err := clients.release.Deploy(bearerContext(ctx, tokenA), &cyclopusv1.DeployRequest{
			EnvironmentId: environmentID, ImageRef: imageRef, GitCommitSha: strings.Repeat("b", 40), IdempotencyKey: "deploy-scoped",
		})
		if err != nil || deploy.GetOperation().GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED {
			t.Fatalf("Deploy() with matching scope = %+v, %v", deploy, err)
		}
		if _, err := clients.auth.RevokeApiToken(sessionCtx, &cyclopusv1.RevokeApiTokenRequest{
			ApiTokenId: tokenAID, IdempotencyKey: "token-revoke-1",
		}); err != nil {
			t.Fatal(err)
		}
		if !queryBool(t, fixture.pool, `SELECT revoked_at IS NOT NULL FROM api_tokens WHERE id=$1`, tokenAID) {
			t.Fatal("RevokeApiToken() did not set revoked_at")
		}
		if _, err := clients.release.Deploy(bearerContext(ctx, tokenA), &cyclopusv1.DeployRequest{
			EnvironmentId: environmentID, ImageRef: imageRef, IdempotencyKey: "deploy-revoked",
		}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Deploy() with revoked token error = %v", err)
		}
	})
}

func TestE2EProjects(t *testing.T) {
	fixture := newAPIFixture(t, "e2eprojects")
	clients := newE2EClients(t, fixture, "projects@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)

	var projectID string
	t.Run("CreatePersistsAndReplays", func(t *testing.T) {
		request := &cyclopusv1.CreateProjectRequest{
			OrganizationId: clients.orgID, IdempotencyKey: "project-1",
			Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
		}
		created, err := clients.project.CreateProject(sessionCtx, request)
		if err != nil {
			t.Fatal(err)
		}
		projectID = created.GetProject().GetId()
		if projectID == "" || created.GetProject().GetName() != "Platform" || created.GetProject().GetSlug() != "platform" ||
			created.GetProject().GetOrganizationId() != clients.orgID {
			t.Fatalf("CreateProject() = %+v", created.GetProject())
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM projects WHERE id=$1 AND org_id=$2 AND name='Platform'
			AND slug='platform' AND git_connection_id IS NULL AND deleted_at IS NULL AND external_credentials_revoked_at IS NULL`, projectID, clients.orgID); count != 1 {
			t.Fatalf("projects rows = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='project.created' AND target_id=$1
			AND after->>'Name'='Platform' AND after->>'Slug'='platform'`, projectID); count != 1 {
			t.Fatalf("project.created audit rows = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM api_idempotency_records WHERE scope='project.create'
			AND idempotency_key='project-1' AND status='completed' AND resource_type='project' AND resource_id=$1`, projectID); count != 1 {
			t.Fatalf("project.create idempotency rows = %d", count)
		}
		replayed, err := clients.project.CreateProject(sessionCtx, request)
		if err != nil || replayed.GetProject().GetId() != projectID {
			t.Fatalf("CreateProject() replay = %+v, %v", replayed, err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM projects`); count != 1 {
			t.Fatalf("projects after replay = %d", count)
		}
	})

	t.Run("DuplicateSlugRejected", func(t *testing.T) {
		_, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
			OrganizationId: clients.orgID, IdempotencyKey: "project-2",
			Project: &cyclopusv1.Project{Name: "Platform Copy", Slug: "platform"},
		})
		requireCode(t, err, codes.AlreadyExists)
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM projects`); count != 1 {
			t.Fatalf("projects after duplicate slug = %d", count)
		}
	})

	t.Run("UpdateMaskName", func(t *testing.T) {
		if _, err := clients.project.UpdateProject(sessionCtx, &cyclopusv1.UpdateProjectRequest{
			Project:        &cyclopusv1.Project{Id: projectID, Name: "Platform Renamed"},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"name"}},
			IdempotencyKey: "project-3",
		}); err != nil {
			t.Fatal(err)
		}
		if !queryBool(t, fixture.pool, `SELECT name='Platform Renamed' AND slug='platform' FROM projects WHERE id=$1`, projectID) {
			t.Fatal("UpdateProject mask {name} changed unexpected columns")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='project.updated' AND target_id=$1
			AND before->>'name'='Platform' AND after->>'Name'='Platform Renamed'`, projectID); count != 1 {
			t.Fatalf("project.updated audit rows = %d", count)
		}
	})
}

func TestE2EProjectPagination(t *testing.T) {
	fixture := newAPIFixture(t, "e2epagination")
	clients := newE2EClients(t, fixture, "pagination@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)

	created := make([]string, 0, 3)
	for index, slug := range []string{"alpha", "beta", "gamma"} {
		project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
			OrganizationId: clients.orgID, IdempotencyKey: "page-" + slug,
			Project: &cyclopusv1.Project{Name: "Project " + string(rune('A'+index)), Slug: slug},
		})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, project.GetProject().GetId())
	}
	var listed []string
	pageToken := ""
	for page := 0; page < 5; page++ {
		response, err := clients.project.ListProjects(sessionCtx, &cyclopusv1.ListProjectsRequest{
			OrganizationId: clients.orgID, PageSize: 1, PageToken: pageToken,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.GetProjects()) != 1 {
			t.Fatalf("page %d returned %d projects", page, len(response.GetProjects()))
		}
		listed = append(listed, response.GetProjects()[0].GetId())
		pageToken = response.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}
	if len(listed) != 3 {
		t.Fatalf("paginated %d projects, want 3", len(listed))
	}
	for index := range created {
		if listed[index] != created[index] {
			t.Fatalf("page order = %v, want creation order %v", listed, created)
		}
	}
	if pageToken != "" {
		t.Fatalf("unexpected 4th page token %q", pageToken)
	}
}

func TestE2EApplications(t *testing.T) {
	fixture := newAPIFixture(t, "e2eapps")
	clients := newE2EClients(t, fixture, "apps@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)

	project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "project-1",
		Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryID := fixture.insertRegistry(t, clients.orgID)

	var appID string
	t.Run("CreatePersistsEveryColumn", func(t *testing.T) {
		created, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
			ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-1", Application: validApplication(registryID),
		})
		if err != nil {
			t.Fatal(err)
		}
		app := created.GetApplication()
		appID = app.GetId()
		if appID == "" || app.GetOrganizationId() != clients.orgID || app.GetProjectId() != project.GetProject().GetId() ||
			app.GetName() != "API" || app.GetSlug() != "api" || app.GetHostnameLabel() != "api" ||
			app.GetRegistryId() != registryID || app.GetImageRepository() != "acme/api" ||
			app.GetContainerPort() != 8080 || app.GetHealthCheckPath() != "/healthz" ||
			app.GetScalingMode() != cyclopusv1.ScalingMode_SCALING_MODE_HPA ||
			app.GetMinReplicas() != 1 || app.GetMaxReplicas() != 3 || app.GetHpaTargetCpuPercent() != 70 ||
			app.GetCpuRequestMillicores() != 100 || app.GetCpuLimitMillicores() != 1000 ||
			app.GetMemoryRequestMib() != 128 || app.GetMemoryLimitMib() != 512 ||
			!app.GetPreviewsEnabled() || app.GetMaxPreviewEnvironments() != 10 || app.GetPreviewIdleTtlHours() != 168 ||
			app.GetDeployTimeoutSeconds() != 600 || app.GetReleaseRetentionCount() != 5 ||
			app.GetRuntimeClass() != cyclopusv1.RuntimeClass_RUNTIME_CLASS_GVISOR {
			t.Fatalf("CreateApplication() = %+v", app)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM apps WHERE id=$1 AND org_id=$2 AND project_id=$3
			AND name='API' AND slug='api' AND hostname_label='api' AND registry_id=$4 AND image_repository='ghcr.io/acme/api'
			AND container_port=8080 AND health_check_path='/healthz' AND scaling_mode='hpa'
			AND min_replicas=1 AND max_replicas=3 AND hpa_target_cpu_percent=70
			AND cpu_request_millicores=100 AND cpu_limit_millicores=1000
			AND memory_request_mib=128 AND memory_limit_mib=512
			AND previews_enabled AND max_preview_environments=10 AND preview_idle_ttl_hours=168
			AND deploy_timeout_seconds=600 AND release_retention_count=5 AND runtime_class='gvisor'
			AND github_workflow_path='.github/workflows/build.yml' AND production_branch='main'
			AND deleted_at IS NULL AND external_credentials_revoked_at IS NULL`,
			appID, clients.orgID, project.GetProject().GetId(), registryID); count != 1 {
			t.Fatalf("apps row mismatch, count = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='app.created' AND target_id=$1`, appID); count != 1 {
			t.Fatalf("app.created audit rows = %d", count)
		}
	})

	t.Run("ValidationRejectionsConsumeNothing", func(t *testing.T) {
		appsBefore := queryInt(t, fixture.pool, `SELECT count(*) FROM apps`)
		hpaZero := validApplication(registryID)
		hpaZero.MinReplicas = 0
		portZero := validApplication(registryID)
		portZero.ContainerPort = 0
		cpuInverted := validApplication(registryID)
		cpuInverted.CpuLimitMillicores = 50
		registryOnly := validApplication(registryID)
		registryOnly.ImageRepository = ""
		for index, bad := range []*cyclopusv1.Application{hpaZero, portZero, cpuInverted, registryOnly} {
			bad.Slug = "bad"
			_, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
				ProjectId: project.GetProject().GetId(), IdempotencyKey: "bad-app-" + string(rune('1'+index)), Application: bad,
			})
			requireCode(t, err, codes.InvalidArgument)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM apps`); count != appsBefore {
			t.Fatalf("apps after invalid creates = %d, want %d", count, appsBefore)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM api_idempotency_records WHERE idempotency_key IN ('bad-app-1','bad-app-2','bad-app-3','bad-app-4')`); count != 0 {
			t.Fatalf("invalid creates consumed idempotency claims = %d", count)
		}
	})

	t.Run("UpdateMask", func(t *testing.T) {
		if _, err := clients.application.UpdateApplication(sessionCtx, &cyclopusv1.UpdateApplicationRequest{
			Application:    &cyclopusv1.Application{Id: appID, HealthCheckPath: "/ready", MaxReplicas: 9},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"health_check_path", "max_replicas"}},
			IdempotencyKey: "app-2",
		}); err != nil {
			t.Fatal(err)
		}
		if !queryBool(t, fixture.pool, `SELECT health_check_path='/ready' AND max_replicas=9
			AND container_port=8080 AND slug='api' AND scaling_mode='hpa' AND runtime_class='gvisor' FROM apps WHERE id=$1`, appID) {
			t.Fatal("UpdateApplication mask changed unexpected columns")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='app.updated' AND target_id=$1
			AND after->>'HealthCheckPath'='/ready'`, appID); count != 1 {
			t.Fatalf("app.updated audit rows = %d", count)
		}
	})
}

func TestE2EPreviewLifecycle(t *testing.T) {
	fixture := newAPIFixture(t, "e2epreview")
	clients := newE2EClients(t, fixture, "preview@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)
	dataStore := store.New(fixture.pool)

	project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "project-1",
		Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryID := fixture.insertRegistry(t, clients.orgID)
	app, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-1", Application: validApplication(registryID),
	})
	if err != nil {
		t.Fatal(err)
	}
	appID := app.GetApplication().GetId()
	seedGitIntegration(t, fixture, clients.orgID, project.GetProject().GetId(), appID)

	var previewEnvID string
	t.Run("FirstPushReservesEnvironment", func(t *testing.T) {
		commit := strings.Repeat("a", 40)
		push, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appID, Branch: "feature-x", CommitSha: commit, IdempotencyKey: "push-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if push.GetDisposition() != "reserved" || push.GetDropped() ||
			push.GetEnvironment().GetKind() != cyclopusv1.EnvironmentKind_ENVIRONMENT_KIND_PREVIEW ||
			push.GetEnvironment().GetLifecycleStatus() != cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_RESERVED {
			t.Fatalf("AcceptPreviewPush() = %+v", push)
		}
		previewEnvID = push.GetEnvironment().GetId()
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM environments WHERE id=$1 AND kind='preview' AND branch='feature-x'
			AND preview_slug IS NOT NULL AND namespace <> '' AND planned_hostname IS NULL AND lifecycle_status='reserved'`, previewEnvID); count != 1 {
			t.Fatalf("preview environment rows = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM environment_lifecycle_jobs WHERE environment_id=$1
			AND kind='provision' AND status='queued'`, previewEnvID); count != 1 {
			t.Fatalf("provision job rows = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM orchestration_jobs WHERE environment_id=$1
			AND status='queued' AND git_ref='refs/heads/feature-x' AND commit_sha=$2`, previewEnvID, commit); count != 1 {
			t.Fatalf("orchestration job rows = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM preview_lifecycle_requests WHERE idempotency_key='push-1'
			AND event_kind='push' AND disposition='reserved' AND environment_id=$1`, previewEnvID); count != 1 {
			t.Fatalf("preview_lifecycle_requests rows = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='preview.push_accepted' AND target_id=$1`, previewEnvID); count != 1 {
			t.Fatalf("preview.push_accepted audit rows = %d", count)
		}
	})

	t.Run("RepeatPushQueuesAndReplayStable", func(t *testing.T) {
		push, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appID, Branch: "feature-x", CommitSha: strings.Repeat("b", 40), IdempotencyKey: "push-2",
		})
		if err != nil || push.GetDisposition() != "queued" || push.GetEnvironment().GetId() != previewEnvID {
			t.Fatalf("repeat AcceptPreviewPush() = %+v, %v", push, err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM environments WHERE app_id=$1 AND kind='preview'`, appID); count != 1 {
			t.Fatalf("preview environments = %d", count)
		}
		replay, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appID, Branch: "feature-x", CommitSha: strings.Repeat("a", 40), IdempotencyKey: "push-1",
		})
		if err != nil || replay.GetDisposition() != "reserved" || replay.GetEnvironment().GetId() != previewEnvID {
			t.Fatalf("replay AcceptPreviewPush() = %+v, %v", replay, err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM preview_lifecycle_requests`); count != 2 {
			t.Fatalf("preview_lifecycle_requests after replay = %d, want 2", count)
		}
	})

	t.Run("PreviewsDisabledDrops", func(t *testing.T) {
		disabled := validApplication(registryID)
		disabled.Name, disabled.Slug, disabled.HostnameLabel = "No Preview", "no-preview", "no-preview"
		disabled.PreviewsEnabled = false
		appNoPreview, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
			ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-np", Application: disabled,
		})
		if err != nil {
			t.Fatal(err)
		}
		push, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appNoPreview.GetApplication().GetId(), Branch: "feature-y",
			CommitSha: strings.Repeat("c", 40), IdempotencyKey: "push-3",
		})
		if err != nil || !push.GetDropped() || push.GetDisposition() != "dropped" {
			t.Fatalf("AcceptPreviewPush() disabled = %+v, %v", push, err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM environments WHERE app_id=$1`, appNoPreview.GetApplication().GetId()); count != 0 {
			t.Fatalf("dropped push created %d environments", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='preview.push_dropped' AND target_id=$1`, appNoPreview.GetApplication().GetId()); count != 1 {
			t.Fatalf("preview.push_dropped audit rows = %d", count)
		}
	})

	t.Run("DefaultBranchRejected", func(t *testing.T) {
		_, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appID, Branch: "main", CommitSha: strings.Repeat("d", 40), IdempotencyKey: "push-4",
		})
		requireCode(t, err, codes.InvalidArgument)
	})

	t.Run("PreviewCapEnforced", func(t *testing.T) {
		capped := validApplication(registryID)
		capped.Name, capped.Slug, capped.HostnameLabel = "Capped", "capped", "capped"
		capped.MaxPreviewEnvironments = 1
		appCapped, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
			ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-cap", Application: capped,
		})
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, fixture.pool, `UPDATE apps SET github_workflow_path='.github/workflows/build.yml' WHERE id=$1`, appCapped.GetApplication().GetId())
		if _, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appCapped.GetApplication().GetId(), Branch: "cap-a",
			CommitSha: strings.Repeat("e", 40), IdempotencyKey: "push-5",
		}); err != nil {
			t.Fatal(err)
		}
		_, err = clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appCapped.GetApplication().GetId(), Branch: "cap-b",
			CommitSha: strings.Repeat("f", 40), IdempotencyKey: "push-6",
		})
		requireCode(t, err, codes.FailedPrecondition)
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM environments WHERE app_id=$1`, appCapped.GetApplication().GetId()); count != 1 {
			t.Fatalf("capped app environments = %d", count)
		}
	})

	t.Run("ProvisionCompletionMarksReady", func(t *testing.T) {
		job := claimLifecycleJob(t, dataStore, clients.orgID)
		if job.EnvironmentID != previewEnvID || job.Kind != "provision" {
			t.Fatalf("claimed lifecycle job kind=%s environment=%s, want provision/%s", job.Kind, job.EnvironmentID, previewEnvID)
		}
		hostname := "feature-x.preview.example.test"
		if err := dataStore.CompleteEnvironmentProvision(ctx, job, &hostname); err != nil {
			t.Fatal(err)
		}
		if !queryBool(t, fixture.pool, `SELECT lifecycle_status='ready' AND planned_hostname=$2 FROM environments WHERE id=$1`, previewEnvID, hostname) {
			t.Fatal("provisioned environment not ready with planned hostname")
		}
		if !queryBool(t, fixture.pool, `SELECT status='completed' AND checkpoint='ready' AND completed_at IS NOT NULL
			FROM environment_lifecycle_jobs WHERE id=$1`, job.ID) {
			t.Fatal("provision job not completed at ready checkpoint")
		}
		environment, err := clients.environment.GetEnvironment(sessionCtx, &cyclopusv1.GetEnvironmentRequest{EnvironmentId: previewEnvID})
		if err != nil {
			t.Fatal(err)
		}
		if environment.GetEnvironment().GetLifecycleStatus() != cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_READY ||
			environment.GetEnvironment().GetPlannedHostname() != hostname {
			t.Fatalf("GetEnvironment() = %+v", environment.GetEnvironment())
		}
	})
}

func TestE2EDeployReleases(t *testing.T) {
	fixture := newAPIFixture(t, "e2edeploy")
	clients := newE2EClients(t, fixture, "deploy@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)
	dataStore := store.New(fixture.pool)

	project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "project-1",
		Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryID := fixture.insertRegistry(t, clients.orgID)
	app, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-1", Application: validApplication(registryID),
	})
	if err != nil {
		t.Fatal(err)
	}
	environmentID := fixture.insertEnvironment(t, clients.orgID, app.GetApplication().GetId())
	imageA := "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64)
	imageB := "ghcr.io/acme/api@sha256:" + strings.Repeat("b", 64)
	imageC := "ghcr.io/acme/api@sha256:" + strings.Repeat("c", 64)
	commitA := strings.Repeat("1", 40)

	var releaseOne, releaseTwo, releaseThree string
	t.Run("DeployAcceptsAndPersists", func(t *testing.T) {
		deploy, err := clients.release.Deploy(sessionCtx, &cyclopusv1.DeployRequest{
			EnvironmentId: environmentID, ImageRef: imageA, GitCommitSha: commitA, IdempotencyKey: "deploy-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		operation := deploy.GetOperation()
		if operation.GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED ||
			operation.GetResourceName() != "releases/"+operation.GetId() {
			t.Fatalf("Deploy() operation = %+v", operation)
		}
		releaseOne = operation.GetId()
		var statusValue, kind, storedImage, storedCommit, configHash string
		var snapshot []byte
		if err := fixture.pool.QueryRow(ctx, `SELECT status, kind, image_ref, git_commit_sha, config_hash, config_snapshot
			FROM releases WHERE id=$1`, releaseOne).Scan(&statusValue, &kind, &storedImage, &storedCommit, &configHash, &snapshot); err != nil {
			t.Fatal(err)
		}
		if statusValue != "pending" || kind != "deploy" || storedImage != imageA || storedCommit != commitA ||
			!queryBool(t, fixture.pool, `SELECT config_hash ~ '^[0-9a-f]{64}$' FROM releases WHERE id=$1`, releaseOne) {
			t.Fatalf("release row status=%s kind=%s image=%s commit=%s hash=%s", statusValue, kind, storedImage, storedCommit, configHash)
		}
		envelope, err := store.DecodeReleaseSnapshot(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		desired := envelope.DesiredState
		if desired.Deployment.Container.Port != 8080 || desired.Deployment.Container.ReadinessProbe.Path != "/healthz" ||
			desired.Deployment.RuntimeClassName != "gvisor" || desired.Ingress.Hostname != "api.example.test" ||
			desired.Scaling.Kind != "hpa" {
			t.Fatalf("accepted desired state = %+v", desired)
		}
		if !queryBool(t, fixture.pool, `SELECT in_progress_release_id=$2 AND lock_acquired_at IS NOT NULL
			FROM environments WHERE id=$1`, environmentID, releaseOne) {
			t.Fatal("deploy lease not persisted on environment")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='release.accepted' AND target_id=$1`, releaseOne); count != 1 {
			t.Fatalf("release.accepted audit rows = %d", count)
		}
	})

	t.Run("DeployRejectsInvalidImageRefs", func(t *testing.T) {
		releasesBefore := queryInt(t, fixture.pool, `SELECT count(*) FROM releases`)
		for index, imageRef := range []string{
			"ghcr.io/acme/api:latest",
			"gcr.io/acme/api@sha256:" + strings.Repeat("a", 64),
			"ghcr.io/acme/other@sha256:" + strings.Repeat("a", 64),
		} {
			_, err := clients.release.Deploy(sessionCtx, &cyclopusv1.DeployRequest{
				EnvironmentId: environmentID, ImageRef: imageRef, IdempotencyKey: "deploy-bad-" + string(rune('1'+index)),
			})
			requireCode(t, err, codes.InvalidArgument)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM releases`); count != releasesBefore {
			t.Fatalf("releases after invalid image refs = %d, want %d", count, releasesBefore)
		}
	})

	t.Run("DeployConflictWhileLeaseHeld", func(t *testing.T) {
		_, err := clients.release.Deploy(sessionCtx, &cyclopusv1.DeployRequest{
			EnvironmentId: environmentID, ImageRef: imageB, GitCommitSha: strings.Repeat("2", 40), IdempotencyKey: "deploy-2",
		})
		// store.AcceptRelease finds the lease held and returns ErrConflict -> handlers.go:883-884 Aborted CONFLICT
		requireErrorReason(t, err, codes.Aborted, "CONFLICT")
	})

	t.Run("FinalizeLive", func(t *testing.T) {
		finalizeReleaseLive(t, fixture, dataStore, clients.orgID, environmentID, releaseOne, clients.userID, "api.example.test")
		if !queryBool(t, fixture.pool, `SELECT status='live' FROM releases WHERE id=$1`, releaseOne) {
			t.Fatal("release not live after finalize")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM aliases WHERE environment_id=$1
			AND hostname='api.example.test' AND release_id=$2`, environmentID, releaseOne); count != 1 {
			t.Fatalf("aliases rows = %d", count)
		}
		if !queryBool(t, fixture.pool, `SELECT in_progress_release_id IS NULL AND lock_acquired_at IS NULL
			FROM environments WHERE id=$1`, environmentID) {
			t.Fatal("deploy lease not cleared after finalize")
		}
		environment, err := clients.environment.GetEnvironment(sessionCtx, &cyclopusv1.GetEnvironmentRequest{EnvironmentId: environmentID})
		if err != nil || environment.GetEnvironment().GetCurrentReleaseId() != releaseOne {
			t.Fatalf("GetEnvironment() = %+v, %v", environment, err)
		}
		release, err := clients.release.GetRelease(sessionCtx, &cyclopusv1.GetReleaseRequest{ReleaseId: releaseOne})
		if err != nil || release.GetRelease().GetStatus() != cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE {
			t.Fatalf("GetRelease() = %+v, %v", release, err)
		}
	})

	t.Run("SecondDeploySupersedes", func(t *testing.T) {
		deploy, err := clients.release.Deploy(sessionCtx, &cyclopusv1.DeployRequest{
			EnvironmentId: environmentID, ImageRef: imageB, GitCommitSha: strings.Repeat("2", 40), IdempotencyKey: "deploy-3",
		})
		if err != nil {
			t.Fatal(err)
		}
		releaseTwo = deploy.GetOperation().GetId()
		finalizeReleaseLive(t, fixture, dataStore, clients.orgID, environmentID, releaseTwo, clients.userID, "api.example.test")
		if !queryBool(t, fixture.pool, `SELECT status='superseded' FROM releases WHERE id=$1`, releaseOne) {
			t.Fatal("first release not superseded")
		}
		if !queryBool(t, fixture.pool, `SELECT release_id=$2 FROM aliases WHERE environment_id=$1`, environmentID, releaseTwo) {
			t.Fatal("alias not moved to second release")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM releases WHERE environment_id=$1 AND status='live'`, environmentID); count != 1 {
			t.Fatalf("live releases = %d", count)
		}
		listed, err := clients.release.ListReleases(sessionCtx, &cyclopusv1.ListReleasesRequest{EnvironmentId: environmentID})
		if err != nil || len(listed.GetReleases()) != 2 {
			t.Fatalf("ListReleases() = %+v, %v", listed, err)
		}
		if listed.GetReleases()[0].GetId() != releaseTwo || listed.GetReleases()[0].GetStatus() != cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE ||
			listed.GetReleases()[1].GetId() != releaseOne || listed.GetReleases()[1].GetStatus() != cyclopusv1.ReleaseStatus_RELEASE_STATUS_SUPERSEDED {
			t.Fatalf("ListReleases() order/status = %+v", listed.GetReleases())
		}
	})

	t.Run("RollbackCopiesTargetConfig", func(t *testing.T) {
		rollback, err := clients.release.Rollback(sessionCtx, &cyclopusv1.RollbackRequest{
			EnvironmentId: environmentID, ReleaseId: releaseOne, IdempotencyKey: "rollback-1",
		})
		if err != nil || rollback.GetOperation().GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED {
			t.Fatalf("Rollback() = %+v, %v", rollback, err)
		}
		releaseThree = rollback.GetOperation().GetId()
		var kind, imageRef, configHash string
		if err := fixture.pool.QueryRow(ctx, `SELECT kind, image_ref, config_hash FROM releases WHERE id=$1`, releaseThree).Scan(&kind, &imageRef, &configHash); err != nil {
			t.Fatal(err)
		}
		targetHash := queryString(t, fixture.pool, `SELECT config_hash FROM releases WHERE id=$1`, releaseOne)
		if kind != "rollback" || imageRef != imageA || configHash != targetHash {
			t.Fatalf("rollback release kind=%s image=%s hash=%s, want rollback/%s/%s", kind, imageRef, configHash, imageA, targetHash)
		}
		finalizeReleaseLive(t, fixture, dataStore, clients.orgID, environmentID, releaseThree, clients.userID, "api.example.test")
		if !queryBool(t, fixture.pool, `SELECT release_id=$2 FROM aliases WHERE environment_id=$1`, environmentID, releaseThree) {
			t.Fatal("alias not moved to rollback release")
		}
		listed, err := clients.release.ListReleases(sessionCtx, &cyclopusv1.ListReleasesRequest{EnvironmentId: environmentID})
		if err != nil || len(listed.GetReleases()) != 3 {
			t.Fatalf("ListReleases() = %+v, %v", listed, err)
		}
		if listed.GetReleases()[0].GetId() != releaseThree || listed.GetReleases()[0].GetStatus() != cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE ||
			listed.GetReleases()[1].GetStatus() != cyclopusv1.ReleaseStatus_RELEASE_STATUS_SUPERSEDED ||
			listed.GetReleases()[2].GetStatus() != cyclopusv1.ReleaseStatus_RELEASE_STATUS_SUPERSEDED {
			t.Fatalf("ListReleases() after rollback = %+v", listed.GetReleases())
		}
	})

	t.Run("RollbackOutsideRetention", func(t *testing.T) {
		mustExec(t, fixture.pool, `UPDATE apps SET release_retention_count=1 WHERE id=$1`, app.GetApplication().GetId())
		deploy, err := clients.release.Deploy(sessionCtx, &cyclopusv1.DeployRequest{
			EnvironmentId: environmentID, ImageRef: imageC, GitCommitSha: strings.Repeat("3", 40), IdempotencyKey: "deploy-4",
		})
		if err != nil {
			t.Fatal(err)
		}
		finalizeReleaseLive(t, fixture, dataStore, clients.orgID, environmentID, deploy.GetOperation().GetId(), clients.userID, "api.example.test")
		releasesBefore := queryInt(t, fixture.pool, `SELECT count(*) FROM releases`)
		_, err = clients.release.Rollback(sessionCtx, &cyclopusv1.RollbackRequest{
			EnvironmentId: environmentID, ReleaseId: releaseOne, IdempotencyKey: "rollback-2",
		})
		// resources.go:385 retention window yields no row -> mapMutationError pgx.ErrNoRows -> ErrNotFound
		requireCode(t, err, codes.NotFound)
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM releases`); count != releasesBefore {
			t.Fatalf("releases after out-of-retention rollback = %d, want %d", count, releasesBefore)
		}
	})

	t.Run("ReleaseStatusFilters", func(t *testing.T) {
		live, err := clients.release.ListReleases(sessionCtx, &cyclopusv1.ListReleasesRequest{
			EnvironmentId: environmentID, Statuses: []cyclopusv1.ReleaseStatus{cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE},
		})
		if err != nil || len(live.GetReleases()) != 1 || live.GetReleases()[0].GetStatus() != cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE {
			t.Fatalf("ListReleases(LIVE) = %+v, %v", live, err)
		}
		superseded, err := clients.release.ListReleases(sessionCtx, &cyclopusv1.ListReleasesRequest{
			EnvironmentId: environmentID, Statuses: []cyclopusv1.ReleaseStatus{cyclopusv1.ReleaseStatus_RELEASE_STATUS_SUPERSEDED},
		})
		if err != nil || len(superseded.GetReleases()) != 3 {
			t.Fatalf("ListReleases(SUPERSEDED) = %+v, %v", superseded, err)
		}
		for _, release := range superseded.GetReleases() {
			if release.GetStatus() != cyclopusv1.ReleaseStatus_RELEASE_STATUS_SUPERSEDED {
				t.Fatalf("ListReleases(SUPERSEDED) contains %s", release.GetStatus())
			}
		}
	})
}

func TestE2EEnvVars(t *testing.T) {
	fixture := newAPIFixture(t, "e2eenvvars")
	clients := newE2EClients(t, fixture, "envvars@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)
	dataStore := store.New(fixture.pool)

	project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "project-1",
		Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryID := fixture.insertRegistry(t, clients.orgID)
	app, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-1", Application: validApplication(registryID),
	})
	if err != nil {
		t.Fatal(err)
	}
	environmentID := fixture.insertEnvironment(t, clients.orgID, app.GetApplication().GetId())
	imageA := "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64)
	deploy, err := clients.release.Deploy(sessionCtx, &cyclopusv1.DeployRequest{
		EnvironmentId: environmentID, ImageRef: imageA, GitCommitSha: strings.Repeat("1", 40), IdempotencyKey: "deploy-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	finalizeReleaseLive(t, fixture, dataStore, clients.orgID, environmentID, deploy.GetOperation().GetId(), clients.userID, "api.example.test")

	var revisionOneID string
	t.Run("SetAndPublish", func(t *testing.T) {
		secretRef := "k8s-secret://app-production/runtime#DATABASE_URL"
		set, err := clients.release.SetEnvironmentVariables(sessionCtx, &cyclopusv1.SetEnvironmentVariablesRequest{
			EnvironmentId:  environmentID,
			Variables:      []*cyclopusv1.EnvironmentVariable{{Key: "DATABASE_URL", Secret: true, Value: secretRef}},
			IdempotencyKey: "env-1",
		})
		if err != nil || set.GetOperation().GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED {
			t.Fatalf("SetEnvironmentVariables() = %+v, %v", set, err)
		}
		releaseID := set.GetOperation().GetId()
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM env_vars WHERE environment_id=$1`, environmentID); count != 1 {
			t.Fatalf("env_vars rows = %d", count)
		}
		if !queryBool(t, fixture.pool, `SELECT key='DATABASE_URL' AND secret_ref=$2 AND is_secret
			FROM env_vars WHERE environment_id=$1`, environmentID, secretRef) {
			t.Fatal("env_vars row mismatch")
		}
		var revision int64
		var createdBy string
		if err := fixture.pool.QueryRow(ctx, `SELECT id::text, revision, created_by_user_id::text
			FROM env_var_revisions WHERE environment_id=$1`, environmentID).Scan(&revisionOneID, &revision, &createdBy); err != nil {
			t.Fatal(err)
		}
		if revision != 1 || createdBy != clients.userID {
			t.Fatalf("env_var_revisions revision=%d created_by=%s", revision, createdBy)
		}
		if !queryBool(t, fixture.pool, `SELECT key='DATABASE_URL' AND secret_ref=$3 AND is_secret
			FROM env_var_revision_entries WHERE env_var_revision_id=$1 AND environment_id=$2`, revisionOneID, environmentID, secretRef) {
			t.Fatal("env_var_revision_entries is not a full copy of env_vars")
		}
		var kind, releaseRevision, releaseImage string
		if err := fixture.pool.QueryRow(ctx, `SELECT kind, env_var_revision_id::text, image_ref FROM releases WHERE id=$1`,
			releaseID).Scan(&kind, &releaseRevision, &releaseImage); err != nil {
			t.Fatal(err)
		}
		if kind != "env_change" || releaseRevision != revisionOneID || releaseImage != imageA ||
			!queryBool(t, fixture.pool, `SELECT status='pending' FROM releases WHERE id=$1`, releaseID) {
			t.Fatalf("env-change release kind=%s revision=%s image=%s", kind, releaseRevision, releaseImage)
		}
		finalizeReleaseLive(t, fixture, dataStore, clients.orgID, environmentID, releaseID, clients.userID, "api.example.test")
		if !queryBool(t, fixture.pool, `SELECT current_env_var_revision_id=$2 FROM environments WHERE id=$1`, environmentID, revisionOneID) {
			t.Fatal("environment did not publish the new env-var revision")
		}
		variables, err := clients.release.GetEnvironmentVariables(sessionCtx, &cyclopusv1.GetEnvironmentVariablesRequest{EnvironmentId: environmentID})
		if err != nil {
			t.Fatal(err)
		}
		revisionResponse := variables.GetRevision()
		if revisionResponse.GetId() != revisionOneID || revisionResponse.GetRevision() != 1 || len(revisionResponse.GetVariables()) != 1 {
			t.Fatalf("GetEnvironmentVariables() = %+v", revisionResponse)
		}
		// handlers.go:688-690 redacts values: the proto carries only Key and Secret
		variable := revisionResponse.GetVariables()[0]
		if variable.GetKey() != "DATABASE_URL" || !variable.GetSecret() || variable.GetValue() != "" {
			t.Fatalf("GetEnvironmentVariables() variable = %+v", variable)
		}
	})

	t.Run("RevisionImmutabilityAndSecondRevision", func(t *testing.T) {
		if _, err := fixture.pool.Exec(ctx, `INSERT INTO env_var_revision_entries
			(org_id, environment_id, env_var_revision_id, key, secret_ref)
			VALUES ($1, $2, $3, 'INJECTED', 'k8s-secret://app-production/runtime#INJECTED')`,
			clients.orgID, environmentID, revisionOneID); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("published revision accepted new entries: %v", err)
		}
		set, err := clients.release.SetEnvironmentVariables(sessionCtx, &cyclopusv1.SetEnvironmentVariablesRequest{
			EnvironmentId:  environmentID,
			Variables:      []*cyclopusv1.EnvironmentVariable{{Key: "CACHE_URL", Secret: true, Value: "k8s-secret://app-production/runtime#CACHE_URL"}},
			IdempotencyKey: "env-2",
		})
		if err != nil {
			t.Fatal(err)
		}
		finalizeReleaseLive(t, fixture, dataStore, clients.orgID, environmentID, set.GetOperation().GetId(), clients.userID, "api.example.test")
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM env_var_revisions WHERE environment_id=$1`, environmentID); count != 2 {
			t.Fatalf("env_var_revisions retained = %d, want 2", count)
		}
		variables, err := clients.release.GetEnvironmentVariables(sessionCtx, &cyclopusv1.GetEnvironmentVariablesRequest{EnvironmentId: environmentID})
		if err != nil {
			t.Fatal(err)
		}
		if variables.GetRevision().GetRevision() != 2 || len(variables.GetRevision().GetVariables()) != 1 ||
			variables.GetRevision().GetVariables()[0].GetKey() != "CACHE_URL" {
			t.Fatalf("GetEnvironmentVariables() after second set = %+v", variables.GetRevision())
		}
	})

	t.Run("CrossNamespaceRejected", func(t *testing.T) {
		_, err := clients.release.SetEnvironmentVariables(sessionCtx, &cyclopusv1.SetEnvironmentVariablesRequest{
			EnvironmentId:  environmentID,
			Variables:      []*cyclopusv1.EnvironmentVariable{{Key: "DATABASE_URL", Secret: true, Value: "k8s-secret://other-namespace/runtime#DATABASE_URL"}},
			IdempotencyKey: "env-bad",
		})
		requireCode(t, err, codes.InvalidArgument)
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM api_idempotency_records WHERE idempotency_key='env-bad'`); count != 0 {
			t.Fatalf("cross-namespace rejection consumed idempotency claims = %d", count)
		}
	})
}

func TestE2ETeardown(t *testing.T) {
	fixture := newAPIFixture(t, "e2eteardown")
	clients := newE2EClients(t, fixture, "teardown@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)
	dataStore := store.New(fixture.pool)

	project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "project-1",
		Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryID := fixture.insertRegistry(t, clients.orgID)
	app, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-1", Application: validApplication(registryID),
	})
	if err != nil {
		t.Fatal(err)
	}
	appID := app.GetApplication().GetId()
	seedGitIntegration(t, fixture, clients.orgID, project.GetProject().GetId(), appID)

	t.Run("TeardownEnvironment", func(t *testing.T) {
		push, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appID, Branch: "teardown-me", CommitSha: strings.Repeat("a", 40), IdempotencyKey: "push-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		environmentID := push.GetEnvironment().GetId()
		teardown, err := clients.environment.TeardownEnvironment(sessionCtx, &cyclopusv1.TeardownEnvironmentRequest{
			EnvironmentId: environmentID, Reason: "cleanup", IdempotencyKey: "teardown-1",
		})
		if err != nil || teardown.GetEnvironment().GetLifecycleStatus() != cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_TEARDOWN_REQUESTED {
			t.Fatalf("TeardownEnvironment() = %+v, %v", teardown, err)
		}
		if !queryBool(t, fixture.pool, `SELECT lifecycle_status='teardown_requested' AND teardown_requested_at IS NOT NULL
			AND lifecycle_reason='cleanup' FROM environments WHERE id=$1`, environmentID) {
			t.Fatal("environment not marked teardown_requested with reason")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM environment_lifecycle_jobs WHERE environment_id=$1
			AND kind='teardown' AND status='queued' AND reason='cleanup'`, environmentID); count != 1 {
			t.Fatalf("teardown job rows = %d", count)
		}
		completeTeardown(t, dataStore, clients.orgID, environmentID)
		if !queryBool(t, fixture.pool, `SELECT lifecycle_status='torn_down' AND torn_down_at IS NOT NULL
			FROM environments WHERE id=$1`, environmentID) {
			t.Fatal("environment not torn down after teardown completion")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM environments WHERE id=$1`, environmentID); count != 1 {
			t.Fatalf("torn-down environment row missing, count = %d", count)
		}
		visible, err := clients.environment.ListEnvironments(sessionCtx, &cyclopusv1.ListEnvironmentsRequest{ApplicationId: appID})
		if err != nil || len(visible.GetEnvironments()) != 0 {
			t.Fatalf("ListEnvironments() default = %+v, %v", visible, err)
		}
		all, err := clients.environment.ListEnvironments(sessionCtx, &cyclopusv1.ListEnvironmentsRequest{ApplicationId: appID, IncludeTornDown: true})
		if err != nil || len(all.GetEnvironments()) != 1 || all.GetEnvironments()[0].GetTornDownAt() == nil {
			t.Fatalf("ListEnvironments(IncludeTornDown) = %+v, %v", all, err)
		}
	})

	t.Run("BranchDeleteTeardown", func(t *testing.T) {
		push, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appID, Branch: "delete-me", CommitSha: strings.Repeat("b", 40), IdempotencyKey: "push-2",
		})
		if err != nil {
			t.Fatal(err)
		}
		environmentID := push.GetEnvironment().GetId()
		deleted, err := clients.environment.AcceptPreviewBranchDelete(sessionCtx, &cyclopusv1.PreviewBranchDeleteRequest{
			ApplicationId: appID, Branch: "delete-me", IdempotencyKey: "delete-1",
		})
		if err != nil || deleted.GetEnvironment().GetLifecycleStatus() != cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_TEARDOWN_REQUESTED {
			t.Fatalf("AcceptPreviewBranchDelete() = %+v, %v", deleted, err)
		}
		// service/preview.go:70-72 maps branch deletes to reason branch_deleted
		if !queryBool(t, fixture.pool, `SELECT lifecycle_status='teardown_requested' AND lifecycle_reason='branch_deleted'
			FROM environments WHERE id=$1`, environmentID) {
			t.Fatal("branch delete did not request teardown")
		}
		completeTeardown(t, dataStore, clients.orgID, environmentID)
		if !queryBool(t, fixture.pool, `SELECT lifecycle_status='torn_down' AND torn_down_at IS NOT NULL
			FROM environments WHERE id=$1`, environmentID) {
			t.Fatal("branch-deleted environment not torn down")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM environments WHERE id=$1`, environmentID); count != 1 {
			t.Fatalf("torn-down environment row missing, count = %d", count)
		}
	})
}

func TestE2ETombstones(t *testing.T) {
	fixture := newAPIFixture(t, "e2etombstone")
	clients := newE2EClients(t, fixture, "tombstone@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)
	dataStore := store.New(fixture.pool)

	project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "project-1",
		Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := project.GetProject().GetId()
	registryID := fixture.insertRegistry(t, clients.orgID)
	app, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: projectID, IdempotencyKey: "app-1", Application: validApplication(registryID),
	})
	if err != nil {
		t.Fatal(err)
	}
	appID := app.GetApplication().GetId()
	seedGitIntegration(t, fixture, clients.orgID, projectID, appID)

	push, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
		ApplicationId: appID, Branch: "doomed", CommitSha: strings.Repeat("a", 40), IdempotencyKey: "push-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	environmentOneID := push.GetEnvironment().GetId()

	t.Run("TombstonePreconditions", func(t *testing.T) {
		_, err := clients.application.DeleteApplication(sessionCtx, &cyclopusv1.DeleteApplicationRequest{
			ApplicationId: appID, ExternalCredentialsRevoked: true, IdempotencyKey: "del-app-1",
		})
		// store.TombstoneApp finds a live descendant environment and returns ErrConflict -> Aborted CONFLICT
		requireErrorReason(t, err, codes.Aborted, "CONFLICT")
		_, err = clients.application.DeleteApplication(sessionCtx, &cyclopusv1.DeleteApplicationRequest{
			ApplicationId: appID, ExternalCredentialsRevoked: false, IdempotencyKey: "del-app-2",
		})
		// store/deletion.go:44-46 requires external credential revocation confirmation before tombstone
		requireCode(t, err, codes.InvalidArgument)
	})

	t.Run("TombstoneFlow", func(t *testing.T) {
		if _, err := clients.environment.TeardownEnvironment(sessionCtx, &cyclopusv1.TeardownEnvironmentRequest{
			EnvironmentId: environmentOneID, Reason: "cleanup", IdempotencyKey: "teardown-1",
		}); err != nil {
			t.Fatal(err)
		}
		completeTeardown(t, dataStore, clients.orgID, environmentOneID)

		pushTwo, err := clients.environment.AcceptPreviewPush(sessionCtx, &cyclopusv1.PreviewPushRequest{
			ApplicationId: appID, Branch: "doomed-2", CommitSha: strings.Repeat("b", 40), IdempotencyKey: "push-2",
		})
		if err != nil {
			t.Fatal(err)
		}
		environmentTwoID := pushTwo.GetEnvironment().GetId()
		// simulate externally compensated teardown that left the queued dispatch behind
		mustExec(t, fixture.pool, `UPDATE environments SET torn_down_at=now() WHERE id=$1`, environmentTwoID)

		deletedApp, err := clients.application.DeleteApplication(sessionCtx, &cyclopusv1.DeleteApplicationRequest{
			ApplicationId: appID, ExternalCredentialsRevoked: true, IdempotencyKey: "del-app-3",
		})
		if err != nil {
			t.Fatal(err)
		}
		if deletedApp.GetOperation().GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_SUCCEEDED ||
			deletedApp.GetOperation().GetResourceName() != "applications/"+appID {
			t.Fatalf("DeleteApplication() = %+v", deletedApp.GetOperation())
		}
		if !queryBool(t, fixture.pool, `SELECT deleted_at IS NOT NULL AND external_credentials_revoked_at IS NOT NULL
			FROM apps WHERE id=$1`, appID) {
			t.Fatal("app not tombstoned with credential revocation")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='app.deleted' AND target_id=$1`, appID); count != 1 {
			t.Fatalf("app.deleted audit rows = %d", count)
		}
		// migration enforce_tombstone_readiness cancels queued dispatches of tombstoned ancestors
		if !queryBool(t, fixture.pool, `SELECT status='cancelled' AND terminal_error_code='ancestor_tombstoned'
			FROM orchestration_jobs WHERE environment_id=$1`, environmentTwoID) {
			t.Fatal("queued orchestration job not cancelled with ancestor_tombstoned")
		}

		deletedProject, err := clients.project.DeleteProject(sessionCtx, &cyclopusv1.DeleteProjectRequest{
			ProjectId: projectID, ExternalCredentialsRevoked: true, IdempotencyKey: "del-proj-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if deletedProject.GetOperation().GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_SUCCEEDED ||
			deletedProject.GetOperation().GetResourceName() != "projects/"+projectID {
			t.Fatalf("DeleteProject() = %+v", deletedProject.GetOperation())
		}
		if !queryBool(t, fixture.pool, `SELECT deleted_at IS NOT NULL AND external_credentials_revoked_at IS NOT NULL
			FROM projects WHERE id=$1`, projectID) {
			t.Fatal("project not tombstoned with credential revocation")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='project.deleted' AND target_id=$1`, projectID); count != 1 {
			t.Fatalf("project.deleted audit rows = %d", count)
		}
		// store/resources.go:91-98 filters tombstoned projects out of GetProject
		if _, err := clients.project.GetProject(sessionCtx, &cyclopusv1.GetProjectRequest{ProjectId: projectID}); status.Code(err) != codes.NotFound {
			t.Fatalf("GetProject() on tombstoned project error = %v", err)
		}
		visible, err := clients.project.ListProjects(sessionCtx, &cyclopusv1.ListProjectsRequest{OrganizationId: clients.orgID})
		if err != nil || len(visible.GetProjects()) != 0 {
			t.Fatalf("ListProjects() default = %+v, %v", visible, err)
		}
		all, err := clients.project.ListProjects(sessionCtx, &cyclopusv1.ListProjectsRequest{OrganizationId: clients.orgID, IncludeDeleted: true})
		if err != nil || len(all.GetProjects()) != 1 || all.GetProjects()[0].GetDeletedAt() == nil {
			t.Fatalf("ListProjects(IncludeDeleted) = %+v, %v", all, err)
		}

		deletedOrg, err := clients.organization.DeleteOrganization(sessionCtx, &cyclopusv1.DeleteOrganizationRequest{
			OrganizationId: clients.orgID, ExternalCredentialsRevoked: true, IdempotencyKey: "del-org-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if deletedOrg.GetOperation().GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_SUCCEEDED ||
			deletedOrg.GetOperation().GetResourceName() != "organizations/"+clients.orgID {
			t.Fatalf("DeleteOrganization() = %+v", deletedOrg.GetOperation())
		}
		if !queryBool(t, fixture.pool, `SELECT deleted_at IS NOT NULL AND external_credentials_revoked_at IS NOT NULL
			FROM orgs WHERE id=$1`, clients.orgID) {
			t.Fatal("org not tombstoned with credential revocation")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='org.deleted' AND target_id=$1`, clients.orgID); count != 1 {
			t.Fatalf("org.deleted audit rows = %d", count)
		}
	})

	t.Run("PhysicalDeleteRejected", func(t *testing.T) {
		for _, statement := range []struct{ query, arg string }{
			{`DELETE FROM apps WHERE id=$1`, appID},
			{`DELETE FROM projects WHERE id=$1`, projectID},
			{`DELETE FROM orgs WHERE id=$1`, clients.orgID},
		} {
			if _, err := fixture.pool.Exec(ctx, statement.query, statement.arg); err == nil || !strings.Contains(err.Error(), "tombstoned") {
				t.Fatalf("%s error = %v, want tombstone trigger rejection", statement.query, err)
			}
		}
	})
}

func TestE2EReconciliation(t *testing.T) {
	fixture := newAPIFixture(t, "e2ereconcile")
	clients := newE2EClients(t, fixture, "reconcile@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)

	started, err := clients.reconciliation.StartReconciliation(sessionCtx, &cyclopusv1.StartReconciliationRequest{
		Scope: &cyclopusv1.ReconcileScope{
			Kind:           cyclopusv1.ReconcileScopeKind_RECONCILE_SCOPE_KIND_ORGANIZATION,
			OrganizationId: clients.orgID,
		},
		IdempotencyKey: "recon-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	operation := started.GetOperation()
	if operation.GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED ||
		operation.GetResourceName() != "reconciliations/"+operation.GetId() {
		t.Fatalf("StartReconciliation() operation = %+v", operation)
	}
	if !queryBool(t, fixture.pool, `SELECT status='queued' AND scope_kind='org' AND scope_id::text=$2
		AND dry_run AND NOT prune FROM reconciliation_runs WHERE id=$1`, operation.GetId(), clients.orgID) {
		t.Fatal("reconciliation_runs row mismatch")
	}
	fetched, err := clients.reconciliation.GetReconciliation(sessionCtx, &cyclopusv1.GetReconciliationRequest{ReconciliationId: operation.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	run := fetched.GetReconciliation()
	if run.GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED || !run.GetDryRun() ||
		run.GetScope().GetKind() != cyclopusv1.ReconcileScopeKind_RECONCILE_SCOPE_KIND_ORGANIZATION ||
		run.GetScope().GetOrganizationId() != clients.orgID {
		t.Fatalf("GetReconciliation() = %+v", run)
	}
	listed, err := clients.reconciliation.ListReconciliations(sessionCtx, &cyclopusv1.ListReconciliationsRequest{OrganizationId: clients.orgID})
	if err != nil || len(listed.GetReconciliations()) != 1 || listed.GetReconciliations()[0].GetId() != operation.GetId() {
		t.Fatalf("ListReconciliations() = %+v, %v", listed, err)
	}
}

func TestE2EUnusedWriteTargets(t *testing.T) {
	fixture := newAPIFixture(t, "e2eunused")
	clients := newE2EClients(t, fixture, "unused@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)

	project, err := clients.project.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "project-1",
		Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryID := fixture.insertRegistry(t, clients.orgID)
	if _, err := clients.application.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: project.GetProject().GetId(), IdempotencyKey: "app-1", Application: validApplication(registryID),
	}); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"domains", "storage_destinations"} {
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM `+table); count != 0 {
			t.Fatalf("table %s has %d rows after API-driven flow: an unexpected v1 write path exists", table, count)
		}
	}
	t.Logf("domains, storage_destinations have no v1 API write path (counts are zero after API-driven flow)")
	t.Logf("git_connections.webhook_secret_ref and registries.credential_secret_ref are write-seed-only: inserted by fixtures/migrations, never read by Go code")
}

func TestE2EWebhooks(t *testing.T) {
	fixture := newAPIFixture(t, "e2ewebhooks")
	clients := newE2EClients(t, fixture, "webhooks@example.com")
	ctx := t.Context()
	sessionCtx := clients.sessionCtx(ctx)

	validEndpoint := func() *cyclopusv1.WebhookEndpoint {
		return &cyclopusv1.WebhookEndpoint{
			TargetUrl: "https://hooks.example.com/cyclopus",
			Events:    []string{"deploy.succeeded"},
			SecretRef: "k8s-secret://cyclopus/hooks#secret",
		}
	}

	t.Run("CreateValidation", func(t *testing.T) {
		for index, mutate := range []func(*cyclopusv1.CreateWebhookRequest){
			func(r *cyclopusv1.CreateWebhookRequest) { r.Webhook.TargetUrl = "http://hooks.example.com/cyclopus" },
			func(r *cyclopusv1.CreateWebhookRequest) { r.Webhook.TargetUrl = "https://localhost/cyclopus" },
			func(r *cyclopusv1.CreateWebhookRequest) { r.Webhook.TargetUrl = "https://127.0.0.1/cyclopus" },
			func(r *cyclopusv1.CreateWebhookRequest) { r.Webhook.SecretRef = "not-a-secret-ref" },
			func(r *cyclopusv1.CreateWebhookRequest) { r.Webhook.Events = []string{"deploy.unknown"} },
			func(r *cyclopusv1.CreateWebhookRequest) {
				r.Webhook.Events = []string{"deploy.succeeded", "deploy.succeeded"}
			},
			func(r *cyclopusv1.CreateWebhookRequest) { r.IdempotencyKey = "" },
			func(r *cyclopusv1.CreateWebhookRequest) { r.Webhook.TargetUrl = "" },
			func(r *cyclopusv1.CreateWebhookRequest) { r.Webhook = nil },
		} {
			request := &cyclopusv1.CreateWebhookRequest{
				OrganizationId: clients.orgID, IdempotencyKey: "webhook-invalid", Webhook: validEndpoint(),
			}
			mutate(request)
			if _, err := clients.webhook.CreateWebhook(sessionCtx, request); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("invalid create %d error = %v, want InvalidArgument", index, err)
			}
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM webhooks`); count != 0 {
			t.Fatalf("webhooks after rejected creates = %d", count)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM api_idempotency_records WHERE scope='webhook.create'`); count != 0 {
			t.Fatalf("rejected creates claimed idempotency records = %d", count)
		}
	})

	createRequest := &cyclopusv1.CreateWebhookRequest{
		OrganizationId: clients.orgID, IdempotencyKey: "webhook-1", Webhook: validEndpoint(),
	}
	var webhookID string
	t.Run("CreateAndReplay", func(t *testing.T) {
		created, err := clients.webhook.CreateWebhook(sessionCtx, createRequest)
		if err != nil {
			t.Fatal(err)
		}
		endpoint := created.GetWebhook()
		if endpoint.GetId() == "" || endpoint.GetOrganizationId() != clients.orgID || !endpoint.GetEnabled() ||
			endpoint.GetConsecutiveFailures() != 0 || endpoint.GetDisabledAt() != nil ||
			endpoint.GetTargetUrl() != "https://hooks.example.com/cyclopus" ||
			len(endpoint.GetEvents()) != 1 || endpoint.GetEvents()[0] != "deploy.succeeded" ||
			endpoint.GetSecretRef() != "k8s-secret://cyclopus/hooks#secret" {
			t.Fatalf("CreateWebhook() = %+v", endpoint)
		}
		webhookID = endpoint.GetId()
		if !queryBool(t, fixture.pool, `SELECT target_url='https://hooks.example.com/cyclopus'
			AND events='{deploy.succeeded}'::text[] AND disabled_at IS NULL AND consecutive_failures=0
			FROM webhooks WHERE id=$1`, webhookID) {
			t.Fatal("webhooks row mismatch")
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='webhook.created'`); count != 1 {
			t.Fatalf("webhook.created audit rows = %d", count)
		}

		replayed, err := clients.webhook.CreateWebhook(sessionCtx, createRequest)
		if err != nil || replayed.GetWebhook().GetId() != webhookID {
			t.Fatalf("idempotent replay = %+v, %v", replayed, err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM webhooks`); count != 1 {
			t.Fatalf("webhooks after replay = %d", count)
		}
	})

	t.Run("GetAndList", func(t *testing.T) {
		fetched, err := clients.webhook.GetWebhook(sessionCtx, &cyclopusv1.GetWebhookRequest{WebhookId: webhookID})
		if err != nil || fetched.GetWebhook().GetTargetUrl() != "https://hooks.example.com/cyclopus" {
			t.Fatalf("GetWebhook() = %+v, %v", fetched, err)
		}
		listed, err := clients.webhook.ListWebhooks(sessionCtx, &cyclopusv1.ListWebhooksRequest{OrganizationId: clients.orgID})
		if err != nil || len(listed.GetWebhooks()) != 1 || listed.GetWebhooks()[0].GetId() != webhookID {
			t.Fatalf("ListWebhooks() = %+v, %v", listed, err)
		}
		if _, err := clients.webhook.ListWebhooks(sessionCtx, &cyclopusv1.ListWebhooksRequest{OrganizationId: "00000000-0000-0000-0000-000000000099"}); status.Code(err) != codes.NotFound {
			t.Fatalf("foreign organization list error = %v", err)
		}
	})

	t.Run("UpdateMergeMask", func(t *testing.T) {
		updated, err := clients.webhook.UpdateWebhook(sessionCtx, &cyclopusv1.UpdateWebhookRequest{
			Webhook: &cyclopusv1.WebhookEndpoint{
				Id:        webhookID,
				TargetUrl: "https://hooks-two.example.com/cyclopus",
				Events:    []string{"deploy.failed", "rollback.completed"},
			},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"target_url", "events"}},
			IdempotencyKey: "webhook-update-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		endpoint := updated.GetWebhook()
		if endpoint.GetTargetUrl() != "https://hooks-two.example.com/cyclopus" ||
			len(endpoint.GetEvents()) != 2 || endpoint.GetEvents()[0] != "deploy.failed" ||
			endpoint.GetSecretRef() != "k8s-secret://cyclopus/hooks#secret" || !endpoint.GetEnabled() {
			t.Fatalf("UpdateWebhook(target/events) = %+v", endpoint)
		}

		if _, err := clients.webhook.UpdateWebhook(sessionCtx, &cyclopusv1.UpdateWebhookRequest{
			Webhook:        &cyclopusv1.WebhookEndpoint{Id: webhookID, SecretRef: "k8s-secret://cyclopus/hooks#rotated"},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"consecutive_failures"}},
			IdempotencyKey: "webhook-update-invalid",
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("unsupported mask path error = %v", err)
		}

		disabled, err := clients.webhook.UpdateWebhook(sessionCtx, &cyclopusv1.UpdateWebhookRequest{
			Webhook:        &cyclopusv1.WebhookEndpoint{Id: webhookID, Enabled: false},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"enabled"}},
			IdempotencyKey: "webhook-update-2",
		})
		if err != nil || disabled.GetWebhook().GetEnabled() || disabled.GetWebhook().GetDisabledAt() == nil {
			t.Fatalf("UpdateWebhook(disable) = %+v, %v", disabled, err)
		}
		if !queryBool(t, fixture.pool, `SELECT disabled_at IS NOT NULL FROM webhooks WHERE id=$1`, webhookID) {
			t.Fatal("webhooks.disabled_at not set")
		}

		reenabled, err := clients.webhook.UpdateWebhook(sessionCtx, &cyclopusv1.UpdateWebhookRequest{
			Webhook:        &cyclopusv1.WebhookEndpoint{Id: webhookID, Enabled: true},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"enabled"}},
			IdempotencyKey: "webhook-update-3",
		})
		if err != nil || !reenabled.GetWebhook().GetEnabled() || reenabled.GetWebhook().GetDisabledAt() != nil {
			t.Fatalf("UpdateWebhook(reenable) = %+v, %v", reenabled, err)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		if _, err := clients.webhook.DeleteWebhook(sessionCtx, &cyclopusv1.DeleteWebhookRequest{
			WebhookId: webhookID, IdempotencyKey: "webhook-delete-1",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := clients.webhook.GetWebhook(sessionCtx, &cyclopusv1.GetWebhookRequest{WebhookId: webhookID}); status.Code(err) != codes.NotFound {
			t.Fatalf("GetWebhook() after delete error = %v", err)
		}
		listed, err := clients.webhook.ListWebhooks(sessionCtx, &cyclopusv1.ListWebhooksRequest{OrganizationId: clients.orgID})
		if err != nil || len(listed.GetWebhooks()) != 0 {
			t.Fatalf("ListWebhooks() after delete = %+v, %v", listed, err)
		}
		if count := queryInt(t, fixture.pool, `SELECT count(*) FROM audit_events WHERE action='webhook.deleted'`); count != 1 {
			t.Fatalf("webhook.deleted audit rows = %d", count)
		}
	})

	t.Run("CrossOrgInvisibility", func(t *testing.T) {
		otherProjectID := fixture.insertOtherTenant(t)
		otherOrgID := queryString(t, fixture.pool, `SELECT org_id::text FROM projects WHERE id=$1`, otherProjectID)
		var otherWebhookID string
		if err := fixture.pool.QueryRow(ctx, `INSERT INTO webhooks(org_id,target_url,secret_ref)
			VALUES ($1,'https://other.example.com/hook','k8s-secret://cyclopus/hooks#other') RETURNING id::text`,
			otherOrgID).Scan(&otherWebhookID); err != nil {
			t.Fatal(err)
		}
		if _, err := clients.webhook.GetWebhook(sessionCtx, &cyclopusv1.GetWebhookRequest{WebhookId: otherWebhookID}); status.Code(err) != codes.NotFound {
			t.Fatalf("cross-org GetWebhook() error = %v, want NotFound", err)
		}
		if _, err := clients.webhook.CreateWebhook(sessionCtx, &cyclopusv1.CreateWebhookRequest{
			OrganizationId: otherOrgID, IdempotencyKey: "webhook-cross-org", Webhook: validEndpoint(),
		}); status.Code(err) != codes.NotFound {
			t.Fatalf("cross-org CreateWebhook() error = %v, want NotFound", err)
		}
		if _, err := clients.webhook.UpdateWebhook(sessionCtx, &cyclopusv1.UpdateWebhookRequest{
			Webhook:        &cyclopusv1.WebhookEndpoint{Id: otherWebhookID, Enabled: false},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"enabled"}},
			IdempotencyKey: "webhook-cross-org-update",
		}); status.Code(err) != codes.NotFound {
			t.Fatalf("cross-org UpdateWebhook() error = %v, want NotFound", err)
		}
		if _, err := clients.webhook.DeleteWebhook(sessionCtx, &cyclopusv1.DeleteWebhookRequest{
			WebhookId: otherWebhookID, IdempotencyKey: "webhook-cross-org-delete",
		}); status.Code(err) != codes.NotFound {
			t.Fatalf("cross-org DeleteWebhook() error = %v, want NotFound", err)
		}
		if !queryBool(t, fixture.pool, `SELECT disabled_at IS NULL FROM webhooks WHERE id=$1`, otherWebhookID) {
			t.Fatal("cross-org webhook was mutated")
		}
	})
}
