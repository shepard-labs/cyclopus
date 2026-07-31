package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	cyclopusv1 "github.com/cyclopus-dev/cyclopus/gen/go/cyclopus/v1"
	"github.com/cyclopus-dev/cyclopus/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

var serviceNames = []string{
	cyclopusv1.AuthenticationService_ServiceDesc.ServiceName,
	cyclopusv1.OrganizationService_ServiceDesc.ServiceName,
	cyclopusv1.ProjectService_ServiceDesc.ServiceName,
	cyclopusv1.ApplicationService_ServiceDesc.ServiceName,
	cyclopusv1.EnvironmentService_ServiceDesc.ServiceName,
	cyclopusv1.ReleaseService_ServiceDesc.ServiceName,
	cyclopusv1.ReconciliationService_ServiceDesc.ServiceName,
	cyclopusv1.WebhookService_ServiceDesc.ServiceName,
	cyclopusv1.DomainService_ServiceDesc.ServiceName,
	cyclopusv1.RegistryService_ServiceDesc.ServiceName,
	cyclopusv1.GitConnectionService_ServiceDesc.ServiceName,
	cyclopusv1.ObservabilityService_ServiceDesc.ServiceName,
}

const (
	readinessRefreshInterval = time.Second
	readinessProbeTimeout    = 2 * time.Second
)

var errNotReady = errors.New("service is not ready")

type ReadinessCheck func(context.Context) error

type Server struct {
	httpServer       *http.Server
	grpcServer       *grpc.Server
	grpcAddress      string
	shutdownTimeout  time.Duration
	listenHTTP       func(network, address string) (net.Listener, error)
	listenGRPC       func(network, address string) (net.Listener, error)
	shutdownHTTP     func(context.Context) error
	gracefulStopGRPC func()
	forceStopGRPC    func()
	httpTLSCertFile  string
	httpTLSKeyFile   string
	healthServer     *health.Server
	readiness        *readinessState
}

type readinessState struct {
	probe        ReadinessCheck
	probeTimeout time.Duration
	ready        atomic.Bool
}

func newReadinessState(probe ReadinessCheck) *readinessState {
	if probe == nil {
		probe = func(context.Context) error { return nil }
	}
	return &readinessState{probe: probe, probeTimeout: readinessProbeTimeout}
}

func (r *readinessState) check(context.Context) error {
	if !r.ready.Load() {
		return errNotReady
	}
	return nil
}

func (r *readinessState) refresh(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, r.probeTimeout)
	defer cancel()
	r.ready.Store(r.probe(probeCtx) == nil)
}

