package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/google/uuid"
)

const (
	CredentialKindSession = "session"
	CredentialKindAPI     = "api"
	tokenPrefixBytes      = 9
	tokenSecretBytes      = 32
	tokenPrefixLength     = 12
	tokenSecretLength     = 43
	tokenEnvelopePrefix   = "cyp_v1_"
	lastUsedTouchInterval = 5 * time.Minute
	maxSessionTTL         = 7 * 24 * time.Hour
	dummyPasswordHash     = "$argon2id$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

var (
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	ErrNotFound        = errors.New("auth: not found")
)

type Store interface {
	ClaimFirstUser(context.Context, store.ClaimFirstUserParams) (store.User, error)
	UserByEmail(context.Context, string) (store.User, error)
	CreateAPIToken(context.Context, store.CreateAPITokenParams) (store.APIToken, error)
	ClaimIdempotency(context.Context, store.IdempotencyClaimParams) (store.IdempotencyResult, error)
	AcceptAPIToken(context.Context, store.AcceptAPITokenParams) (store.APIToken, error)
	APITokenByPrefix(context.Context, string) (store.APIToken, error)
	TouchAPIToken(context.Context, string, time.Time, time.Time) error
	RevokeAPIToken(context.Context, string, string, string, time.Time, *store.AuditEvent) error
	AuthorizeApp(context.Context, string, string) error
	InstallationClaimed(context.Context) (bool, error)
}

type GitHubIdentity struct {
	UserID int64
	Email  string
}

type GitHubOAuth interface {
	Exchange(context.Context, string) (GitHubIdentity, error)
}

type Service struct {
	store            Store
	pepper           []byte
	passwordParams   PasswordParams
	now              func() time.Time
	bootstrapMu      sync.RWMutex
	bootstrapDigest  [sha256.Size]byte
	bootstrapEnabled bool
}

func NewService(dataStore Store, pepper, bootstrapSecret []byte) (*Service, error) {
	if dataStore == nil {
		return nil, errors.New("auth store is required")
	}
	if len(pepper) < 32 {
		return nil, errors.New("token pepper must contain at least 32 bytes")
	}
	if len(bootstrapSecret) < 32 {
		return nil, errors.New("bootstrap secret must contain at least 32 bytes")
	}
	digest := sha256.Sum256(bootstrapSecret)
	return &Service{store: dataStore, pepper: append([]byte(nil), pepper...),
		passwordParams: DefaultPasswordParams(), now: time.Now, bootstrapDigest: digest, bootstrapEnabled: true}, nil
}

type Principal struct {
	UserID   string
	OrgID    string
	Email    string
	Scopes   []string
	TokenID  string
	Kind     string
	Operator bool
}

func (p Principal) CanDeployApp(appID string) bool {
	scope, err := DeployAppScope(appID)
	if err != nil {
		return false
	}
	for _, candidate := range p.Scopes {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(scope)) == 1 {
			return true
		}
	}
	return false
}

func (s *Service) ClaimFirstUser(ctx context.Context, email, password, bootstrapToken string) (Principal, error) {
	claimed, err := s.store.InstallationClaimed(ctx)
	if err != nil {
		return Principal{}, err
	}
	if claimed || !s.bootstrapValid(bootstrapToken) {
		if claimed {
			s.disableBootstrap()
		}
		return Principal{}, ErrUnauthenticated
	}
	email = normalizeEmail(email)
	if email == "" {
		return Principal{}, ErrInvalidCredential
	}
	hash, err := HashPassword(password, s.passwordParams)
	if err != nil {
		return Principal{}, err
	}
	user, err := s.store.ClaimFirstUser(ctx, store.ClaimFirstUserParams{Email: email, PasswordHash: &hash,
		Audit: authAudit(nil)})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.disableBootstrap()
		}
		return Principal{}, err
	}
	s.disableBootstrap()
	return principalFromUser(user), nil
}

func (s *Service) bootstrapValid(token string) bool {
	s.bootstrapMu.RLock()
	enabled, expected := s.bootstrapEnabled, s.bootstrapDigest
	s.bootstrapMu.RUnlock()
	actual := sha256.Sum256([]byte(token))
	return enabled && subtle.ConstantTimeCompare(actual[:], expected[:]) == 1
}

func (s *Service) disableBootstrap() {
	s.bootstrapMu.Lock()
	s.bootstrapEnabled = false
	for i := range s.bootstrapDigest {
		s.bootstrapDigest[i] = 0
	}
	s.bootstrapMu.Unlock()
}

func (s *Service) AuthenticatePassword(ctx context.Context, email, password string) (Principal, error) {
	user, err := s.store.UserByEmail(ctx, normalizeEmail(email))
	if err != nil || user.PasswordHash == nil {
		_, _ = VerifyPassword(password, dummyPasswordHash)
		return Principal{}, ErrUnauthenticated
	}
	valid, err := VerifyPassword(password, *user.PasswordHash)
	if err != nil || !valid {
		return Principal{}, ErrUnauthenticated
	}
	return principalFromUser(user), nil
}

