package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	"github.com/shepard-labs/cyclopus/internal/config"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type rateLimitAuthenticationService struct {
	cyclopusv1.UnimplementedAuthenticationServiceServer
}

func (rateLimitAuthenticationService) Login(context.Context, *cyclopusv1.LoginRequest) (*cyclopusv1.LoginResponse, error) {
	return nil, status.Error(codes.Unauthenticated, "invalid credentials")
}

func TestHealthAndReadiness(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		readiness ReadinessCheck
		wantCode  int
		wantBody  string
	}{
		{name: "healthy", path: "/healthz", readiness: func(context.Context) error { return errors.New("ignored") }, wantCode: http.StatusOK, wantBody: `"status":"ok"`},
		{name: "ready", path: "/readyz", readiness: func(context.Context) error { return nil }, wantCode: http.StatusOK, wantBody: `"status":"ready"`},
		{name: "not ready", path: "/readyz", readiness: func(context.Context) error { return errors.New("database password leaked") }, wantCode: http.StatusServiceUnavailable, wantBody: `"status":"unavailable"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, err := Handler(test.readiness, Services{})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantCode {
				t.Fatalf("status = %d, want %d", response.Code, test.wantCode)
			}
			if !strings.Contains(response.Body.String(), test.wantBody) {
				t.Fatalf("body = %q, want %q", response.Body.String(), test.wantBody)
			}
			if strings.Contains(response.Body.String(), "password") {
				t.Fatalf("readiness response exposed internal error: %s", response.Body.String())
			}
		})
	}
}

func TestMainHandlerDoesNotExposeMetrics(t *testing.T) {
	called := atomic.Int32{}
	handler, err := Handler(nil, Services{Metrics: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if r.URL.Path != "/metrics" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, "bounded_metric 1\n")
	})})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusNotFound || called.Load() != 0 {
		t.Fatalf("response=%d %q calls=%d", response.Code, response.Body.String(), called.Load())
	}
}

func TestOperatorListenerServesConfiguredMetrics(t *testing.T) {
	server, err := New(config.HTTP{Address: "127.0.0.1:0", ReadTimeout: time.Second, ReadHeaderTimeout: time.Second,
		WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second, MaxBodyBytes: 1 << 20},
		config.GRPC{Address: "127.0.0.1:0", MaxBodyBytes: 1 << 20}, config.Metrics{Address: "127.0.0.1:0"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "bounded_metric 1\n")
		})})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.metricsServer.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || response.Body.String() != "bounded_metric 1\n" {
		t.Fatalf("operator metrics response = %d %q", response.Code, response.Body.String())
	}
}

func TestNativeGRPCHealthStartsNotServing(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := newGRPCServer(Services{})
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for _, serviceName := range serviceNames {
		response, err := healthv1.NewHealthClient(conn).Check(context.Background(), &healthv1.HealthCheckRequest{Service: serviceName})
		if err != nil {
			t.Fatalf("health check %s: %v", serviceName, err)
		}
		if response.Status != healthv1.HealthCheckResponse_NOT_SERVING {
			t.Fatalf("health status for %s = %s", serviceName, response.Status)
		}
	}
}

func TestGRPCApplicationMethodsAreGatedUntilReadyWhileHealthRemainsAvailable(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	grpcServer, _, err := buildGRPCServer(Services{}, func(context.Context) error { return errors.New("recovering") }, config.GRPC{MaxBodyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_, err = cyclopusv1.NewAuthenticationServiceClient(conn).Login(context.Background(), &cyclopusv1.LoginRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("Login() error = %v, want Unavailable", err)
	}
	response, err := healthv1.NewHealthClient(conn).Check(context.Background(), &healthv1.HealthCheckRequest{Service: serviceNames[0]})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if response.Status != healthv1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("health status = %s", response.Status)
	}
}

func TestGRPCHealthTransitionsWithReadinessAndShutdown(t *testing.T) {
	ready := false
	server, err := New(config.HTTP{Address: "127.0.0.1:0", ReadTimeout: time.Second, ReadHeaderTimeout: time.Second,
		WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second, MaxBodyBytes: 1 << 20},
		config.GRPC{Address: "127.0.0.1:0", MaxBodyBytes: 1 << 20}, config.Metrics{Address: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(context.Context) error {
			if !ready {
				return errors.New("recovering")
			}
			return nil
		}, Services{})
	if err != nil {
		t.Fatal(err)
	}
	checkStatus := func(want healthv1.HealthCheckResponse_ServingStatus) {
		t.Helper()
		for _, serviceName := range append([]string{""}, serviceNames...) {
			response, checkErr := server.healthServer.Check(context.Background(), &healthv1.HealthCheckRequest{Service: serviceName})
			if checkErr != nil {
				t.Fatalf("health check %q: %v", serviceName, checkErr)
			}
			if response.Status != want {
				t.Fatalf("health status for %q = %s, want %s", serviceName, response.Status, want)
			}
		}
	}

	checkStatus(healthv1.HealthCheckResponse_NOT_SERVING)
	ready = true
	server.refreshHealth(context.Background())
	checkStatus(healthv1.HealthCheckResponse_SERVING)
	if err := server.stop(); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	checkStatus(healthv1.HealthCheckResponse_NOT_SERVING)
}

func TestTLSConfigurationRequiresTLS12(t *testing.T) {
	tlsConfig := minimumTLSConfig(tls.Certificate{})
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("minimum TLS version = %d, want TLS 1.2", tlsConfig.MinVersion)
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Fatalf("certificates = %d, want 1", len(tlsConfig.Certificates))
	}
	server, err := New(config.HTTP{Address: "127.0.0.1:0", ReadTimeout: time.Second, ReadHeaderTimeout: time.Second,
		WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second, TLSCertFile: "http.crt",
		TLSKeyFile: "http.key", MaxBodyBytes: 1 << 20}, config.GRPC{Address: "127.0.0.1:0", MaxBodyBytes: 1 << 20},
		config.Metrics{Address: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{})
	if err != nil {
		t.Fatal(err)
	}
	if server.httpServer.TLSConfig == nil || server.httpServer.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("HTTP TLS config = %+v", server.httpServer.TLSConfig)
	}
}

func TestDefaultServicesReturnUnimplemented(t *testing.T) {
	handler, err := Handler(nil, Services{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/projects/project-id", nil))
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestAPIMethodsAreGatedUntilReady(t *testing.T) {
	handler, err := Handler(func(context.Context) error { return errors.New("recovering") }, Services{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"email":"a@example.com","password":"password"}`)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestHTTPBodyLimit(t *testing.T) {
	handler, err := Handler(nil, Services{RequestBodyBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(strings.Repeat("x", 64))))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestProductionHTTPBodyLimitsSeparateGlobalAndAuthCaps(t *testing.T) {
	server, err := New(config.HTTP{Address: "127.0.0.1:0", ReadTimeout: time.Second, ReadHeaderTimeout: time.Second,
		WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second, MaxBodyBytes: 128},
		config.GRPC{Address: "127.0.0.1:0", MaxBodyBytes: 1 << 20}, config.Metrics{Address: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		Services{Authentication: rateLimitAuthenticationService{}, RequestBodyBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	server.refreshHealth(context.Background())

	nonAuth := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(nonAuth, httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader("{}"+strings.Repeat(" ", 62))))
	if nonAuth.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("non-auth REST incorrectly used auth limit: status=%d body=%s", nonAuth.Code, nonAuth.Body.String())
	}

	auth := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(auth, httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader("{}"+strings.Repeat(" ", 62))))
	if auth.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("auth REST status = %d, want 413; body=%s", auth.Code, auth.Body.String())
	}

	global := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(global, httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(strings.Repeat("x", 129))))
	if global.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("non-auth REST global-limit status = %d, want 413; body=%s", global.Code, global.Body.String())
	}
}

