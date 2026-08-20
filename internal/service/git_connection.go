package service

import (
	"context"
	"encoding/json"

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

func (s *Service) DisconnectGitConnection(ctx context.Context, actor ActorContext, key, id string) (store.GitConnection, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.GitConnection{}, err
	}
	request := struct{ ID string }{ID: id}
	var replay store.GitConnection
	claimed, err := s.claim(ctx, actor, "git_connection.disconnect", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	idem := s.completion(actor, "git_connection.disconnect", key, request, json.RawMessage(`{}`), 200)
	value, err := s.store.DisconnectGitConnection(ctx, store.DisconnectGitConnectionParams{OrgID: actor.OrgID, ID: id, Idempotency: idem, Audit: audit(actor)})
	return value, mapError(err)
}
