package service

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/cyclopus-dev/cyclopus/internal/secretref"
	"github.com/cyclopus-dev/cyclopus/internal/store"
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
	var replay store.GitConnection
	claimed, err := s.claim(ctx, actor, "git_connection.create", key, input, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	value := store.GitConnection{ID: newID(), OrgID: actor.OrgID, Provider: input.Provider, InstallationID: input.InstallationID,
		RepoFullName: input.RepoFullName, DefaultBranch: input.DefaultBranch, WebhookSecretRef: input.WebhookSecretRef}
	body, _ := json.Marshal(value)
	idem := s.completion(actor, "git_connection.create", key, input, body, 201)
	value, err = s.store.CreateGitConnection(ctx, store.SaveGitConnectionParams{GitConnection: value, Idempotency: idem, Audit: audit(actor)})
	return value, mapError(err)
}

func (s *Service) DeleteGitConnection(ctx context.Context, actor ActorContext, key, id string) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if err := validateHostnameID(id); err != nil {
		return err
	}
	request := struct{ ID string }{id}
	var replay struct{}
	claimed, err := s.claim(ctx, actor, "git_connection.delete", key, request, &replay)
	if err != nil || !claimed {
		return err
	}
	idem := s.completion(actor, "git_connection.delete", key, request, json.RawMessage(`{}`), 200)
	idem.OrgID = actor.OrgID
	return mapError(s.store.DeleteGitConnection(ctx, actor.OrgID, id, idem, audit(actor)))
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
