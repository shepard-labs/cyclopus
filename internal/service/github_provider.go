package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const (
	importExternalTimeout = 2 * time.Minute
	importLeaseDuration   = 3 * time.Minute
)

var (
	ErrGitHubInstallationSuspended = errors.New("service: GitHub installation suspended")
	ErrGitHubInstallationRemoved   = errors.New("service: GitHub installation access removed")
	ErrGitHubRepositoryRemoved     = errors.New("service: GitHub repository access removed")
	ErrWorkflowIncompatible        = errors.New("service: GitHub workflow incompatible")
	ErrRegistryAuthRequired        = errors.New("service: registry authentication required")
)

type GitHubAPIError struct {
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *GitHubAPIError) Error() string {
	return fmt.Sprintf("GitHub API status %d: %v", e.Status, e.Err)
}
func (e *GitHubAPIError) Unwrap() error { return e.Err }

type GitHubRepositoryPage struct {
	Repositories  []store.GitHubRepository
	NextPageToken string
}
type GitHubWorkflow struct {
	Path, Name, State string
	Analysis          WorkflowAnalysis
}
type GitHubWorkflowPage struct {
	Workflows     []GitHubWorkflow
	NextPageToken string
}

type githubPageCursor struct {
	Version                                      int
	Kind, OrgID, Scope, InstallationID, Snapshot string
	Page, PageSize                               int
	IssuedAt, ExpiresAt                          int64
}

const githubCursorVersion = 1
const githubCursorTTL = 10 * time.Minute

func requireAdministrator(actor ActorContext) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if !actor.Operator {
		return ErrForbidden
	}
	return nil
}

func (s *Service) BeginGitHubAppSetup(ctx context.Context, actor ActorContext, key string) (GitHubFlow, error) {
	if s.github == nil {
		return GitHubFlow{}, ErrGitHubAppNotConfigured
	}
	return s.github.BeginAppSetup(ctx, actor, key)
}
func (s *Service) BeginGitHubInstallation(ctx context.Context, actor ActorContext, key string) (GitHubFlow, error) {
	if s.github == nil {
		return GitHubFlow{}, ErrGitHubAppNotConfigured
	}
	return s.github.BeginInstallation(ctx, actor, key)
}
func (s *Service) RefreshGitHubInstallation(ctx context.Context, actor ActorContext, key, id string) (store.GitHubInstallation, error) {
	if s.github == nil {
		return store.GitHubInstallation{}, ErrGitHubAppNotConfigured
	}
	numericID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || numericID <= 0 {
		return store.GitHubInstallation{}, ErrInvalidArgument
	}
	return s.github.RefreshInstallation(ctx, actor, key, numericID)
}
func (s *Service) GitHubManifest() ([]byte, error) {
	if s.github == nil {
		return nil, ErrGitHubAppNotConfigured
	}
	return s.github.Manifest()
}
func (s *Service) ValidateGitHubStart(ctx context.Context, actor ActorContext, state, kind string) error {
	if s.github == nil {
		return ErrGitHubAppNotConfigured
	}
	return s.github.ValidateStart(ctx, actor, state, kind)
}
func (s *Service) CheckpointGitHubCallback(ctx context.Context, state, kind string) (GitHubCallbackIdentity, error) {
	if s.github == nil {
		return GitHubCallbackIdentity{}, ErrGitHubAppNotConfigured
	}
	return s.github.CheckpointGitHubCallback(ctx, state, kind)
}
func (s *Service) CompleteGitHubManifest(ctx context.Context, actor ActorContext, state, code string) error {
	if s.github == nil {
		return ErrGitHubAppNotConfigured
	}
	return s.github.HandleManifestCallback(ctx, actor, state, code)
}
func (s *Service) CompleteGitHubInstallation(ctx context.Context, actor ActorContext, state string, id int64) (store.GitHubInstallation, error) {
	if s.github == nil {
		return store.GitHubInstallation{}, ErrGitHubAppNotConfigured
	}
	return s.github.HandleInstallationCallback(ctx, actor, state, id)
}
func (s *Service) GitHubInstallationURL() (string, error) {
	if s.github == nil {
		return "", ErrGitHubAppNotConfigured
	}
	return s.github.InstallationURL()
}
func (s *Service) GitHubFlowExpiry() time.Duration { return setupTTL }

