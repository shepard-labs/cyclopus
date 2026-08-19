package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cyclopusv1 "github.com/shepard-labs/cyclopus/gen/go/cyclopus/v1"
	internalauth "github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/service"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDisconnectGitConnectionRequiresAdministratorBeforeValidation(t *testing.T) {
	handler := &apiHandlers{}
	ctx := authenticatedServerContext(false)
	_, err := handler.DisconnectGitConnection(ctx, &cyclopusv1.DisconnectGitConnectionRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("DisconnectGitConnection() code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestEveryGitProviderMutationRequiresAdministratorBeforeValidation(t *testing.T) {
	handler := &apiHandlers{}
	ctx := authenticatedServerContext(false)
	tests := map[string]func() error{
		"begin app setup": func() error {
			_, err := handler.BeginGitHubAppSetup(ctx, &cyclopusv1.BeginGitHubAppSetupRequest{})
			return err
		},
		"begin installation": func() error {
			_, err := handler.BeginGitHubInstallation(ctx, &cyclopusv1.BeginGitHubInstallationRequest{})
			return err
		},
		"refresh installation": func() error {
			_, err := handler.RefreshGitHubInstallation(ctx, &cyclopusv1.RefreshGitHubInstallationRequest{})
			return err
		},
		"disconnect": func() error {
			_, err := handler.DisconnectGitConnection(ctx, &cyclopusv1.DisconnectGitConnectionRequest{})
			return err
		},
		"import": func() error {
			_, err := handler.ImportGitRepository(ctx, &cyclopusv1.ImportGitRepositoryRequest{})
			return err
		},
		"rotate credential": func() error {
			_, err := handler.RotateGhcrCredential(ctx, &cyclopusv1.RotateGhcrCredentialRequest{})
			return err
		},
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			if code := status.Code(call()); code != codes.PermissionDenied {
				t.Fatalf("code = %v, want PermissionDenied", code)
			}
		})
	}
}

func TestGitProviderReadAuthorizationBehavior(t *testing.T) {
	handler := &apiHandlers{}
	ctx := authenticatedServerContext(false)
	adminReads := map[string]func() error{
		"app": func() error {
			_, err := handler.GetGitHubAppStatus(ctx, &cyclopusv1.GetGitHubAppStatusRequest{})
			return err
		},
		"installation": func() error {
			_, err := handler.GetGitHubInstallation(ctx, &cyclopusv1.GetGitHubInstallationRequest{})
			return err
		},
		"installations": func() error {
			_, err := handler.ListGitHubInstallations(ctx, &cyclopusv1.ListGitHubInstallationsRequest{})
			return err
		},
		"repositories": func() error {
			_, err := handler.ListGitHubRepositories(ctx, &cyclopusv1.ListGitHubRepositoriesRequest{})
			return err
		},
		"workflows": func() error {
			_, err := handler.ListGitHubRepositoryWorkflows(ctx, &cyclopusv1.ListGitHubRepositoryWorkflowsRequest{})
			return err
		},
		"operation": func() error {
			_, err := handler.GetImportOperation(ctx, &cyclopusv1.GetImportOperationRequest{})
			return err
		},
		"credential": func() error {
			_, err := handler.GetGhcrCredentialStatus(ctx, &cyclopusv1.GetGhcrCredentialStatusRequest{})
			return err
		},
	}
	for name, call := range adminReads {
		t.Run(name, func(t *testing.T) {
			if status.Code(call()) != codes.PermissionDenied {
				t.Fatalf("read did not require administrator")
			}
		})
	}
	_, err := handler.GetGitConnection(ctx, &cyclopusv1.GetGitConnectionRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("connection read code = %v, want validation for authenticated member", status.Code(err))
	}
}

func TestRefreshGitHubInstallationRejectsNonPositiveID(t *testing.T) {
	handler := &apiHandlers{}
	ctx := authenticatedServerContext(true)
	for _, installationID := range []string{"0", "-1"} {
		t.Run(installationID, func(t *testing.T) {
			_, err := handler.RefreshGitHubInstallation(ctx, &cyclopusv1.RefreshGitHubInstallationRequest{InstallationId: installationID, IdempotencyKey: "refresh"})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("RefreshGitHubInstallation(%q) code = %v, want InvalidArgument", installationID, status.Code(err))
			}
		})
	}
}

func TestAllInstallationInputsRejectNonPositiveIDs(t *testing.T) {
	handler := &apiHandlers{}
	ctx := authenticatedServerContext(true)
	for _, id := range []string{"0", "-1"} {
		_, getErr := handler.GetGitHubInstallation(ctx, &cyclopusv1.GetGitHubInstallationRequest{InstallationId: id})
		_, listErr := handler.ListGitHubRepositories(ctx, &cyclopusv1.ListGitHubRepositoriesRequest{InstallationId: id})
		if status.Code(getErr) != codes.InvalidArgument || status.Code(listErr) != codes.FailedPrecondition {
			t.Errorf("installation %s codes = %v, %v", id, status.Code(getErr), status.Code(listErr))
		}
	}
}