func TestAuthRateLimitRESTReturnsTooManyRequests(t *testing.T) {
	services := Services{Authentication: rateLimitAuthenticationService{}, RateLimiter: NewAuthRateLimiter(1), RequestBodyBytes: 65536}
	handler, err := Handler(nil, services)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"email":"user@example.com","password":"wrong"}`))
		request.RemoteAddr = "192.0.2.10:1234"
		handler.ServeHTTP(response, request)
		if attempt == 0 && response.Code != http.StatusUnauthorized {
			t.Fatalf("first REST login status = %d, body=%s", response.Code, response.Body.String())
		}
		if attempt == 1 && (response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "") {
			t.Fatalf("second REST login status = %d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
		}
	}
}

func TestAuthRateLimitRESTIgnoresSpoofedClientHeader(t *testing.T) {
	services := Services{Authentication: rateLimitAuthenticationService{}, RateLimiter: NewAuthRateLimiter(1), RequestBodyBytes: 65536}
	handler, err := Handler(nil, services)
	if err != nil {
		t.Fatal(err)
	}
	for attempt, spoofedClient := range []string{"spoofed-client-one", "spoofed-client-two"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"email":"user@example.com","password":"wrong"}`))
		request.RemoteAddr = "192.0.2.10:1234"
		request.Header.Set("X-Cyclopus-Client", spoofedClient)
		handler.ServeHTTP(response, request)
		if attempt == 0 && response.Code != http.StatusUnauthorized {
			t.Fatalf("first REST login status = %d, body=%s", response.Code, response.Body.String())
		}
		if attempt == 1 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("second REST login status = %d, body=%s", response.Code, response.Body.String())
		}
	}
}

