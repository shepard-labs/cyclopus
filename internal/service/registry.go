package service

import (
	"context"

	"github.com/shepard-labs/cyclopus/internal/store"
)

func (s *Service) GetRegistry(ctx context.Context, actor ActorContext, id string) (store.Registry, error) {
	if err := validateActor(actor); err != nil {
		return store.Registry{}, err
	}
	return s.store.GetRegistry(ctx, actor.OrgID, id)
}

func (s *Service) ListRegistries(ctx context.Context, actor ActorContext) ([]store.Registry, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListRegistries(ctx, actor.OrgID)
}
