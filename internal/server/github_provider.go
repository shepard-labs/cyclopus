package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (h *apiHandlers) GetGitHubAppStatus(ctx context.Context, _ *cyclopusv1.GetGitHubAppStatusRequest) (*cyclopusv1.GetGitHubAppStatusResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	value, err := h.service.GetGitHubApp(ctx, actor)
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetGitHubAppStatusResponse{GithubApp: githubAppProto(value)}, nil
}

func (h *apiHandlers) BeginGitHubAppSetup(ctx context.Context, req *cyclopusv1.BeginGitHubAppSetupRequest) (*cyclopusv1.BeginGitHubAppSetupResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	if h.service == nil {
		return nil, typedError(codes.FailedPrecondition, "GitHub App is not configured", "GITHUB_APP_NOT_CONFIGURED")
	}
	flow, err := h.service.BeginGitHubAppSetup(ctx, actor, req.GetIdempotencyKey())
	if err != nil {
		return nil, githubProviderError(err)
	}
	return &cyclopusv1.BeginGitHubAppSetupResponse{Flow: &cyclopusv1.BeginGitHubFlowResponse{StartUrl: flow.StartURL, ExpiresAt: timestamppb.New(flow.ExpiresAt)}}, nil
}

func (h *apiHandlers) BeginGitHubInstallation(ctx context.Context, req *cyclopusv1.BeginGitHubInstallationRequest) (*cyclopusv1.BeginGitHubInstallationResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	if h.service == nil {
		return nil, typedError(codes.FailedPrecondition, "GitHub App is not configured", "GITHUB_APP_NOT_CONFIGURED")
	}
	flow, err := h.service.BeginGitHubInstallation(ctx, actor, req.GetIdempotencyKey())
	if err != nil {
		return nil, githubProviderError(err)
	}
	return &cyclopusv1.BeginGitHubInstallationResponse{Flow: &cyclopusv1.BeginGitHubFlowResponse{StartUrl: flow.StartURL, ExpiresAt: timestamppb.New(flow.ExpiresAt)}}, nil
}

