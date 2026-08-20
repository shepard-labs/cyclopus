package ui

import (
	"context"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/store"
)

type actorAuthStore struct{ token store.APIToken }

func (s *actorAuthStore) ClaimFirstUser(context.Context, store.ClaimFirstUserParams) (store.User, error) {
	return store.User{}, store.ErrNotFound
}
func (s *actorAuthStore) UserByEmail(context.Context, string) (store.User, error) {
	return store.User{}, store.ErrNotFound
}
func (s *actorAuthStore) UserByID(context.Context, string, string) (store.User, error) {
	return store.User{}, store.ErrNotFound
}
func (s *actorAuthStore) UpdateUserProfile(context.Context, store.UpdateUserProfileParams) (store.User, error) {
	return store.User{}, store.ErrNotFound
}
func (s *actorAuthStore) ChangeUserPassword(context.Context, store.ChangeUserPasswordParams) error {
	return store.ErrNotFound
}
func (s *actorAuthStore) CreateAPIToken(_ context.Context, params store.CreateAPITokenParams) (store.APIToken, error) {
	s.token = store.APIToken{ID: "session-id", UserID: params.UserID, OrgID: params.OrgID, Kind: params.Kind, Prefix: params.Prefix, Hash: params.Hash, Scopes: params.Scopes, ExpiresAt: params.ExpiresAt}
	return s.token, nil
}
func (s *actorAuthStore) ClaimIdempotency(context.Context, store.IdempotencyClaimParams) (store.IdempotencyResult, error) {
	return store.IdempotencyResult{}, store.ErrNotFound
}
func (s *actorAuthStore) AcceptAPIToken(context.Context, store.AcceptAPITokenParams) (store.APIToken, error) {
	return store.APIToken{}, store.ErrNotFound
}
func (s *actorAuthStore) APITokenByPrefix(context.Context, string) (store.APIToken, error) {
	return s.token, nil
}
func (s *actorAuthStore) TouchAPIToken(context.Context, string, time.Time, time.Time) error {
	return nil
}
func (s *actorAuthStore) RevokeAPIToken(context.Context, string, string, string, time.Time, *store.AuditEvent) error {
	return nil
}
func (s *actorAuthStore) AuthorizeApp(context.Context, string, string) error { return nil }
func (s *actorAuthStore) InstallationClaimed(context.Context) (bool, error)  { return true, nil }

func TestActorCarriesAuthenticatedSessionTokenID(t *testing.T) {
	data := &actorAuthStore{}
	authService, err := auth.NewService(data, []byte("0123456789abcdef0123456789abcdef"), []byte("bootstrap-0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	created, err := authService.CreateToken(t.Context(), auth.Principal{UserID: "user", OrgID: "org", Operator: true}, auth.CreateTokenParams{Kind: auth.CredentialKindSession, Name: "test", ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	data.token.Email, data.token.Operator = "admin@example.com", true
	actor, err := NewBackend(nil, authService, 0).actor(t.Context(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	if actor.SessionID == "" || actor.SessionID != data.token.ID {
		t.Fatalf("SessionID = %q, token ID = %q", actor.SessionID, data.token.ID)
	}
}
