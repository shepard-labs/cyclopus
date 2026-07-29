//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	cyclopusv1 "github.com/cyclopus-dev/cyclopus/gen/go/cyclopus/v1"
	internalauth "github.com/cyclopus-dev/cyclopus/internal/auth"
	"github.com/cyclopus-dev/cyclopus/internal/service"
	"github.com/cyclopus-dev/cyclopus/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const serverIntegrationPepper = "0123456789abcdef0123456789abcdef"
const serverIntegrationBootstrap = "bootstrap-0123456789abcdef0123456789abcdef"

func TestPostgresNativeGRPCAPI(t *testing.T) {
	fixture := newAPIFixture(t, "grpc")
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

	authClient := cyclopusv1.NewAuthenticationServiceClient(conn)
	legacyHeaderCtx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-cyclopus-bootstrap-token", serverIntegrationBootstrap))
	if _, err := authClient.Bootstrap(legacyHeaderCtx, &cyclopusv1.BootstrapRequest{
		Email: "operator@example.com", Password: "local password", IdempotencyKey: "claim-header",
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Bootstrap() legacy header error = %v", err)
	}
	bootstrap, err := authClient.Bootstrap(t.Context(), &cyclopusv1.BootstrapRequest{Email: "operator@example.com", Password: "local password", IdempotencyKey: "claim-1", BootstrapToken: serverIntegrationBootstrap})
	if err != nil || bootstrap.GetSession().GetAccessToken() == "" || bootstrap.GetSession().GetExpiresAt() == nil {
		t.Fatalf("Bootstrap() = %+v, %v; %s", bootstrap, err, fixture.bootstrapFailureState(t))
	}
	login, err := authClient.Login(t.Context(), &cyclopusv1.LoginRequest{Email: "operator@example.com", Password: "local password"})
	if err != nil {
		t.Fatal(err)
	}
	sessionCtx := bearerContext(t.Context(), login.GetSession().GetAccessToken())

	projectClient := cyclopusv1.NewProjectServiceClient(conn)
	createProject := &cyclopusv1.CreateProjectRequest{OrganizationId: login.GetSession().GetUser().GetOrganizationId(), IdempotencyKey: "project-1", Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"}}
	projectOne, err := projectClient.CreateProject(sessionCtx, createProject)
	if err != nil {
		t.Fatal(err)
	}
	projectReplay, err := projectClient.CreateProject(sessionCtx, createProject)
	if err != nil || projectReplay.GetProject().GetId() != projectOne.GetProject().GetId() {
		t.Fatalf("idempotent replay = %+v, %v", projectReplay, err)
	}
	listedProjects, err := projectClient.ListProjects(sessionCtx, &cyclopusv1.ListProjectsRequest{OrganizationId: createProject.OrganizationId})
	if err != nil || len(listedProjects.GetProjects()) != 1 {
		t.Fatalf("ListProjects() = %+v, %v", listedProjects, err)
	}

	registryID := fixture.insertRegistry(t, createProject.OrganizationId)
	applicationClient := cyclopusv1.NewApplicationServiceClient(conn)
	application, err := applicationClient.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{
		ProjectId: projectOne.GetProject().GetId(), IdempotencyKey: "app-1", Application: validApplication(registryID),
	})
	if err != nil {
		t.Fatal(err)
	}
	listedApps, err := applicationClient.ListApplications(sessionCtx, &cyclopusv1.ListApplicationsRequest{ProjectId: projectOne.GetProject().GetId()})
	if err != nil || len(listedApps.GetApplications()) != 1 {
		t.Fatalf("ListApplications() = %+v, %v", listedApps, err)
	}
	environmentID := fixture.insertEnvironment(t, createProject.OrganizationId, application.GetApplication().GetId())

	scope, err := internalauth.DeployAppScope(application.GetApplication().GetId())
	if err != nil {
		t.Fatal(err)
	}
	createdToken, err := authClient.CreateApiToken(sessionCtx, &cyclopusv1.CreateApiTokenRequest{Name: "CI", Scopes: []string{scope}, IdempotencyKey: "token-1"})
	if err != nil || createdToken.GetToken() == "" || createdToken.GetResult() != cyclopusv1.CreateApiTokenResult_CREATE_API_TOKEN_RESULT_CREATED {
		t.Fatalf("CreateApiToken() = %+v, %v", createdToken, err)
	}
	replayedToken, err := authClient.CreateApiToken(sessionCtx, &cyclopusv1.CreateApiTokenRequest{Name: "CI", Scopes: []string{scope}, IdempotencyKey: "token-1"})
	if err != nil || replayedToken.GetToken() != "" || replayedToken.GetApiToken().GetId() != createdToken.GetApiToken().GetId() ||
		replayedToken.GetResult() != cyclopusv1.CreateApiTokenResult_CREATE_API_TOKEN_RESULT_ALREADY_CREATED_SECRET_NOT_REPLAYABLE {
		t.Fatalf("CreateApiToken() replay = %+v, %v", replayedToken, err)
	}
	var persistedSecret bool
	if err := fixture.pool.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM api_idempotency_records WHERE response_body::text LIKE '%cyp_v1_%'
		UNION ALL SELECT 1 FROM audit_events WHERE coalesce(before::text, '') LIKE '%cyp_v1_%' OR coalesce(after::text, '') LIKE '%cyp_v1_%')`).Scan(&persistedSecret); err != nil {
		t.Fatal(err)
	}
	if persistedSecret {
		t.Fatal("plaintext bearer secret persisted in idempotency or audit JSON")
	}
	var completionMarker string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT error_code FROM api_idempotency_records
		WHERE scope = 'api_token.create' AND idempotency_key = 'token-1'`).Scan(&completionMarker); err != nil {
		t.Fatal(err)
	}
	if completionMarker != "ALREADY_CREATED_SECRET_NOT_REPLAYABLE" {
		t.Fatalf("idempotency completion marker = %q", completionMarker)
	}
	apiCtx := bearerContext(t.Context(), createdToken.GetToken())
	if _, err := projectClient.ListProjects(apiCtx, &cyclopusv1.ListProjectsRequest{OrganizationId: createProject.OrganizationId}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("deploy-only token management error = %v", err)
	}

	releaseClient := cyclopusv1.NewReleaseServiceClient(conn)
	digest := strings.Repeat("a", 64)
	deploy, err := releaseClient.Deploy(apiCtx, &cyclopusv1.DeployRequest{EnvironmentId: environmentID, ImageRef: "ghcr.io/acme/api@sha256:" + digest, GitCommitSha: strings.Repeat("b", 40), IdempotencyKey: "deploy-1"})
	if err != nil || deploy.GetOperation().GetStatus() != cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED {
		t.Fatalf("Deploy() = %+v, %v", deploy, err)
	}
	fixture.assertReleasePersistence(t, deploy.GetOperation().GetId(), strings.Repeat("b", 40))
	var acceptedSnapshot []byte
	var acceptedHash string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT config_snapshot, config_hash FROM releases WHERE id=$1`, deploy.GetOperation().GetId()).Scan(&acceptedSnapshot, &acceptedHash); err != nil {
		t.Fatal(err)
	}
	_, err = applicationClient.UpdateApplication(sessionCtx, &cyclopusv1.UpdateApplicationRequest{
		Application:    &cyclopusv1.Application{Id: application.GetApplication().GetId(), HostnameLabel: "changed", HealthCheckPath: "/changed", ScalingMode: cyclopusv1.ScalingMode_SCALING_MODE_KEDA_HTTP, MinReplicas: 0, MaxReplicas: 9, HpaTargetCpuPercent: 88, CpuRequestMillicores: 777, CpuLimitMillicores: 888, MemoryRequestMib: 777, MemoryLimitMib: 888, RuntimeClass: cyclopusv1.RuntimeClass_RUNTIME_CLASS_RUNC},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"hostname_label", "health_check_path", "scaling_mode", "min_replicas", "max_replicas", "hpa_target_cpu_percent", "cpu_request_millicores", "cpu_limit_millicores", "memory_request_mib", "memory_limit_mib", "runtime_class"}},
		IdempotencyKey: "app-after-deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	var persistedSnapshot []byte
	var persistedHash string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT config_snapshot, config_hash FROM releases WHERE id=$1`, deploy.GetOperation().GetId()).Scan(&persistedSnapshot, &persistedHash); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(persistedSnapshot, acceptedSnapshot) || persistedHash != acceptedHash {
		t.Fatalf("accepted snapshot changed after app update: hash=%s/%s", persistedHash, acceptedHash)
	}
	envelope, err := store.DecodeReleaseSnapshot(persistedSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	desired := envelope.DesiredState
	if desired.Deployment.Container.Resources.CPURequestMillicores != 100 || desired.Deployment.Container.ReadinessProbe.Path != "/healthz" || desired.Deployment.RuntimeClassName != "gvisor" || desired.Ingress.Hostname != "api.example.test" || desired.Scaling.Kind != "hpa" {
		t.Fatalf("accepted desired state was not preserved: %+v", desired)
	}

	otherProjectID := fixture.insertOtherTenant(t)
	if _, err := projectClient.GetProject(sessionCtx, &cyclopusv1.GetProjectRequest{ProjectId: otherProjectID}); status.Code(err) != codes.NotFound || status.Convert(err).Message() != "resource not found" {
		t.Fatalf("cross-tenant error = %v", err)
	}
	_, err = projectClient.GetProject(sessionCtx, &cyclopusv1.GetProjectRequest{ProjectId: "not-a-uuid"})
	assertTypedFieldError(t, err, "project_id")
}