type CreateTokenParams struct {
	Kind        string
	Name        string
	Scopes      []string
	ExpiresAt   *time.Time
	AuditAction string
}

type CreatedToken struct {
	ID        string
	Token     string
	Prefix    string
	Scopes    []string
	ExpiresAt *time.Time
}

func (s *Service) CreateToken(ctx context.Context, principal Principal, params CreateTokenParams) (CreatedToken, error) {
	if params.Kind == "" {
		params.Kind = CredentialKindAPI
	}
	if err := s.validateToken(ctx, principal, params); err != nil {
		return CreatedToken{}, err
	}
	prefix, secret, plaintext, err := generateToken()
	if err != nil {
		return CreatedToken{}, err
	}
	digest := s.tokenDigest(secret)
	record, err := s.store.CreateAPIToken(ctx, store.CreateAPITokenParams{
		UserID: principal.UserID, OrgID: principal.OrgID, Kind: params.Kind, Name: strings.TrimSpace(params.Name),
		Prefix: prefix, Hash: digest, Scopes: append([]string(nil), params.Scopes...), ExpiresAt: params.ExpiresAt,
		Audit: authAudit(&principal.UserID), Action: params.AuditAction,
	})
	if err != nil {
		return CreatedToken{}, err
	}
	return CreatedToken{ID: record.ID, Token: plaintext, Prefix: prefix,
		Scopes: append([]string(nil), record.Scopes...), ExpiresAt: record.ExpiresAt}, nil
}

type IdempotentAPITokenResult struct {
	Token    store.APIToken
	Secret   string
	Replayed bool
}

func (s *Service) CreateIdempotentAPIToken(ctx context.Context, principal Principal, key string, params CreateTokenParams) (IdempotentAPITokenResult, error) {
	if principal.UserID == "" || principal.OrgID == "" || strings.TrimSpace(key) == "" {
		return IdempotentAPITokenResult{}, ErrNotFound
	}
	params.Kind = CredentialKindAPI
	if err := s.validateToken(ctx, principal, params); err != nil {
		return IdempotentAPITokenResult{}, err
	}
	request := struct {
		Name      string     `json:"name"`
		Scopes    []string   `json:"scopes"`
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
	}{strings.TrimSpace(params.Name), append([]string(nil), params.Scopes...), params.ExpiresAt}
	encoded, _ := json.Marshal(request)
	hash := sha256.Sum256(encoded)
	requestHash := hex.EncodeToString(hash[:])
	claim, err := s.store.ClaimIdempotency(ctx, store.IdempotencyClaimParams{OrgID: principal.OrgID,
		Scope: "api_token.create", Key: key, RequestHash: requestHash, Owner: principal.UserID,
		LeaseDuration: time.Minute, Retention: 24 * time.Hour})
	if err != nil {
		return IdempotentAPITokenResult{}, err
	}
	switch claim.Disposition {
	case store.IdempotencyConflict:
		return IdempotentAPITokenResult{}, store.ErrConflict
	case store.IdempotencyInProgress:
		return IdempotentAPITokenResult{}, store.ErrIdempotencyActive
	case store.IdempotencyReplay:
		var token store.APIToken
		if err := json.Unmarshal(claim.Body, &token); err != nil {
			return IdempotentAPITokenResult{}, fmt.Errorf("decode API token idempotency metadata: %w", err)
		}
		token.Hash = ""
		return IdempotentAPITokenResult{Token: token, Replayed: true}, nil
	}
	prefix, secret, plaintext, err := generateToken()
	if err != nil {
		return IdempotentAPITokenResult{}, err
	}
	id := uuid.Must(uuid.NewV7()).String()
	resourceType := "api_token"
	completion := store.CompleteIdempotencyParams{OrgID: principal.OrgID, Scope: "api_token.create", Key: key,
		RequestHash: requestHash, Owner: principal.UserID, Status: "completed", StatusCode: 201, Body: json.RawMessage(`{}`),
		ResourceType: &resourceType, ResourceID: &id}
	record, err := s.store.AcceptAPIToken(ctx, store.AcceptAPITokenParams{Create: store.CreateAPITokenParams{
		ID: id, UserID: principal.UserID, OrgID: principal.OrgID, Kind: CredentialKindAPI, Name: request.Name,
		Prefix: prefix, Hash: s.tokenDigest(secret), Scopes: request.Scopes, ExpiresAt: request.ExpiresAt,
	}, Idempotency: completion, Audit: authAudit(&principal.UserID)})
	if err != nil {
		return IdempotentAPITokenResult{}, err
	}
	record.Hash = ""
	return IdempotentAPITokenResult{Token: record, Secret: plaintext}, nil
}