func TestAuthRateLimitSharedAcrossGRPCAndREST(t *testing.T) {
	services := Services{Authentication: rateLimitAuthenticationService{}, RateLimiter: NewAuthRateLimiter(1), RequestBodyBytes: 65536}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := newGRPCServer(services)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, err = cyclopusv1.NewAuthenticationServiceClient(conn).Login(t.Context(), &cyclopusv1.LoginRequest{Email: " User@Example.com ", Password: "wrong"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("first gRPC login error = %v", err)
	}

	handler, err := Handler(nil, services)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"email":"user@example.com","password":"wrong"}`))
	request.RemoteAddr = "bufconn"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("REST status = %d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}

func TestAuthRateLimitGRPCReturnsRetryInfo(t *testing.T) {
	services := Services{Authentication: rateLimitAuthenticationService{}, RateLimiter: NewAuthRateLimiter(1), RequestBodyBytes: 65536}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := newGRPCServer(services)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := cyclopusv1.NewAuthenticationServiceClient(conn)
	request := &cyclopusv1.LoginRequest{Email: "user@example.com", Password: "wrong"}
	_, _ = client.Login(t.Context(), request)
	_, err = client.Login(t.Context(), request)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second gRPC login error = %v", err)
	}
	for _, detail := range status.Convert(err).Details() {
		if retry, ok := detail.(*errdetails.RetryInfo); ok && retry.GetRetryDelay() != nil {
			return
		}
	}
	t.Fatalf("gRPC rate limit error has no RetryInfo: %v", err)
}

func TestAuthRateLimitGRPCIgnoresSpoofedClientMetadata(t *testing.T) {
	services := Services{Authentication: rateLimitAuthenticationService{}, RateLimiter: NewAuthRateLimiter(1), RequestBodyBytes: 65536}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := newGRPCServer(services)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := cyclopusv1.NewAuthenticationServiceClient(conn)
	request := &cyclopusv1.LoginRequest{Email: "user@example.com", Password: "wrong"}
	first := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-cyclopus-client", "spoofed-client-one"))
	if _, err := client.Login(first, request); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("first gRPC login error = %v", err)
	}
	second := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-cyclopus-client", "spoofed-client-two"))
	if _, err := client.Login(second, request); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second gRPC login error = %v, want ResourceExhausted", err)
	}
}

func TestAuthRateLimiterCardinalityIsStrictlyBounded(t *testing.T) {
	limiter := NewAuthRateLimiter(1)
	for index := range authRateLimiterMaxEntries * 2 {
		client := fmt.Sprintf("192.0.2.%d", index)
		account := fmt.Sprintf("user-%d@example.com", index)
		limiter.allow(client, account)
	}
	if got := len(limiter.perKey); got != authRateLimiterMaxEntries {
		t.Fatalf("limiter entries = %d, want %d", got, authRateLimiterMaxEntries)
	}
	for element := limiter.recency.Front(); element != nil; element = element.Next() {
		key := element.Value.(rateBucket).key
		if !strings.HasPrefix(key, "account:") && !strings.HasPrefix(key, "client:") {
			t.Fatalf("unexpected limiter key %q", key)
		}
	}
}

func TestAuthRateLimiterSprayCannotResetExhaustedBucket(t *testing.T) {
	limiter := NewAuthRateLimiter(1)
	limiter.maxEntries = 4
	if allowed, _ := limiter.allow("192.0.2.10", "target@example.com"); !allowed {
		t.Fatal("target request was unexpectedly limited")
	}
	if allowed, _ := limiter.allow("192.0.2.11", "spray-1@example.com"); !allowed {
		t.Fatal("request filling remaining capacity was unexpectedly limited")
	}
	for index := range 100 {
		allowed, retryAfter := limiter.allow(fmt.Sprintf("198.51.100.%d", index), fmt.Sprintf("spray-%d@example.com", index+2))
		if allowed || retryAfter <= 0 {
			t.Fatalf("spray request %d = allowed %t retry %s, want fail closed", index, allowed, retryAfter)
		}
	}
	if _, ok := limiter.perKey["account:target@example.com"]; !ok {
		t.Fatal("spray evicted exhausted target account bucket")
	}
	if _, ok := limiter.perKey["client:192.0.2.10"]; !ok {
		t.Fatal("spray evicted exhausted target client bucket")
	}
	if allowed, _ := limiter.allow("192.0.2.10", "target@example.com"); allowed {
		t.Fatal("spray reset exhausted target buckets")
	}
}

func TestAuthRateLimiterSaturationEvictsLRUBucketWithUsableToken(t *testing.T) {
	now := time.Unix(1_000, 0)
	limiter := NewAuthRateLimiter(2)
	limiter.maxEntries = 4
	limiter.now = func() time.Time { return now }
	if allowed, _ := limiter.allow("192.0.2.10", "old@example.com"); !allowed {
		t.Fatal("initial request was unexpectedly limited")
	}
	if allowed, _ := limiter.allow("192.0.2.11", "recent@example.com"); !allowed {
		t.Fatal("capacity-filling request was unexpectedly limited")
	}
	if allowed, retryAfter := limiter.allow("192.0.2.12", "new@example.com"); !allowed {
		t.Fatalf("unseen identity was globally denied with usable buckets: retry after %s", retryAfter)
	}
	if len(limiter.perKey) != limiter.maxEntries {
		t.Fatalf("limiter entries = %d, want %d", len(limiter.perKey), limiter.maxEntries)
	}
	if _, ok := limiter.perKey["account:old@example.com"]; ok {
		t.Fatal("least-recently-used usable account bucket was not evicted")
	}
	if _, ok := limiter.perKey["client:192.0.2.10"]; ok {
		t.Fatal("least-recently-used usable client bucket was not evicted")
	}
}

func TestAuthRateLimiterSaturationProtectsExhaustedAndEvictsUsable(t *testing.T) {
	now := time.Unix(1_000, 0)
	limiter := NewAuthRateLimiter(2)
	limiter.maxEntries = 4
	limiter.now = func() time.Time { return now }
	if allowed, _ := limiter.allow("192.0.2.10", "exhausted@example.com"); !allowed {
		t.Fatal("first target request was unexpectedly limited")
	}
	if allowed, _ := limiter.allow("192.0.2.10", "exhausted@example.com"); !allowed {
		t.Fatal("second target request was unexpectedly limited")
	}
	if allowed, _ := limiter.allow("192.0.2.11", "usable@example.com"); !allowed {
		t.Fatal("usable request was unexpectedly limited")
	}
	if allowed, retryAfter := limiter.allow("192.0.2.12", "new@example.com"); !allowed {
		t.Fatalf("unseen identity was denied despite usable eviction candidates: %s", retryAfter)
	}
	for _, key := range []string{"account:exhausted@example.com", "client:192.0.2.10"} {
		if _, ok := limiter.perKey[key]; !ok {
			t.Fatalf("exhausted bucket %q was evicted", key)
		}
	}
	if allowed, _ := limiter.allow("192.0.2.10", "exhausted@example.com"); allowed {
		t.Fatal("exhausted protected identity was reset")
	}
}

func TestAuthRateLimiterFailedReservationDoesNotPartiallyEvict(t *testing.T) {
	now := time.Unix(1_000, 0)
	limiter := NewAuthRateLimiter(2)
	limiter.maxEntries = 3
	limiter.now = func() time.Time { return now }
	if allowed, _ := limiter.allow("192.0.2.10", "exhausted@example.com"); !allowed {
		t.Fatal("first request was unexpectedly limited")
	}
	if allowed, _ := limiter.allow("192.0.2.10", "exhausted@example.com"); !allowed {
		t.Fatal("second request was unexpectedly limited")
	}
	limiter.storeBucket(rateBucket{key: "account:usable@example.com", tokens: 1, updated: now})
	if allowed, retryAfter := limiter.allow("192.0.2.11", "new@example.com"); allowed || retryAfter <= 0 {
		t.Fatalf("reservation = allowed %t retry %s, want fail closed", allowed, retryAfter)
	}
	if _, ok := limiter.perKey["account:usable@example.com"]; !ok {
		t.Fatal("failed reservation partially evicted a usable bucket")
	}
	if len(limiter.perKey) != 3 {
		t.Fatalf("limiter entries = %d, want 3", len(limiter.perKey))
	}
}

func TestAuthRateLimiterReclaimsFullyRefilledBuckets(t *testing.T) {
	now := time.Unix(1_000, 0)
	limiter := NewAuthRateLimiter(1)
	limiter.maxEntries = 2
	limiter.now = func() time.Time { return now }
	if allowed, _ := limiter.allow("192.0.2.10", "old@example.com"); !allowed {
		t.Fatal("initial request was unexpectedly limited")
	}
	now = now.Add(time.Minute)
	if allowed, retryAfter := limiter.allow("192.0.2.11", "new@example.com"); !allowed {
		t.Fatalf("stale buckets were not reclaimed: retry after %s", retryAfter)
	}
	if len(limiter.perKey) != 2 {
		t.Fatalf("limiter entries = %d, want 2", len(limiter.perKey))
	}
	if _, ok := limiter.perKey["account:old@example.com"]; ok {
		t.Fatal("fully refilled account bucket was not reclaimed")
	}
	if _, ok := limiter.perKey["client:192.0.2.10"]; ok {
		t.Fatal("fully refilled client bucket was not reclaimed")
	}
}

func TestAuthRateLimiterClientBucketSurvivesAccountRotation(t *testing.T) {
	limiter := NewAuthRateLimiter(1)
	if allowed, _ := limiter.allow("192.0.2.10", "first@example.com"); !allowed {
		t.Fatal("first request was unexpectedly limited")
	}
	if allowed, _ := limiter.allow("192.0.2.10", "second@example.com"); allowed {
		t.Fatal("rotating account bypassed direct-client limit")
	}
}

func TestAuthRateLimiterAccountBucketSurvivesClientRotation(t *testing.T) {
	limiter := NewAuthRateLimiter(1)
	if allowed, _ := limiter.allow("192.0.2.10", "user@example.com"); !allowed {
		t.Fatal("first request was unexpectedly limited")
	}
	if allowed, _ := limiter.allow("192.0.2.11", " USER@example.com "); allowed {
		t.Fatal("rotating client bypassed normalized account limit")
	}
}

func TestAuthRequestBodyLimitAcrossTransports(t *testing.T) {
	services := Services{Authentication: rateLimitAuthenticationService{}, RateLimiter: NewAuthRateLimiter(100), RequestBodyBytes: 32}
	handler, err := Handler(nil, services)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"email":"user@example.com","password":"`+strings.Repeat("x", 64)+`"}`)))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("REST status = %d, body = %s", response.Code, response.Body.String())
	}

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := newGRPCServer(services)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, err = cyclopusv1.NewAuthenticationServiceClient(conn).Login(t.Context(), &cyclopusv1.LoginRequest{
		Email: "user@example.com", Password: strings.Repeat("x", 64),
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("gRPC oversized login error = %v", err)
	}
}