func New(httpConfig config.HTTP, grpcConfig config.GRPC, logger *slog.Logger, readiness ReadinessCheck, services Services) (*Server, error) {
	services = withDefaults(services)
	readinessState := newReadinessState(readiness)
	handler, err := buildHandler(readinessState.check, services, httpConfig.MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	httpServer := &http.Server{
		Addr: httpConfig.Address, Handler: handler, ReadTimeout: httpConfig.ReadTimeout,
		ReadHeaderTimeout: httpConfig.ReadHeaderTimeout, WriteTimeout: httpConfig.WriteTimeout,
		IdleTimeout: httpConfig.IdleTimeout, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	if httpConfig.TLSCertFile != "" {
		httpServer.TLSConfig = minimumTLSConfig()
	}
	grpcServer, healthServer, err := buildGRPCServer(services, readinessState.check, grpcConfig)
	if err != nil {
		return nil, err
	}
	server := &Server{httpServer: httpServer, grpcServer: grpcServer, grpcAddress: grpcConfig.Address,
		shutdownTimeout: httpConfig.ShutdownTimeout, listenHTTP: net.Listen, listenGRPC: net.Listen}
	server.httpTLSCertFile, server.httpTLSKeyFile = httpConfig.TLSCertFile, httpConfig.TLSKeyFile
	server.healthServer, server.readiness = healthServer, readinessState
	server.shutdownHTTP = httpServer.Shutdown
	server.gracefulStopGRPC = grpcServer.GracefulStop
	server.forceStopGRPC = grpcServer.Stop
	return server, nil
}

func Handler(readiness ReadinessCheck, services Services) (http.Handler, error) {
	return buildHandler(readiness, services, 0)
}

func buildHandler(readiness ReadinessCheck, services Services, maxBodyBytes int64) (http.Handler, error) {
	if readiness == nil {
		readiness = func(context.Context) error { return nil }
	}
	services = withDefaults(services)
	gateway := runtime.NewServeMux(runtime.WithErrorHandler(gatewayErrorHandler), runtime.WithMetadata(func(_ context.Context, r *http.Request) metadata.MD {
		return metadata.Pairs("authorization", r.Header.Get("Authorization"),
			"x-cyclopus-bootstrap-token", r.Header.Get("X-Cyclopus-Bootstrap-Token"))
	}))
	ctx := context.Background()
	registrations := []func() error{
		func() error {
			return cyclopusv1.RegisterAuthenticationServiceHandlerServer(ctx, gateway, services.Authentication)
		},
		func() error {
			return cyclopusv1.RegisterOrganizationServiceHandlerServer(ctx, gateway, services.Organization)
		},
		func() error { return cyclopusv1.RegisterProjectServiceHandlerServer(ctx, gateway, services.Project) },
		func() error {
			return cyclopusv1.RegisterApplicationServiceHandlerServer(ctx, gateway, services.Application)
		},
		func() error {
			return cyclopusv1.RegisterEnvironmentServiceHandlerServer(ctx, gateway, services.Environment)
		},
		func() error { return cyclopusv1.RegisterReleaseServiceHandlerServer(ctx, gateway, services.Release) },
		func() error {
			return cyclopusv1.RegisterReconciliationServiceHandlerServer(ctx, gateway, services.Reconciliation)
		},
		func() error { return cyclopusv1.RegisterWebhookServiceHandlerServer(ctx, gateway, services.Webhook) },
		func() error { return cyclopusv1.RegisterDomainServiceHandlerServer(ctx, gateway, services.Domain) },
		func() error { return cyclopusv1.RegisterRegistryServiceHandlerServer(ctx, gateway, services.Registry) },
		func() error {
			return cyclopusv1.RegisterGitConnectionServiceHandlerServer(ctx, gateway, services.GitConnection)
		},
		func() error {
			return cyclopusv1.RegisterObservabilityServiceHandlerServer(ctx, gateway, services.Observability)
		},
	}
	for _, register := range registrations {
		if err := register(); err != nil {
			return nil, fmt.Errorf("register REST gateway: %w", err)
		}
	}
	router := chi.NewRouter()
	router.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w, http.StatusOK, "ok") })
	router.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := readiness(r.Context()); err != nil {
			writeStatus(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		writeStatus(w, http.StatusOK, "ready")
	})
	api := http.Handler(gateway)
	if services.Authenticator != nil {
		api = services.Authenticator.HTTPMiddleware(api)
	}
	api = authRateLimitHTTPMiddleware(services.RateLimiter, api)
	api = requestBodyLimitHTTPMiddleware(maxBodyBytes, services.RequestBodyBytes, api)
	api = readinessHTTPMiddleware(readiness, api)
	api = clientContextHTTPMiddleware(api)
	router.Mount("/", api)
	return router, nil
}

func newGRPCServer(services Services) *grpc.Server {
	server, _, err := buildGRPCServer(services, func(context.Context) error { return nil }, config.GRPC{})
	if err != nil {
		panic(err)
	}
	return server
}

