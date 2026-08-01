//go:build integration

package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	internalauth "github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// e2eTelemetry records the scope the API stamps and returns canned results, so
// the e2e test can assert tenant scoping through the whole gRPC path.
type e2eTelemetry struct {
	lastScope observability.TenantScope
	called    bool
}

func (r *e2eTelemetry) QueryLogs(_ context.Context, scope observability.TenantScope, _ observability.TimeRange, _ observability.LogFilter) (observability.LogPage, error) {
	r.called = true
	r.lastScope = scope
	return observability.LogPage{Entries: []observability.LogEntry{{Body: "hello", Severity: observability.SeverityInfo, Timestamp: time.Unix(1000, 0)}}}, nil
}

func (r *e2eTelemetry) ListTraces(_ context.Context, scope observability.TenantScope, _ observability.TimeRange, _ observability.TraceFilter) (observability.TracePage, error) {
	r.called = true
	r.lastScope = scope
	return observability.TracePage{}, nil
}

func (r *e2eTelemetry) GetTrace(_ context.Context, scope observability.TenantScope, _ observability.TimeRange, _ string) (observability.TraceSummary, bool, error) {
	r.called = true
	r.lastScope = scope
	return observability.TraceSummary{}, false, nil
}

func (r *e2eTelemetry) GetOperatorMetrics(context.Context) ([]observability.OperatorMetric, error) {
	r.called = true
	return []observability.OperatorMetric{{Name: "node-1/cpu.usage", Value: 12.5, Unit: "percent"}}, nil
}

func newTelemetryFixture(t *testing.T, telemetry service.TelemetryProvider) apiFixture {
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
	schema := fmt.Sprintf("server_telemetry_%d", time.Now().UnixNano())
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
	pool, err := pgxpool.New(ctx, testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dataStore := store.New(pool)
	authService, err := internalauth.NewService(dataStore, []byte(serverIntegrationPepper), []byte(serverIntegrationBootstrap))
	if err != nil {
		t.Fatal(err)
	}
	domainService := service.New(dataStore, service.WithTelemetry(telemetry))
	services := NewServices(authService, domainService, time.Hour)
	services.Authenticator = NewAuthenticator(authService)
	services.RateLimiter = NewAuthRateLimiter(1000)
	return apiFixture{pool: pool, services: services}
}

func TestTelemetryTenantScopingEndToEnd(t *testing.T) {
	fake := &e2eTelemetry{}
	fixture := newTelemetryFixture(t, fake)

	server := newGRPCServer(fixture.services)
	listener, dial := listenGRPC(t)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dial))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	authClient := cyclopusv1.NewAuthenticationServiceClient(conn)
	bootstrap, err := authClient.Bootstrap(t.Context(), &cyclopusv1.BootstrapRequest{Email: "op@example.com", Password: "local password", BootstrapToken: serverIntegrationBootstrap})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	orgID := bootstrap.GetSession().GetUser().GetOrganizationId()
	sessionCtx := bearerContext(t.Context(), bootstrap.GetSession().GetAccessToken())

	projectClient := cyclopusv1.NewProjectServiceClient(conn)
	project, err := projectClient.CreateProject(sessionCtx, &cyclopusv1.CreateProjectRequest{OrganizationId: orgID, IdempotencyKey: "p1", Project: &cyclopusv1.Project{Name: "Platform", Slug: "platform"}})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	registryID := fixture.insertRegistry(t, orgID)
	appClient := cyclopusv1.NewApplicationServiceClient(conn)
	app, err := appClient.CreateApplication(sessionCtx, &cyclopusv1.CreateApplicationRequest{ProjectId: project.GetProject().GetId(), IdempotencyKey: "a1", Application: validApplication(registryID)})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	appID := app.GetApplication().GetId()
	environmentID := fixture.insertEnvironment(t, orgID, appID)

	obsClient := cyclopusv1.NewObservabilityServiceClient(conn)

	// Authorized query: the stamped scope must exactly match the validated IDs.
	logs, err := obsClient.QueryApplicationLogs(sessionCtx, &cyclopusv1.QueryApplicationLogsRequest{ApplicationId: appID, EnvironmentId: environmentID})
	if err != nil {
		t.Fatalf("QueryApplicationLogs: %v", err)
	}
	if len(logs.GetEntries()) != 1 || logs.GetEntries()[0].GetBody() != "hello" {
		t.Fatalf("unexpected log entries: %+v", logs.GetEntries())
	}
	if fake.lastScope.OrgID != orgID || fake.lastScope.AppID != appID || fake.lastScope.EnvironmentID != environmentID {
		t.Fatalf("tenant scope not stamped from validated IDs: %+v", fake.lastScope)
	}

	// Cross-tenant probe: an app_id that is not in this org must return NotFound
	// and must NOT reach the adapter.
	fake.called = false
	if _, err := obsClient.QueryApplicationLogs(sessionCtx, &cyclopusv1.QueryApplicationLogsRequest{ApplicationId: "00000000-0000-0000-0000-0000000000ff"}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant app_id error = %v, want NotFound", err)
	}
	if fake.called {
		t.Fatal("adapter reached for a cross-tenant application")
	}

	// Operator metrics are install-level: any authenticated member may read them.
	metrics, err := obsClient.GetOperatorMetrics(sessionCtx, &cyclopusv1.GetOperatorMetricsRequest{})
	if err != nil {
		t.Fatalf("GetOperatorMetrics: %v", err)
	}
	if len(metrics.GetMetrics()) != 1 || metrics.GetMetrics()[0].GetName() != "node-1/cpu.usage" {
		t.Fatalf("unexpected operator metrics: %+v", metrics.GetMetrics())
	}

	// Unauthenticated call is rejected.
	if _, err := obsClient.GetOperatorMetrics(t.Context(), &cyclopusv1.GetOperatorMetricsRequest{}); status.Code(err) == codes.OK {
		t.Fatal("operator metrics must require authentication")
	}
}

func listenGRPC(t *testing.T) (net.Listener, func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	return listener, func(context.Context, string) (net.Conn, error) { return listener.Dial() }
}
