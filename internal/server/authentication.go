package server

import (
	"context"
	"net/http"
	"strings"

	cyclopusv1 "github.com/cyclopus-dev/cyclopus/gen/go/cyclopus/v1"
	internalauth "github.com/cyclopus-dev/cyclopus/internal/auth"
	"github.com/cyclopus-dev/cyclopus/internal/service"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type contextKey int

const (
	principalContextKey contextKey = iota
	actorContextKey
)

type Authenticator struct {
	auth *internalauth.Service
}

func NewAuthenticator(authService *internalauth.Service) *Authenticator {
	return &Authenticator{auth: authService}
}

func (a *Authenticator) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if isPublicMethod(info.FullMethod) {
			return handler(ctx, req)
		}
		authenticated, err := a.authenticate(ctx, authorizationFromMetadata(ctx))
		if err != nil {
			return nil, err
		}
		return handler(authenticated, req)
	}
}

func (a *Authenticator) AuthenticateAuthorization(ctx context.Context, authorization string) (context.Context, error) {
	return a.authenticate(ctx, authorization)
}

func (a *Authenticator) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/v1/auth/bootstrap" || r.URL.Path == "/v1/auth/login" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, err := a.authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			gatewayErrorHandler(r.Context(), nil, &runtime.JSONPb{}, w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Authenticator) authenticate(ctx context.Context, authorization string) (context.Context, error) {
	if a == nil || a.auth == nil {
		return nil, status.Error(codes.Unauthenticated, "authentication required")
	}
	token, ok := strings.CutPrefix(strings.TrimSpace(authorization), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return nil, status.Error(codes.Unauthenticated, "authentication required")
	}
	principal, err := a.auth.AuthenticateToken(ctx, strings.TrimSpace(token))
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	actor := service.ActorContext{UserID: principal.UserID, OrgID: principal.OrgID, Operator: principal.Operator}
	ctx = context.WithValue(ctx, principalContextKey, principal)
	ctx = context.WithValue(ctx, actorContextKey, actor)
	return ctx, nil
}

func principalFromContext(ctx context.Context) (internalauth.Principal, error) {
	principal, ok := ctx.Value(principalContextKey).(internalauth.Principal)
	if !ok {
		return internalauth.Principal{}, status.Error(codes.Unauthenticated, "authentication required")
	}
	return principal, nil
}

func actorFromContext(ctx context.Context) (service.ActorContext, error) {
	actor, ok := ctx.Value(actorContextKey).(service.ActorContext)
	if !ok {
		return service.ActorContext{}, status.Error(codes.Unauthenticated, "authentication required")
	}
	return actor, nil
}

func authorizationFromMetadata(ctx context.Context) string {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func isPublicMethod(method string) bool {
	return method == cyclopusv1.AuthenticationService_Bootstrap_FullMethodName ||
		method == cyclopusv1.AuthenticationService_Login_FullMethodName ||
		strings.HasPrefix(method, "/grpc.health.v1.Health/")
}