func TestProductionReadinessRequestsUseCachedProbeState(t *testing.T) {
	var calls atomic.Int32
	server, err := New(config.HTTP{Address: "127.0.0.1:0", ReadTimeout: time.Second, ReadHeaderTimeout: time.Second,
		WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second, MaxBodyBytes: 1 << 20},
		config.GRPC{Address: "127.0.0.1:0", MaxBodyBytes: 1 << 20}, config.Metrics{Address: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(context.Context) error { calls.Add(1); return nil }, Services{})
	if err != nil {
		t.Fatal(err)
	}
	server.refreshHealth(context.Background())
	for index := range 10 {
		for _, path := range []string{"/readyz", "/v1/projects/project-id"} {
			response := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code == http.StatusServiceUnavailable {
				t.Fatalf("request %d to %s was unexpectedly unready", index, path)
			}
		}
	}
	for range 10 {
		if _, err := server.healthServer.Check(context.Background(), &healthv1.HealthCheckRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("underlying readiness probe calls = %d, want 1", got)
	}
}

func TestReadinessProbeIsBoundedByTimeout(t *testing.T) {
	probeErr := make(chan error, 1)
	state := newReadinessState(func(ctx context.Context) error {
		<-ctx.Done()
		probeErr <- ctx.Err()
		return ctx.Err()
	})
	state.probeTimeout = 10 * time.Millisecond
	state.refresh(context.Background())
	if err := <-probeErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe context error = %v, want DeadlineExceeded", err)
	}
	if err := state.check(context.Background()); !errors.Is(err, errNotReady) {
		t.Fatalf("readiness check error = %v, want not ready", err)
	}
}

func TestRunReturnsHTTPStartupError(t *testing.T) {
	want := errors.New("HTTP bind failed")
	server := newTestServer(t)
	server.listenHTTP = func(string, string) (net.Listener, error) { return nil, want }
	if err := server.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want %v", err, want)
	}
}