func (s *Service) GetGitHubApp(ctx context.Context, actor ActorContext) (store.GitHubApp, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.GitHubApp{}, err
	}
	value, err := s.store.GetGitHubApp(ctx)
	if err == store.ErrNotFound {
		value, err = store.GitHubApp{State: "absent"}, nil
	}
	if err == nil && s.github != nil {
		value.WebhookURL = s.github.WebhookURL()
	}
	return value, mapError(err)
}
func (s *Service) GetGitHubInstallation(ctx context.Context, actor ActorContext, id string) (store.GitHubInstallation, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.GitHubInstallation{}, err
	}
	numericID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || numericID <= 0 {
		return store.GitHubInstallation{}, ErrInvalidArgument
	}
	value, err := s.store.GetGitHubInstallation(ctx, actor.OrgID, numericID)
	return value, mapError(err)
}
func (s *Service) ListGitHubInstallations(ctx context.Context, actor ActorContext) ([]store.GitHubInstallation, error) {
	if err := requireAdministrator(actor); err != nil {
		return nil, err
	}
	values, err := s.store.ListGitHubInstallations(ctx, actor.OrgID)
	return values, mapError(err)
}

func (s *Service) ListGitHubRepositories(ctx context.Context, actor ActorContext, installationID string, pageSize int, pageToken string) (GitHubRepositoryPage, error) {
	if err := requireAdministrator(actor); err != nil {
		return GitHubRepositoryPage{}, err
	}
	if s.githubClients == nil || len(s.githubCursorKey) < 32 {
		return GitHubRepositoryPage{}, ErrGitHubAppNotConfigured
	}
	installation, err := s.store.GetGitHubInstallationByID(ctx, actor.OrgID, installationID)
	if err != nil {
		return GitHubRepositoryPage{}, mapError(err)
	}
	if err := activeInstallation(installation); err != nil {
		return GitHubRepositoryPage{}, err
	}
	snapshot := githubInstallationSnapshot(installation)
	page, err := s.decodeGitHubCursor(pageToken, "repositories", actor.OrgID, installation.ID, installation.ID, snapshot, pageSize)
	if err != nil {
		return GitHubRepositoryPage{}, err
	}
	client, err := s.githubClients.Client(installation.GitHubInstallationID)
	if err != nil {
		return GitHubRepositoryPage{}, err
	}
	list, response, err := client.Apps.ListRepos(ctx, &gh.ListOptions{Page: page, PerPage: pageSize})
	if err != nil {
		return GitHubRepositoryPage{}, classifyGitHubAPIError(err)
	}
	values := make([]store.GitHubRepository, 0, len(list.Repositories))
	for _, repository := range list.Repositories {
		if repository.GetID() <= 0 || repository.GetNodeID() == "" || repository.GetOwner().GetLogin() == "" || repository.GetName() == "" || repository.GetFullName() == "" || repository.GetDefaultBranch() == "" {
			return GitHubRepositoryPage{}, ErrGitHubRepositoryRemoved
		}
		visibility := repository.GetVisibility()
		if visibility == "" {
			if repository.GetPrivate() {
				visibility = "private"
			} else {
				visibility = "public"
			}
		}
		values = append(values, store.GitHubRepository{GitHubRepositoryID: repository.GetID(), GitHubInstallationID: installation.GitHubInstallationID,
			OrgID: actor.OrgID, NodeID: repository.GetNodeID(), OwnerLogin: repository.GetOwner().GetLogin(), Name: repository.GetName(), FullName: repository.GetFullName(),
			Visibility: visibility, Archived: repository.GetArchived(), DefaultBranch: repository.GetDefaultBranch(), AccessState: "active"})
	}
	values, generation, err := s.store.UpsertGitHubRepositories(ctx, actor.OrgID, installation, values, audit(actor))
	if err != nil {
		return GitHubRepositoryPage{}, mapError(err)
	}
	next := ""
	if response != nil && response.NextPage > 0 {
		next = s.newGitHubCursor("repositories", actor.OrgID, installation.ID, installation.ID, githubRepositoryGenerationSnapshot(generation), response.NextPage, pageSize)
	}
	return GitHubRepositoryPage{Repositories: values, NextPageToken: next}, nil
}