func (s *Service) AuthenticateToken(ctx context.Context, plaintext string) (Principal, error) {
	prefix, secret, err := parseToken(plaintext)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	record, err := s.store.APITokenByPrefix(ctx, prefix)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	expected, err := hex.DecodeString(record.Hash)
	if err != nil || len(expected) != sha256.Size {
		return Principal{}, ErrUnauthenticated
	}
	actual, _ := hex.DecodeString(s.tokenDigest(secret))
	validKind := record.Kind == CredentialKindSession && len(record.Scopes) == 0 && record.ExpiresAt != nil
	if record.Kind == CredentialKindAPI {
		validKind = ValidateScopes(record.Scopes) == nil
	}
	if subtle.ConstantTimeCompare(actual, expected) != 1 || record.RevokedAt != nil ||
		(record.ExpiresAt != nil && !record.ExpiresAt.After(s.now())) || !validKind {
		return Principal{}, ErrUnauthenticated
	}
	now := s.now()
	if record.LastUsedAt == nil || record.LastUsedAt.Before(now.Add(-lastUsedTouchInterval)) {
		_ = s.store.TouchAPIToken(ctx, record.ID, now, now.Add(-lastUsedTouchInterval))
	}
	return Principal{UserID: record.UserID, OrgID: record.OrgID, Email: record.Email, Operator: record.Operator,
		Scopes: append([]string(nil), record.Scopes...), TokenID: record.ID, Kind: record.Kind}, nil
}

func (s *Service) RevokeToken(ctx context.Context, principal Principal, tokenID string) error {
	if principal.UserID == "" || principal.OrgID == "" || tokenID == "" {
		return ErrNotFound
	}
	if err := s.store.RevokeAPIToken(ctx, principal.OrgID, principal.UserID, tokenID, s.now(), authAudit(&principal.UserID)); err != nil {
		return ErrNotFound
	}
	return nil
}

func (s *Service) validateToken(ctx context.Context, principal Principal, params CreateTokenParams) error {
	if principal.UserID == "" || principal.OrgID == "" || strings.TrimSpace(params.Name) == "" {
		return ErrNotFound
	}
	if params.Kind != CredentialKindSession && params.Kind != CredentialKindAPI {
		return ErrInvalidCredential
	}
	if params.Kind == CredentialKindSession {
		if len(params.Scopes) != 0 || params.ExpiresAt == nil {
			return ErrInvalidCredential
		}
	} else if err := ValidateScopes(params.Scopes); err != nil {
		return err
	}
	if params.ExpiresAt != nil && !params.ExpiresAt.After(s.now()) {
		return ErrInvalidCredential
	}
	if params.Kind == CredentialKindSession && params.ExpiresAt.After(s.now().Add(maxSessionTTL)) {
		return ErrInvalidCredential
	}
	for _, scope := range params.Scopes {
		appID, _ := ParseScope(scope)
		if err := s.store.AuthorizeApp(ctx, principal.OrgID, appID); err != nil {
			return ErrNotFound
		}
	}
	return nil
}

func authAudit(actorUserID *string) *store.AuditEvent {
	return &store.AuditEvent{ID: uuid.Must(uuid.NewV7()).String(), ActorUserID: actorUserID}
}

func (s *Service) AuthorizeDeployApp(ctx context.Context, principal Principal, appID string) error {
	if principal.OrgID == "" || !principal.CanDeployApp(appID) {
		return ErrNotFound
	}
	if err := s.store.AuthorizeApp(ctx, principal.OrgID, appID); err != nil {
		return ErrNotFound
	}
	return nil
}

func (s *Service) tokenDigest(secret []byte) string {
	mac := hmac.New(sha256.New, s.pepper)
	_, _ = mac.Write(secret)
	return hex.EncodeToString(mac.Sum(nil))
}

func generateToken() (string, []byte, string, error) {
	prefixBytes := make([]byte, tokenPrefixBytes)
	secret := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(prefixBytes); err != nil {
		return "", nil, "", fmt.Errorf("generate token prefix: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return "", nil, "", fmt.Errorf("generate token secret: %w", err)
	}
	prefix := base64.RawURLEncoding.EncodeToString(prefixBytes)
	encodedSecret := base64.RawURLEncoding.EncodeToString(secret)
	return prefix, secret, tokenEnvelopePrefix + prefix + "_" + encodedSecret, nil
}

func parseToken(plaintext string) (string, []byte, error) {
	if len(plaintext) != len(tokenEnvelopePrefix)+tokenPrefixLength+1+tokenSecretLength ||
		!strings.HasPrefix(plaintext, tokenEnvelopePrefix) {
		return "", nil, ErrInvalidCredential
	}
	remainder := strings.TrimPrefix(plaintext, tokenEnvelopePrefix)
	if remainder[tokenPrefixLength] != '_' {
		return "", nil, ErrInvalidCredential
	}
	prefix := remainder[:tokenPrefixLength]
	prefixBytes, err := base64.RawURLEncoding.Strict().DecodeString(prefix)
	if err != nil || len(prefixBytes) != tokenPrefixBytes {
		return "", nil, ErrInvalidCredential
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(remainder[tokenPrefixLength+1:])
	if err != nil || len(secret) != tokenSecretBytes {
		return "", nil, ErrInvalidCredential
	}
	return prefix, secret, nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func principalFromUser(user store.User) Principal {
	return Principal{UserID: user.ID, OrgID: user.OrgID, Email: user.Email, Operator: user.Operator}
}