func TestRunReturnsGRPCStartupError(t *testing.T) {
	want := errors.New("gRPC bind failed")
	server := newTestServer(t)
	server.listenHTTP = func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") }
	server.listenGRPC = func(string, string) (net.Listener, error) { return nil, want }
	if err := server.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want %v", err, want)
	}
}

func TestRunReturnsMetricsStartupError(t *testing.T) {
	want := errors.New("metrics bind failed")
	server := newTestServer(t)
	server.listenHTTP = func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") }
	server.listenGRPC = func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") }
	server.listenMetrics = func(string, string) (net.Listener, error) { return nil, want }
	if err := server.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want %v", err, want)
	}
}

func TestRunReturnsShutdownError(t *testing.T) {
	want := errors.New("drain failed")
	server := newTestServer(t)
	started := configureEphemeralListeners(server)
	server.shutdownHTTP = func(context.Context) error { return want }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want %v", err, want)
	}
}

func TestStopStartsGRPCDrainWhileHTTPShutdownIsBlocked(t *testing.T) {
	server := newTestServer(t)
	httpStarted := make(chan struct{})
	releaseHTTP := make(chan struct{})
	grpcStarted := make(chan struct{})
	server.shutdownHTTP = func(context.Context) error {
		close(httpStarted)
		<-releaseHTTP
		return nil
	}
	server.gracefulStopGRPC = func() { close(grpcStarted) }
	done := make(chan error, 1)
	go func() { done <- server.stop() }()
	<-httpStarted
	select {
	case <-grpcStarted:
	case <-time.After(time.Second):
		t.Fatal("gRPC drain did not start while HTTP shutdown was blocked")
	}
	close(releaseHTTP)
	if err := <-done; err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	server := newTestServer(t)
	started := configureEphemeralListeners(server)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func configureEphemeralListeners(server *Server) <-chan struct{} {
	started := make(chan struct{})
	listeners := 0
	listen := func(network, _ string) (net.Listener, error) {
		listener, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			listeners++
			if listeners == 3 {
				close(started)
			}
		}
		return listener, err
	}
	server.listenHTTP = listen
	server.listenGRPC = listen
	server.listenMetrics = listen
	return started
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(config.HTTP{Address: "127.0.0.1:0", ReadTimeout: time.Second, ReadHeaderTimeout: time.Second,
		WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second, MaxBodyBytes: 1 << 20}, config.GRPC{Address: "127.0.0.1:0", MaxBodyBytes: 1 << 20},
		config.Metrics{Address: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Services{})
	if err != nil {
		t.Fatal(err)
	}
	return server
}