func (s *Service) ListGitHubRepositoryWorkflows(ctx context.Context, actor ActorContext, repositoryID int64, pageSize int, pageToken string) (GitHubWorkflowPage, error) {
	if err := requireAdministrator(actor); err != nil {
		return GitHubWorkflowPage{}, err
	}
	if s == nil || s.githubClients == nil || len(s.githubCursorKey) < 32 {
		return GitHubWorkflowPage{}, ErrGitHubAppNotConfigured
	}
	repository, installation, client, err := s.authoritativeRepository(ctx, actor, repositoryID)
	if err != nil {
		return GitHubWorkflowPage{}, err
	}
	snapshot := githubInstallationSnapshot(installation) + ":" + githubRepositorySnapshot(repository)
	page, err := s.decodeGitHubCursor(pageToken, "workflows", actor.OrgID, strconv.FormatInt(repositoryID, 10), installation.ID, snapshot, pageSize)
	if err != nil {
		return GitHubWorkflowPage{}, err
	}
	owner, name := splitFullName(repository.FullName)
	workflows, response, err := client.Actions.ListWorkflows(ctx, owner, name, &gh.ListOptions{Page: page, PerPage: pageSize})
	if err != nil {
		return GitHubWorkflowPage{}, classifyGitHubAPIError(err)
	}
	values := make([]GitHubWorkflow, 0, len(workflows.Workflows))
	for _, workflow := range workflows.Workflows {
		state := workflow.GetState()
		if state != "active" {
			values = append(values, GitHubWorkflow{Path: workflow.GetPath(), Name: workflow.GetName(), State: state, Analysis: WorkflowAnalysis{Reasons: []string{"workflow_not_active"}}})
			continue
		}
		content, _, err := fetchWorkflowContent(ctx, client, owner, name, workflow.GetPath(), repository.DefaultBranch)
		if err != nil {
			if reason := workflowContentFailureReason(err); reason != "" {
				values = append(values, GitHubWorkflow{Path: workflow.GetPath(), Name: workflow.GetName(), State: state, Analysis: WorkflowAnalysis{Reasons: []string{reason}}})
				continue
			}
			if githubStatus(err) != http.StatusNotFound && !errors.Is(err, ErrWorkflowIncompatible) {
				return GitHubWorkflowPage{}, classifyGitHubAPIError(err)
			}
			values = append(values, GitHubWorkflow{Path: workflow.GetPath(), Name: workflow.GetName(), State: state, Analysis: WorkflowAnalysis{Reasons: []string{"workflow_content_unavailable"}}})
			continue
		}
		values = append(values, GitHubWorkflow{Path: workflow.GetPath(), Name: workflow.GetName(), State: state, Analysis: AnalyzeGitHubWorkflow(content, "")})
	}
	sort.SliceStable(values, func(i, j int) bool { return values[i].Path < values[j].Path })
	next := ""
	if response != nil && response.NextPage > 0 {
		next = s.newGitHubCursor("workflows", actor.OrgID, strconv.FormatInt(repositoryID, 10), installation.ID, snapshot, response.NextPage, pageSize)
	}
	return GitHubWorkflowPage{Workflows: values, NextPageToken: next}, nil
}

type ImportGitRepositoryInput struct {
	RepositoryID                                                                                                                               int64
	WorkflowPath, ProjectName, ProjectSlug, ApplicationName, ApplicationSlug, HostnameLabel, ProductionBranch, ImageRepository, IdempotencyKey string
	App                                                                                                                                        AppInput
	PrivateCredentialRequested                                                                                                                 bool
}

