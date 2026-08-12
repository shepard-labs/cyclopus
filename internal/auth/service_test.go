package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
)

type fakeStore struct {
	user          store.User
	token         store.APIToken
	claimedUser   store.ClaimFirstUserParams
	claimCalls    int
	created       store.CreateAPITokenParams
	touched       int
	revoked       bool
	authorizedOrg string
	authorizedApp string
	claimed       bool
	idempotency   store.IdempotencyResult
}

func (f *fakeStore) UserByID(context.Context, string, string) (store.User, error) {
	if f.user.ID == "" {
		return store.User{}, store.ErrNotFound
	}
	return f.user, nil
}

func (f *fakeStore) UpdateUserProfile(_ context.Context, params store.UpdateUserProfileParams) (store.User, error) {
	f.user.Email = params.Email
	f.user.FirstName = params.FirstName
	f.user.LastName = params.LastName
	f.user.DisplayName = params.DisplayName
	f.user.AvatarKey = params.AvatarKey
	return f.user, nil
}

func (f *fakeStore) ChangeUserPassword(_ context.Context, params store.ChangeUserPasswordParams) error {
	if f.user.PasswordHash == nil || *f.user.PasswordHash != params.ExpectedPasswordHash {
		return store.ErrConflict
	}
	f.user.PasswordHash = &params.PasswordHash
	return nil
}

func (f *fakeStore) InstallationClaimed(context.Context) (bool, error) { return f.claimed, nil }

func (f *fakeStore) ClaimFirstUser(_ context.Context, params store.ClaimFirstUserParams) (store.User, error) {
	f.claimedUser = params
	f.claimCalls++
	f.claimed = true
	return f.user, nil
}

func (f *fakeStore) UserByEmail(context.Context, string) (store.User, error) {
	if f.user.ID == "" {
		return store.User{}, store.ErrNotFound
	}
	return f.user, nil
}

func (f *fakeStore) CreateAPIToken(_ context.Context, params store.CreateAPITokenParams) (store.APIToken, error) {
	f.created = params
	f.token.ID = "token-id"
	f.token.UserID = params.UserID
	f.token.OrgID = params.OrgID
	f.token.Kind = params.Kind
	f.token.Prefix = params.Prefix
	f.token.Hash = params.Hash
	f.token.Scopes = params.Scopes
	f.token.ExpiresAt = params.ExpiresAt
	return f.token, nil
}

func (f *fakeStore) ClaimIdempotency(context.Context, store.IdempotencyClaimParams) (store.IdempotencyResult, error) {
	if f.idempotency.Disposition == 0 {
		return store.IdempotencyResult{Disposition: store.IdempotencyClaimed}, nil
	}
	return f.idempotency, nil
}

func (f *fakeStore) AcceptAPIToken(ctx context.Context, params store.AcceptAPITokenParams) (store.APIToken, error) {
	return f.CreateAPIToken(ctx, params.Create)
}

func (f *fakeStore) APITokenByPrefix(context.Context, string) (store.APIToken, error) {
	if f.token.ID == "" {
		return store.APIToken{}, store.ErrNotFound
	}
	return f.token, nil
}

func (f *fakeStore) TouchAPIToken(context.Context, string, time.Time, time.Time) error {
	f.touched++
	return nil
}

func (f *fakeStore) RevokeAPIToken(context.Context, string, string, string, time.Time, *store.AuditEvent) error {
	f.revoked = true
	return nil
}

func (f *fakeStore) AuthorizeApp(_ context.Context, orgID, appID string) error {
	f.authorizedOrg, f.authorizedApp = orgID, appID
	return nil
}

func TestTokenLifecycleDoesNotPersistPlaintext(t *testing.T) {
	const appID = "00000000-0000-0000-0000-000000000003"
	scope, _ := DeployAppScope(appID)
	fake := &fakeStore{user: store.User{ID: "user-id", OrgID: "org-id", Email: "user@example.com"}}
	service, err := NewService(fake, []byte("0123456789abcdef0123456789abcdef"), []byte("bootstrap-0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	created, err := service.CreateToken(context.Background(), principalFromUser(fake.user), CreateTokenParams{
		Kind: CredentialKindAPI, Name: "deploy", Scopes: []string{scope},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.created.Hash == "" || fake.created.Hash == created.Token || len(fake.created.Hash) != 64 {
		t.Fatalf("stored token hash = %q", fake.created.Hash)
	}
	if fake.created.Prefix == created.Token || fake.created.Prefix != created.Prefix {
		t.Fatalf("stored prefix = %q, returned prefix = %q", fake.created.Prefix, created.Prefix)
	}
	fake.token.Email = fake.user.Email
	principal, err := service.AuthenticateToken(context.Background(), created.Token)
	if err != nil || principal.OrgID != fake.user.OrgID || !principal.CanDeployApp(appID) || fake.touched != 1 {
		t.Fatalf("AuthenticateToken() = %+v, %v, touches=%d", principal, err, fake.touched)
	}
	if _, err := service.AuthenticateToken(context.Background(), created.Token+"x"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("malformed token error = %v", err)
	}
	tampered := created.Token[:len(created.Token)-1] + "A"
	if tampered == created.Token {
		tampered = created.Token[:len(created.Token)-1] + "B"
	}
	if _, err := service.AuthenticateToken(context.Background(), tampered); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("tampered token error = %v", err)
	}
}

func TestAuthorizeDeployAppIsNonDisclosing(t *testing.T) {
	service, err := NewService(&fakeStore{}, []byte("0123456789abcdef0123456789abcdef"), []byte("bootstrap-0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	err = service.AuthorizeDeployApp(context.Background(), Principal{OrgID: "org"},
		"00000000-0000-0000-0000-000000000003")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("AuthorizeDeployApp() error = %v", err)
	}
}

func TestBootstrapRequiresTokenPersistsOperatorAndDisablesAfterClaim(t *testing.T) {
	const bootstrapToken = "bootstrap-0123456789abcdef0123456789abcdef"
	fake := &fakeStore{user: store.User{ID: "user-id", OrgID: "org-id", Email: "operator@example.com", Operator: true}}
	service, err := NewService(fake, []byte("0123456789abcdef0123456789abcdef"), []byte(bootstrapToken))
	if err != nil {
		t.Fatal(err)
	}
	service.passwordParams = PasswordParams{MemoryKiB: minMemoryKiB, Iterations: 1, Parallelism: 1, SaltBytes: 16}
	if _, err := service.ClaimFirstUser(context.Background(), fake.user.Email, "password", "wrong-token-0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong bootstrap token error = %v", err)
	}
	principal, err := service.ClaimFirstUser(context.Background(), fake.user.Email, "password", bootstrapToken)
	if err != nil || !principal.Operator {
		t.Fatalf("ClaimFirstUser() = %+v, %v", principal, err)
	}
	if fake.claimCalls != 1 || fake.claimedUser.PasswordHash == nil || *fake.claimedUser.PasswordHash == "password" {
		t.Fatalf("bootstrap persistence calls=%d params=%+v", fake.claimCalls, fake.claimedUser)
	}
	if _, err := service.ClaimFirstUser(context.Background(), "other@example.com", "password", bootstrapToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("reused bootstrap token error = %v", err)
	}
}

