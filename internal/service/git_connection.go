package service

import (
	"context"

	"github.com/shepard-labs/cyclopus/internal/store"
)

func (s *Service) GetGitConnection(ctx context.Context, actor ActorContext, id string) (store.GitConnection, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.GitConnection{}, err
	}
	return s.store.GetGitConnection(ctx, actor.OrgID, id)
}

func (s *Service) ListGitConnections(ctx context.Context, actor ActorContext) ([]store.GitConnection, error) {
	if err := requireAdministrator(actor); err != nil {
		return nil, err
	}
	return s.store.ListGitConnections(ctx, actor.OrgID)
}
