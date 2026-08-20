package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	internalauth "github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
	maxSessionTTL   = 7 * 24 * time.Hour
)

type apiHandlers struct {
	cyclopusv1.UnimplementedAuthenticationServiceServer
	cyclopusv1.UnimplementedOrganizationServiceServer
	cyclopusv1.UnimplementedProjectServiceServer
	cyclopusv1.UnimplementedApplicationServiceServer
	cyclopusv1.UnimplementedEnvironmentServiceServer
	cyclopusv1.UnimplementedReleaseServiceServer
	cyclopusv1.UnimplementedReconciliationServiceServer
	cyclopusv1.UnimplementedWebhookServiceServer
	cyclopusv1.UnimplementedDomainServiceServer
	cyclopusv1.UnimplementedGitProviderServiceServer
	cyclopusv1.UnimplementedObservabilityServiceServer
	cyclopusv1.UnimplementedAliasServiceServer
	cyclopusv1.UnimplementedAuditEventServiceServer
	cyclopusv1.UnimplementedCertificateServiceServer
	auth       *internalauth.Service
	service    *service.Service
	sessionTTL time.Duration
}

func NewServices(authService *internalauth.Service, domainService *service.Service, sessionTTL time.Duration) Services {
	if sessionTTL > maxSessionTTL {
		sessionTTL = maxSessionTTL
	}
	handler := &apiHandlers{auth: authService, service: domainService, sessionTTL: sessionTTL}
	return Services{Authentication: handler, Organization: handler, Project: handler, Application: handler,
		Environment: handler, Release: handler, Reconciliation: handler, Webhook: handler, Domain: handler,
		GitProvider: handler, Observability: handler, Alias: handler,
		AuditEvent: handler, Certificate: handler}
}

func (h *apiHandlers) Bootstrap(ctx context.Context, req *cyclopusv1.BootstrapRequest) (*cyclopusv1.BootstrapResponse, error) {
	if err := requiredFields(map[string]string{"email": req.GetEmail(), "password": req.GetPassword()}); err != nil {
		return nil, err
	}
	principal, err := h.auth.ClaimFirstUser(ctx, req.GetEmail(), req.GetPassword(), req.GetBootstrapToken())
	if err != nil {
		return nil, rpcError(err)
	}
	session, err := h.issueSession(ctx, principal, "auth.bootstrap_session_created")
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.BootstrapResponse{Session: session}, nil
}

func (h *apiHandlers) Login(ctx context.Context, req *cyclopusv1.LoginRequest) (*cyclopusv1.LoginResponse, error) {
	if err := requiredFields(map[string]string{"email": req.GetEmail(), "password": req.GetPassword()}); err != nil {
		return nil, err
	}
	principal, err := h.auth.AuthenticatePassword(ctx, req.GetEmail(), req.GetPassword())
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	session, err := h.issueSession(ctx, principal, "auth.login")
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.LoginResponse{Session: session}, nil
}

func (h *apiHandlers) issueSession(ctx context.Context, principal internalauth.Principal, auditAction string) (*cyclopusv1.Session, error) {
	expires := time.Now().Add(h.sessionTTL)
	created, err := h.auth.CreateToken(ctx, principal, internalauth.CreateTokenParams{
		Kind: internalauth.CredentialKindSession, Name: "local session", ExpiresAt: &expires, AuditAction: auditAction,
	})
	if err != nil {
		return nil, err
	}
	user, err := h.service.CurrentUser(ctx, service.ActorContext{UserID: principal.UserID, OrgID: principal.OrgID})
	if err != nil {
		return nil, err
	}
	return &cyclopusv1.Session{AccessToken: created.Token, ExpiresAt: timestamppb.New(expires), User: userProto(user)}, nil
}

func (h *apiHandlers) Logout(ctx context.Context, _ *cyclopusv1.LogoutRequest) (*cyclopusv1.LogoutResponse, error) {
	if _, err := sessionActor(ctx); err != nil {
		return nil, err
	}
	principal, err := principalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.RevokeToken(ctx, principal, principal.TokenID); err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.LogoutResponse{}, nil
}

func (h *apiHandlers) GetCurrentUser(ctx context.Context, _ *cyclopusv1.GetCurrentUserRequest) (*cyclopusv1.GetCurrentUserResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	user, err := h.service.CurrentUser(ctx, actor)
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetCurrentUserResponse{User: userProto(user)}, nil
}

func (h *apiHandlers) UpdateCurrentUser(ctx context.Context, req *cyclopusv1.UpdateCurrentUserRequest) (*cyclopusv1.UpdateCurrentUserResponse, error) {
	if _, err := sessionActor(ctx); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"email": req.GetEmail(), "avatar_key": req.GetAvatarKey()}); err != nil {
		return nil, err
	}
	principal, err := principalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	user, err := h.auth.UpdateProfile(ctx, principal, internalauth.UpdateProfileParams{
		Email: req.GetEmail(), FirstName: req.GetFirstName(), LastName: req.GetLastName(),
		DisplayName: req.GetDisplayName(), AvatarKey: req.GetAvatarKey(), CurrentPassword: req.GetCurrentPassword(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.UpdateCurrentUserResponse{User: userProto(user)}, nil
}

func (h *apiHandlers) ChangePassword(ctx context.Context, req *cyclopusv1.ChangePasswordRequest) (*cyclopusv1.ChangePasswordResponse, error) {
	if _, err := sessionActor(ctx); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"current_password": req.GetCurrentPassword(), "new_password": req.GetNewPassword()}); err != nil {
		return nil, err
	}
	principal, err := principalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.ChangePassword(ctx, principal, req.GetCurrentPassword(), req.GetNewPassword()); err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.ChangePasswordResponse{}, nil
}

func (h *apiHandlers) CreateApiToken(ctx context.Context, req *cyclopusv1.CreateApiTokenRequest) (*cyclopusv1.CreateApiTokenResponse, error) {
	if _, err := sessionActor(ctx); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"name": req.GetName(), "idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	var expires *time.Time
	if req.ExpiresAt != nil {
		value := req.ExpiresAt.AsTime()
		expires = &value
	}
	principal, err := principalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	created, err := h.auth.CreateIdempotentAPIToken(ctx, principal, req.GetIdempotencyKey(), internalauth.CreateTokenParams{
		Name: req.GetName(), Scopes: req.GetScopes(), ExpiresAt: expires,
	})
	if err != nil {
		return nil, rpcError(err)
	}
	result := cyclopusv1.CreateApiTokenResult_CREATE_API_TOKEN_RESULT_CREATED
	if created.Replayed {
		result = cyclopusv1.CreateApiTokenResult_CREATE_API_TOKEN_RESULT_ALREADY_CREATED_SECRET_NOT_REPLAYABLE
	}
	return &cyclopusv1.CreateApiTokenResponse{ApiToken: apiTokenProto(created.Token), Token: created.Secret, Result: result}, nil
}

func (h *apiHandlers) ListApiTokens(ctx context.Context, req *cyclopusv1.ListApiTokensRequest) (*cyclopusv1.ListApiTokensResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	values, err := h.service.ListAPITokens(ctx, actor)
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("api_tokens", actor.OrgID, actor.UserID)
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.APIToken) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.APIToken) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListApiTokensResponse{ApiTokens: make([]*cyclopusv1.ApiToken, 0, len(page)), NextPageToken: next}
	for _, value := range page {
		response.ApiTokens = append(response.ApiTokens, apiTokenProto(value))
	}
	return response, nil
}