func TestImportGitRepositoryRequiredFieldValidation(t *testing.T) {
	handler := &apiHandlers{}
	ctx := authenticatedServerContext(true)
	valid := &cyclopusv1.ImportGitRepositoryRequest{RepositoryId: 42, GithubWorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Platform", ProjectSlug: "platform", ApplicationName: "API", ApplicationSlug: "api", HostnameLabel: "api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "import"}
	if _, err := handler.ImportGitRepository(ctx, valid); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("valid request code = %v", status.Code(err))
	}
	for name, request := range map[string]*cyclopusv1.ImportGitRepositoryRequest{
		"zero repository":    {GithubWorkflowPath: valid.GithubWorkflowPath, ProjectName: valid.ProjectName, ProjectSlug: valid.ProjectSlug, ApplicationName: valid.ApplicationName, ApplicationSlug: valid.ApplicationSlug, HostnameLabel: valid.HostnameLabel, ImageRepository: valid.ImageRepository, IdempotencyKey: valid.IdempotencyKey},
		"missing workflow":   {RepositoryId: 42, ProjectName: valid.ProjectName, ProjectSlug: valid.ProjectSlug, ApplicationName: valid.ApplicationName, ApplicationSlug: valid.ApplicationSlug, HostnameLabel: valid.HostnameLabel, ImageRepository: valid.ImageRepository, IdempotencyKey: valid.IdempotencyKey},
		"partial credential": {RepositoryId: 42, GithubWorkflowPath: valid.GithubWorkflowPath, ProjectName: valid.ProjectName, ProjectSlug: valid.ProjectSlug, ApplicationName: valid.ApplicationName, ApplicationSlug: valid.ApplicationSlug, HostnameLabel: valid.HostnameLabel, ImageRepository: valid.ImageRepository, IdempotencyKey: valid.IdempotencyKey, GhcrCredential: &cyclopusv1.GhcrCredential{Username: "octocat"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.ImportGitRepository(ctx, request); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v", status.Code(err))
			}
		})
	}
}

func TestRotateGhcrCredentialHandlerWithoutServiceIsNonEnumerating(t *testing.T) {
	handler := &apiHandlers{service: service.New(nil)}
	ctx := authenticatedServerContext(true)
	_, err := handler.RotateGhcrCredential(ctx, &cyclopusv1.RotateGhcrCredentialRequest{
		Credential:     &cyclopusv1.GhcrCredential{Username: "octocat", PersonalAccessToken: "pat-value"},
		IdempotencyKey: "rotate",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == "REGISTRY_AUTH_REQUIRED" {
			return
		}
	}
	t.Fatalf("error %v lacks REGISTRY_AUTH_REQUIRED reason", err)
}

func TestRotateGhcrCredentialValidationPrecedesService(t *testing.T) {
	handler := &apiHandlers{service: service.New(nil)}
	ctx := authenticatedServerContext(true)
	for name, request := range map[string]*cyclopusv1.RotateGhcrCredentialRequest{
		"missing credential": {IdempotencyKey: "rotate"},
		"missing username":   {Credential: &cyclopusv1.GhcrCredential{PersonalAccessToken: "pat"}, IdempotencyKey: "rotate"},
		"missing pat":        {Credential: &cyclopusv1.GhcrCredential{Username: "octocat"}, IdempotencyKey: "rotate"},
		"missing key":        {Credential: &cyclopusv1.GhcrCredential{Username: "octocat", PersonalAccessToken: "pat"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.RotateGhcrCredential(ctx, request); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
			}
		})
	}
}

func TestGhcrCredentialErrorMapping(t *testing.T) {
	for name, test := range map[string]struct {
		err    error
		code   codes.Code
		reason string
	}{
		"not configured":   {service.ErrRegistryAuthRequired, codes.FailedPrecondition, "REGISTRY_AUTH_REQUIRED"},
		"already exists":   {service.ErrRegistryCredentialExists, codes.FailedPrecondition, "REGISTRY_CREDENTIAL_EXISTS"},
		"in progress":      {service.ErrRegistryCredentialInProgress, codes.Aborted, "CONFLICT"},
		"transient source": {service.ErrRegistryCredentialRetry, codes.Unavailable, "CREDENTIAL_SOURCE_UNAVAILABLE"},
		"invalid":          {service.ErrInvalidArgument, codes.InvalidArgument, "INVALID_ARGUMENT"},
		"forbidden":        {service.ErrForbidden, codes.PermissionDenied, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := ghcrCredentialError(test.err)
			if status.Code(err) != test.code {
				t.Fatalf("code = %v, want %v", status.Code(err), test.code)
			}
			if test.reason == "" {
				return
			}
			for _, detail := range status.Convert(err).Details() {
				if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == test.reason {
					return
				}
			}
			t.Fatalf("error %v lacks %s reason", err, test.reason)
		})
	}
}

