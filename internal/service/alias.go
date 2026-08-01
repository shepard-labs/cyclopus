package service

import (
	"context"

	"github.com/shepard-labs/cyclopus/internal/store"
)

// GetAlias returns a single alias org-scoped to the actor. It is a pure read:
// validate the actor, then delegate to the org-scoped store query, which makes
// a cross-org id indistinguishable from a missing one (ErrNotFound).
func (s *Service) GetAlias(ctx context.Context, actor ActorContext, id string) (store.Alias, error) {
	if err := validateActor(actor); err != nil {
		return store.Alias{}, err
	}
	return s.store.GetAlias(ctx, actor.OrgID, id)
}

// ListAliases returns the aliases for an environment, org-scoped to the actor.
// The store filters aliases by the actor's org, so an environment belonging to
// another org returns an empty list rather than leaking rows.
func (s *Service) ListAliases(ctx context.Context, actor ActorContext, environmentID string) ([]store.Alias, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListAliases(ctx, actor.OrgID, environmentID)
}