func buildGRPCServer(services Services, readiness ReadinessCheck, grpcConfig config.GRPC) (*grpc.Server, *health.Server, error) {
	services = withDefaults(services)
	if readiness == nil {
		readiness = func(context.Context) error { return nil }
	}
	options := []grpc.ServerOption{grpc.MaxRecvMsgSize(grpcConfig.MaxBodyBytes)}
	if grpcConfig.MaxBodyBytes <= 0 {
		options = []grpc.ServerOption{grpc.MaxRecvMsgSize(1 << 20)}
	}
	if grpcConfig.TLSCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(grpcConfig.TLSCertFile, grpcConfig.TLSKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("load gRPC TLS certificate: %w", err)
		}
		options = append(options, grpc.Creds(credentials.NewTLS(minimumTLSConfig(certificate))))
	}
	interceptor := unaryBoundaryInterceptor(readiness, services)
	options = append(options, grpc.UnaryInterceptor(interceptor))
	server := grpc.NewServer(options...)
	cyclopusv1.RegisterAuthenticationServiceServer(server, services.Authentication)
	cyclopusv1.RegisterOrganizationServiceServer(server, services.Organization)
	cyclopusv1.RegisterProjectServiceServer(server, services.Project)
	cyclopusv1.RegisterApplicationServiceServer(server, services.Application)
	cyclopusv1.RegisterEnvironmentServiceServer(server, services.Environment)
	cyclopusv1.RegisterReleaseServiceServer(server, services.Release)
	cyclopusv1.RegisterReconciliationServiceServer(server, services.Reconciliation)
	cyclopusv1.RegisterWebhookServiceServer(server, services.Webhook)
	cyclopusv1.RegisterDomainServiceServer(server, services.Domain)
	cyclopusv1.RegisterRegistryServiceServer(server, services.Registry)
	cyclopusv1.RegisterGitConnectionServiceServer(server, services.GitConnection)
	cyclopusv1.RegisterObservabilityServiceServer(server, services.Observability)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthv1.HealthCheckResponse_NOT_SERVING)
	for _, serviceName := range serviceNames {
		healthServer.SetServingStatus(serviceName, healthv1.HealthCheckResponse_NOT_SERVING)
	}
	healthv1.RegisterHealthServer(server, healthServer)
	return server, healthServer, nil
}

func minimumTLSConfig(certificates ...tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12}
}

func (s *Server) Run(ctx context.Context) error {
	httpListener, err := s.listenHTTP("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listen HTTP on %s: %w", s.httpServer.Addr, err)
	}
	grpcListener, err := s.listenGRPC("tcp", s.grpcAddress)
	if err != nil {
		_ = httpListener.Close()
		return fmt.Errorf("listen gRPC on %s: %w", s.grpcAddress, err)
	}
	serveErr := make(chan error, 2)
	s.refreshHealth(context.Background())
	go func() {
		ticker := time.NewTicker(readinessRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshHealth(ctx)
			}
		}
	}()
	go func() {
		if s.httpTLSCertFile != "" {
			serveErr <- normalizeServeError("HTTP", s.httpServer.ServeTLS(httpListener, s.httpTLSCertFile, s.httpTLSKeyFile))
			return
		}
		serveErr <- normalizeServeError("HTTP", s.httpServer.Serve(httpListener))
	}()
	go func() { serveErr <- normalizeServeError("gRPC", s.grpcServer.Serve(grpcListener)) }()
	select {
	case err := <-serveErr:
		stopErr := s.stop()
		return errors.Join(err, <-serveErr, stopErr)
	case <-ctx.Done():
		stopErr := s.stop()
		return errors.Join(<-serveErr, <-serveErr, stopErr)
	}
}

func (s *Server) refreshHealth(ctx context.Context) {
	s.readiness.refresh(ctx)
	statusValue := healthv1.HealthCheckResponse_SERVING
	if s.readiness.check(ctx) != nil {
		statusValue = healthv1.HealthCheckResponse_NOT_SERVING
	}
	s.healthServer.SetServingStatus("", statusValue)
	for _, serviceName := range serviceNames {
		s.healthServer.SetServingStatus(serviceName, statusValue)
	}
}