func TestGhcrCredentialInternalPendingStatesMapToCreating(t *testing.T) {
	for _, state := range []string{"provisioning", "reconciling"} {
		if got := ghcrCredentialState(state); got != cyclopusv1.GhcrCredentialState_GHCR_CREDENTIAL_STATE_CREATING {
			t.Errorf("ghcrCredentialState(%q) = %v, want creating", state, got)
		}
	}
}

func TestGhcrCredentialErrorsNeverCarrySecretMaterial(t *testing.T) {
	handler := &apiHandlers{service: service.New(nil)}
	_, err := handler.RotateGhcrCredential(authenticatedServerContext(true), &cyclopusv1.RotateGhcrCredentialRequest{
		Credential:     &cyclopusv1.GhcrCredential{Username: "sentinel-user-9f2c", PersonalAccessToken: "sentinel-pat-71ad"},
		IdempotencyKey: "rotate",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	message := status.Convert(err).Message()
	if strings.Contains(message, "sentinel-user-9f2c") || strings.Contains(message, "sentinel-pat-71ad") || strings.Contains(message, "dockerconfigjson") || strings.Contains(message, "k8s-secret://") {
		t.Fatalf("error leaked credential material: %q", message)
	}
}

func TestIntegrationFailureCodeContract(t *testing.T) {
	cases := map[string]cyclopusv1.IntegrationFailureCode{
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
		"registry_auth_required":             cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_REGISTRY_AUTH_REQUIRED,
		"registry_auth_failed":               cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_REGISTRY_AUTH_FAILED,
		"image_pull_secret_reconcile_failed": cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_IMAGE_PULL_SECRET_RECONCILE_FAILED,
		"github_run_missing":                 cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_MISSING,
		"github_run_ambiguous":               cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_AMBIGUOUS,
		"github_run_cancel_failed":           cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_CANCEL_FAILED,
		"registry_challenge_invalid":         cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_REGISTRY_CHALLENGE_INVALID,
		"image_not_published":                cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_IMAGE_NOT_PUBLISHED,
		"image_digest_invalid":               cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_IMAGE_DIGEST_INVALID,
		"oci_annotation_mismatch":            cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_OCI_ANNOTATION_MISMATCH,
		"github_run_timed_out":               cyclopusv1.IntegrationFailureCode_INTEGRATION_FAILURE_CODE_GITHUB_RUN_TIMED_OUT,
	}
	if got, want := len(cases), len(cyclopusv1.IntegrationFailureCode_name)-1; got != want {
		t.Fatalf("mapping cases = %d, want one for each of %d specified enum values", got, want)
	}
	for stableCode, want := range cases {
		if got := integrationFailureCode(stableCode); got != want {
			t.Errorf("integrationFailureCode(%q) = %v, want %v", stableCode, got, want)
		}
	}
}

func TestGitHubAppNotConfiguredPlaceholdersUseFailedPrecondition(t *testing.T) {
	handler := &apiHandlers{}
	ctx := authenticatedServerContext(true)
	tests := map[string]func() error{
		"begin setup": func() error {
			_, err := handler.BeginGitHubAppSetup(ctx, &cyclopusv1.BeginGitHubAppSetupRequest{IdempotencyKey: "setup"})
			return err
		},
		"begin installation": func() error {
			_, err := handler.BeginGitHubInstallation(ctx, &cyclopusv1.BeginGitHubInstallationRequest{IdempotencyKey: "install"})
			return err
		},
		"refresh installation": func() error {
			_, err := handler.RefreshGitHubInstallation(ctx, &cyclopusv1.RefreshGitHubInstallationRequest{InstallationId: "42", IdempotencyKey: "refresh"})
			return err
		},
		"repository discovery": func() error {
			_, err := handler.ListGitHubRepositories(ctx, &cyclopusv1.ListGitHubRepositoriesRequest{InstallationId: "42"})
			return err
		},
		"workflow discovery": func() error {
			_, err := handler.ListGitHubRepositoryWorkflows(ctx, &cyclopusv1.ListGitHubRepositoryWorkflowsRequest{RepositoryId: 42})
			return err
		},
		"disconnect connection": func() error {
			_, err := handler.DisconnectGitConnection(ctx, &cyclopusv1.DisconnectGitConnectionRequest{GitConnectionId: "68f33e0d-dcd7-4ed1-b701-b2472ec94242", IdempotencyKey: "disconnect"})
			return err
		},
		"import repository": func() error {
			_, err := handler.ImportGitRepository(ctx, &cyclopusv1.ImportGitRepositoryRequest{RepositoryId: 42, GithubWorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Platform", ProjectSlug: "platform", ApplicationName: "API", ApplicationSlug: "api", HostnameLabel: "api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "import"})
			return err
		},
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			err := call()
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
			}
			for _, detail := range status.Convert(err).Details() {
				if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == "GITHUB_APP_NOT_CONFIGURED" {
					return
				}
			}
			t.Fatalf("error %v lacks GITHUB_APP_NOT_CONFIGURED reason", err)
		})
	}
}

