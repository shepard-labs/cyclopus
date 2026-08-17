package service

import (
	"context"
	"strconv"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
)

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
		value = store.GitHubApp{State: "absent"}
		err = nil
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

func (s *Service) ListGitHubRepositories(ctx context.Context, actor ActorContext, installationID string) ([]store.GitHubRepository, error) {
	if err := requireAdministrator(actor); err != nil {
		return nil, err
	}
	numericID, err := strconv.ParseInt(installationID, 10, 64)
	if err != nil || numericID <= 0 {
		return nil, ErrInvalidArgument
	}
	values, err := s.store.ListGitHubRepositories(ctx, actor.OrgID, numericID)
	return values, mapError(err)
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
	values, err := s.store.ListRegistries(ctx, actor.OrgID)
	if err != nil {
		return store.Registry{}, mapError(err)
	}
	if len(values) == 0 {
		return store.Registry{State: "absent"}, nil
	}
	return values[0], nil
}