func (h *apiHandlers) RevokeApiToken(ctx context.Context, req *cyclopusv1.RevokeApiTokenRequest) (*cyclopusv1.RevokeApiTokenResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("api_token_id", req.GetApiTokenId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	if _, err := h.service.RevokeAPIToken(ctx, actor, req.GetIdempotencyKey(), req.GetApiTokenId()); err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.RevokeApiTokenResponse{}, nil
}

func (h *apiHandlers) GetOrganization(ctx context.Context, req *cyclopusv1.GetOrganizationRequest) (*cyclopusv1.GetOrganizationResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("organization_id", req.GetOrganizationId()); err != nil {
		return nil, err
	}
	if req.GetOrganizationId() != actor.OrgID {
		return nil, notFound()
	}
	value, err := h.service.CurrentOrganization(ctx, actor)
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetOrganizationResponse{Organization: organizationProto(value)}, nil
}

func (h *apiHandlers) DeleteOrganization(ctx context.Context, req *cyclopusv1.DeleteOrganizationRequest) (*cyclopusv1.DeleteOrganizationResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("organization_id", req.GetOrganizationId())
	if strings.TrimSpace(req.GetIdempotencyKey()) == "" {
		violations.add("idempotency_key", "is required")
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	if req.GetOrganizationId() != actor.OrgID {
		return nil, notFound()
	}
	operation, err := h.service.DeleteOrganization(ctx, actor, req.GetIdempotencyKey(), req.GetExternalCredentialsRevoked())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.DeleteOrganizationResponse{Operation: operationProto(operation, "organizations/")}, nil
}

func (h *apiHandlers) CreateProject(ctx context.Context, req *cyclopusv1.CreateProjectRequest) (*cyclopusv1.CreateProjectResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.Project == nil {
		return nil, fieldError("project", "is required")
	}
	if err := validateUUID("organization_id", req.GetOrganizationId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey(), "project.name": req.Project.GetName(), "project.slug": req.Project.GetSlug()}); err != nil {
		return nil, err
	}
	if req.GetOrganizationId() != actor.OrgID {
		return nil, notFound()
	}
	value, err := h.service.CreateProject(ctx, actor, req.GetIdempotencyKey(), projectInput(req.Project))
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.CreateProjectResponse{Project: projectProto(value)}, nil
}

func (h *apiHandlers) GetProject(ctx context.Context, req *cyclopusv1.GetProjectRequest) (*cyclopusv1.GetProjectResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("project_id", req.GetProjectId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetProject(ctx, actor, req.GetProjectId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetProjectResponse{Project: projectProto(value)}, nil
}

func (h *apiHandlers) ListProjects(ctx context.Context, req *cyclopusv1.ListProjectsRequest) (*cyclopusv1.ListProjectsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("organization_id", req.GetOrganizationId()); err != nil {
		return nil, err
	}
	if req.GetOrganizationId() != actor.OrgID {
		return nil, notFound()
	}
	values, err := h.service.ListProjects(ctx, actor, req.GetIncludeDeleted())
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("projects", actor.OrgID, req.GetOrganizationId(), req.GetIncludeDeleted())
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.Project) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.Project) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListProjectsResponse{Projects: make([]*cyclopusv1.Project, 0, len(page)), NextPageToken: next}
	for _, value := range page {
		response.Projects = append(response.Projects, projectProto(value))
	}
	return response, nil
}

func (h *apiHandlers) UpdateProject(ctx context.Context, req *cyclopusv1.UpdateProjectRequest) (*cyclopusv1.UpdateProjectResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.Project == nil {
		return nil, fieldError("project", "is required")
	}
	if err := validateUUID("project.id", req.Project.GetId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	current, err := h.service.GetProject(ctx, actor, req.Project.GetId())
	if err != nil {
		return nil, rpcError(err)
	}
	merged := projectProto(current)
	if err := mergeMaskedMessage(merged, req.Project, req.GetUpdateMask(), map[string]bool{"name": true, "slug": true, "git_connection_id": true}); err != nil {
		return nil, err
	}
	value, err := h.service.UpdateProject(ctx, actor, req.GetIdempotencyKey(), req.Project.GetId(), projectInput(merged))
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.UpdateProjectResponse{Project: projectProto(value)}, nil
}

func (h *apiHandlers) DeleteProject(ctx context.Context, req *cyclopusv1.DeleteProjectRequest) (*cyclopusv1.DeleteProjectResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("project_id", req.GetProjectId())
	if strings.TrimSpace(req.GetIdempotencyKey()) == "" {
		violations.add("idempotency_key", "is required")
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	operation, err := h.service.DeleteProject(ctx, actor, req.GetIdempotencyKey(), req.GetProjectId(), req.GetExternalCredentialsRevoked())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.DeleteProjectResponse{Operation: operationProto(operation, "projects/")}, nil
}

func (h *apiHandlers) CreateApplication(ctx context.Context, req *cyclopusv1.CreateApplicationRequest) (*cyclopusv1.CreateApplicationResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.Application == nil {
		return nil, fieldError("application", "is required")
	}
	if err := validateUUID("project_id", req.GetProjectId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey(), "application.name": req.Application.GetName(), "application.slug": req.Application.GetSlug(), "application.hostname_label": req.Application.GetHostnameLabel()}); err != nil {
		return nil, err
	}
	input := appInput(req.Application)
	input.ProjectID = req.GetProjectId()
	value, err := h.service.CreateApp(ctx, actor, req.GetIdempotencyKey(), input)
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.CreateApplicationResponse{Application: applicationProto(value)}, nil
}

func (h *apiHandlers) GetApplication(ctx context.Context, req *cyclopusv1.GetApplicationRequest) (*cyclopusv1.GetApplicationResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("application_id", req.GetApplicationId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetApp(ctx, actor, req.GetApplicationId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetApplicationResponse{Application: applicationProto(value)}, nil
}

func (h *apiHandlers) ListApplications(ctx context.Context, req *cyclopusv1.ListApplicationsRequest) (*cyclopusv1.ListApplicationsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("project_id", req.GetProjectId()); err != nil {
		return nil, err
	}
	values, err := h.service.ListApps(ctx, actor, req.GetProjectId(), req.GetIncludeDeleted())
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("applications", actor.OrgID, req.GetProjectId(), req.GetIncludeDeleted())
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.App) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.App) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListApplicationsResponse{Applications: make([]*cyclopusv1.Application, 0, len(page)), NextPageToken: next}
	for _, value := range page {
		response.Applications = append(response.Applications, applicationProto(value))
	}
	return response, nil
}

func (h *apiHandlers) UpdateApplication(ctx context.Context, req *cyclopusv1.UpdateApplicationRequest) (*cyclopusv1.UpdateApplicationResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.Application == nil {
		return nil, fieldError("application", "is required")
	}
	if err := validateUUID("application.id", req.Application.GetId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	current, err := h.service.GetApp(ctx, actor, req.Application.GetId())
	if err != nil {
		return nil, rpcError(err)
	}
	merged := applicationProto(current)
	allowed := map[string]bool{"name": true, "slug": true, "hostname_label": true, "image_repository": true, "container_port": true, "health_check_path": true, "scaling_mode": true, "min_replicas": true, "max_replicas": true, "hpa_target_cpu_percent": true, "keda_cron_schedules": true, "keda_http_target_pending_requests": true, "cpu_request_millicores": true, "cpu_limit_millicores": true, "memory_request_mib": true, "memory_limit_mib": true, "previews_enabled": true, "max_preview_environments": true, "preview_idle_ttl_hours": true, "deploy_timeout_seconds": true, "release_retention_count": true, "runtime_class": true}
	if err := mergeMaskedMessage(merged, req.Application, req.GetUpdateMask(), allowed); err != nil {
		return nil, err
	}
	input := appInput(merged)
	value, err := h.service.UpdateApp(ctx, actor, req.GetIdempotencyKey(), req.Application.GetId(), input)
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.UpdateApplicationResponse{Application: applicationProto(value)}, nil
}

func (h *apiHandlers) DeleteApplication(ctx context.Context, req *cyclopusv1.DeleteApplicationRequest) (*cyclopusv1.DeleteApplicationResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("application_id", req.GetApplicationId())
	if strings.TrimSpace(req.GetIdempotencyKey()) == "" {
		violations.add("idempotency_key", "is required")
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	operation, err := h.service.DeleteApp(ctx, actor, req.GetIdempotencyKey(), req.GetApplicationId(), req.GetExternalCredentialsRevoked())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.DeleteApplicationResponse{Operation: operationProto(operation, "applications/")}, nil
}

func (h *apiHandlers) GetEnvironment(ctx context.Context, req *cyclopusv1.GetEnvironmentRequest) (*cyclopusv1.GetEnvironmentResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("environment_id", req.GetEnvironmentId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetEnvironment(ctx, actor, req.GetEnvironmentId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetEnvironmentResponse{Environment: environmentProto(value)}, nil
}

func (h *apiHandlers) ListEnvironments(ctx context.Context, req *cyclopusv1.ListEnvironmentsRequest) (*cyclopusv1.ListEnvironmentsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("application_id", req.GetApplicationId()); err != nil {
		return nil, err
	}
	values, err := h.service.ListEnvironments(ctx, actor, req.GetApplicationId(), req.GetIncludeTornDown())
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("environments", actor.OrgID, req.GetApplicationId(), req.GetIncludeTornDown())
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.Environment) cursorKey { return cursorKey{CreatedAt: timeValue(v.CreatedAt), ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.Environment) cursorKey { return cursorKey{CreatedAt: timeValue(v.CreatedAt), ID: v.ID} })
	response := &cyclopusv1.ListEnvironmentsResponse{Environments: make([]*cyclopusv1.Environment, 0, len(page)), NextPageToken: next}
	for _, value := range page {
		response.Environments = append(response.Environments, environmentProto(value))
	}
	return response, nil
}

func (h *apiHandlers) AcceptPreviewPush(ctx context.Context, req *cyclopusv1.PreviewPushRequest) (*cyclopusv1.PreviewPushResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("application_id", req.GetApplicationId())
	for field, value := range map[string]string{"branch": req.GetBranch(), "commit_sha": req.GetCommitSha(), "idempotency_key": req.GetIdempotencyKey()} {
		if strings.TrimSpace(value) == "" {
			violations.add(field, "is required")
		}
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	result, err := h.service.AcceptPreviewPush(ctx, actor, req.GetIdempotencyKey(), service.PreviewPushInput{
		AppID: req.GetApplicationId(), Branch: req.GetBranch(), CommitSHA: req.GetCommitSha(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	response := &cyclopusv1.PreviewPushResponse{Disposition: result.Disposition, Dropped: result.Dropped}
	if !result.Dropped {
		response.Environment = environmentProto(result.Environment)
	}
	return response, nil
}

func (h *apiHandlers) AcceptPreviewBranchDelete(ctx context.Context, req *cyclopusv1.PreviewBranchDeleteRequest) (*cyclopusv1.PreviewBranchDeleteResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("application_id", req.GetApplicationId())
	if strings.TrimSpace(req.GetBranch()) == "" {
		violations.add("branch", "is required")
	}
	if strings.TrimSpace(req.GetIdempotencyKey()) == "" {
		violations.add("idempotency_key", "is required")
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	environment, err := h.service.RequestPreviewBranchDelete(ctx, actor, req.GetIdempotencyKey(), req.GetApplicationId(), req.GetBranch())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.PreviewBranchDeleteResponse{Environment: environmentProto(environment)}, nil
}

func (h *apiHandlers) TeardownEnvironment(ctx context.Context, req *cyclopusv1.TeardownEnvironmentRequest) (*cyclopusv1.TeardownEnvironmentResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("environment_id", req.GetEnvironmentId())
	if strings.TrimSpace(req.GetReason()) == "" {
		violations.add("reason", "is required")
	}
	if strings.TrimSpace(req.GetIdempotencyKey()) == "" {
		violations.add("idempotency_key", "is required")
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	environment, err := h.service.RequestEnvironmentTeardown(ctx, actor, req.GetIdempotencyKey(), req.GetEnvironmentId(), req.GetReason())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.TeardownEnvironmentResponse{Environment: environmentProto(environment)}, nil
}

func (h *apiHandlers) CreateProductionEnvironment(ctx context.Context, req *cyclopusv1.CreateProductionEnvironmentRequest) (*cyclopusv1.CreateProductionEnvironmentResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("application_id", req.GetApplicationId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	value, err := h.service.CreateProductionEnvironment(ctx, actor, req.GetIdempotencyKey(), service.CreateProductionEnvironmentInput{
		AppID:    req.GetApplicationId(),
		Branch:   req.GetBranch(),
		Hostname: req.GetHostname(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.CreateProductionEnvironmentResponse{Environment: environmentProto(value)}, nil
}

func (h *apiHandlers) Deploy(ctx context.Context, req *cyclopusv1.DeployRequest) (*cyclopusv1.DeployResponse, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("environment_id", req.GetEnvironmentId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"image_ref": req.GetImageRef(), "idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	principal, err := principalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	environment, err := h.service.GetEnvironment(ctx, actor, req.GetEnvironmentId())
	if err != nil {
		return nil, rpcError(err)
	}
	if principal.Kind == internalauth.CredentialKindAPI {
		if err := h.auth.AuthorizeDeployApp(ctx, principal, environment.AppID); err != nil {
			return nil, notFound()
		}
	}
	operation, err := h.service.AcceptDeploy(ctx, actor, req.GetIdempotencyKey(), req.GetEnvironmentId(), service.DeployInput{ImageRef: req.GetImageRef(), GitCommitSHA: req.GetGitCommitSha()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.DeployResponse{Operation: operationProto(operation, "releases/")}, nil
}

func (h *apiHandlers) Rollback(ctx context.Context, req *cyclopusv1.RollbackRequest) (*cyclopusv1.RollbackResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("environment_id", req.GetEnvironmentId()); err != nil {
		return nil, err
	}
	if err := validateUUID("release_id", req.GetReleaseId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	operation, err := h.service.AcceptRollback(ctx, actor, req.GetIdempotencyKey(), req.GetEnvironmentId(), req.GetReleaseId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.RollbackResponse{Operation: operationProto(operation, "releases/")}, nil
}

func (h *apiHandlers) GetRelease(ctx context.Context, req *cyclopusv1.GetReleaseRequest) (*cyclopusv1.GetReleaseResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("release_id", req.GetReleaseId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetRelease(ctx, actor, req.GetReleaseId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetReleaseResponse{Release: releaseProto(value)}, nil
}

func (h *apiHandlers) ListReleases(ctx context.Context, req *cyclopusv1.ListReleasesRequest) (*cyclopusv1.ListReleasesResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("environment_id", req.GetEnvironmentId()); err != nil {
		return nil, err
	}
	statuses, validationErr := releaseStatusFilters(req.GetStatuses())
	if validationErr != nil {
		return nil, validationErr
	}
	values, err := h.service.ListReleases(ctx, actor, req.GetEnvironmentId(), statuses)
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("releases", actor.OrgID, req.GetEnvironmentId(), statuses)
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.Release) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.Release) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListReleasesResponse{Releases: make([]*cyclopusv1.Release, 0, len(page)), NextPageToken: next}
	for _, value := range page {
		response.Releases = append(response.Releases, releaseProto(value))
	}
	return response, nil
}

func (h *apiHandlers) GetEnvironmentVariables(ctx context.Context, req *cyclopusv1.GetEnvironmentVariablesRequest) (*cyclopusv1.GetEnvironmentVariablesResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("environment_id", req.GetEnvironmentId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetEnvironmentVariables(ctx, actor, req.GetEnvironmentId())
	if err != nil {
		return nil, rpcError(err)
	}
	variables := make([]*cyclopusv1.EnvironmentVariable, 0, len(value.Variables))
	for _, variable := range value.Variables {
		variables = append(variables, &cyclopusv1.EnvironmentVariable{Key: variable.Key, Secret: variable.IsSecret})
	}
	return &cyclopusv1.GetEnvironmentVariablesResponse{Revision: &cyclopusv1.EnvironmentVariableRevision{Id: value.ID, OrganizationId: value.OrgID, EnvironmentId: value.EnvironmentID, Revision: value.Revision, CreatedByUserId: value.CreatedByUserID, Variables: variables, CreatedAt: timestamppb.New(value.CreatedAt)}}, nil
}

func (h *apiHandlers) SetEnvironmentVariables(ctx context.Context, req *cyclopusv1.SetEnvironmentVariablesRequest) (*cyclopusv1.SetEnvironmentVariablesResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	violations.uuid("environment_id", req.GetEnvironmentId())
	if strings.TrimSpace(req.GetIdempotencyKey()) == "" {
		violations.add("idempotency_key", "is required")
	}
	inputs := make([]service.EnvVarInput, 0, len(req.GetVariables()))
	seen := map[string]bool{}
	for index, variable := range req.GetVariables() {
		if variable == nil {
			violations.add(fmt.Sprintf("variables[%d]", index), "is required")
			continue
		}
		if !variable.GetSecret() {
			violations.add(fmt.Sprintf("variables[%d].secret", index), "must be true")
		}
		if strings.TrimSpace(variable.GetValue()) == "" {
			violations.add(fmt.Sprintf("variables[%d].value", index), "must contain a secret reference")
		}
		if seen[variable.GetKey()] {
			violations.add(fmt.Sprintf("variables[%d].key", index), "must not be duplicated")
		}
		seen[variable.GetKey()] = true
		inputs = append(inputs, service.EnvVarInput{Key: variable.GetKey(), SecretRef: variable.GetValue()})
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	operation, err := h.service.SetEnvironmentVariables(ctx, actor, req.GetIdempotencyKey(), req.GetEnvironmentId(), inputs)
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.SetEnvironmentVariablesResponse{Operation: operationProto(operation, "releases/")}, nil
}

func (h *apiHandlers) StartReconciliation(ctx context.Context, req *cyclopusv1.StartReconciliationRequest) (*cyclopusv1.StartReconciliationResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.Scope == nil {
		return nil, fieldError("scope", "is required")
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	input, validationErr := reconciliationInput(req)
	if validationErr != nil {
		return nil, validationErr
	}
	value, err := h.service.CreateReconciliation(ctx, actor, req.GetIdempotencyKey(), input)
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.StartReconciliationResponse{Operation: &cyclopusv1.Operation{Id: value.ID, ResourceName: "reconciliations/" + value.ID, Status: operationStatus(value.Status)}}, nil
}

func (h *apiHandlers) GetReconciliation(ctx context.Context, req *cyclopusv1.GetReconciliationRequest) (*cyclopusv1.GetReconciliationResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("reconciliation_id", req.GetReconciliationId()); err != nil {
		return nil, err
	}
	value, err := h.service.ReconciliationStatus(ctx, actor, req.GetReconciliationId())
	if err != nil {
		return nil, rpcError(err)
	}
	projected, err := reconciliationProto(value)
	if err != nil {
		return nil, status.Error(codes.Internal, "invalid persisted reconciliation result")
	}
	return &cyclopusv1.GetReconciliationResponse{Reconciliation: projected}, nil
}

func (h *apiHandlers) ListReconciliations(ctx context.Context, req *cyclopusv1.ListReconciliationsRequest) (*cyclopusv1.ListReconciliationsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	if req.GetOrganizationId() != "" {
		violations.uuid("organization_id", req.GetOrganizationId())
	}
	if req.GetApplicationId() != "" {
		violations.uuid("application_id", req.GetApplicationId())
	}
	if req.GetEnvironmentId() != "" {
		violations.uuid("environment_id", req.GetEnvironmentId())
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	if req.GetOrganizationId() != "" && req.GetOrganizationId() != actor.OrgID {
		return nil, notFound()
	}
	values, err := h.service.ListReconciliations(ctx, actor, pointer(req.GetApplicationId()), pointer(req.GetEnvironmentId()))
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("reconciliations", actor.OrgID, req.GetApplicationId(), req.GetEnvironmentId())
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.ReconciliationRun) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.ReconciliationRun) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListReconciliationsResponse{Reconciliations: make([]*cyclopusv1.ReconciliationRun, 0, len(page)), NextPageToken: next}
	for _, value := range page {
		projected, err := reconciliationProto(value)
		if err != nil {
			return nil, status.Error(codes.Internal, "invalid persisted reconciliation result")
		}
		response.Reconciliations = append(response.Reconciliations, projected)
	}
	return response, nil
}

func (h *apiHandlers) ListReconciliationEvidence(ctx context.Context, req *cyclopusv1.ListReconciliationEvidenceRequest) (*cyclopusv1.ListReconciliationEvidenceResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("reconciliation_id", req.GetReconciliationId()); err != nil {
		return nil, err
	}
	size, err := boundedPageSize(req.GetPageSize())
	if err != nil {
		return nil, err
	}
	afterSequence, err := reconciliationEvidenceCursor(req.GetPageToken(), pageScope("reconciliation_evidence", actor.OrgID, req.GetReconciliationId()))
	if err != nil {
		return nil, err
	}
	values, err := h.service.ListReconciliationEvidence(ctx, actor, req.GetReconciliationId(), afterSequence, size+1)
	if err != nil {
		return nil, rpcError(err)
	}
	hasNext := len(values) > size
	if hasNext {
		values = values[:size]
	}
	response := &cyclopusv1.ListReconciliationEvidenceResponse{Evidence: make([]*cyclopusv1.ReconciliationEvidence, 0, len(values))}
	for _, value := range values {
		projected, err := reconciliationEvidenceProto(value)
		if err != nil {
			return nil, status.Error(codes.Internal, "invalid persisted reconciliation evidence")
		}
		response.Evidence = append(response.Evidence, projected)
	}
	if hasNext {
		response.NextPageToken = encodeReconciliationEvidenceCursor(pageScope("reconciliation_evidence", actor.OrgID, req.GetReconciliationId()), values[len(values)-1].Sequence)
	}
	return response, nil
}

func (h *apiHandlers) CreateWebhook(ctx context.Context, req *cyclopusv1.CreateWebhookRequest) (*cyclopusv1.CreateWebhookResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.Webhook == nil {
		return nil, fieldError("webhook", "is required")
	}
	if err := validateUUID("organization_id", req.GetOrganizationId()); err != nil {
		return nil, err
	}
	if req.GetOrganizationId() != actor.OrgID {
		return nil, notFound()
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey(), "webhook.target_url": req.Webhook.GetTargetUrl(), "webhook.secret_ref": req.Webhook.GetSecretRef()}); err != nil {
		return nil, err
	}
	value, err := h.service.CreateWebhook(ctx, actor, req.GetIdempotencyKey(), webhookInput(req.Webhook))
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.CreateWebhookResponse{Webhook: webhookProto(value)}, nil
}

func (h *apiHandlers) GetWebhook(ctx context.Context, req *cyclopusv1.GetWebhookRequest) (*cyclopusv1.GetWebhookResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("webhook_id", req.GetWebhookId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetWebhook(ctx, actor, req.GetWebhookId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetWebhookResponse{Webhook: webhookProto(value)}, nil
}

func (h *apiHandlers) ListWebhooks(ctx context.Context, req *cyclopusv1.ListWebhooksRequest) (*cyclopusv1.ListWebhooksResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("organization_id", req.GetOrganizationId()); err != nil {
		return nil, err
	}
	if req.GetOrganizationId() != actor.OrgID {
		return nil, notFound()
	}
	values, err := h.service.ListWebhooks(ctx, actor)
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("webhooks", actor.OrgID)
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.Webhook) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.Webhook) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListWebhooksResponse{NextPageToken: next, Webhooks: make([]*cyclopusv1.WebhookEndpoint, 0, len(page))}
	for _, value := range page {
		response.Webhooks = append(response.Webhooks, webhookProto(value))
	}
	return response, nil
}

func (h *apiHandlers) UpdateWebhook(ctx context.Context, req *cyclopusv1.UpdateWebhookRequest) (*cyclopusv1.UpdateWebhookResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.Webhook == nil {
		return nil, fieldError("webhook", "is required")
	}
	if err := validateUUID("webhook.id", req.Webhook.GetId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	current, err := h.service.GetWebhook(ctx, actor, req.Webhook.GetId())
	if err != nil {
		return nil, rpcError(err)
	}
	merged := webhookProto(current)
	if err := mergeMaskedMessage(merged, req.Webhook, req.GetUpdateMask(), map[string]bool{"target_url": true, "events": true, "secret_ref": true, "enabled": true}); err != nil {
		return nil, err
	}
	value, err := h.service.UpdateWebhook(ctx, actor, req.GetIdempotencyKey(), req.Webhook.GetId(), webhookInput(merged))
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.UpdateWebhookResponse{Webhook: webhookProto(value)}, nil
}

func (h *apiHandlers) DeleteWebhook(ctx context.Context, req *cyclopusv1.DeleteWebhookRequest) (*cyclopusv1.DeleteWebhookResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("webhook_id", req.GetWebhookId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	if err := h.service.DeleteWebhook(ctx, actor, req.GetIdempotencyKey(), req.GetWebhookId()); err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.DeleteWebhookResponse{}, nil
}

func (h *apiHandlers) CreateDomain(ctx context.Context, req *cyclopusv1.CreateDomainRequest) (*cyclopusv1.CreateDomainResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.Domain == nil {
		return nil, fieldError("domain", "is required")
	}
	if err := validateUUID("application_id", req.GetApplicationId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey(), "domain.hostname": req.Domain.GetHostname()}); err != nil {
		return nil, err
	}
	value, instructions, err := h.service.CreateDomain(ctx, actor, req.GetIdempotencyKey(), domainInput(req.GetApplicationId(), req.Domain))
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.CreateDomainResponse{Domain: domainProto(value, instructions)}, nil
}

func (h *apiHandlers) GetDomain(ctx context.Context, req *cyclopusv1.GetDomainRequest) (*cyclopusv1.GetDomainResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("domain_id", req.GetDomainId()); err != nil {
		return nil, err
	}
	value, instructions, err := h.service.GetDomain(ctx, actor, req.GetDomainId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetDomainResponse{Domain: domainProto(value, instructions)}, nil
}

func (h *apiHandlers) ListDomains(ctx context.Context, req *cyclopusv1.ListDomainsRequest) (*cyclopusv1.ListDomainsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("application_id", req.GetApplicationId()); err != nil {
		return nil, err
	}
	values, err := h.service.ListDomains(ctx, actor, req.GetApplicationId())
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("domains", actor.OrgID+"/"+req.GetApplicationId())
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.Domain) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.Domain) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListDomainsResponse{NextPageToken: next, Domains: make([]*cyclopusv1.Domain, 0, len(page))}
	for _, value := range page {
		response.Domains = append(response.Domains, domainProto(value, service.DomainInstructions{}))
	}
	return response, nil
}

func (h *apiHandlers) VerifyDomain(ctx context.Context, req *cyclopusv1.VerifyDomainRequest) (*cyclopusv1.VerifyDomainResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("domain_id", req.GetDomainId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	value, instructions, err := h.service.VerifyDomain(ctx, actor, req.GetIdempotencyKey(), req.GetDomainId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.VerifyDomainResponse{Domain: domainProto(value, instructions)}, nil
}

func (h *apiHandlers) DeleteDomain(ctx context.Context, req *cyclopusv1.DeleteDomainRequest) (*cyclopusv1.DeleteDomainResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("domain_id", req.GetDomainId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	if err := h.service.DeleteDomain(ctx, actor, req.GetIdempotencyKey(), req.GetDomainId()); err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.DeleteDomainResponse{}, nil
}

func (h *apiHandlers) GetGitConnection(ctx context.Context, req *cyclopusv1.GetGitConnectionRequest) (*cyclopusv1.GetGitConnectionResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("git_connection_id", req.GetGitConnectionId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetGitConnection(ctx, actor, req.GetGitConnectionId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetGitConnectionResponse{GitConnection: gitConnectionProto(value)}, nil
}

func (h *apiHandlers) ListGitConnections(ctx context.Context, req *cyclopusv1.ListGitConnectionsRequest) (*cyclopusv1.ListGitConnectionsResponse, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return nil, err
	}
	values, err := h.service.ListGitConnections(ctx, actor)
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("git_connections", actor.OrgID)
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.GitConnection) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.GitConnection) cursorKey { return cursorKey{CreatedAt: v.CreatedAt, ID: v.ID} })
	response := &cyclopusv1.ListGitConnectionsResponse{NextPageToken: next, GitConnections: make([]*cyclopusv1.GitConnection, 0, len(page))}
	for _, value := range page {
		response.GitConnections = append(response.GitConnections, gitConnectionProto(value))
	}
	return response, nil
}

func (h *apiHandlers) DisconnectGitConnection(ctx context.Context, req *cyclopusv1.DisconnectGitConnectionRequest) (*cyclopusv1.DisconnectGitConnectionResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("git_connection_id", req.GetGitConnectionId()); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	if h.service == nil {
		return nil, typedError(codes.FailedPrecondition, "GitHub connection lifecycle is unavailable", "GITHUB_APP_NOT_CONFIGURED")
	}
	value, err := h.service.DisconnectGitConnection(ctx, actor, req.GetIdempotencyKey(), req.GetGitConnectionId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.DisconnectGitConnectionResponse{GitConnection: gitConnectionProto(value)}, nil
}

func sessionActor(ctx context.Context) (service.ActorContext, error) {
	principal, err := principalFromContext(ctx)
	if err != nil {
		return service.ActorContext{}, err
	}
	if principal.Kind != internalauth.CredentialKindSession {
		return service.ActorContext{}, status.Error(codes.PermissionDenied, "session credential required")
	}
	return actorFromContext(ctx)
}

func rpcError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, internalauth.ErrNotFound):
		return notFound()
	case errors.Is(err, internalauth.ErrUnauthenticated):
		return status.Error(codes.Unauthenticated, "invalid credentials")
	case errors.Is(err, internalauth.ErrRateLimited):
		return status.Error(codes.ResourceExhausted, "too many authentication attempts")
	case errors.Is(err, service.ErrInvalidArgument), errors.Is(err, internalauth.ErrInvalidScope), errors.Is(err, internalauth.ErrInvalidCredential):
		return fieldError("request", "contains invalid values")
	case errors.Is(err, service.ErrForbidden):
		return status.Error(codes.PermissionDenied, "permission denied")
	case errors.Is(err, store.ErrAlreadyExists):
		return typedError(codes.AlreadyExists, "request conflicts with existing state", "CONFLICT")
	case errors.Is(err, store.ErrFailedPrecondition):
		return typedError(codes.FailedPrecondition, "resource is not in the required state", "FAILED_PRECONDITION")
	case errors.Is(err, service.ErrConflict), errors.Is(err, store.ErrConflict):
		return typedError(codes.Aborted, "request conflicts with current state", "CONFLICT")
	case errors.Is(err, service.ErrInProgress):
		return typedError(codes.Aborted, "operation is in progress", "IN_PROGRESS")
	case errors.Is(err, store.ErrIdempotencyActive):
		return typedError(codes.Aborted, "operation is in progress", "IN_PROGRESS")
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

func notFound() error { return typedError(codes.NotFound, "resource not found", "NOT_FOUND") }

func fieldError(field, description string) error {
	base := status.New(codes.InvalidArgument, "request validation failed")
	withDetails, err := base.WithDetails(
		&errdetails.ErrorInfo{Reason: "INVALID_ARGUMENT", Domain: "cyclopus.dev"},
		&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: description}}},
	)
	if err != nil {
		return base.Err()
	}
	return withDetails.Err()
}

func typedError(code codes.Code, message, reason string) error {
	base := status.New(code, message)
	withDetails, err := base.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "cyclopus.dev"})
	if err != nil {
		return base.Err()
	}
	return withDetails.Err()
}

func requiredFields(fields map[string]string) error {
	violations := make(validationViolations, 0, len(fields))
	for field, value := range fields {
		if strings.TrimSpace(value) == "" {
			violations = append(violations, fieldViolation{field, "is required"})
		}
	}
	return violations.err()
}

func validateUUID(field, value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value {
		return fieldError(field, "must be a canonical UUID")
	}
	return nil
}

func pointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func projectInput(value *cyclopusv1.Project) service.ProjectInput {
	return service.ProjectInput{Name: value.GetName(), Slug: value.GetSlug(), GitConnectionID: pointer(value.GetGitConnectionId())}
}

func appInput(value *cyclopusv1.Application) service.AppInput {
	runtimeClass := map[cyclopusv1.RuntimeClass]string{cyclopusv1.RuntimeClass_RUNTIME_CLASS_GVISOR: "gvisor", cyclopusv1.RuntimeClass_RUNTIME_CLASS_RUNC: "runc"}[value.GetRuntimeClass()]
	input := service.AppInput{ProjectID: value.GetProjectId(), Name: value.GetName(), Slug: value.GetSlug(), HostnameLabel: value.GetHostnameLabel(),
		RegistryID: pointer(value.GetRegistryId()), ImageRepository: pointer(value.GetImageRepository()), GitHubWorkflowPath: pointer(value.GetGithubWorkflowPath()), ProductionBranch: value.GetProductionBranch(), ContainerPort: int(value.GetContainerPort()),
		HealthCheckPath: value.GetHealthCheckPath(), ScalingMode: scalingModeString(value.GetScalingMode()), MinReplicas: int(value.GetMinReplicas()), MaxReplicas: int(value.GetMaxReplicas()), HPATargetCPUPercent: int(value.GetHpaTargetCpuPercent()),
		KEDACronSchedules: kedaCronSchedulesInput(value.GetKedaCronSchedules()), KEDAHTTPTargetPendingRequests: int(value.GetKedaHttpTargetPendingRequests()),
		CPURequestMillicores: int(value.GetCpuRequestMillicores()), CPULimitMillicores: int(value.GetCpuLimitMillicores()), MemoryRequestMiB: int(value.GetMemoryRequestMib()), MemoryLimitMiB: int(value.GetMemoryLimitMib()),
		PreviewsEnabled: value.GetPreviewsEnabled(), MaxPreviewEnvironments: int(value.GetMaxPreviewEnvironments()), PreviewIdleTTLHours: int(value.GetPreviewIdleTtlHours()), DeployTimeoutSeconds: int(value.GetDeployTimeoutSeconds()), ReleaseRetentionCount: int(value.GetReleaseRetentionCount())}
	if runtimeClass != "" {
		input.RuntimeClass = &runtimeClass
	}
	return input
}

func webhookInput(value *cyclopusv1.WebhookEndpoint) service.WebhookInput {
	return service.WebhookInput{TargetURL: value.GetTargetUrl(), Events: append([]string(nil), value.GetEvents()...), SecretRef: value.GetSecretRef(), Enabled: value.GetEnabled()}
}

func domainInput(applicationID string, value *cyclopusv1.Domain) service.DomainInput {
	return service.DomainInput{AppID: applicationID, Hostname: value.GetHostname(), VerificationMethod: domainVerificationMethodString(value.GetVerificationMethod())}
}

func domainVerificationMethodString(v cyclopusv1.DomainVerificationMethod) string {
	return map[cyclopusv1.DomainVerificationMethod]string{
		cyclopusv1.DomainVerificationMethod_DOMAIN_VERIFICATION_METHOD_DNS_TXT: "dns_txt",
		cyclopusv1.DomainVerificationMethod_DOMAIN_VERIFICATION_METHOD_CNAME:   "cname",
	}[v]
}

func domainVerificationMethodProto(v string) cyclopusv1.DomainVerificationMethod {
	return map[string]cyclopusv1.DomainVerificationMethod{
		"dns_txt": cyclopusv1.DomainVerificationMethod_DOMAIN_VERIFICATION_METHOD_DNS_TXT,
		"cname":   cyclopusv1.DomainVerificationMethod_DOMAIN_VERIFICATION_METHOD_CNAME,
	}[v]
}

func reconciliationInput(req *cyclopusv1.StartReconciliationRequest) (service.ReconciliationInput, error) {
	scope := req.GetScope()
	violations := validationViolations{}
	if scope == nil {
		return service.ReconciliationInput{}, fieldError("scope", "is required")
	}
	setIDs := 0
	for field, value := range map[string]string{"scope.organization_id": scope.GetOrganizationId(), "scope.application_id": scope.GetApplicationId(), "scope.environment_id": scope.GetEnvironmentId()} {
		if value != "" {
			setIDs++
			violations.uuid(field, value)
		}
	}
	var input service.ReconciliationInput
	switch scope.GetKind() {
	case cyclopusv1.ReconcileScopeKind_RECONCILE_SCOPE_KIND_ORGANIZATION:
		if setIDs != 1 || scope.GetOrganizationId() == "" {
			violations.add("scope", "organization scope requires only organization_id")
		}
		input = service.ReconciliationInput{ScopeKind: "org", ScopeID: scope.GetOrganizationId()}
	case cyclopusv1.ReconcileScopeKind_RECONCILE_SCOPE_KIND_APPLICATION:
		if setIDs != 1 || scope.GetApplicationId() == "" {
			violations.add("scope", "application scope requires only application_id")
		}
		input = service.ReconciliationInput{ScopeKind: "app", ScopeID: scope.GetApplicationId(), ScopeAppID: pointer(scope.GetApplicationId())}
	case cyclopusv1.ReconcileScopeKind_RECONCILE_SCOPE_KIND_ENVIRONMENT:
		if setIDs != 1 || scope.GetEnvironmentId() == "" {
			violations.add("scope", "environment scope requires only environment_id")
		}
		input = service.ReconciliationInput{ScopeKind: "environment", ScopeID: scope.GetEnvironmentId(), ScopeEnvironmentID: pointer(scope.GetEnvironmentId())}
	default:
		violations.add("scope.kind", "must be specified")
	}
	seen := map[cyclopusv1.ReconcileCategory]bool{}
	for index, category := range req.GetCategories() {
		name := reconcileCategoryString(category)
		if name == "" {
			violations.add(fmt.Sprintf("categories[%d]", index), "must be specified")
		} else if seen[category] {
			violations.add(fmt.Sprintf("categories[%d]", index), "must not be duplicated")
		} else {
			seen[category] = true
			input.Categories = append(input.Categories, name)
		}
	}
	if req.GetPrune() && (req.DryRun == nil || req.GetDryRun()) {
		violations.add("prune", "requires dry_run to be explicitly false")
	}
	if err := violations.err(); err != nil {
		return service.ReconciliationInput{}, err
	}
	if req.DryRun != nil {
		value := req.GetDryRun()
		input.DryRun = &value
	}
	input.Prune = req.GetPrune()
	return input, nil
}

func userProto(v store.User) *cyclopusv1.User {
	return &cyclopusv1.User{Id: v.ID, OrganizationId: v.OrgID, Email: v.Email, FirstName: v.FirstName,
		LastName: v.LastName, DisplayName: v.DisplayName, AvatarKey: v.AvatarKey, CreatedAt: timestamppb.New(v.CreatedAt)}
}
func organizationProto(v store.Organization) *cyclopusv1.Organization {
	return &cyclopusv1.Organization{Id: v.ID, Name: v.Name, Slug: v.Slug, CreatedAt: timestamppb.New(v.CreatedAt), DeletedAt: timeProto(v.DeletedAt)}
}
func projectProto(v store.Project) *cyclopusv1.Project {
	return &cyclopusv1.Project{Id: v.ID, OrganizationId: v.OrgID, Name: v.Name, Slug: v.Slug, GitConnectionId: stringValue(v.GitConnectionID), CreatedAt: timestamppb.New(v.CreatedAt), DeletedAt: timeProto(v.DeletedAt)}
}
func applicationProto(v store.App) *cyclopusv1.Application {
	return &cyclopusv1.Application{Id: v.ID, OrganizationId: v.OrgID, ProjectId: v.ProjectID, Name: v.Name, Slug: v.Slug, HostnameLabel: v.HostnameLabel, RegistryId: stringValue(v.RegistryID), ImageRepository: stringValue(v.ImageRepository), ContainerPort: int32(v.ContainerPort), HealthCheckPath: v.HealthCheckPath, ScalingMode: scalingModeProto(v.ScalingMode), MinReplicas: int32(v.MinReplicas), MaxReplicas: int32(v.MaxReplicas), HpaTargetCpuPercent: int32(v.HPATargetCPUPercent), CpuRequestMillicores: int32(v.CPURequestMillicores), CpuLimitMillicores: int32(v.CPULimitMillicores), MemoryRequestMib: int32(v.MemoryRequestMiB), MemoryLimitMib: int32(v.MemoryLimitMiB), PreviewsEnabled: v.PreviewsEnabled, MaxPreviewEnvironments: int32(v.MaxPreviewEnvironments), PreviewIdleTtlHours: int32(v.PreviewIdleTTLHours), DeployTimeoutSeconds: int32(v.DeployTimeoutSeconds), ReleaseRetentionCount: int32(v.ReleaseRetentionCount), RuntimeClass: runtimeClassProto(v.RuntimeClass), GithubWorkflowPath: v.GitHubWorkflowPath, ProductionBranch: v.ProductionBranch, AnnotationPolicy: cyclopusv1.OciAnnotationPolicy_OCI_ANNOTATION_POLICY_VALIDATE_IF_PRESENT, CreatedAt: timestamppb.New(v.CreatedAt), DeletedAt: timeProto(v.DeletedAt), KedaCronSchedules: kedaCronSchedulesProto(v.KEDACronSchedules), KedaHttpTargetPendingRequests: int32(v.KEDAHTTPTargetPendingRequests)}
}

func kedaCronSchedulesInput(values []*cyclopusv1.KedaCronSchedule) []store.KEDACronSchedule {
	schedules := make([]store.KEDACronSchedule, 0, len(values))
	for _, value := range values {
		schedules = append(schedules, store.KEDACronSchedule{Timezone: value.GetTimezone(), Start: value.GetStart(), End: value.GetEnd(), DesiredReplicas: int(value.GetDesiredReplicas())})
	}
	return schedules
}

func kedaCronSchedulesProto(values []store.KEDACronSchedule) []*cyclopusv1.KedaCronSchedule {
	schedules := make([]*cyclopusv1.KedaCronSchedule, 0, len(values))
	for _, value := range values {
		schedules = append(schedules, &cyclopusv1.KedaCronSchedule{Timezone: value.Timezone, Start: value.Start, End: value.End, DesiredReplicas: int32(value.DesiredReplicas)})
	}
	return schedules
}
func environmentProto(v store.Environment) *cyclopusv1.Environment {
	return &cyclopusv1.Environment{Id: v.ID, OrganizationId: v.OrgID, ApplicationId: v.AppID, Branch: v.Branch, Kind: environmentKindProto(v.Kind), PreviewSlug: stringValue(v.PreviewSlug), Namespace: v.Namespace, PlannedHostname: stringValue(v.PlannedHostname), CurrentReleaseId: stringValue(v.CurrentReleaseID), InProgressReleaseId: stringValue(v.InProgressReleaseID), CurrentEnvVarRevisionId: stringValue(v.CurrentEnvVarRevisionID), CreatedAt: timeProto(v.CreatedAt), TornDownAt: timeProto(v.TornDownAt), LifecycleStatus: environmentLifecycleStatusProto(v.LifecycleStatus), LifecycleReason: stringValue(v.LifecycleReason), LifecycleErrorCode: stringValue(v.LifecycleErrorCode), LifecycleErrorMessage: stringValue(v.LifecycleErrorMessage), TeardownRequestedAt: timeProto(v.TeardownRequestedAt)}
}

func environmentLifecycleStatusProto(value string) cyclopusv1.EnvironmentLifecycleStatus {
	return map[string]cyclopusv1.EnvironmentLifecycleStatus{
		"reserved":           cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_RESERVED,
		"policy_blocked":     cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_POLICY_BLOCKED,
		"ready":              cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_READY,
		"teardown_requested": cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_TEARDOWN_REQUESTED,
		"torn_down":          cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_TORN_DOWN,
		"failed":             cyclopusv1.EnvironmentLifecycleStatus_ENVIRONMENT_LIFECYCLE_STATUS_FAILED,
	}[value]
}
func releaseProto(v store.Release) *cyclopusv1.Release {
	return &cyclopusv1.Release{Id: v.ID, OrganizationId: v.OrgID, EnvironmentId: v.EnvironmentID, ImageRef: v.ImageRef, GitCommitSha: stringValue(v.GitCommitSHA), TriggeredByUserId: stringValue(v.TriggeredByUserID), Kind: releaseKindProto(v.Kind), EnvVarRevisionId: stringValue(v.EnvVarRevisionID), Status: releaseStatusProto(v.Status), FailureReason: v.FailureReason, CreatedAt: timestamppb.New(v.CreatedAt)}
}
func apiTokenProto(v store.APIToken) *cyclopusv1.ApiToken {
	return &cyclopusv1.ApiToken{Id: v.ID, UserId: v.UserID, Name: v.Name, Scopes: append([]string(nil), v.Scopes...), ExpiresAt: timeProto(v.ExpiresAt), CreatedAt: timestamppb.New(v.CreatedAt), LastUsedAt: timeProto(v.LastUsedAt)}
}
func webhookProto(v store.Webhook) *cyclopusv1.WebhookEndpoint {
	return &cyclopusv1.WebhookEndpoint{Id: v.ID, OrganizationId: v.OrgID, TargetUrl: v.TargetURL, Events: append([]string(nil), v.Events...), SecretRef: v.SecretRef, Enabled: v.DisabledAt == nil, ConsecutiveFailures: int32(v.ConsecutiveFailures), DisabledAt: timeProto(v.DisabledAt), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt)}
}
func domainProto(v store.Domain, instructions service.DomainInstructions) *cyclopusv1.Domain {
	token := ""
	if v.VerificationToken != nil {
		token = *v.VerificationToken
	}
	proto := &cyclopusv1.Domain{
		Id: v.ID, OrganizationId: v.OrgID, ApplicationId: v.AppID, Hostname: v.Hostname,
		VerificationMethod: domainVerificationMethodProto(v.VerificationMethod), VerificationToken: token,
		VerifiedAt: timeProto(v.VerifiedAt), CreatedAt: timestamppb.New(v.CreatedAt),
	}
	if instructions.RecordType != "" {
		proto.Instructions = &cyclopusv1.DomainDnsInstructions{
			RecordType: instructions.RecordType, RecordName: instructions.RecordName,
			RecordValue: instructions.RecordValue, ApexARecordTarget: instructions.ApexARecordTarget,
		}
	}
	return proto
}
func gitConnectionProto(v store.GitConnection) *cyclopusv1.GitConnection {
	return &cyclopusv1.GitConnection{Id: v.ID, Repository: &cyclopusv1.GitHubRepository{Id: v.GitHubRepositoryID,
		InstallationId: v.InstallationID, NodeId: v.RepositoryNodeID, Owner: v.OwnerLogin, Name: v.RepositoryName,
		FullName: v.RepositoryFullName, Private: v.RepositoryPrivate, Archived: v.RepositoryArchived,
		DefaultBranch: v.RepositoryDefaultBranch, AccessStatus: githubRepositoryAccessStatus(v.RepositoryAccessState),
		SyncedAt: timeProto(v.RepositorySyncedAt)}, Status: gitConnectionStatus(v.State, v.AccessState),
		ConnectedAt: timestamppb.New(v.CreatedAt), DisconnectedAt: timeProto(v.DisconnectedAt)}
}

func githubRepositoryAccessStatus(value string) cyclopusv1.GitHubRepositoryAccessStatus {
	return map[string]cyclopusv1.GitHubRepositoryAccessStatus{
		"active":       cyclopusv1.GitHubRepositoryAccessStatus_GITHUB_REPOSITORY_ACCESS_STATUS_ACTIVE,
		"inaccessible": cyclopusv1.GitHubRepositoryAccessStatus_GITHUB_REPOSITORY_ACCESS_STATUS_INACCESSIBLE,
		"deleted":      cyclopusv1.GitHubRepositoryAccessStatus_GITHUB_REPOSITORY_ACCESS_STATUS_DELETED,
	}[value]
}

func gitConnectionStatus(state, accessState string) cyclopusv1.GitConnectionStatus {
	if state == "disconnected" {
		return cyclopusv1.GitConnectionStatus_GIT_CONNECTION_STATUS_DISCONNECTED
	}
	return map[string]cyclopusv1.GitConnectionStatus{
		"active":       cyclopusv1.GitConnectionStatus_GIT_CONNECTION_STATUS_ACTIVE,
		"suspended":    cyclopusv1.GitConnectionStatus_GIT_CONNECTION_STATUS_SUSPENDED,
		"inaccessible": cyclopusv1.GitConnectionStatus_GIT_CONNECTION_STATUS_INACCESSIBLE,
		"deleted":      cyclopusv1.GitConnectionStatus_GIT_CONNECTION_STATUS_DELETED,
	}[accessState]
}
func operationProto(v service.AcceptedOperation, prefix string) *cyclopusv1.Operation {
	return &cyclopusv1.Operation{Id: v.OperationID, ResourceName: prefix + v.ResourceID, Status: operationStatus(v.Status)}
}
func reconciliationProto(v store.ReconciliationRun) (*cyclopusv1.ReconciliationRun, error) {
	categories := make([]cyclopusv1.ReconcileCategory, 0, len(v.Categories))
	for _, category := range v.Categories {
		categories = append(categories, reconcileCategoryProto(category))
	}
	plan, err := reconciliationPlanProto(v.Plan)
	if err != nil {
		return nil, err
	}
	report, findings, err := reconciliationReportProto(v.Report)
	if err != nil {
		return nil, err
	}
	return &cyclopusv1.ReconciliationRun{Id: v.ID, Scope: &cyclopusv1.ReconcileScope{Kind: reconcileScopeKindProto(v.ScopeKind), OrganizationId: v.OrgID, ApplicationId: stringValue(v.ScopeAppID), EnvironmentId: stringValue(v.ScopeEnvironmentID)}, Categories: categories, Status: operationStatus(v.Status), CreatedAt: timeProto(&v.CreatedAt), StartedAt: timeProto(v.StartedAt), CompletedAt: timeProto(v.CompletedAt), DryRun: v.DryRun, Prune: v.Prune, Plan: plan, Report: report, Findings: findings, ErrorCode: stringValue(v.ErrorCode), ErrorMessage: stringValue(v.ErrorMessage)}, nil
}

type reconciliationObjectJSON struct {
	Kind            string `json:"kind"`
	ObjectKind      string `json:"object_kind"`
	Namespace       string `json:"namespace"`
	ObjectNamespace string `json:"object_namespace"`
	Name            string `json:"name"`
	ObjectName      string `json:"object_name"`
}

type reconciliationOperationJSON struct {
	Action          string          `json:"action"`
	Object          json.RawMessage `json:"object"`
	Kind            string          `json:"kind"`
	ObjectKind      string          `json:"object_kind"`
	Namespace       string          `json:"namespace"`
	ObjectNamespace string          `json:"object_namespace"`
	ObjectName      string          `json:"object_name"`
	Name            string          `json:"name"`
	DesiredHash     string          `json:"desired_hash"`
	ObservedHash    string          `json:"observed_hash"`
	Details         map[string]any  `json:"details"`
}

func reconciliationPlanProto(raw json.RawMessage) (*cyclopusv1.ReconciliationPlan, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var value struct {
		Create  []reconciliationOperationJSON `json:"create"`
		Patch   []reconciliationOperationJSON `json:"patch"`
		Delete  []reconciliationOperationJSON `json:"delete"`
		Actions []reconciliationOperationJSON `json:"actions"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	for _, operation := range value.Actions {
		switch operation.Action {
		case "create":
			value.Create = append(value.Create, operation)
		case "patch", "update":
			value.Patch = append(value.Patch, operation)
		case "delete", "prune":
			value.Delete = append(value.Delete, operation)
		default:
			return nil, fmt.Errorf("unknown reconciliation action %q", operation.Action)
		}
	}
	create, err := reconciliationOperationsProto(value.Create)
	if err != nil {
		return nil, err
	}
	patch, err := reconciliationOperationsProto(value.Patch)
	if err != nil {
		return nil, err
	}
	deleted, err := reconciliationOperationsProto(value.Delete)
	if err != nil {
		return nil, err
	}
	return &cyclopusv1.ReconciliationPlan{Create: create, Patch: patch, Delete: deleted}, nil
}

func reconciliationOperationsProto(values []reconciliationOperationJSON) ([]*cyclopusv1.ReconciliationPlanOperation, error) {
	operations := make([]*cyclopusv1.ReconciliationPlanOperation, 0, len(values))
	for _, value := range values {
		object, err := reconciliationOperationObjectProto(value)
		if err != nil {
			return nil, err
		}
		details, err := valueProto(value.Details)
		if err != nil {
			return nil, err
		}
		operations = append(operations, &cyclopusv1.ReconciliationPlanOperation{Object: object, DesiredHash: value.DesiredHash, ObservedHash: value.ObservedHash, Details: details})
	}
	return operations, nil
}

func reconciliationOperationObjectProto(value reconciliationOperationJSON) (*cyclopusv1.ReconciliationObject, error) {
	object := reconciliationObjectJSON{Kind: value.Kind, ObjectKind: value.ObjectKind, Namespace: value.Namespace, ObjectNamespace: value.ObjectNamespace, Name: value.Name, ObjectName: value.ObjectName}
	if len(value.Object) != 0 && string(value.Object) != "null" {
		var name string
		if err := json.Unmarshal(value.Object, &name); err == nil {
			object.Name = name
		} else if err := json.Unmarshal(value.Object, &object); err != nil {
			return nil, err
		}
	}
	return reconciliationObjectProto(object), nil
}

type reconciliationFindingJSON struct {
	Type                 string   `json:"type"`
	Kind                 string   `json:"kind"`
	Category             string   `json:"category"`
	Status               string   `json:"status"`
	ResourceName         string   `json:"resource_name"`
	Summary              string   `json:"summary"`
	ExpectedRuntimeClass string   `json:"expected_runtime_class"`
	ObservedRuntimeClass string   `json:"observed_runtime_class"`
	MissingPolicies      []string `json:"missing_policies"`
	ObservedAt           string   `json:"observed_at"`
}

func reconciliationReportProto(raw json.RawMessage) (*cyclopusv1.ReconciliationReport, []*cyclopusv1.ReconcileFinding, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	var value struct {
		Orphans  []reconciliationObjectJSON  `json:"orphans"`
		Pruned   []reconciliationObjectJSON  `json:"pruned"`
		Planned  int32                       `json:"planned"`
		Applied  int32                       `json:"applied"`
		DryRun   bool                        `json:"dry_run"`
		Findings []reconciliationFindingJSON `json:"findings"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, nil, err
	}
	orphans := make([]*cyclopusv1.ReconciliationObject, 0, len(value.Orphans))
	for _, object := range value.Orphans {
		orphans = append(orphans, reconciliationObjectProto(object))
	}
	pruned := make([]*cyclopusv1.ReconciliationObject, 0, len(value.Pruned))
	for _, object := range value.Pruned {
		pruned = append(pruned, reconciliationObjectProto(object))
	}
	findings := make([]*cyclopusv1.ReconcileFinding, 0, len(value.Findings))
	for _, finding := range value.Findings {
		projected, err := reconciliationFindingProto(finding)
		if err != nil {
			return nil, nil, err
		}
		findings = append(findings, projected)
	}
	return &cyclopusv1.ReconciliationReport{Orphans: orphans, Pruned: pruned, Planned: value.Planned, Applied: value.Applied, DryRun: value.DryRun}, findings, nil
}

func reconciliationFindingProto(value reconciliationFindingJSON) (*cyclopusv1.ReconcileFinding, error) {
	finding := &cyclopusv1.ReconcileFinding{Category: reconcileCategoryProto(value.Category), Status: reconcileFindingStatusProto(value.Status), ResourceName: value.ResourceName, Summary: value.Summary}
	if value.ObservedAt != "" {
		observedAt, err := time.Parse(time.RFC3339Nano, value.ObservedAt)
		if err != nil {
			return nil, err
		}
		finding.ObservedAt = timestamppb.New(observedAt)
	}
	switch firstNonEmpty(value.Type, value.Kind) {
	case "runtime_class_drift":
		finding.Details = &cyclopusv1.ReconcileFinding_RuntimeClassDrift{RuntimeClassDrift: &cyclopusv1.RuntimeClassDriftFinding{ExpectedRuntimeClass: value.ExpectedRuntimeClass, ObservedRuntimeClass: value.ObservedRuntimeClass}}
	case "network_policy_drift":
		finding.Details = &cyclopusv1.ReconcileFinding_NetworkPolicyDrift{NetworkPolicyDrift: &cyclopusv1.NetworkPolicyDriftFinding{MissingPolicies: append([]string(nil), value.MissingPolicies...)}}
	default:
		return nil, fmt.Errorf("unknown reconciliation finding type %q", firstNonEmpty(value.Type, value.Kind))
	}
	return finding, nil
}

func reconciliationObjectProto(value reconciliationObjectJSON) *cyclopusv1.ReconciliationObject {
	return &cyclopusv1.ReconciliationObject{Kind: firstNonEmpty(value.Kind, value.ObjectKind), Namespace: firstNonEmpty(value.Namespace, value.ObjectNamespace), Name: firstNonEmpty(value.Name, value.ObjectName)}
}

func reconciliationEvidenceProto(value store.ReconciliationEvidence) (*cyclopusv1.ReconciliationEvidence, error) {
	var details any
	if len(value.Details) != 0 && string(value.Details) != "null" {
		if err := json.Unmarshal(value.Details, &details); err != nil {
			return nil, err
		}
	}
	structured, err := valueProto(details)
	if err != nil {
		return nil, err
	}
	return &cyclopusv1.ReconciliationEvidence{Sequence: value.Sequence, Kind: value.Kind, Object: &cyclopusv1.ReconciliationObject{Kind: stringValue(value.ObjectKind), Namespace: stringValue(value.ObjectNamespace), Name: stringValue(value.ObjectName)}, Action: stringValue(value.Action), DesiredHash: stringValue(value.DesiredHash), ObservedHash: stringValue(value.ObservedHash), Details: structured, ObservedAt: timeProto(&value.ObservedAt)}, nil
}

func valueProto(value any) (*structpb.Value, error) {
	if value == nil {
		return nil, nil
	}
	return structpb.NewValue(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func timeProto(value *time.Time) *timestamppb.Timestamp {
	if value == nil || value.IsZero() {
		return nil
	}
	return timestamppb.New(*value)
}
func scalingModeString(v cyclopusv1.ScalingMode) string {
	return map[cyclopusv1.ScalingMode]string{cyclopusv1.ScalingMode_SCALING_MODE_HPA: "hpa", cyclopusv1.ScalingMode_SCALING_MODE_KEDA_CRON: "keda_cron", cyclopusv1.ScalingMode_SCALING_MODE_KEDA_HTTP: "keda_http"}[v]
}
func scalingModeProto(v string) cyclopusv1.ScalingMode {
	return map[string]cyclopusv1.ScalingMode{"hpa": cyclopusv1.ScalingMode_SCALING_MODE_HPA, "keda_cron": cyclopusv1.ScalingMode_SCALING_MODE_KEDA_CRON, "keda_http": cyclopusv1.ScalingMode_SCALING_MODE_KEDA_HTTP}[v]
}
func runtimeClassProto(v string) cyclopusv1.RuntimeClass {
	return map[string]cyclopusv1.RuntimeClass{"gvisor": cyclopusv1.RuntimeClass_RUNTIME_CLASS_GVISOR, "runc": cyclopusv1.RuntimeClass_RUNTIME_CLASS_RUNC}[v]
}
func environmentKindProto(v string) cyclopusv1.EnvironmentKind {
	return map[string]cyclopusv1.EnvironmentKind{"production": cyclopusv1.EnvironmentKind_ENVIRONMENT_KIND_PRODUCTION, "preview": cyclopusv1.EnvironmentKind_ENVIRONMENT_KIND_PREVIEW}[v]
}
func releaseKindProto(v string) cyclopusv1.ReleaseKind {
	return map[string]cyclopusv1.ReleaseKind{"deploy": cyclopusv1.ReleaseKind_RELEASE_KIND_DEPLOY, "env_change": cyclopusv1.ReleaseKind_RELEASE_KIND_ENV_CHANGE, "rollback": cyclopusv1.ReleaseKind_RELEASE_KIND_ROLLBACK, "reconcile": cyclopusv1.ReleaseKind_RELEASE_KIND_RECONCILE}[v]
}
func releaseStatusProto(v string) cyclopusv1.ReleaseStatus {
	return map[string]cyclopusv1.ReleaseStatus{"pending": cyclopusv1.ReleaseStatus_RELEASE_STATUS_PENDING, "deploying": cyclopusv1.ReleaseStatus_RELEASE_STATUS_DEPLOYING, "live": cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE, "failed": cyclopusv1.ReleaseStatus_RELEASE_STATUS_FAILED, "superseded": cyclopusv1.ReleaseStatus_RELEASE_STATUS_SUPERSEDED}[v]
}
func operationStatus(v string) cyclopusv1.OperationStatus {
	return map[string]cyclopusv1.OperationStatus{"queued": cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED, "accepted": cyclopusv1.OperationStatus_OPERATION_STATUS_QUEUED, "leased": cyclopusv1.OperationStatus_OPERATION_STATUS_RUNNING, "running": cyclopusv1.OperationStatus_OPERATION_STATUS_RUNNING, "completed": cyclopusv1.OperationStatus_OPERATION_STATUS_SUCCEEDED, "succeeded": cyclopusv1.OperationStatus_OPERATION_STATUS_SUCCEEDED, "failed": cyclopusv1.OperationStatus_OPERATION_STATUS_FAILED, "cancelled": cyclopusv1.OperationStatus_OPERATION_STATUS_CANCELLED}[v]
}
func reconcileScopeKindProto(v string) cyclopusv1.ReconcileScopeKind {
	return map[string]cyclopusv1.ReconcileScopeKind{"org": cyclopusv1.ReconcileScopeKind_RECONCILE_SCOPE_KIND_ORGANIZATION, "app": cyclopusv1.ReconcileScopeKind_RECONCILE_SCOPE_KIND_APPLICATION, "environment": cyclopusv1.ReconcileScopeKind_RECONCILE_SCOPE_KIND_ENVIRONMENT}[v]
}

type fieldViolation struct{ field, description string }
type validationViolations []fieldViolation

func (v *validationViolations) add(field, description string) {
	*v = append(*v, fieldViolation{field, description})
}
func (v *validationViolations) uuid(field, value string) {
	if value == "" {
		return
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value {
		v.add(field, "must be a canonical UUID")
	}
}
func (v validationViolations) err() error {
	if len(v) == 0 {
		return nil
	}
	sort.Slice(v, func(i, j int) bool {
		if v[i].field == v[j].field {
			return v[i].description < v[j].description
		}
		return v[i].field < v[j].field
	})
	details := make([]*errdetails.BadRequest_FieldViolation, 0, len(v))
	for _, violation := range v {
		details = append(details, &errdetails.BadRequest_FieldViolation{Field: violation.field, Description: violation.description})
	}
	base := status.New(codes.InvalidArgument, "request validation failed")
	withDetails, err := base.WithDetails(&errdetails.ErrorInfo{Reason: "INVALID_ARGUMENT", Domain: "cyclopus.dev"}, &errdetails.BadRequest{FieldViolations: details})
	if err != nil {
		return base.Err()
	}
	return withDetails.Err()
}

type cursorKey struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}
type pageCursor struct {
	Scope string    `json:"scope"`
	Key   cursorKey `json:"key"`
}

type reconciliationEvidencePageCursor struct {
	Scope    string `json:"scope"`
	Sequence int64  `json:"sequence"`
}

func boundedPageSize(requested int32) (int, error) {
	if requested < 0 {
		return 0, fieldError("page_size", "must not be negative")
	}
	size := int(requested)
	if size == 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	return size, nil
}

func reconciliationEvidenceCursor(token, scope string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, fieldError("page_token", "is invalid")
	}
	var cursor reconciliationEvidencePageCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Scope != scope || cursor.Sequence <= 0 {
		return 0, fieldError("page_token", "is invalid")
	}
	return cursor.Sequence, nil
}

func encodeReconciliationEvidenceCursor(scope string, sequence int64) string {
	raw, _ := json.Marshal(reconciliationEvidencePageCursor{Scope: scope, Sequence: sequence})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func pageScope(values ...any) string {
	raw, _ := json.Marshal(values)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func pageWindow[T any](requested int32, token, scope string, values []T, key func(T) cursorKey) (int, int, error) {
	if requested < 0 {
		return 0, 0, fieldError("page_size", "must not be negative")
	}
	size := int(requested)
	if size == 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	if token == "" {
		return 0, size, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, 0, fieldError("page_token", "is invalid")
	}
	var cursor pageCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Scope != scope || cursor.Key.ID == "" {
		return 0, 0, fieldError("page_token", "is invalid")
	}
	for index, value := range values {
		if key(value) == cursor.Key {
			return index + 1, size, nil
		}
	}
	return 0, 0, fieldError("page_token", "is stale or invalid")
}
func pageValues[T any](values []T, start, size int, scope string, key func(T) cursorKey) ([]T, string) {
	if start > len(values) {
		start = len(values)
	}
	end := start + size
	if end > len(values) {
		end = len(values)
	}
	page := values[start:end]
	if end == len(values) || len(page) == 0 {
		return page, ""
	}
	encoded, _ := json.Marshal(pageCursor{Scope: scope, Key: key(page[len(page)-1])})
	return page, base64.RawURLEncoding.EncodeToString(encoded)
}

func mergeMaskedMessage(destination, source proto.Message, mask *fieldmaskpb.FieldMask, allowed map[string]bool) error {
	violations := validationViolations{}
	if mask == nil || len(mask.GetPaths()) == 0 {
		violations.add("update_mask", "must contain at least one mutable field")
		return violations.err()
	}
	paths := append([]string(nil), mask.GetPaths()...)
	sort.Strings(paths)
	destinationReflect, sourceReflect := destination.ProtoReflect(), source.ProtoReflect()
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			violations.add("update_mask.paths", "must not contain duplicate path "+path)
			continue
		}
		seen[path] = true
		if strings.Contains(path, ".") || !allowed[path] {
			violations.add("update_mask.paths", "contains unsupported path "+path)
			continue
		}
		field := destinationReflect.Descriptor().Fields().ByName(protoreflect.Name(path))
		if field == nil {
			violations.add("update_mask.paths", "contains unknown path "+path)
			continue
		}
		destinationReflect.Set(field, sourceReflect.Get(field))
	}
	return violations.err()
}

func releaseStatusFilters(values []cyclopusv1.ReleaseStatus) ([]string, error) {
	seen := map[cyclopusv1.ReleaseStatus]bool{}
	filters := make([]string, 0, len(values))
	violations := validationViolations{}
	for index, value := range values {
		name := map[cyclopusv1.ReleaseStatus]string{cyclopusv1.ReleaseStatus_RELEASE_STATUS_PENDING: "pending", cyclopusv1.ReleaseStatus_RELEASE_STATUS_DEPLOYING: "deploying", cyclopusv1.ReleaseStatus_RELEASE_STATUS_LIVE: "live", cyclopusv1.ReleaseStatus_RELEASE_STATUS_FAILED: "failed", cyclopusv1.ReleaseStatus_RELEASE_STATUS_SUPERSEDED: "superseded"}[value]
		if name == "" {
			violations.add(fmt.Sprintf("statuses[%d]", index), "must be specified")
		} else if seen[value] {
			violations.add(fmt.Sprintf("statuses[%d]", index), "must not be duplicated")
		} else {
			seen[value] = true
			filters = append(filters, name)
		}
	}
	sort.Strings(filters)
	return filters, violations.err()
}
func reconcileCategoryString(value cyclopusv1.ReconcileCategory) string {
	return map[cyclopusv1.ReconcileCategory]string{cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_DATABASE: "database", cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_NAMESPACE: "namespace", cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_WORKLOAD: "workload", cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_NETWORK_POLICY: "network_policy", cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_INGRESS: "ingress", cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_TLS: "tls", cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_SCALING: "scaling", cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_ENVIRONMENT_VARIABLES: "environment_variables", cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_ORCHESTRATION: "orchestration"}[value]
}
func reconcileCategoryProto(value string) cyclopusv1.ReconcileCategory {
	return map[string]cyclopusv1.ReconcileCategory{"database": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_DATABASE, "namespace": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_NAMESPACE, "workload": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_WORKLOAD, "network_policy": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_NETWORK_POLICY, "ingress": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_INGRESS, "tls": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_TLS, "scaling": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_SCALING, "environment_variables": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_ENVIRONMENT_VARIABLES, "orchestration": cyclopusv1.ReconcileCategory_RECONCILE_CATEGORY_ORCHESTRATION}[value]
}
func reconcileFindingStatusProto(value string) cyclopusv1.ReconcileFindingStatus {
	return map[string]cyclopusv1.ReconcileFindingStatus{"in_sync": cyclopusv1.ReconcileFindingStatus_RECONCILE_FINDING_STATUS_IN_SYNC, "drifted": cyclopusv1.ReconcileFindingStatus_RECONCILE_FINDING_STATUS_DRIFTED, "missing": cyclopusv1.ReconcileFindingStatus_RECONCILE_FINDING_STATUS_MISSING, "orphaned": cyclopusv1.ReconcileFindingStatus_RECONCILE_FINDING_STATUS_ORPHANED, "error": cyclopusv1.ReconcileFindingStatus_RECONCILE_FINDING_STATUS_ERROR}[value]
}
func timeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