func TestListGitHubRepositoryWorkflowsMapsMissingDiscoveryConfiguration(t *testing.T) {
	handler := &apiHandlers{service: service.New(nil)}
	_, err := handler.ListGitHubRepositoryWorkflows(authenticatedServerContext(true), &cyclopusv1.ListGitHubRepositoryWorkflowsRequest{RepositoryId: 42})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == "GITHUB_APP_NOT_CONFIGURED" {
			return
		}
	}
	t.Fatalf("error %v lacks GITHUB_APP_NOT_CONFIGURED reason", err)
}

func TestGitHubDiscoveryTypedErrors(t *testing.T) {
	for name, test := range map[string]struct {
		err    error
		code   codes.Code
		reason string
	}{
		"suspended":  {service.ErrGitHubInstallationSuspended, codes.FailedPrecondition, "GITHUB_INSTALLATION_SUSPENDED"},
		"deleted":    {service.ErrGitHubInstallationRemoved, codes.FailedPrecondition, "GITHUB_INSTALLATION_ACCESS_REMOVED"},
		"repository": {service.ErrGitHubRepositoryRemoved, codes.FailedPrecondition, "GITHUB_REPOSITORY_ACCESS_REMOVED"},
		"workflow":   {service.ErrWorkflowIncompatible, codes.FailedPrecondition, "WORKFLOW_INCOMPATIBLE"},
		"conflict":   {service.ErrConflict, codes.Aborted, "CONFLICT"},
		"rate limit": {&service.GitHubAPIError{Status: 403, RetryAfter: 2 * time.Minute, Err: errors.New("rate limited")}, codes.Unavailable, "GITHUB_API_UNAVAILABLE"},
	} {
		t.Run(name, func(t *testing.T) {
			err := githubDiscoveryError(test.err)
			if status.Code(err) != test.code {
				t.Fatalf("code = %v", status.Code(err))
			}
			for _, detail := range status.Convert(err).Details() {
				if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == test.reason {
					return
				}
			}
			t.Fatalf("error %v lacks %s", err, test.reason)
		})
	}
}

func TestHonestPlaceholdersReturnNoFakeResourcesOrURLs(t *testing.T) {
	handler := &apiHandlers{}
	ctx := authenticatedServerContext(true)
	setup, setupErr := handler.BeginGitHubAppSetup(ctx, &cyclopusv1.BeginGitHubAppSetupRequest{IdempotencyKey: "setup"})
	installation, installationErr := handler.BeginGitHubInstallation(ctx, &cyclopusv1.BeginGitHubInstallationRequest{IdempotencyKey: "install"})
	operation, importErr := handler.ImportGitRepository(ctx, &cyclopusv1.ImportGitRepositoryRequest{RepositoryId: 42, GithubWorkflowPath: ".github/workflows/deploy.yml", ProjectName: "Platform", ProjectSlug: "platform", ApplicationName: "API", ApplicationSlug: "api", HostnameLabel: "api", ImageRepository: "ghcr.io/acme/api", IdempotencyKey: "import"})
	if setup != nil || status.Code(setupErr) != codes.FailedPrecondition {
		t.Errorf("setup response = %#v, error = %v", setup, setupErr)
	}
	if installation != nil || status.Code(installationErr) != codes.FailedPrecondition {
		t.Errorf("installation response = %#v, error = %v", installation, installationErr)
	}
	if operation != nil || status.Code(importErr) != codes.FailedPrecondition {
		t.Errorf("import response = %#v, error = %v", operation, importErr)
	}
}

func authenticatedServerContext(operator bool) context.Context {
	ctx := context.WithValue(context.Background(), principalContextKey, internalauth.Principal{Kind: internalauth.CredentialKindSession, Operator: operator})
	return context.WithValue(ctx, actorContextKey, service.ActorContext{UserID: "user", OrgID: "org", Operator: operator})
}