func (s *Service) ImportGitRepository(ctx context.Context, actor ActorContext, input ImportGitRepositoryInput) (store.ImportOperation, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.ImportOperation{}, err
	}
	if s == nil || s.store == nil {
		return store.ImportOperation{}, ErrGitHubAppNotConfigured
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" || input.RepositoryID <= 0 || !validWorkflowPath(input.WorkflowPath) {
		return store.ImportOperation{}, ErrInvalidArgument
	}
	if _, err := ParsePublicGHCRRepository(input.ImageRepository); err != nil {
		return store.ImportOperation{}, ErrInvalidArgument
	}
	if err := validateProject(ProjectInput{Name: input.ProjectName, Slug: input.ProjectSlug}); err != nil {
		return store.ImportOperation{}, err
	}
	request := struct {
		ActorUserID string
		Operator    bool
		Input       ImportGitRepositoryInput
	}{actor.UserID, actor.Operator, input}
	digest := requestHash(request)
	ownerLease, err := randomGeneration()
	if err != nil {
		return store.ImportOperation{}, err
	}
	claim := store.IdempotencyClaimParams{OrgID: actor.OrgID, Scope: "github.repository.import", Key: input.IdempotencyKey, RequestHash: digest, Owner: ownerLease, LeaseDuration: importLeaseDuration, Retention: idempotencyRetention}
	claimed, err := s.store.ClaimIdempotency(ctx, claim)
	if err != nil {
		return store.ImportOperation{}, mapError(err)
	}
	if claimed.Disposition == store.IdempotencyConflict {
		return store.ImportOperation{}, ErrConflict
	}
	if claimed.Disposition == store.IdempotencyInProgress {
		return store.ImportOperation{}, ErrInProgress
	}
	if claimed.Disposition == store.IdempotencyReplay {
		var operation store.ImportOperation
		if err := json.Unmarshal(claimed.Body, &operation); err != nil {
			return store.ImportOperation{}, fmt.Errorf("decode import replay: %w", err)
		}
		if claimed.Status == "failed" {
			return operation, importFailureError(claimed.ErrorCode)
		}
		return operation, nil
	}
	operationID := newID()
	fail := func(code string, cause error) (store.ImportOperation, error) {
		completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		completion := store.CompleteIdempotencyParams{OrgID: actor.OrgID, Scope: claim.Scope, Key: claim.Key, RequestHash: digest, Owner: ownerLease, Status: "failed", StatusCode: http.StatusUnprocessableEntity, ErrorCode: &code}
		operation, err := s.store.FailImportGitRepository(completionCtx, store.FailImportRepositoryParams{OperationID: operationID, OrgID: actor.OrgID, ActorUserID: actor.UserID,
			AuditID: newID(), IdempotencyKey: input.IdempotencyKey, RequestDigest: digest, RepositoryID: input.RepositoryID, FailureCode: code, FailureMessage: cause.Error(), Completion: completion})
		if err != nil {
			_ = s.store.AbandonIdempotency(completionCtx, claim)
			return store.ImportOperation{}, mapError(err)
		}
		return operation, cause
	}
	if input.PrivateCredentialRequested {
		return fail("registry_auth_required", ErrRegistryAuthRequired)
	}
	if s.githubClients == nil || len(s.githubCursorKey) < 32 {
		return fail("github_app_not_configured", ErrGitHubAppNotConfigured)
	}
	externalCtx, cancel := context.WithTimeout(ctx, importExternalTimeout)
	defer cancel()
	repository, installation, client, err := s.authoritativeRepository(externalCtx, actor, input.RepositoryID)
	if err != nil {
		return s.finishImportExternalError(ctx, claim, fail, err)
	}
	owner, name := splitFullName(repository.FullName)
	branch, headSHA, err := selectProductionBranch(externalCtx, client, owner, name, repository.DefaultBranch, input.ProductionBranch)
	if err != nil {
		return s.finishImportExternalError(ctx, claim, fail, err)
	}
	content, revision, err := fetchWorkflowContent(externalCtx, client, owner, name, input.WorkflowPath, headSHA)
	if err != nil {
		return s.finishImportExternalError(ctx, claim, fail, classifyGitHubAPIError(err))
	}
	if analysis := AnalyzeGitHubWorkflow(content, input.ImageRepository); !analysis.Compatible {
		return fail("workflow_incompatible", fmt.Errorf("%w: %s", ErrWorkflowIncompatible, strings.Join(analysis.Reasons, ",")))
	}
	appInput := applyAppCreateDefaults(normalizeAppInput(input.App))
	appInput.ProjectID = newID()
	appInput.Name, appInput.Slug, appInput.HostnameLabel = input.ApplicationName, input.ApplicationSlug, input.HostnameLabel
	appInput.ImageRepository, appInput.GitHubWorkflowPath, appInput.ProductionBranch = &input.ImageRepository, &input.WorkflowPath, branch
	runtimeClass := "gvisor"
	if appInput.RuntimeClass != nil {
		runtimeClass = *appInput.RuntimeClass
	}
	if err := validateApp(appInput, runtimeClass); err != nil {
		return fail("invalid_argument", err)
	}
	hostname, err := productionHostname(input.HostnameLabel, s.hostnameProvider)
	if err != nil {
		return fail("invalid_argument", ErrInvalidArgument)
	}
	if err := s.store.RenewIdempotency(ctx, claim); err != nil {
		return store.ImportOperation{}, mapError(err)
	}
	projectID, appID, environmentID := newID(), newID(), newID()
	app := appValue(actor.OrgID, appID, appInput, runtimeClass)
	app.ProjectID, app.GitHubWorkflowRevision, app.PreviewsEnabled = projectID, revision, input.App.PreviewsEnabled
	project := store.Project{ID: projectID, OrgID: actor.OrgID, Name: input.ProjectName, Slug: input.ProjectSlug}
	plannedHostname := hostname
	environment := store.Environment{ID: environmentID, OrgID: actor.OrgID, AppID: appID, Branch: branch, Kind: "production", Namespace: input.HostnameLabel + "-production", PlannedHostname: &plannedHostname, LifecycleStatus: "reserved"}
	completion := store.CompleteIdempotencyParams{OrgID: actor.OrgID, Scope: claim.Scope, Key: claim.Key, RequestHash: digest, Status: "completed", StatusCode: http.StatusOK}
	audits := []string{newID(), newID(), newID(), newID(), newID()}
	operation, result, err := s.store.ImportGitRepository(ctx, store.ImportRepositoryParams{OperationID: operationID, OrgID: actor.OrgID, ActorUserID: actor.UserID,
		IdempotencyKey: input.IdempotencyKey, RequestDigest: digest, RepositoryID: input.RepositoryID, Project: project, App: app, Environment: environment,
		ExpectedInstallationID: installation.ID, ExpectedGitHubInstallationID: installation.GitHubInstallationID, ExpectedInstallationGeneration: installation.RepositoryReconciliationGeneration,
		ExpectedRepositoryNodeID: repository.NodeID, ExpectedRepositoryGeneration: repository.ReconciliationGeneration,
		WorkflowRevision: revision, BranchHeadSHA: headSHA, Idempotency: claim, Completion: completion, AuditIDs: audits})
	if err != nil {
		if isDeterministicStoreImportError(err) {
			mapped := deterministicStoreImportError(err)
			return fail(importFailureCode(mapped), mapped)
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		if abandonErr := s.store.AbandonIdempotency(cleanupCtx, claim); abandonErr != nil && !errors.Is(abandonErr, store.ErrLeaseNotOwned) {
			return store.ImportOperation{}, mapError(abandonErr)
		}
		return store.ImportOperation{}, mapError(err)
	}
	switch result.Disposition {
	case store.IdempotencyConflict:
		return store.ImportOperation{}, ErrConflict
	case store.IdempotencyInProgress:
		return store.ImportOperation{}, ErrInProgress
	}
	return operation, nil
}