func TestBootstrapClaimedCheckPrecedesPasswordHashing(t *testing.T) {
	fake := &fakeStore{claimed: true}
	service, err := NewService(fake, []byte("0123456789abcdef0123456789abcdef"), []byte("bootstrap-0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	passwordTooLongToHash := string(make([]byte, maxPasswordBytes+1))
	_, err = service.ClaimFirstUser(context.Background(), "operator@example.com", passwordTooLongToHash,
		"bootstrap-0123456789abcdef0123456789abcdef")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("ClaimFirstUser() error = %v, want unauthenticated", err)
	}
	if fake.claimCalls != 0 {
		t.Fatalf("ClaimFirstUser store calls = %d, want 0", fake.claimCalls)
	}
}

func TestClaimFirstUserAssignsAvatar(t *testing.T) {
	const bootstrapToken = "bootstrap-0123456789abcdef0123456789abcdef"
	fake := &fakeStore{user: store.User{ID: "user-id", OrgID: "org-id", Email: "user@example.com"}}
	service, err := NewService(fake, []byte("0123456789abcdef0123456789abcdef"), []byte(bootstrapToken))
	if err != nil {
		t.Fatal(err)
	}
	service.avatarKey = func() (string, error) { return "21d7a04043", nil }
	if _, err := service.ClaimFirstUser(context.Background(), "user@example.com", "password", bootstrapToken); err != nil {
		t.Fatal(err)
	}
	if fake.claimedUser.AvatarKey != "21d7a04043" {
		t.Fatalf("claimed avatar = %q", fake.claimedUser.AvatarKey)
	}
}

func TestOperatorComesOnlyFromPersistedUserAndToken(t *testing.T) {
	const operatorEmail = "operator@example.com"
	passwordHash, err := HashPassword("password", PasswordParams{MemoryKiB: minMemoryKiB, Iterations: 1, Parallelism: 1, SaltBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{user: store.User{ID: "user-id", OrgID: "org-id", Email: operatorEmail,
		PasswordHash: &passwordHash, Operator: false}}
	service, err := NewService(fake, []byte("0123456789abcdef0123456789abcdef"), []byte("bootstrap-0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.AuthenticatePassword(context.Background(), operatorEmail, "password")
	if err != nil || principal.Operator {
		t.Fatalf("AuthenticatePassword() = %+v, %v", principal, err)
	}
	fake.token = store.APIToken{ID: "token-id", UserID: "user-id", OrgID: "org-id", Email: operatorEmail,
		Operator: true, Kind: CredentialKindAPI, Prefix: "AAAAAAAAAAAA", Hash: service.tokenDigest(make([]byte, tokenSecretBytes)),
		Scopes: []string{"deploy:app:00000000-0000-0000-0000-000000000003"}}
	plaintext := tokenEnvelopePrefix + fake.token.Prefix + "_" + base64.RawURLEncoding.EncodeToString(make([]byte, tokenSecretBytes))
	principal, err = service.AuthenticateToken(context.Background(), plaintext)
	if err != nil || !principal.Operator {
		t.Fatalf("AuthenticateToken() = %+v, %v", principal, err)
	}
}

func TestSessionTTLIsCapped(t *testing.T) {
	fake := &fakeStore{user: store.User{ID: "user-id", OrgID: "org-id", Email: "user@example.com"}}
	service, err := NewService(fake, []byte("0123456789abcdef0123456789abcdef"), []byte("bootstrap-0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	service.now = func() time.Time { return now }
	expires := now.Add(7*24*time.Hour + time.Second)
	_, err = service.CreateToken(context.Background(), principalFromUser(fake.user), CreateTokenParams{
		Kind: CredentialKindSession, Name: "session", ExpiresAt: &expires,
	})
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("CreateToken() error = %v", err)
	}
}
