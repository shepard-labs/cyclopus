package service

import (
	"context"
	"regexp"
	"strings"

	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/store"
)

// gitConnectionProvider is the only allowed value for git_connections.provider
// in v1; validated in the service layer so a bad value returns ErrInvalidArgument
// (400) rather than a 500 from the DB CHECK.
const gitConnectionProvider = "github"

// repoFullNamePattern mirrors the git_connections.repo_full_name CHECK:
// "owner/repo" where each side is 1-100 chars with no slashes or whitespace.
var repoFullNamePattern = regexp.MustCompile(`^[^/\s]{1,100}/[^/\s]{1,100}$`)

// GitConnectionInput is the caller-provided data for CreateGitConnection.
// WebhookSecretRef is a k8s-secret://…#… REFERENCE — never a raw secret.
type GitConnectionInput struct {
	Provider         string
	InstallationID   int64
	RepoFullName     string
	DefaultBranch    string
	WebhookSecretRef string
}

func (s *Service) GetGitConnection(ctx context.Context, actor ActorContext, id string) (store.GitConnection, error) {
	if err := validateActor(actor); err != nil {
		return store.GitConnection{}, err
	}
	return s.store.GetGitConnection(ctx, actor.OrgID, id)
}

func (s *Service) ListGitConnections(ctx context.Context, actor ActorContext) ([]store.GitConnection, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListGitConnections(ctx, actor.OrgID)
}

// CreateGitConnection registers a GitHub App installation for the actor's org.
// The provider, installation_id, repo_full_name and default_branch are validated
// here, and WebhookSecretRef is validated as a k8s-secret:// reference (never a
// raw secret) before insert.
func (s *Service) CreateGitConnection(ctx context.Context, actor ActorContext, key string, input GitConnectionInput) (store.GitConnection, error) {
	if err := validateActor(actor); err != nil {
		return store.GitConnection{}, err
	}
	input = normalizeGitConnectionInput(input)
	if err := validateGitConnection(input); err != nil {
		return store.GitConnection{}, err
	}
	return store.GitConnection{}, store.ErrFailedPrecondition
}

func (s *Service) DeleteGitConnection(ctx context.Context, actor ActorContext, key, id string) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	return store.ErrFailedPrecondition
}

// normalizeGitConnectionInput applies the table defaults so a caller can omit
// provider ("github") and default_branch ("main"), matching the DB DEFAULTs.
func normalizeGitConnectionInput(input GitConnectionInput) GitConnectionInput {
	if input.Provider == "" {
		input.Provider = gitConnectionProvider
	}
	if input.DefaultBranch == "" {
		input.DefaultBranch = "main"
	}
	return input
}

// validateGitConnection enforces the git_connections invariants in the service
// layer: provider must be "github", installation_id positive, repo_full_name
// well-formed, default_branch non-empty within the length bound, and a
// well-formed k8s-secret:// webhook secret reference. It NEVER accepts a raw
// secret — only the secret reference string.
func validateGitConnection(input GitConnectionInput) error {
	if input.Provider != gitConnectionProvider {
		return ErrInvalidArgument
	}
	if input.InstallationID <= 0 {
		return ErrInvalidArgument
	}
	if !repoFullNamePattern.MatchString(input.RepoFullName) {
		return ErrInvalidArgument
	}
	branch := strings.TrimSpace(input.DefaultBranch)
	if branch == "" || len(input.DefaultBranch) > 255 || branch != input.DefaultBranch {
		return ErrInvalidArgument
	}
	if _, err := secretref.Parse(input.WebhookSecretRef); err != nil {
		return ErrInvalidArgument
	}
	return nil
}