func (s *Service) finishImportExternalError(ctx context.Context, claim store.IdempotencyClaimParams, fail func(string, error) (store.ImportOperation, error), err error) (store.ImportOperation, error) {
	if isDeterministicImportError(err) {
		return fail(importFailureCode(err), err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if abandonErr := s.store.AbandonIdempotency(cleanupCtx, claim); abandonErr != nil && !errors.Is(abandonErr, store.ErrLeaseNotOwned) {
		return store.ImportOperation{}, mapError(abandonErr)
	}
	return store.ImportOperation{}, err
}

func isDeterministicImportError(err error) bool {
	return errors.Is(err, ErrGitHubRepositoryRemoved) || errors.Is(err, ErrGitHubInstallationRemoved) || errors.Is(err, ErrGitHubInstallationSuspended) || errors.Is(err, ErrWorkflowIncompatible) || errors.Is(err, ErrInvalidArgument)
}

func isDeterministicStoreImportError(err error) bool {
	return errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrInvalidArgument) || errors.Is(err, store.ErrFailedPrecondition) || errors.Is(err, store.ErrConflict)
}

func deterministicStoreImportError(err error) error {
	if errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrConflict) {
		return ErrConflict
	}
	return mapError(err)
}

func importFailureCode(err error) string {
	switch {
	case errors.Is(err, ErrGitHubRepositoryRemoved):
		return "github_repository_access_removed"
	case errors.Is(err, ErrGitHubInstallationRemoved):
		return "github_installation_access_removed"
	case errors.Is(err, ErrGitHubInstallationSuspended):
		return "github_installation_suspended"
	case errors.Is(err, ErrWorkflowIncompatible):
		return "workflow_incompatible"
	case errors.Is(err, store.ErrAlreadyExists), errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, store.ErrFailedPrecondition):
		return "failed_precondition"
	default:
		return "invalid_argument"
	}
}

