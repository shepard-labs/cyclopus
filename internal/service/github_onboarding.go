package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"
	githubadapter "github.com/shepard-labs/cyclopus/internal/adapters/github"
	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const setupTTL = 15 * time.Minute
const githubResponseLimit = 1 << 20

var (
	ErrGitHubAppNotConfigured = errors.New("github_app_not_configured")
	ErrGitHubSetupState       = errors.New("github_setup_state_invalid")
)

type GitHubOnboardingConfig struct {
	ExternalURL, APIBaseURL, UploadURL, APIVersion string
	CredentialSecretRef                            string
	RequestTimeout                                 time.Duration
	SealingKey                                     []byte
}

type GitHubOnboarding struct {
	store                  *store.Store
	secrets                secretref.ManagedWriter
	config                 GitHubOnboardingConfig
	client                 *http.Client
	now                    func() time.Time
	generation             func() (string, error)
	exchangeManifest       func(context.Context, string) (*gh.AppConfig, error)
	verifyAppMetadata      func(context.Context, githubadapter.AppCredential, store.GitHubApp) error
	verifyInstallationByID func(context.Context, githubadapter.AppCredential, int64) (store.GitHubInstallation, error)
	beforeStoreCommit      func() error
}

type GitHubFlow struct {
	State, StartURL string
	ExpiresAt       time.Time
}

type GitHubOnboardingOption func(*GitHubOnboarding)

func WithGitHubOnboardingClock(now func() time.Time) GitHubOnboardingOption {
	return func(g *GitHubOnboarding) {
		if now != nil {
			g.now = now
		}
	}
}

func WithGitHubStateGenerator(generator func() (string, string, error)) GitHubOnboardingOption {
	return func(g *GitHubOnboarding) {
		if generator != nil {
			g.generation = func() (string, error) {
				state, _, err := generator()
				return state, err
			}
		}
	}
}

func WithGitHubManifestExchanger(exchange func(context.Context, string) (*gh.AppConfig, error)) GitHubOnboardingOption {
	return func(g *GitHubOnboarding) {
		if exchange != nil {
			g.exchangeManifest = exchange
		}
	}
}

func WithGitHubAppVerifier(verify func(context.Context, githubadapter.AppCredential) error) GitHubOnboardingOption {
	return func(g *GitHubOnboarding) {
		if verify != nil {
			g.verifyAppMetadata = func(ctx context.Context, credential githubadapter.AppCredential, _ store.GitHubApp) error {
				return verify(ctx, credential)
			}
		}
	}
}

func WithGitHubInstallationVerifier(verify func(context.Context, githubadapter.AppCredential, int64) (store.GitHubInstallation, error)) GitHubOnboardingOption {
	return func(g *GitHubOnboarding) {
		if verify != nil {
			g.verifyInstallationByID = verify
		}
	}
}