func (h *apiHandlers) GetGitHubInstallation(ctx context.Context, req *cyclopusv1.GetGitHubInstallationRequest) (*cyclopusv1.GetGitHubInstallationResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	value, err := h.service.GetGitHubInstallation(ctx, actor, req.GetInstallationId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetGitHubInstallationResponse{Installation: githubInstallationProto(value)}, nil
}

func (h *apiHandlers) ListGitHubInstallations(ctx context.Context, req *cyclopusv1.ListGitHubInstallationsRequest) (*cyclopusv1.ListGitHubInstallationsResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	values, err := h.service.ListGitHubInstallations(ctx, actor)
	if err != nil {
		return nil, rpcError(err)
	}
	scope := pageScope("github_installations", actor.OrgID)
	start, size, err := pageWindow(req.GetPageSize(), req.GetPageToken(), scope, values, func(v store.GitHubInstallation) cursorKey {
		return cursorKey{CreatedAt: v.CreatedAt, ID: strconv.FormatInt(v.GitHubInstallationID, 10)}
	})
	if err != nil {
		return nil, err
	}
	page, next := pageValues(values, start, size, scope, func(v store.GitHubInstallation) cursorKey {
		return cursorKey{CreatedAt: v.CreatedAt, ID: strconv.FormatInt(v.GitHubInstallationID, 10)}
	})
	response := &cyclopusv1.ListGitHubInstallationsResponse{NextPageToken: next, Installations: make([]*cyclopusv1.GitHubInstallation, 0, len(page))}
	for _, value := range page {
		response.Installations = append(response.Installations, githubInstallationProto(value))
	}
	return response, nil
}

func (h *apiHandlers) RefreshGitHubInstallation(ctx context.Context, req *cyclopusv1.RefreshGitHubInstallationRequest) (*cyclopusv1.RefreshGitHubInstallationResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	installationID, err := strconv.ParseInt(req.GetInstallationId(), 10, 64)
	if err != nil || installationID <= 0 {
		return nil, fieldError("installation_id", "must be a positive decimal GitHub installation ID")
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	if h.service == nil {
		return nil, typedError(codes.FailedPrecondition, "GitHub App is not configured", "GITHUB_APP_NOT_CONFIGURED")
	}
	value, err := h.service.RefreshGitHubInstallation(ctx, actor, req.GetIdempotencyKey(), req.GetInstallationId())
	if err != nil {
		return nil, githubProviderError(err)
	}
	return &cyclopusv1.RefreshGitHubInstallationResponse{Installation: githubInstallationProto(value)}, nil
}

func githubProviderError(err error) error {
	switch {
	case errors.Is(err, service.ErrGitHubAppNotConfigured):
		return typedError(codes.FailedPrecondition, "GitHub App is not configured", "GITHUB_APP_NOT_CONFIGURED")
	case errors.Is(err, service.ErrGitHubSetupState), errors.Is(err, store.ErrFailedPrecondition):
		return typedError(codes.FailedPrecondition, "GitHub setup state is invalid, expired, or consumed", "GITHUB_SETUP_STATE_INVALID")
	case errors.Is(err, store.ErrAlreadyExists):
		return typedError(codes.Aborted, "GitHub App setup is already in progress", "GITHUB_APP_SETUP_IN_PROGRESS")
	case errors.Is(err, service.ErrConflict):
		return typedError(codes.Aborted, "idempotency key conflicts with a different GitHub request", "CONFLICT")
	case errors.Is(err, service.ErrInProgress):
		return typedError(codes.Aborted, "GitHub operation is already in progress", "CONFLICT")
	default:
		return rpcError(err)
	}
}

func (h *apiHandlers) ListGitHubRepositories(ctx context.Context, req *cyclopusv1.ListGitHubRepositoriesRequest) (*cyclopusv1.ListGitHubRepositoriesResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	if h.service == nil {
		return nil, typedError(codes.FailedPrecondition, "GitHub App is not configured", "GITHUB_APP_NOT_CONFIGURED")
	}
	size, err := boundedPageSize(req.GetPageSize())
	if err != nil {
		return nil, err
	}
	if err := validateUUID("installation_id", req.GetInstallationId()); err != nil {
		return nil, err
	}
	page, err := h.service.ListGitHubRepositories(ctx, actor, req.GetInstallationId(), size, req.GetPageToken())
	if err != nil {
		return nil, githubDiscoveryError(err)
	}
	response := &cyclopusv1.ListGitHubRepositoriesResponse{NextPageToken: page.NextPageToken, Repositories: make([]*cyclopusv1.GitHubRepository, 0, len(page.Repositories))}
	for _, value := range page.Repositories {
		response.Repositories = append(response.Repositories, githubRepositoryProto(value))
	}
	return response, nil
}

func (h *apiHandlers) ListGitHubRepositoryWorkflows(ctx context.Context, req *cyclopusv1.ListGitHubRepositoryWorkflowsRequest) (*cyclopusv1.ListGitHubRepositoryWorkflowsResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	if h.service == nil {
		return nil, typedError(codes.FailedPrecondition, "GitHub App is not configured", "GITHUB_APP_NOT_CONFIGURED")
	}
	if req.GetRepositoryId() <= 0 {
		return nil, fieldError("repository_id", "must be positive")
	}
	size, err := boundedPageSize(req.GetPageSize())
	if err != nil {
		return nil, err
	}
	page, err := h.service.ListGitHubRepositoryWorkflows(ctx, actor, req.GetRepositoryId(), size, req.GetPageToken())
	if err != nil {
		return nil, githubDiscoveryError(err)
	}
	response := &cyclopusv1.ListGitHubRepositoryWorkflowsResponse{NextPageToken: page.NextPageToken, Workflows: make([]*cyclopusv1.GitHubWorkflow, 0, len(page.Workflows))}
	for _, value := range page.Workflows {
		response.Workflows = append(response.Workflows, githubWorkflowProto(value))
	}
	return response, nil
}

func (h *apiHandlers) ImportGitRepository(ctx context.Context, req *cyclopusv1.ImportGitRepositoryRequest) (*cyclopusv1.ImportGitRepositoryResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	violations := validationViolations{}
	if req.GetRepositoryId() <= 0 {
		violations.add("repository_id", "must be positive")
	}
	for field, value := range map[string]string{"github_workflow_path": req.GetGithubWorkflowPath(), "project_name": req.GetProjectName(), "project_slug": req.GetProjectSlug(), "application_name": req.GetApplicationName(), "application_slug": req.GetApplicationSlug(), "hostname_label": req.GetHostnameLabel(), "image_repository": req.GetImageRepository(), "idempotency_key": req.GetIdempotencyKey()} {
		if strings.TrimSpace(value) == "" {
			violations.add(field, "is required")
		}
	}
	if credential := req.GetGhcrCredential(); credential != nil && (strings.TrimSpace(credential.GetUsername()) == "" || strings.TrimSpace(credential.GetPersonalAccessToken()) == "") {
		violations.add("ghcr_credential", "username and personal_access_token are required together")
	}
	if err := violations.err(); err != nil {
		return nil, err
	}
	app := service.AppInput{ContainerPort: int(req.GetContainerPort()), HealthCheckPath: req.GetHealthCheckPath(), ScalingMode: scalingModeString(req.GetScalingMode()),
		MinReplicas: int(req.GetMinReplicas()), MaxReplicas: int(req.GetMaxReplicas()), HPATargetCPUPercent: int(req.GetHpaTargetCpuPercent()),
		KEDACronSchedules: kedaCronSchedulesInput(req.GetKedaCronSchedules()), KEDAHTTPTargetPendingRequests: int(req.GetKedaHttpTargetPendingRequests()),
		CPURequestMillicores: int(req.GetCpuRequestMillicores()), CPULimitMillicores: int(req.GetCpuLimitMillicores()), MemoryRequestMiB: int(req.GetMemoryRequestMib()), MemoryLimitMiB: int(req.GetMemoryLimitMib()),
		PreviewsEnabled: req.GetPreviewsEnabled(), MaxPreviewEnvironments: int(req.GetMaxPreviewEnvironments()), PreviewIdleTTLHours: int(req.GetPreviewIdleTtlHours()), DeployTimeoutSeconds: int(req.GetDeployTimeoutSeconds()), ReleaseRetentionCount: int(req.GetReleaseRetentionCount())}
	if runtime := map[cyclopusv1.RuntimeClass]string{cyclopusv1.RuntimeClass_RUNTIME_CLASS_GVISOR: "gvisor", cyclopusv1.RuntimeClass_RUNTIME_CLASS_RUNC: "runc"}[req.GetRuntimeClass()]; runtime != "" {
		app.RuntimeClass = &runtime
	}
	var credential *service.GhcrCredentialInput
	if raw := req.GetGhcrCredential(); raw != nil {
		credential = &service.GhcrCredentialInput{Username: []byte(raw.GetUsername()), PersonalAccessToken: []byte(raw.GetPersonalAccessToken())}
		defer func() {
			clear(credential.Username)
			clear(credential.PersonalAccessToken)
		}()
	}
	operation, err := h.service.ImportGitRepository(ctx, actor, service.ImportGitRepositoryInput{RepositoryID: req.GetRepositoryId(), WorkflowPath: req.GetGithubWorkflowPath(), ProjectName: req.GetProjectName(), ProjectSlug: req.GetProjectSlug(), ApplicationName: req.GetApplicationName(), ApplicationSlug: req.GetApplicationSlug(), HostnameLabel: req.GetHostnameLabel(), ProductionBranch: req.GetProductionBranch(), ImageRepository: req.GetImageRepository(), IdempotencyKey: req.GetIdempotencyKey(), App: app, GhcrCredential: credential, PrivateCredentialRequested: req.GetGhcrCredential() != nil})
	if err != nil {
		return nil, ghcrCredentialError(err)
	}
	return &cyclopusv1.ImportGitRepositoryResponse{Operation: importOperationProto(operation)}, nil
}

func githubDiscoveryError(err error) error {
	var api *service.GitHubAPIError
	switch {
	case errors.Is(err, service.ErrGitHubAppNotConfigured):
		return typedError(codes.FailedPrecondition, "GitHub App is not configured", "GITHUB_APP_NOT_CONFIGURED")
	case errors.Is(err, service.ErrGitHubInstallationSuspended):
		return typedError(codes.FailedPrecondition, "GitHub installation is suspended", "GITHUB_INSTALLATION_SUSPENDED")
	case errors.Is(err, service.ErrGitHubInstallationRemoved):
		return typedError(codes.FailedPrecondition, "GitHub installation access was removed", "GITHUB_INSTALLATION_ACCESS_REMOVED")
	case errors.Is(err, service.ErrGitHubRepositoryRemoved):
		return typedError(codes.FailedPrecondition, "GitHub repository access was removed", "GITHUB_REPOSITORY_ACCESS_REMOVED")
	case errors.Is(err, service.ErrWorkflowIncompatible):
		return typedError(codes.FailedPrecondition, "GitHub workflow is incompatible", "WORKFLOW_INCOMPATIBLE")
	case errors.Is(err, service.ErrRegistryAuthRequired):
		return typedError(codes.FailedPrecondition, "GHCR authentication is required", "REGISTRY_AUTH_REQUIRED")
	case errors.Is(err, service.ErrConflict):
		return typedError(codes.Aborted, "idempotency key conflicts with a different import", "CONFLICT")
	case errors.Is(err, service.ErrInProgress):
		return typedError(codes.Aborted, "import is already in progress", "CONFLICT")
	case errors.As(err, &api):
		message := fmt.Sprintf("GitHub API request failed with status %d", api.Status)
		if api.RetryAfter > 0 {
			message += "; retry after " + api.RetryAfter.Round(time.Second).String()
		}
		return typedError(codes.Unavailable, message, "GITHUB_API_UNAVAILABLE")
	default:
		return rpcError(err)
	}
}

func (h *apiHandlers) GetImportOperation(ctx context.Context, req *cyclopusv1.GetImportOperationRequest) (*cyclopusv1.GetImportOperationResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUID("import_operation_id", req.GetImportOperationId()); err != nil {
		return nil, err
	}
	value, err := h.service.GetImportOperation(ctx, actor, req.GetImportOperationId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetImportOperationResponse{Operation: importOperationProto(value)}, nil
}

func (h *apiHandlers) GetGhcrCredentialStatus(ctx context.Context, _ *cyclopusv1.GetGhcrCredentialStatusRequest) (*cyclopusv1.GetGhcrCredentialStatusResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	value, err := h.service.GetGhcrCredentialStatus(ctx, actor)
	if err != nil {
		return nil, rpcError(err)
	}
	return &cyclopusv1.GetGhcrCredentialStatusResponse{Credential: ghcrCredentialStatusProto(value)}, nil
}

func (h *apiHandlers) RotateGhcrCredential(ctx context.Context, req *cyclopusv1.RotateGhcrCredentialRequest) (*cyclopusv1.RotateGhcrCredentialResponse, error) {
	actor, err := administratorActor(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetCredential() == nil {
		return nil, fieldError("credential", "is required")
	}
	if err := requiredFields(map[string]string{"credential.username": req.GetCredential().GetUsername(), "credential.personal_access_token": req.GetCredential().GetPersonalAccessToken(), "idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	input := service.GhcrCredentialInput{Username: []byte(req.GetCredential().GetUsername()), PersonalAccessToken: []byte(req.GetCredential().GetPersonalAccessToken())}
	defer func() {
		clear(input.Username)
		clear(input.PersonalAccessToken)
	}()
	value, err := h.service.RotateGhcrCredential(ctx, actor, input, req.GetIdempotencyKey())
	if err != nil {
		return nil, ghcrCredentialError(err)
	}
	return &cyclopusv1.RotateGhcrCredentialResponse{Credential: ghcrCredentialStatusProto(value)}, nil
}

// ghcrCredentialError maps credential flow errors without echoing usernames,
// PATs, dockerconfig payloads, or Secret references. Cross-organization
// resources and missing configuration share the same non-enumerating
// failed-precondition body.
func ghcrCredentialError(err error) error {
	switch {
	case errors.Is(err, service.ErrRegistryCredentialInProgress):
		return typedError(codes.Aborted, "GHCR credential operation is already in progress", "CONFLICT")
	case errors.Is(err, service.ErrRegistryCredentialRetry):
		return typedError(codes.Unavailable, "GHCR credential source write failed; retry with the same idempotency key", "CREDENTIAL_SOURCE_UNAVAILABLE")
	case errors.Is(err, service.ErrRegistryCredentialExists):
		return typedError(codes.FailedPrecondition, "GHCR credential is already configured; use rotation to replace it", "REGISTRY_CREDENTIAL_EXISTS")
	case errors.Is(err, service.ErrRegistryAuthRequired):
		return typedError(codes.FailedPrecondition, "GHCR authentication is required", "REGISTRY_AUTH_REQUIRED")
	default:
		return githubDiscoveryError(err)
	}
}

func administratorActor(ctx context.Context) (service.ActorContext, error) {
	actor, err := sessionActor(ctx)
	if err != nil {
		return service.ActorContext{}, err
	}
	if !actor.Operator {
		return service.ActorContext{}, typedError(codes.PermissionDenied, "administrator permission required", "PERMISSION_DENIED")
	}
	return actor, nil
}

func githubAppProto(v store.GitHubApp) *cyclopusv1.GitHubAppStatus {
	status := &cyclopusv1.GitHubAppStatus{SetupState: githubAppState(v.State), Slug: v.Slug, Name: v.Name, HtmlUrl: v.HTMLURL,
		WebhookUrl:          v.WebhookURL,
		CredentialReadiness: githubCredentialReadiness(v.State, v.HasCredential), FailureCode: integrationFailureCode(stringValue(v.FailureCode)), FailureMessage: stringValue(v.FailureMessage), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt)}
	if v.AppID != nil {
		status.AppId = *v.AppID
	}
	if v.OwnerLogin != nil {
		status.OwnerLogin = *v.OwnerLogin
	}
	if v.OwnerType != nil {
		status.OwnerType = githubAccountType(*v.OwnerType)
	}
	return status
}

func githubInstallationProto(v store.GitHubInstallation) *cyclopusv1.GitHubInstallation {
	return &cyclopusv1.GitHubInstallation{Id: v.ID, GithubInstallationId: v.GitHubInstallationID,
		AccountId: v.AccountID, AccountLogin: v.AccountLogin, AccountType: githubAccountType(v.AccountType), RepositorySelection: githubRepositorySelection(v.RepositorySelection),
		LifecycleStatus: githubInstallationStatus(v.AccessState), SuspendedAt: timeProto(v.SuspendedAt), DeletedAt: timeProto(v.DeletedAt), SyncedAt: timeProto(v.LastReconciledAt),
		SettingsUrl: "https://github.com/settings/installations/" + strconv.FormatInt(v.GitHubInstallationID, 10), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt)}
}

func githubRepositoryProto(v store.GitHubRepository) *cyclopusv1.GitHubRepository {
	return &cyclopusv1.GitHubRepository{Id: v.GitHubRepositoryID, InstallationId: v.InstallationID, NodeId: v.NodeID, Owner: v.OwnerLogin, Name: v.Name, FullName: v.FullName,
		Private: v.Visibility != "public", Archived: v.Archived, DefaultBranch: v.DefaultBranch, AccessStatus: map[string]cyclopusv1.GitHubRepositoryAccessStatus{"active": cyclopusv1.GitHubRepositoryAccessStatus_GITHUB_REPOSITORY_ACCESS_STATUS_ACTIVE, "inaccessible": cyclopusv1.GitHubRepositoryAccessStatus_GITHUB_REPOSITORY_ACCESS_STATUS_INACCESSIBLE, "deleted": cyclopusv1.GitHubRepositoryAccessStatus_GITHUB_REPOSITORY_ACCESS_STATUS_DELETED}[v.AccessState], SyncedAt: timeProto(v.LastReconciledAt)}
}

func githubWorkflowProto(v service.GitHubWorkflow) *cyclopusv1.GitHubWorkflow {
	compatibility := cyclopusv1.GitHubWorkflowCompatibility_GITHUB_WORKFLOW_COMPATIBILITY_INCOMPATIBLE
	if v.Analysis.Compatible {
		compatibility = cyclopusv1.GitHubWorkflowCompatibility_GITHUB_WORKFLOW_COMPATIBILITY_COMPATIBLE
	}
	state := cyclopusv1.GitHubWorkflowState_GITHUB_WORKFLOW_STATE_DISABLED
	if v.State == "active" {
		state = cyclopusv1.GitHubWorkflowState_GITHUB_WORKFLOW_STATE_ACTIVE
	}
	return &cyclopusv1.GitHubWorkflow{Path: v.Path, Name: v.Name, State: state, Compatibility: compatibility, IncompatibilityReasons: v.Analysis.Reasons}
}

func importOperationProto(v store.ImportOperation) *cyclopusv1.ImportOperation {
	return &cyclopusv1.ImportOperation{Id: v.ID, State: importOperationState(v.State), GithubRepositoryId: v.GitHubRepositoryID,
		GitConnectionId: stringValue(v.GitConnectionID), ProjectId: stringValue(v.ProjectID), ApplicationId: stringValue(v.AppID), EnvironmentId: stringValue(v.EnvironmentID),
		OrchestrationJobId: stringValue(v.OrchestrationJobID), FailureCode: integrationFailureCode(stringValue(v.FailureCode)), FailureMessage: stringValue(v.FailureMessage), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), CompletedAt: timeProto(v.CompletedAt)}
}

func ghcrCredentialStatusProto(v store.Registry) *cyclopusv1.GhcrCredentialStatus {
	return &cyclopusv1.GhcrCredentialStatus{Configured: v.HasActiveCredential, Generation: v.ActiveGeneration, State: ghcrCredentialState(v.State),
		FailureCode: integrationFailureCode(stringValue(v.FailureCode)), FailureMessage: stringValue(v.FailureMessage), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt)}
}

func githubAccountType(v string) cyclopusv1.GitHubAccountType {
	return map[string]cyclopusv1.GitHubAccountType{"User": cyclopusv1.GitHubAccountType_GITHUB_ACCOUNT_TYPE_USER, "Organization": cyclopusv1.GitHubAccountType_GITHUB_ACCOUNT_TYPE_ORGANIZATION}[v]
}
func githubRepositorySelection(v string) cyclopusv1.GitHubInstallationRepositorySelection {
	return map[string]cyclopusv1.GitHubInstallationRepositorySelection{"all": cyclopusv1.GitHubInstallationRepositorySelection_GITHUB_INSTALLATION_REPOSITORY_SELECTION_ALL, "selected": cyclopusv1.GitHubInstallationRepositorySelection_GITHUB_INSTALLATION_REPOSITORY_SELECTION_SELECTED}[v]
}
func githubInstallationStatus(v string) cyclopusv1.GitHubInstallationLifecycleStatus {
	return map[string]cyclopusv1.GitHubInstallationLifecycleStatus{"active": cyclopusv1.GitHubInstallationLifecycleStatus_GITHUB_INSTALLATION_LIFECYCLE_STATUS_ACTIVE, "suspended": cyclopusv1.GitHubInstallationLifecycleStatus_GITHUB_INSTALLATION_LIFECYCLE_STATUS_SUSPENDED, "deleted": cyclopusv1.GitHubInstallationLifecycleStatus_GITHUB_INSTALLATION_LIFECYCLE_STATUS_DELETED}[v]
}
func githubAppState(v string) cyclopusv1.GitHubAppSetupState {
	return map[string]cyclopusv1.GitHubAppSetupState{"absent": cyclopusv1.GitHubAppSetupState_GITHUB_APP_SETUP_STATE_NOT_CONFIGURED, "exchanging": cyclopusv1.GitHubAppSetupState_GITHUB_APP_SETUP_STATE_SETUP_IN_PROGRESS, "secret_pending": cyclopusv1.GitHubAppSetupState_GITHUB_APP_SETUP_STATE_SETUP_IN_PROGRESS, "ready": cyclopusv1.GitHubAppSetupState_GITHUB_APP_SETUP_STATE_READY, "failed": cyclopusv1.GitHubAppSetupState_GITHUB_APP_SETUP_STATE_FAILED, "orphaned": cyclopusv1.GitHubAppSetupState_GITHUB_APP_SETUP_STATE_ORPHANED, "replacing": cyclopusv1.GitHubAppSetupState_GITHUB_APP_SETUP_STATE_REPLACING}[v]
}
func githubCredentialReadiness(state string, ready bool) cyclopusv1.GitHubCredentialReadiness {
	if ready {
		return cyclopusv1.GitHubCredentialReadiness_GITHUB_CREDENTIAL_READINESS_READY
	}
	if state == "failed" || state == "orphaned" {
		return cyclopusv1.GitHubCredentialReadiness_GITHUB_CREDENTIAL_READINESS_FAILED
	}
	if state == "absent" {
		return cyclopusv1.GitHubCredentialReadiness_GITHUB_CREDENTIAL_READINESS_NOT_CONFIGURED
	}
	return cyclopusv1.GitHubCredentialReadiness_GITHUB_CREDENTIAL_READINESS_PENDING
}
func importOperationState(v string) cyclopusv1.ImportOperationState {
	return map[string]cyclopusv1.ImportOperationState{"pending": cyclopusv1.ImportOperationState_IMPORT_OPERATION_STATE_PENDING, "credential_pending": cyclopusv1.ImportOperationState_IMPORT_OPERATION_STATE_CREDENTIAL_PENDING, "committing": cyclopusv1.ImportOperationState_IMPORT_OPERATION_STATE_COMMITTING, "pending_dispatch": cyclopusv1.ImportOperationState_IMPORT_OPERATION_STATE_PENDING_DISPATCH, "completed": cyclopusv1.ImportOperationState_IMPORT_OPERATION_STATE_COMPLETED, "failed": cyclopusv1.ImportOperationState_IMPORT_OPERATION_STATE_FAILED, "cancelled": cyclopusv1.ImportOperationState_IMPORT_OPERATION_STATE_CANCELLED}[v]
}
func ghcrCredentialState(v string) cyclopusv1.GhcrCredentialState {
	return map[string]cyclopusv1.GhcrCredentialState{"absent": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_NOT_CONFIGURED, "creating": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_CREATING, "provisioning": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_CREATING, "ready": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_READY, "rotating": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_ROTATING, "reconciling": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_CREATING, "failed": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_FAILED}[v]
}

func integrationFailureCode(v string) cyclopusv1.IntegrationFailureCode {
	return map[string]cyclopusv1.IntegrationFailureCode{
		"github_app_not_configured":          cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_APP_NOT_CONFIGURED,
		"github_app_setup_in_progress":       cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_APP_SETUP_IN_PROGRESS,
		"github_manifest_code_invalid":       cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_MANIFEST_CODE_INVALID,
		"github_manifest_code_consumed":      cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_MANIFEST_CODE_CONSUMED,
		"github_app_secret_write_failed":     cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_APP_SECRET_WRITE_FAILED,
		"github_setup_state_invalid":         cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_SETUP_STATE_INVALID,
		"github_installation_suspended":      cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_INSTALLATION_SUSPENDED,
		"github_installation_access_removed": cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_INSTALLATION_ACCESS_REMOVED,
		"github_repository_access_removed":   cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_REPOSITORY_ACCESS_REMOVED,
		"workflow_incompatible":              cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_WORKFLOW_INCOMPATIBLE,
		"github_run_missing":                 cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_MISSING,
		"github_run_ambiguous":               cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_AMBIGUOUS,
		"github_run_cancel_failed":           cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_CANCEL_FAILED,
		"github_run_timed_out":               cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_TIMED_OUT,
		"registry_auth_required":             cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_REGISTRY_AUTH_REQUIRED,
		"registry_auth_failed":               cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_REGISTRY_AUTH_FAILED,
		"registry_challenge_invalid":         cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_REGISTRY_CHALLENGE_INVALID,
		"image_not_published":                cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_IMAGE_NOT_PUBLISHED,
		"image_digest_invalid":               cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_IMAGE_DIGEST_INVALID,
		"oci_annotation_mismatch":            cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_OCI_ANNOTATION_MISMATCH,
		"image_pull_secret_reconcile_failed": cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_IMAGE_PULL_SECRET_RECONCILE_FAILED,
	}[v]
}
