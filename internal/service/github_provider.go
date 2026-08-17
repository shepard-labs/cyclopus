package service

import (
	"context"
	"strconv"

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

func (s *Service) GetGitHubApp(ctx context.Context, actor ActorContext) (store.GitHubApp, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.GitHubApp{}, err
	}
	value, err := s.store.GetGitHubApp(ctx)
	if err == store.ErrNotFound {
		return store.GitHubApp{State: "absent"}, nil
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