func TestPostgresRESTAPI(t *testing.T) {
	fixture := newAPIFixture(t, "rest")
	handler, err := Handler(nil, fixture.services)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	bootstrap := restJSON(t, server.URL, http.MethodPost, "/v1/auth/bootstrap", "", `{"email":"rest@example.com","password":"local password","idempotencyKey":"claim-rest","bootstrapToken":"`+serverIntegrationBootstrap+`"}`)
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s; %s", bootstrap.Code, bootstrap.Body.String(), fixture.bootstrapFailureState(t))
	}
	login := restJSON(t, server.URL, http.MethodPost, "/v1/auth/login", "", `{"email":"rest@example.com","password":"local password"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	var loginBody struct {
		Session struct {
			AccessToken string `json:"accessToken"`
			User        struct {
				OrganizationID string `json:"organizationId"`
			} `json:"user"`
		} `json:"session"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	project := restJSON(t, server.URL, http.MethodPost, "/v1/organizations/"+loginBody.Session.User.OrganizationID+"/projects", loginBody.Session.AccessToken, `{"name":"REST Project","slug":"rest-project"}`)
	if project.Code != http.StatusBadRequest || !strings.Contains(project.Body.String(), "idempotency_key") {
		t.Fatalf("missing idempotency typed error status=%d body=%s", project.Code, project.Body.String())
	}
	project = restJSON(t, server.URL, http.MethodPost, "/v1/organizations/"+loginBody.Session.User.OrganizationID+"/projects?idempotencyKey=rest-project-1", loginBody.Session.AccessToken, `{"name":"REST Project","slug":"rest-project"}`)
	if project.Code != http.StatusOK {
		t.Fatalf("create project status=%d body=%s", project.Code, project.Body.String())
	}
	listed := restJSON(t, server.URL, http.MethodGet, "/v1/organizations/"+loginBody.Session.User.OrganizationID+"/projects", loginBody.Session.AccessToken, "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "rest-project") {
		t.Fatalf("list projects status=%d body=%s", listed.Code, listed.Body.String())
	}
	unauthenticated := restJSON(t, server.URL, http.MethodGet, "/v1/organizations/"+loginBody.Session.User.OrganizationID+"/projects", "", "")
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	invalid := restJSON(t, server.URL, http.MethodGet, "/v1/projects/not-a-uuid", loginBody.Session.AccessToken, "")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "google.rpc.BadRequest") || !strings.Contains(invalid.Body.String(), "project_id") {
		t.Fatalf("typed REST error status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

type apiFixture struct {
	pool     *pgxpool.Pool
	services Services
}

func (f apiFixture) bootstrapFailureState(t *testing.T) string {
	t.Helper()
	var users, tokens, audits int
	if err := f.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM users),
		(SELECT count(*) FROM api_tokens),
		(SELECT count(*) FROM audit_events)`).Scan(&users, &tokens, &audits); err != nil {
		return fmt.Sprintf("bootstrap database state unavailable: %v", err)
	}
	return fmt.Sprintf("bootstrap database state users=%d tokens=%d audits=%d", users, tokens, audits)
}

func newAPIFixture(t *testing.T, suffix string) apiFixture {
	t.Helper()
	databaseURL := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("POSTGRES_TEST_URL")
	}
	if databaseURL == "" {
		t.Skip("CYCLOPUS_TEST_DATABASE_URL or POSTGRES_TEST_URL is not set")
	}
	ctx := t.Context()
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
	schema := fmt.Sprintf("server_%s_%d", suffix, time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(context.Background())
	})
	testURL := apiSearchPathURL(t, databaseURL, schema)
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
			return fmt.Errorf("read API test search_path: %w", err)
		}
		if want := (pgx.Identifier{schema}).Sanitize(); searchPath != want {
			return fmt.Errorf("API test search_path = %q, want %q", searchPath, want)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dataStore := store.New(pool)
	authService, err := internalauth.NewService(dataStore, []byte(serverIntegrationPepper), []byte(serverIntegrationBootstrap))
	if err != nil {
		t.Fatal(err)
	}
	domainService := service.New(dataStore)
	services := NewServices(authService, domainService, time.Hour)
	services.Authenticator = NewAuthenticator(authService)
	services.RateLimiter = NewAuthRateLimiter(1000)
	return apiFixture{pool: pool, services: services}
}

func apiSearchPathURL(t *testing.T, databaseURL, schema string) string {
	t.Helper()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(config.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for key, value := range config.RuntimeParams {
		query.Set(key, value)
	}
	query.Set("search_path", pgx.Identifier{schema}.Sanitize())
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func (f apiFixture) insertRegistry(t *testing.T, orgID string) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(t.Context(), `INSERT INTO registries (org_id,provider,registry_url,credential_secret_ref) VALUES ($1,'ghcr','https://ghcr.io','k8s-secret://cyclopus/registry#password') RETURNING id::text`, orgID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f apiFixture) insertEnvironment(t *testing.T, orgID, appID string) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(t.Context(), `INSERT INTO environments (org_id,app_id,branch,kind,namespace,planned_hostname) VALUES ($1,$2,'main','production','app-production','api.example.test') RETURNING id::text`, orgID, appID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f apiFixture) insertOtherTenant(t *testing.T) string {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), "DROP INDEX users_singleton"); err != nil {
		t.Fatal(err)
	}
	var projectID string
	err := f.pool.QueryRow(t.Context(), `WITH other_org AS (INSERT INTO orgs(name,slug) VALUES ('Other','other') RETURNING id) INSERT INTO projects(org_id,name,slug) SELECT id,'Secret','secret' FROM other_org RETURNING id::text`).Scan(&projectID)
	if err != nil {
		t.Fatal(err)
	}
	return projectID
}

func (f apiFixture) assertReleasePersistence(t *testing.T, releaseID, commitSHA string) {
	t.Helper()
	var storedCommit string
	var snapshot []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT git_commit_sha,config_snapshot FROM releases WHERE id=$1`, releaseID).Scan(&storedCommit, &snapshot); err != nil {
		t.Fatal(err)
	}
	if storedCommit != commitSHA {
		t.Fatalf("git_commit_sha=%q want=%q", storedCommit, commitSHA)
	}
	if _, err := store.DecodeReleaseSnapshot(snapshot); err != nil {
		t.Fatalf("invalid release snapshot: %v", err)
	}
}