func bodyLimitMiddleware(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > limit {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func requestBodyLimitHTTPMiddleware(globalLimit, authLimit int64, next http.Handler) http.Handler {
	if authLimit <= 0 {
		authLimit = 65536
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := globalLimit
		if isPublicAuthPath(r.URL.Path) && (limit <= 0 || authLimit < limit) {
			limit = authLimit
		}
		if limit <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		bodyLimitMiddleware(limit, next).ServeHTTP(w, r)
	})
}

func readinessHTTPMiddleware(readiness ReadinessCheck, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := readiness(r.Context()); err != nil {
			gatewayErrorHandler(r.Context(), nil, &runtime.JSONPb{}, w, r, status.Error(codes.Unavailable, "service is starting"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientContextHTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(contextWithClient(r.Context(), clientFromRemoteAddr(r.RemoteAddr))))
	})
}

func authRateLimitHTTPMiddleware(limiter *authRateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isPublicAuthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		var payload struct {
			Email string `json:"email"`
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		_ = json.Unmarshal(body, &payload)
		if allowed, retryAfter := limiter.allow(clientFromRESTContext(r.Context()), payload.Email); !allowed {
			gatewayErrorHandler(r.Context(), nil, &runtime.JSONPb{}, w, r, rateLimitError(retryAfter))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPublicAuthPath(path string) bool {
	return path == "/v1/auth/bootstrap" || path == "/v1/auth/login"
}

func unaryBoundaryInterceptor(readiness ReadinessCheck, services Services) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") {
			return handler(ctx, req)
		}
		if err := readiness(ctx); err != nil {
			return nil, status.Error(codes.Unavailable, "service is starting")
		}
		if isPublicMethod(info.FullMethod) {
			if message, ok := req.(proto.Message); ok && services.RequestBodyBytes > 0 && int64(proto.Size(message)) > services.RequestBodyBytes {
				return nil, status.Error(codes.ResourceExhausted, "authentication request body too large")
			}
			if allowed, retryAfter := services.RateLimiter.allow(clientFromGRPCPeer(ctx), authAccount(req)); !allowed {
				return nil, rateLimitError(retryAfter)
			}
		}
		if services.Authenticator != nil {
			return services.Authenticator.UnaryServerInterceptor()(ctx, req, info, handler)
		}
		return handler(ctx, req)
	}
}

func (s *Server) stop() error {
	s.healthServer.SetServingStatus("", healthv1.HealthCheckResponse_NOT_SERVING)
	for _, serviceName := range serviceNames {
		s.healthServer.SetServingStatus(serviceName, healthv1.HealthCheckResponse_NOT_SERVING)
	}
	s.healthServer.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	httpDone := make(chan error, 1)
	grpcDone := make(chan struct{})
	go func() { httpDone <- s.shutdownHTTP(shutdownCtx) }()
	go func() { s.gracefulStopGRPC(); close(grpcDone) }()

	var shutdownErr error
	for httpDone != nil || grpcDone != nil {
		select {
		case err := <-httpDone:
			httpDone = nil
			if err != nil {
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown HTTP: %w", err))
				_ = s.httpServer.Close()
			}
		case <-grpcDone:
			grpcDone = nil
		case <-shutdownCtx.Done():
			_ = s.httpServer.Close()
			s.forceStopGRPC()
			if httpDone != nil {
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown HTTP: %w", shutdownCtx.Err()))
			}
			if grpcDone != nil {
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown gRPC: %w", shutdownCtx.Err()))
				<-grpcDone
			}
			return shutdownErr
		}
	}
	return shutdownErr
}

func normalizeServeError(transport string, err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return fmt.Errorf("serve %s: %w", transport, err)
}

func gatewayErrorHandler(ctx context.Context, mux *runtime.ServeMux, marshaler runtime.Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	grpcStatus := status.Convert(err)
	for _, detail := range grpcStatus.Details() {
		if retry, ok := detail.(*errdetails.RetryInfo); ok && retry.GetRetryDelay() != nil {
			seconds := int(math.Ceil(retry.GetRetryDelay().AsDuration().Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(runtime.HTTPStatusFromCode(grpcStatus.Code()))
	if encodeErr := marshaler.NewEncoder(w).Encode(grpcStatus.Proto()); encodeErr != nil {
		runtime.DefaultHTTPErrorHandler(ctx, mux, marshaler, w, r, encodeErr)
	}
}

func rateLimitError(retryAfter time.Duration) error {
	if retryAfter <= 0 {
		retryAfter = time.Second
	}
	base := status.New(codes.ResourceExhausted, "authentication rate limit exceeded")
	withDetails, err := base.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(retryAfter)})
	if err != nil {
		return base.Err()
	}
	return withDetails.Err()
}

func writeStatus(w http.ResponseWriter, code int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": value})
}