func NewGitHubOnboarding(dataStore *store.Store, secrets secretref.ManagedWriter, config GitHubOnboardingConfig, client *http.Client, options ...GitHubOnboardingOption) (*GitHubOnboarding, error) {
	if dataStore == nil || secrets == nil || config.ExternalURL == "" || config.RequestTimeout <= 0 || len(config.SealingKey) < 32 {
		return nil, fmt.Errorf("invalid GitHub onboarding configuration")
	}
	if client == nil {
		client = &http.Client{Timeout: config.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	} else {
		cloned := *client
		cloned.Timeout = config.RequestTimeout
		cloned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &cloned
	}
	config.SealingKey = append([]byte(nil), config.SealingKey...)
	g := &GitHubOnboarding{store: dataStore, secrets: secrets, config: config, client: client, now: time.Now, generation: randomGeneration}
	g.exchangeManifest = g.exchangeManifestCode
	g.verifyAppMetadata = g.verifyPersistedApp
	g.verifyInstallationByID = g.fetchInstallation
	for _, option := range options {
		if option != nil {
			option(g)
		}
	}
	return g, nil
}

func (g *GitHubOnboarding) BeginAppSetup(ctx context.Context, actor ActorContext, key string) (GitHubFlow, error) {
	if err := requireAdministrator(actor); err != nil {
		return GitHubFlow{}, err
	}
	return g.beginIdempotent(ctx, actor, key, "github.app_setup.begin", "app_setup", nil, nil, "/github/app-manifest/start", "not_ready")
}

func (g *GitHubOnboarding) BeginInstallation(ctx context.Context, actor ActorContext, key string) (GitHubFlow, error) {
	if err := requireAdministrator(actor); err != nil {
		return GitHubFlow{}, err
	}
	purpose := "additional_owner"
	return g.beginIdempotent(ctx, actor, key, "github.installation.begin", "installation", &actor.OrgID, &purpose, "/github/app-installation/start", "ready")
}

type githubFlowReplay struct {
	Generation string    `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (g *GitHubOnboarding) beginIdempotent(ctx context.Context, actor ActorContext, key, scope, kind string, orgID, purpose *string, path, requiredAppState string) (GitHubFlow, error) {
	if strings.TrimSpace(key) == "" {
		return GitHubFlow{}, ErrInvalidArgument
	}
	request := struct {
		Kind      string `json:"kind"`
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
	}{Kind: kind, UserID: actor.UserID, SessionID: actor.SessionID}
	generation, err := g.generation()
	if err != nil {
		return GitHubFlow{}, err
	}
	state := g.deriveState(kind, generation, actor.UserID, digest(actor.SessionID))
	started := g.now().UTC()
	expires := started.Add(setupTTL)
	body, _ := json.Marshal(githubFlowReplay{Generation: generation, ExpiresAt: expires})
	owner, err := randomGeneration()
	if err != nil {
		return GitHubFlow{}, err
	}
	claim := store.IdempotencyClaimParams{OrgID: actor.OrgID, Scope: scope, Key: key, RequestHash: requestHash(request), Owner: owner, LeaseDuration: idempotencyLease, Retention: idempotencyRetention}
	completion := store.CompleteIdempotencyParams{OrgID: actor.OrgID, Scope: scope, Key: key, RequestHash: requestHash(request), Status: "completed", StatusCode: http.StatusOK, Body: body}
	_, result, err := g.store.BeginGitHubSetupSessionIdempotent(ctx, store.BeginGitHubSetupSessionParams{StateHash: digest(state), Generation: generation, FlowKind: kind, UserID: actor.UserID, SessionHash: digest(actor.SessionID), OrgID: orgID, Purpose: purpose, StartedAt: started, ExpiresAt: expires, Idempotency: claim, Completion: completion, Audit: g.userAudit(actor, "rpc"), BeforeCommit: g.beforeStoreCommit, RequiredAppState: requiredAppState})
	if err != nil {
		if requiredAppState == "ready" && errors.Is(err, store.ErrFailedPrecondition) {
			return GitHubFlow{}, ErrGitHubAppNotConfigured
		}
		return GitHubFlow{}, mapError(err)
	}
	if result.Disposition != store.IdempotencyClaimed {
		var replay githubFlowReplay
		if result.Disposition == store.IdempotencyConflict {
			return GitHubFlow{}, ErrConflict
		}
		if result.Disposition == store.IdempotencyInProgress {
			return GitHubFlow{}, ErrInProgress
		}
		if err := json.Unmarshal(result.Body, &replay); err != nil {
			return GitHubFlow{}, fmt.Errorf("decode GitHub idempotency replay: %w", err)
		}
		state = g.deriveState(kind, replay.Generation, actor.UserID, digest(actor.SessionID))
		if err := g.store.ValidateGitHubSetupSession(ctx, digest(state), actor.UserID, digest(actor.SessionID), kind, g.now().UTC()); err != nil {
			return GitHubFlow{}, mapError(err)
		}
		expires = replay.ExpiresAt
	}
	return GitHubFlow{State: state, StartURL: g.config.ExternalURL + path + "?state=" + url.QueryEscape(state), ExpiresAt: expires}, nil
}

func (g *GitHubOnboarding) userAudit(actor ActorContext, source string) store.GitHubAuditContext {
	return store.GitHubAuditContext{OrgID: actor.OrgID, Event: audit(actor), Source: source}
}
func (g *GitHubOnboarding) systemAudit(source string) store.GitHubAuditContext {
	return store.GitHubAuditContext{Event: &store.AuditEvent{ID: newID()}, Source: source}
}

func (g *GitHubOnboarding) deriveState(kind, generation, userID, sessionID string) string {
	mac := hmac.New(sha256.New, g.config.SealingKey)
	_, _ = mac.Write([]byte("cyclopus-github-state-v1\x00" + kind + "\x00" + generation + "\x00" + userID + "\x00" + sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (g *GitHubOnboarding) Manifest() ([]byte, error) {
	manifest := map[string]any{
		"name": "Cyclopus", "url": g.config.ExternalURL, "hook_attributes": map[string]any{"url": g.config.ExternalURL + "/webhooks/github", "active": true},
		"redirect_url": g.config.ExternalURL + "/github/app-manifest/callback", "setup_url": g.config.ExternalURL + "/github/app/setup", "public": true,
		"description": "Self-hosted Cyclopus deployment integration", "default_permissions": map[string]string{"metadata": "read", "contents": "read", "actions": "write"},
		"default_events": []string{"push", "installation", "installation_repositories"},
	}
	return json.Marshal(manifest)
}

func (g *GitHubOnboarding) ValidateStart(ctx context.Context, actor ActorContext, state, kind string) error {
	if err := requireAdministrator(actor); err != nil {
		return err
	}
	if len(state) < 40 || actor.SessionID == "" {
		return ErrGitHubSetupState
	}
	wanted := digest(state)
	session, err := g.store.RedirectGitHubSetupSession(ctx, wanted, actor.UserID, digest(actor.SessionID), kind, g.now().UTC(), g.userAudit(actor, "browser_start"))
	if err != nil || subtle.ConstantTimeCompare([]byte(state), []byte(g.deriveState(kind, session.StateGeneration, session.UserID, session.SessionHash))) != 1 {
		return ErrGitHubSetupState
	}
	return nil
}

type GitHubCallbackIdentity struct {
	UserID, SessionHash string
}

func (g *GitHubOnboarding) CheckpointGitHubCallback(ctx context.Context, state, kind string) (GitHubCallbackIdentity, error) {
	if len(state) < 40 {
		return GitHubCallbackIdentity{}, ErrGitHubSetupState
	}
	session, err := g.store.GitHubSetupSessionByState(ctx, digest(state), kind, g.now().UTC())
	if err != nil || subtle.ConstantTimeCompare([]byte(state), []byte(g.deriveState(kind, session.StateGeneration, session.UserID, session.SessionHash))) != 1 {
		return GitHubCallbackIdentity{}, ErrGitHubSetupState
	}
	// The current deployment key must still authenticate the state. The session ID
	// itself remains unavailable here, so continuation performs the final binding.
	return GitHubCallbackIdentity{UserID: session.UserID, SessionHash: session.SessionHash}, nil
}

func (g *GitHubOnboarding) HandleManifestCallback(ctx context.Context, actor ActorContext, state, code string) error {
	if strings.TrimSpace(code) == "" {
		return ErrGitHubSetupState
	}
	session, err := g.consume(ctx, actor, state, "app_setup")
	if err != nil {
		return err
	}
	if err := g.store.StartGitHubAppExchange(ctx, session.ID, g.userAudit(actor, "manifest_callback")); err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, g.config.RequestTimeout)
	defer cancel()
	config, err := g.exchangeManifest(requestCtx, code)
	if err != nil {
		_ = g.store.FailGitHubAppSetup(ctx, session.ID, "github_manifest_code_invalid", "GitHub rejected the one-time manifest code; restart setup.", false, g.userAudit(actor, "manifest_callback"))
		return ErrGitHubSetupState
	}
	metadata, err := manifestMetadata(config)
	if err != nil {
		_ = g.store.FailGitHubAppSetup(ctx, session.ID, "github_manifest_code_invalid", "GitHub returned an incomplete App manifest response; restart setup.", false, g.userAudit(actor, "manifest_callback"))
		return err
	}
	if err := g.store.RecordGitHubAppMetadata(ctx, session.ID, metadata, g.userAudit(actor, "manifest_callback")); err != nil {
		return err
	}
	if err := g.store.MarkGitHubAppSecretWrite(ctx, session.ID, false, g.userAudit(actor, "manifest_callback")); err != nil {
		return err
	}
	_, err = g.secrets.WriteGitHubApp(ctx, secretref.ManagedSecret{Data: map[string][]byte{secretref.GitHubPrivateKeyKey: []byte(config.GetPEM()), secretref.GitHubWebhookKey: []byte(config.GetWebhookSecret())}})
	if err != nil {
		_ = g.store.FailGitHubAppSetup(ctx, session.ID, "github_app_secret_write_failed", "The GitHub App was created but its managed credential Secret could not be written. Delete the orphan App and restart setup.", true, g.userAudit(actor, "manifest_callback"))
		return err
	}
	if err := g.store.MarkGitHubAppSecretWrite(ctx, session.ID, true, g.userAudit(actor, "manifest_callback")); err != nil {
		_ = g.store.FailGitHubAppSetup(ctx, session.ID, "github_app_secret_write_failed", "The managed credential Secret was written but readiness could not be committed. Delete the orphan App and restart setup.", true, g.userAudit(actor, "manifest_callback"))
		return err
	}
	if err := g.store.ActivateGitHubApp(ctx, session.ID, g.config.CredentialSecretRef, g.userAudit(actor, "manifest_callback")); err != nil {
		_ = g.store.FailGitHubAppSetup(ctx, session.ID, "github_app_secret_write_failed", "The managed credential Secret was written but readiness could not be committed. Delete the orphan App and restart setup.", true, g.userAudit(actor, "manifest_callback"))
		return err
	}
	return nil
}

func (g *GitHubOnboarding) Reconcile(ctx context.Context) error {
	app, err := g.store.GetGitHubApp(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	secret, secretErr := g.secrets.ReadGitHubApp(ctx)
	credential, credentialErr := reconcileCredential(app, secret, secretErr)
	if app.State == "ready" {
		if credentialErr != nil {
			return g.store.FailGitHubAppSetup(ctx, app.SetupSessionID, "github_app_credential_unavailable", "The managed GitHub App credential is unavailable or invalid; restart setup.", false, g.systemAudit("startup_reconciliation"))
		}
		if err := g.verifyAppMetadata(ctx, credential, app); err != nil {
			return g.store.FailGitHubAppSetup(ctx, app.SetupSessionID, "github_app_identity_invalid", "The managed GitHub App credential no longer matches the configured App; restart setup.", false, g.systemAudit("startup_reconciliation"))
		}
		return nil
	}
	if app.State == "exchanging" {
		return g.store.FailGitHubAppSetup(ctx, app.SetupSessionID, "github_manifest_code_consumed", "GitHub App setup was interrupted before metadata was received; restart setup.", false, g.systemAudit("startup_reconciliation"))
	}
	if app.State == "secret_pending" && app.AppID != nil {
		if credentialErr != nil {
			return g.store.FailGitHubAppSetup(ctx, app.SetupSessionID, "github_app_secret_write_failed", "The GitHub App was created but its managed credential Secret is unavailable or invalid. Delete the orphan App and restart setup.", true, g.systemAudit("startup_reconciliation"))
		}
		if err := g.verifyAppMetadata(ctx, credential, app); err != nil {
			return g.store.FailGitHubAppSetup(ctx, app.SetupSessionID, "github_app_identity_invalid", "The managed credential does not match the created GitHub App. Delete the orphan App and restart setup.", true, g.systemAudit("startup_reconciliation"))
		}
		if err := g.store.MarkGitHubAppSecretWrite(ctx, appSetupSession(app), true, g.systemAudit("startup_reconciliation")); err != nil {
			return err
		}
		return g.store.ActivateGitHubApp(ctx, appSetupSession(app), g.config.CredentialSecretRef, g.systemAudit("startup_reconciliation"))
	}
	return nil
}

func reconcileCredential(app store.GitHubApp, secret secretref.ManagedSecret, secretErr error) (githubadapter.AppCredential, error) {
	if secretErr != nil || app.AppID == nil {
		return githubadapter.AppCredential{}, fmt.Errorf("managed GitHub App credential unavailable")
	}
	if err := secretref.ValidateGitHubAppSecret(secret.Data); err != nil {
		return githubadapter.AppCredential{}, err
	}
	return githubadapter.AppCredential{AppID: *app.AppID, PrivateKey: append([]byte(nil), secret.Data[secretref.GitHubPrivateKeyKey]...), ResourceVersion: secret.ResourceVersion}, nil
}

func (g *GitHubOnboarding) verifyPersistedApp(ctx context.Context, credential githubadapter.AppCredential, persisted store.GitHubApp) error {
	client, err := githubadapter.NewAppClient(credential, g.config.APIBaseURL, g.config.UploadURL, g.config.APIVersion, g.client.Transport)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, g.config.RequestTimeout)
	defer cancel()
	app, _, err := client.Apps.Get(requestCtx, "")
	if err != nil {
		return fmt.Errorf("verify GitHub App identity: %w", err)
	}
	if app.GetID() != credential.AppID || persisted.AppID == nil || app.GetID() != *persisted.AppID ||
		app.GetSlug() != persisted.Slug || persisted.OwnerID == nil || app.GetOwner().GetID() != *persisted.OwnerID ||
		persisted.OwnerLogin == nil || app.GetOwner().GetLogin() != *persisted.OwnerLogin ||
		persisted.OwnerType == nil || app.GetOwner().GetType() != *persisted.OwnerType {
		return ErrGitHubSetupState
	}
	return nil
}

func appSetupSession(app store.GitHubApp) string { return app.SetupSessionID }

func (g *GitHubOnboarding) HandleInstallationCallback(ctx context.Context, actor ActorContext, state string, installationID int64) (store.GitHubInstallation, error) {
	if installationID <= 0 {
		return store.GitHubInstallation{}, ErrInvalidArgument
	}
	session, err := g.consume(ctx, actor, state, "installation")
	if err != nil {
		return store.GitHubInstallation{}, err
	}
	if session.OrgID == nil || subtle.ConstantTimeCompare([]byte(*session.OrgID), []byte(actor.OrgID)) != 1 {
		return store.GitHubInstallation{}, ErrGitHubSetupState
	}
	installation, err := g.verifyInstallation(ctx, installationID)
	if err != nil {
		return store.GitHubInstallation{}, err
	}
	return g.store.UpsertGitHubInstallation(ctx, actor.OrgID, installation, g.userAudit(actor, "installation_callback"), session.ID)
}

func (g *GitHubOnboarding) RefreshInstallation(ctx context.Context, actor ActorContext, key string, installationID int64) (store.GitHubInstallation, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.GitHubInstallation{}, err
	}
	if strings.TrimSpace(key) == "" || installationID <= 0 {
		return store.GitHubInstallation{}, ErrInvalidArgument
	}
	request := struct {
		InstallationID int64 `json:"installation_id"`
	}{InstallationID: installationID}
	owner, err := randomGeneration()
	if err != nil {
		return store.GitHubInstallation{}, err
	}
	hash := requestHash(request)
	claim := store.IdempotencyClaimParams{OrgID: actor.OrgID, Scope: "github.installation.refresh", Key: key, RequestHash: hash, Owner: owner, LeaseDuration: g.config.RequestTimeout + time.Minute, Retention: idempotencyRetention}
	result, err := g.store.ClaimIdempotency(ctx, claim)
	if err != nil {
		return store.GitHubInstallation{}, mapError(err)
	}
	switch result.Disposition {
	case store.IdempotencyConflict:
		return store.GitHubInstallation{}, ErrConflict
	case store.IdempotencyInProgress:
		return store.GitHubInstallation{}, ErrInProgress
	case store.IdempotencyReplay:
		if result.Status == "failed" {
			return store.GitHubInstallation{}, store.ErrFailedPrecondition
		}
		var replay store.GitHubInstallation
		if err := json.Unmarshal(result.Body, &replay); err != nil {
			return store.GitHubInstallation{}, fmt.Errorf("decode GitHub idempotency replay: %w", err)
		}
		return replay, nil
	}
	if _, err := g.store.GetGitHubInstallation(ctx, actor.OrgID, installationID); err != nil {
		g.failRefreshIdempotency(ctx, actor, claim)
		return store.GitHubInstallation{}, err
	}
	installation, err := g.verifyInstallation(ctx, installationID)
	if err != nil {
		g.failRefreshIdempotency(ctx, actor, claim)
		return store.GitHubInstallation{}, err
	}
	completion := store.CompleteIdempotencyParams{OrgID: actor.OrgID, Scope: "github.installation.refresh", Key: key, RequestHash: hash, Status: "completed", StatusCode: http.StatusOK}
	installation, result, err = g.store.SaveGitHubInstallationIdempotent(ctx, store.SaveGitHubInstallationIdempotentParams{OrgID: actor.OrgID, Installation: installation,
		Idempotency: claim,
		Completion:  completion, Audit: g.userAudit(actor, "rpc"), BeforeCommit: g.beforeStoreCommit})
	if err != nil {
		abandonCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = g.store.AbandonIdempotency(abandonCtx, claim)
		return store.GitHubInstallation{}, mapError(err)
	}
	if result.Disposition == store.IdempotencyConflict {
		return store.GitHubInstallation{}, ErrConflict
	}
	if result.Disposition == store.IdempotencyInProgress {
		return store.GitHubInstallation{}, ErrInProgress
	}
	if result.Disposition == store.IdempotencyReplay {
		if err := json.Unmarshal(result.Body, &installation); err != nil {
			return store.GitHubInstallation{}, fmt.Errorf("decode GitHub idempotency replay: %w", err)
		}
	}
	return installation, nil
}

func (g *GitHubOnboarding) failRefreshIdempotency(ctx context.Context, actor ActorContext, claim store.IdempotencyClaimParams) {
	body, _ := json.Marshal(map[string]string{"error": "github_installation_refresh_failed"})
	code := "github_installation_refresh_failed"
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = g.store.CompleteIdempotency(completionCtx, store.CompleteIdempotencyParams{OrgID: claim.OrgID, Scope: claim.Scope, Key: claim.Key, RequestHash: claim.RequestHash,
		Owner: claim.Owner, Status: "failed", StatusCode: http.StatusFailedDependency, Body: body, ErrorCode: &code, Audit: g.userAudit(actor, "rpc").Event})
}

func (g *GitHubOnboarding) consume(ctx context.Context, actor ActorContext, state, kind string) (store.GitHubSetupSession, error) {
	if err := requireAdministrator(actor); err != nil {
		return store.GitHubSetupSession{}, err
	}
	if len(state) < 40 || actor.SessionID == "" {
		return store.GitHubSetupSession{}, ErrGitHubSetupState
	}
	wanted := digest(state)
	session, err := g.store.ConsumeGitHubSetupSession(ctx, wanted, actor.UserID, digest(actor.SessionID), kind, g.now().UTC(), g.userAudit(actor, "callback"))
	derived := g.deriveState(kind, session.StateGeneration, session.UserID, digest(actor.SessionID))
	if err != nil || subtle.ConstantTimeCompare([]byte(session.StateHash), []byte(wanted)) != 1 || subtle.ConstantTimeCompare([]byte(derived), []byte(state)) != 1 {
		return store.GitHubSetupSession{}, ErrGitHubSetupState
	}
	return session, nil
}

func (g *GitHubOnboarding) verifyInstallation(ctx context.Context, installationID int64) (store.GitHubInstallation, error) {
	credential, err := g.AppCredential(ctx)
	if err != nil {
		return store.GitHubInstallation{}, err
	}
	return g.verifyInstallationByID(ctx, credential, installationID)
}

func (g *GitHubOnboarding) fetchInstallation(ctx context.Context, credential githubadapter.AppCredential, installationID int64) (store.GitHubInstallation, error) {
	client, err := githubadapter.NewAppClient(credential, g.config.APIBaseURL, g.config.UploadURL, g.config.APIVersion, g.client.Transport)
	if err != nil {
		return store.GitHubInstallation{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, g.config.RequestTimeout)
	defer cancel()
	value, _, err := client.Apps.GetInstallation(requestCtx, installationID)
	if err != nil {
		var responseError *gh.ErrorResponse
		if errors.As(err, &responseError) && responseError.Response != nil && responseError.Response.StatusCode == http.StatusNotFound {
			return store.GitHubInstallation{GitHubInstallationID: installationID, AccessState: "deleted"}, nil
		}
		return store.GitHubInstallation{}, err
	}
	accountType := value.GetAccount().GetType()
	selection := value.GetRepositorySelection()
	if value.GetAppID() != credential.AppID || value.GetID() != installationID || value.GetAccount().GetID() <= 0 || value.GetAccount().GetLogin() == "" ||
		(accountType != "User" && accountType != "Organization") || (selection != "all" && selection != "selected") {
		return store.GitHubInstallation{}, ErrGitHubSetupState
	}
	state := "active"
	var suspendedAt *time.Time
	if value.SuspendedAt != nil {
		state = "suspended"
		t := value.SuspendedAt.Time
		suspendedAt = &t
	}
	return store.GitHubInstallation{GitHubInstallationID: installationID, AccountID: value.GetAccount().GetID(), AccountLogin: value.GetAccount().GetLogin(),
		AccountType: accountType, RepositorySelection: selection, AccessState: state, SuspendedAt: suspendedAt}, nil
}

func (g *GitHubOnboarding) exchangeManifestCode(ctx context.Context, code string) (*gh.AppConfig, error) {
	base, err := url.Parse(g.config.APIBaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("invalid GitHub API URL")
	}
	target := base.ResolveReference(&url.URL{Path: "app-manifests/" + url.PathEscape(code) + "/conversions"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub manifest request")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", g.config.APIVersion)
	request.Header.Set("User-Agent", "cyclopus")
	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("exchange GitHub manifest code")
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, githubResponseLimit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read GitHub manifest response")
	}
	if len(body) > githubResponseLimit {
		return nil, fmt.Errorf("GitHub manifest response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub manifest exchange returned status %d", response.StatusCode)
	}
	var config gh.AppConfig
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode GitHub manifest response")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("decode GitHub manifest response")
	}
	return &config, nil
}

func (g *GitHubOnboarding) AppCredential(ctx context.Context) (githubadapter.AppCredential, error) {
	app, err := g.store.GetGitHubApp(ctx)
	if err != nil || app.State != "ready" || app.AppID == nil {
		return githubadapter.AppCredential{}, ErrGitHubAppNotConfigured
	}
	secret, err := g.secrets.ReadGitHubApp(ctx)
	if err != nil {
		return githubadapter.AppCredential{}, err
	}
	key := secret.Data[secretref.GitHubPrivateKeyKey]
	if len(key) == 0 {
		return githubadapter.AppCredential{}, ErrGitHubAppNotConfigured
	}
	return githubadapter.AppCredential{AppID: *app.AppID, PrivateKey: append([]byte(nil), key...), ResourceVersion: secret.ResourceVersion}, nil
}

func randomGeneration() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func manifestMetadata(v *gh.AppConfig) (store.GitHubAppMetadata, error) {
	if v == nil || v.GetID() <= 0 || v.GetSlug() == "" || v.GetPEM() == "" || v.GetWebhookSecret() == "" || v.GetOwner().GetID() <= 0 {
		return store.GitHubAppMetadata{}, fmt.Errorf("GitHub manifest response was incomplete")
	}
	ownerType := v.GetOwner().GetType()
	if ownerType != "User" && ownerType != "Organization" {
		return store.GitHubAppMetadata{}, fmt.Errorf("GitHub App owner type was invalid")
	}
	return store.GitHubAppMetadata{AppID: v.GetID(), Slug: v.GetSlug(), Name: v.GetName(), HTMLURL: v.GetHTMLURL(), OwnerID: v.GetOwner().GetID(), OwnerLogin: v.GetOwner().GetLogin(), OwnerType: ownerType}, nil
}

func (g *GitHubOnboarding) InstallationURL() (string, error) {
	app, err := g.store.GetGitHubApp(context.Background())
	if err != nil || app.State != "ready" {
		return "", ErrGitHubAppNotConfigured
	}
	return "https://github.com/apps/" + url.PathEscape(app.Slug) + "/installations/new", nil
}
func (g *GitHubOnboarding) WebhookURL() string { return g.config.ExternalURL + "/webhooks/github" }
func parseInstallationID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidArgument
	}
	return id, nil
}