func importFailureError(code *string) error {
	if code == nil {
		return ErrInvalidArgument
	}
	switch *code {
	case "github_app_not_configured":
		return ErrGitHubAppNotConfigured
	case "github_repository_access_removed":
		return ErrGitHubRepositoryRemoved
	case "github_installation_access_removed":
		return ErrGitHubInstallationRemoved
	case "github_installation_suspended":
		return ErrGitHubInstallationSuspended
	case "workflow_incompatible":
		return ErrWorkflowIncompatible
	case "registry_auth_required":
		return ErrRegistryAuthRequired
	case "conflict":
		return ErrConflict
	case "failed_precondition":
		return store.ErrFailedPrecondition
	default:
		return ErrInvalidArgument
	}
}

// ParsePublicGHCRRepository validates and returns a canonical public GHCR
// repository name. It intentionally never normalizes caller input.
func ParsePublicGHCRRepository(value string) (string, error) {
	if len(value) == 0 || len(value) > 255 || !strings.HasPrefix(value, "ghcr.io/") || strings.TrimSpace(value) != value {
		return "", ErrInvalidArgument
	}
	path := strings.TrimPrefix(value, "ghcr.io/")
	components := strings.Split(path, "/")
	if len(components) < 2 {
		return "", ErrInvalidArgument
	}
	component := regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_{1,2}|-+)[a-z0-9]+)*$`)
	for _, part := range components {
		if part == "" || part == "." || part == ".." || !component.MatchString(part) {
			return "", ErrInvalidArgument
		}
	}
	return value, nil
}

func (s *Service) GetImportOperation(ctx context.Context, actor ActorContext, id string) (store.ImportOperation, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.ImportOperation{}, err
	}
	value, err := s.store.GetImportOperation(ctx, actor.OrgID, id)
	return value, mapError(err)
}
func (s *Service) GetGhcrCredentialStatus(ctx context.Context, actor ActorContext) (store.Registry, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.Registry{}, err
	}
	value, err := s.store.GetGHCRRegistry(ctx, actor.OrgID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Registry{State: "absent"}, nil
	}
	if err != nil {
		return store.Registry{}, mapError(err)
	}
	return value, nil
}

func (s *Service) authoritativeRepository(ctx context.Context, actor ActorContext, repositoryID int64) (store.GitHubRepository, store.GitHubInstallation, *gh.Client, error) {
	repository, err := s.store.GetGitHubRepository(ctx, actor.OrgID, repositoryID)
	if err != nil {
		return repository, store.GitHubInstallation{}, nil, mapError(err)
	}
	installation, err := s.store.GetGitHubInstallation(ctx, actor.OrgID, repository.GitHubInstallationID)
	if err != nil {
		return repository, installation, nil, mapError(err)
	}
	if err := activeInstallation(installation); err != nil {
		return repository, installation, nil, err
	}
	if repository.AccessState != "active" || repository.Archived {
		return repository, installation, nil, ErrGitHubRepositoryRemoved
	}
	client, err := s.githubClients.Client(installation.GitHubInstallationID)
	if err != nil {
		return repository, installation, nil, err
	}
	authoritative, _, err := client.Repositories.GetByID(ctx, repositoryID)
	if err != nil {
		if githubStatus(err) == http.StatusNotFound {
			return repository, installation, nil, ErrGitHubRepositoryRemoved
		}
		return repository, installation, nil, classifyGitHubAPIError(err)
	}
	if authoritative.GetID() != repositoryID || authoritative.GetNodeID() == "" || authoritative.GetNodeID() != repository.NodeID || authoritative.GetOwner().GetLogin() == "" || authoritative.GetName() == "" || authoritative.GetFullName() == "" || authoritative.GetDefaultBranch() == "" {
		return repository, installation, nil, ErrGitHubRepositoryRemoved
	}
	visibility := authoritative.GetVisibility()
	if visibility == "" {
		visibility = "public"
		if authoritative.GetPrivate() {
			visibility = "private"
		}
	}
	refreshed, _, err := s.store.UpsertGitHubRepositories(ctx, actor.OrgID, installation, []store.GitHubRepository{{GitHubRepositoryID: repositoryID, NodeID: authoritative.GetNodeID(), OwnerLogin: authoritative.GetOwner().GetLogin(), Name: authoritative.GetName(), FullName: authoritative.GetFullName(), Visibility: visibility, Archived: authoritative.GetArchived(), DefaultBranch: authoritative.GetDefaultBranch(), AccessState: "active"}}, audit(actor))
	if err != nil || len(refreshed) != 1 {
		return repository, installation, nil, mapError(err)
	}
	installation, err = s.store.GetGitHubInstallation(ctx, actor.OrgID, refreshed[0].GitHubInstallationID)
	if err != nil {
		return repository, installation, nil, mapError(err)
	}
	if installation.ID != refreshed[0].InstallationID {
		return repository, installation, nil, store.ErrFailedPrecondition
	}
	if err := activeInstallation(installation); err != nil {
		return repository, installation, nil, err
	}
	return refreshed[0], installation, client, nil
}

func activeInstallation(value store.GitHubInstallation) error {
	if value.AccessState == "suspended" {
		return ErrGitHubInstallationSuspended
	}
	if value.AccessState != "active" {
		return ErrGitHubInstallationRemoved
	}
	return nil
}
func splitFullName(value string) (string, string) {
	owner, name, _ := strings.Cut(value, "/")
	return owner, name
}
func fetchWorkflowContent(ctx context.Context, client *gh.Client, owner, repository, path, ref string) ([]byte, string, error) {
	file, _, _, err := client.Repositories.GetContents(ctx, owner, repository, path, &gh.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, "", err
	}
	if file == nil || file.GetSHA() == "" {
		return nil, "", ErrWorkflowIncompatible
	}
	if file.GetSize() > maxWorkflowBytes {
		return nil, "", fmt.Errorf("%w: workflow_too_large", ErrWorkflowIncompatible)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, "", fmt.Errorf("%w: workflow_yaml_invalid", ErrWorkflowIncompatible)
	}
	if len(content) > maxWorkflowBytes {
		return nil, "", fmt.Errorf("%w: workflow_too_large", ErrWorkflowIncompatible)
	}
	return []byte(content), strings.ToLower(file.GetSHA()), nil
}

func workflowContentFailureReason(err error) string {
	if !errors.Is(err, ErrWorkflowIncompatible) {
		return ""
	}
	for _, reason := range []string{"workflow_too_large", "workflow_yaml_invalid"} {
		if strings.Contains(err.Error(), reason) {
			return reason
		}
	}
	return "workflow_content_unavailable"
}
func selectProductionBranch(ctx context.Context, client *gh.Client, owner, repo, defaultBranch, explicit string) (string, string, error) {
	candidates := []string{"main", "master", defaultBranch}
	if explicit != "" {
		normalized := strings.TrimPrefix(explicit, "refs/heads/")
		if normalized == "" || strings.Contains(normalized, "..") || strings.HasPrefix(normalized, "/") {
			return "", "", ErrInvalidArgument
		}
		candidates = []string{normalized}
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		branch, _, err := client.Repositories.GetBranch(ctx, owner, repo, candidate, 0)
		if err != nil {
			if githubStatus(err) == http.StatusNotFound {
				continue
			}
			return "", "", classifyGitHubAPIError(err)
		}
		sha := strings.ToLower(branch.GetCommit().GetSHA())
		if !commitPattern.MatchString(sha) {
			return "", "", ErrGitHubRepositoryRemoved
		}
		return candidate, sha, nil
	}
	return "", "", ErrGitHubRepositoryRemoved
}
func (s *Service) encodeGitHubCursor(cursor githubPageCursor) string {
	raw, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, s.githubCursorKey)
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(append(raw, mac.Sum(nil)...))
}
func (s *Service) newGitHubCursor(kind, orgID, scope, installationID, snapshot string, page, pageSize int) string {
	now := s.now().UTC()
	return s.encodeGitHubCursor(githubPageCursor{Version: githubCursorVersion, Kind: kind, OrgID: orgID, Scope: scope,
		InstallationID: installationID, Snapshot: snapshot, Page: page, PageSize: pageSize, IssuedAt: now.Unix(), ExpiresAt: now.Add(githubCursorTTL).Unix()})
}
func (s *Service) decodeGitHubCursor(token, kind, orgID, scope, installationID, snapshot string, pageSize int) (int, error) {
	if token == "" {
		return 1, nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(encoded) <= sha256.Size {
		return 0, ErrInvalidArgument
	}
	raw, sig := encoded[:len(encoded)-sha256.Size], encoded[len(encoded)-sha256.Size:]
	mac := hmac.New(sha256.New, s.githubCursorKey)
	_, _ = mac.Write(raw)
	var cursor githubPageCursor
	now := s.now().Unix()
	if !hmac.Equal(sig, mac.Sum(nil)) || json.Unmarshal(raw, &cursor) != nil || cursor.Version != githubCursorVersion || cursor.Kind != kind || cursor.OrgID != orgID || cursor.Scope != scope ||
		cursor.InstallationID != installationID || cursor.Snapshot != snapshot || cursor.Page < 1 || cursor.PageSize != pageSize || cursor.IssuedAt <= 0 || cursor.ExpiresAt <= cursor.IssuedAt || now < cursor.IssuedAt || now >= cursor.ExpiresAt {
		return 0, ErrInvalidArgument
	}
	return cursor.Page, nil
}

func githubInstallationSnapshot(value store.GitHubInstallation) string {
	return githubRepositoryGenerationSnapshot(value.RepositoryReconciliationGeneration)
}

func githubRepositoryGenerationSnapshot(generation int64) string {
	return strconv.FormatInt(generation, 10)
}

func githubRepositorySnapshot(value store.GitHubRepository) string {
	return strconv.FormatInt(value.UpdatedAt.UTC().UnixNano(), 10) + ":" + value.AccessState
}
func githubStatus(err error) int {
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		return response.Response.StatusCode
	}
	if strings.Contains(err.Error(), "404 Not Found") {
		return http.StatusNotFound
	}
	return 0
}
func classifyGitHubAPIError(err error) error {
	status := githubStatus(err)
	retry := time.Duration(0)
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) {
		retry = abuse.GetRetryAfter()
	}
	var rate *gh.RateLimitError
	if errors.As(err, &rate) {
		if rate.Response != nil {
			status = rate.Response.StatusCode
		}
		retry = time.Until(rate.Rate.Reset.Time)
	}
	return &GitHubAPIError{Status: status, RetryAfter: retry, Err: err}
}