func validApplication(registryID string) *cyclopusv1.Application {
	return &cyclopusv1.Application{Name: "API", Slug: "api", HostnameLabel: "api", RegistryId: registryID, ImageRepository: "acme/api", ContainerPort: 8080, HealthCheckPath: "/healthz", ScalingMode: cyclopusv1.ScalingMode_SCALING_MODE_HPA, MinReplicas: 1, MaxReplicas: 3, HpaTargetCpuPercent: 70, CpuRequestMillicores: 100, CpuLimitMillicores: 1000, MemoryRequestMib: 128, MemoryLimitMib: 512, PreviewsEnabled: true, MaxPreviewEnvironments: 10, PreviewIdleTtlHours: 168, DeployTimeoutSeconds: 600, ReleaseRetentionCount: 5, RuntimeClass: cyclopusv1.RuntimeClass_RUNTIME_CLASS_GVISOR}
}

func bearerContext(ctx context.Context, token string) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
}

func assertTypedFieldError(t *testing.T, err error, field string) {
	t.Helper()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error=%v code=%s", err, status.Code(err))
	}
	found := false
	for _, detail := range status.Convert(err).Details() {
		if badRequest, ok := detail.(*errdetails.BadRequest); ok {
			for _, violation := range badRequest.GetFieldViolations() {
				found = found || violation.GetField() == field
			}
		}
	}
	if !found {
		t.Fatalf("error %v lacks field violation for %q", err, field)
	}
}

func restJSON(t *testing.T, baseURL, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, baseURL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	clientResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer clientResponse.Body.Close()
	response.Code = clientResponse.StatusCode
	for key, values := range clientResponse.Header {
		response.Header()[key] = append([]string(nil), values...)
	}
	_, _ = response.Body.ReadFrom(clientResponse.Body)
	return response
}
