package server

import (
	"context"
	"strconv"
	"strings"

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
	if _, err := administratorActor(ctx); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	return nil, typedError(codes.FailedPrecondition, "GitHub App setup is not configured", "GITHUB_APP_NOT_CONFIGURED")
}

func (h *apiHandlers) BeginGitHubInstallation(ctx context.Context, req *cyclopusv1.BeginGitHubInstallationRequest) (*cyclopusv1.BeginGitHubInstallationResponse, error) {
	if _, err := administratorActor(ctx); err != nil {
		return nil, err
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	return nil, typedError(codes.FailedPrecondition, "GitHub App setup is not configured", "GITHUB_APP_NOT_CONFIGURED")
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
	if _, err := administratorActor(ctx); err != nil {
		return nil, err
	}
	installationID, err := strconv.ParseInt(req.GetInstallationId(), 10, 64)
	if err != nil || installationID <= 0 {
		return nil, fieldError("installation_id", "must be a positive decimal GitHub installation ID")
	}
	if err := requiredFields(map[string]string{"idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	return nil, typedError(codes.FailedPrecondition, "GitHub installation refresh is unavailable until GitHub App setup is configured", "GITHUB_APP_NOT_CONFIGURED")
}

func (h *apiHandlers) ListGitHubRepositories(ctx context.Context, req *cyclopusv1.ListGitHubRepositoriesRequest) (*cyclopusv1.ListGitHubRepositoriesResponse, error) {
	if _, err := administratorActor(ctx); err != nil {
		return nil, err
	}
	if _, err := boundedPageSize(req.GetPageSize()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetPageToken()) != "" {
		return nil, fieldError("page_token", "is not available until GitHub discovery is configured")
	}
	installationID, err := strconv.ParseInt(req.GetInstallationId(), 10, 64)
	if err != nil || installationID <= 0 {
		return nil, fieldError("installation_id", "must be a positive decimal GitHub installation ID")
	}
	return nil, typedError(codes.FailedPrecondition, "GitHub repository discovery is unavailable until GitHub App setup is configured", "GITHUB_APP_NOT_CONFIGURED")
}

func (h *apiHandlers) ListGitHubRepositoryWorkflows(ctx context.Context, req *cyclopusv1.ListGitHubRepositoryWorkflowsRequest) (*cyclopusv1.ListGitHubRepositoryWorkflowsResponse, error) {
	if _, err := administratorActor(ctx); err != nil {
		return nil, err
	}
	if req.GetRepositoryId() <= 0 {
		return nil, fieldError("repository_id", "must be positive")
	}
	if _, err := boundedPageSize(req.GetPageSize()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetPageToken()) != "" {
		return nil, fieldError("page_token", "is not available until GitHub discovery is configured")
	}
	return nil, typedError(codes.FailedPrecondition, "GitHub workflow discovery is unavailable until GitHub App setup is configured", "GITHUB_APP_NOT_CONFIGURED")
}

func (h *apiHandlers) ImportGitRepository(ctx context.Context, req *cyclopusv1.ImportGitRepositoryRequest) (*cyclopusv1.ImportGitRepositoryResponse, error) {
	if _, err := administratorActor(ctx); err != nil {
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
	return nil, typedError(codes.FailedPrecondition, "GitHub repository import is not configured", "GITHUB_APP_NOT_CONFIGURED")
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
	if _, err := administratorActor(ctx); err != nil {
		return nil, err
	}
	if req.GetCredential() == nil {
		return nil, fieldError("credential", "is required")
	}
	if err := requiredFields(map[string]string{"credential.username": req.GetCredential().GetUsername(), "credential.personal_access_token": req.GetCredential().GetPersonalAccessToken(), "idempotency_key": req.GetIdempotencyKey()}); err != nil {
		return nil, err
	}
	return nil, typedError(codes.FailedPrecondition, "GHCR credential rotation is not configured", "REGISTRY_AUTH_REQUIRED")
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
	return &cyclopusv1.GitHubInstallation{Id: strconv.FormatInt(v.GitHubInstallationID, 10), GithubInstallationId: v.GitHubInstallationID,
		AccountId: v.AccountID, AccountLogin: v.AccountLogin, AccountType: githubAccountType(v.AccountType), RepositorySelection: githubRepositorySelection(v.RepositorySelection),
		LifecycleStatus: githubInstallationStatus(v.AccessState), SuspendedAt: timeProto(v.SuspendedAt), DeletedAt: timeProto(v.DeletedAt), SyncedAt: timeProto(v.LastReconciledAt),
		SettingsUrl: "https://github.com/settings/installations/" + strconv.FormatInt(v.GitHubInstallationID, 10), CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt)}
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
	return map[string]cyclopusv1.GhcrCredentialState{"absent": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_NOT_CONFIGURED, "creating": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_CREATING, "ready": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_READY, "rotating": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_ROTATING, "reconciling": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_RECONCILING, "failed": cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_FAILED}[v]
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
