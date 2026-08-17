package ui

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	"github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
)

// ErrUnauthorized marks a session authentication failure. The UI handler maps
// it to clearing the session cookie and redirecting to /login (it replaces the
// 401-status check the old HTTP client had to perform).
var ErrUnauthorized = errors.New("ui: unauthorized")

// IsUnauthorized reports whether err is an authentication failure (missing,
// expired, or bad session credentials).
func IsUnauthorized(err error) bool { return errors.Is(err, ErrUnauthorized) }

// maxSessionTTL mirrors the session-lifetime ceiling server.NewServices applies.
const maxSessionTTL = 7 * 24 * time.Hour

// Backend serves the admin UI's data needs by calling the service and auth
// layers directly, in-process. Every method authenticates the session token the
// same way the gRPC layer did, and non-session (API/deploy) tokens are rejected
// exactly as sessionActor rejected them.
//
// Return values are plain store/service/observability types — the templates
// consume those directly. Enum-like fields carry store values ("hpa", "live",
// "ready"), which is also what the forms post back.
//
// List responses return an empty next-page token (the UI never rendered
// pagination controls for those lists); audit events keep working pagination
// via an offset token.
type Backend struct {
	svc        *service.Service
	auth       *auth.Service
	sessionTTL time.Duration
}

// NewBackend constructs the UI backend. sessionTTL is clamped to maxSessionTTL,
// mirroring server.NewServices.
func NewBackend(svc *service.Service, authSvc *auth.Service, sessionTTL time.Duration) *Backend {
	if sessionTTL > maxSessionTTL {
		sessionTTL = maxSessionTTL
	}
	return &Backend{svc: svc, auth: authSvc, sessionTTL: sessionTTL}
}

// actor authenticates the session token and enforces the session-kind rule the
// REST layer enforced via sessionActor (deploy/API tokens cannot drive the UI).
func (b *Backend) actor(ctx context.Context, token string) (service.ActorContext, error) {
	principal, err := b.auth.AuthenticateToken(ctx, token)
	if err != nil {
		return service.ActorContext{}, ErrUnauthorized
	}
	if principal.Kind != auth.CredentialKindSession {
		return service.ActorContext{}, service.ErrForbidden
	}
	return service.ActorContext{UserID: principal.UserID, OrgID: principal.OrgID, Operator: principal.Operator}, nil
}

// ── Auth ────────────────────────────────────────────────────────────────────

func (b *Backend) Login(ctx context.Context, email, password string) (*Session, error) {
	principal, err := b.auth.AuthenticatePassword(ctx, email, password)
	if err != nil {
		return nil, ErrUnauthorized
	}
	return b.issueSession(ctx, principal, "auth.login")
}

func (b *Backend) Bootstrap(ctx context.Context, email, password, bootstrapToken string) (*Session, error) {
	principal, err := b.auth.ClaimFirstUser(ctx, email, password, bootstrapToken)
	if err != nil {
		return nil, err
	}
	return b.issueSession(ctx, principal, "auth.bootstrap_session_created")
}

// issueSession mirrors server.apiHandlers.issueSession: same session-token
// shape, name, and audit action, and the Session still carries the current user.
func (b *Backend) issueSession(ctx context.Context, principal auth.Principal, auditAction string) (*Session, error) {
	expires := time.Now().Add(b.sessionTTL)
	created, err := b.auth.CreateToken(ctx, principal, auth.CreateTokenParams{
		Kind: auth.CredentialKindSession, Name: "local session", ExpiresAt: &expires, AuditAction: auditAction,
	})
	if err != nil {
		return nil, err
	}
	user, err := b.svc.CurrentUser(ctx, service.ActorContext{UserID: principal.UserID, OrgID: principal.OrgID})
	if err != nil {
		return nil, err
	}
	return &Session{AccessToken: created.Token, ExpiresAt: expires, User: user}, nil
}

func (b *Backend) Logout(ctx context.Context, token string) error {
	principal, err := b.auth.AuthenticateToken(ctx, token)
	if err != nil {
		return ErrUnauthorized
	}
	return b.auth.RevokeToken(ctx, principal, principal.TokenID)
}

func (b *Backend) GetCurrentUser(ctx context.Context, token string) (store.User, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return store.User{}, err
	}
	return b.svc.CurrentUser(ctx, actor)
}

func (b *Backend) UpdateCurrentUser(ctx context.Context, token string, params auth.UpdateProfileParams) (store.User, error) {
	principal, err := b.auth.AuthenticateToken(ctx, token)
	if err != nil {
		return store.User{}, ErrUnauthorized
	}
	return b.auth.UpdateProfile(ctx, principal, params)
}

func (b *Backend) ChangePassword(ctx context.Context, token, currentPassword, newPassword string) error {
	principal, err := b.auth.AuthenticateToken(ctx, token)
	if err != nil {
		return ErrUnauthorized
	}
	return b.auth.ChangePassword(ctx, principal, currentPassword, newPassword)
}

// ── Organization ────────────────────────────────────────────────────────────

func (b *Backend) GetOrganization(ctx context.Context, token, orgID string) (store.Organization, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return store.Organization{}, err
	}
	// The REST handler hid non-owned orgs as NotFound; keep that contract.
	if orgID != actor.OrgID {
		return store.Organization{}, store.ErrNotFound
	}
	return b.svc.CurrentOrganization(ctx, actor)
}

// ── Projects ────────────────────────────────────────────────────────────────

// ListProjects returns the full org list. The pageToken parameter is accepted
// for call-site compatibility and ignored: the projects page never rendered the
// REST page cursor.
func (b *Backend) ListProjects(ctx context.Context, token, _, _ string) ([]store.Project, string, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, "", err
	}
	values, err := b.svc.ListProjects(ctx, actor, false)
	return values, "", err
}

func (b *Backend) CreateProject(ctx context.Context, token, _, idemKey, name, slug string) (*store.Project, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	value, err := b.svc.CreateProject(ctx, actor, idemKey, service.ProjectInput{Name: name, Slug: slug})
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (b *Backend) GetProject(ctx context.Context, token, projectID string) (*store.Project, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	value, err := b.svc.GetProject(ctx, actor, projectID)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// ── Applications ────────────────────────────────────────────────────────────

func (b *Backend) ListApplications(ctx context.Context, token, projectID, _ string) ([]store.App, string, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, "", err
	}
	values, err := b.svc.ListApps(ctx, actor, projectID, false)
	return values, "", err
}

// CreateApplication sends exactly the fields the settings-less create form
// collects; unset fields get platform defaults inside the service
// (applyAppCreateDefaults), as they did over REST.
func (b *Backend) CreateApplication(ctx context.Context, token, _, idemKey string, input service.AppInput) (*store.App, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	value, err := b.svc.CreateApp(ctx, actor, idemKey, input)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (b *Backend) GetApplication(ctx context.Context, token, appID string) (*store.App, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	value, err := b.svc.GetApp(ctx, actor, appID)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// UpdateApplication replays the REST handler's mask semantics in-process: load
// the current app, apply only the masked fields, then UpdateApp with the full
// input. Enum-valued fields arrive as store values ("hpa", "keda_cron", ...)
// because the settings form now posts them directly.
func (b *Backend) UpdateApplication(ctx context.Context, token, idemKey, appID string, fields map[string]any, paths []string) (*store.App, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	current, err := b.svc.GetApp(ctx, actor, appID)
	if err != nil {
		return nil, err
	}
	input := appInputFromStore(current)
	for _, path := range paths {
		if err := applyAppField(&input, path, fields[path]); err != nil {
			return nil, err
		}
	}
	value, err := b.svc.UpdateApp(ctx, actor, idemKey, appID, input)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// ── Environments & releases ─────────────────────────────────────────────────

func (b *Backend) ListEnvironments(ctx context.Context, token, appID string, includeTornDown bool) ([]store.Environment, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListEnvironments(ctx, actor, appID, includeTornDown)
}

func (b *Backend) GetEnvironment(ctx context.Context, token, envID string) (*store.Environment, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	value, err := b.svc.GetEnvironment(ctx, actor, envID)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (b *Backend) ListReleases(ctx context.Context, token, envID, _ string) ([]store.Release, string, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, "", err
	}
	values, err := b.svc.ListReleases(ctx, actor, envID, nil)
	return values, "", err
}

func (b *Backend) GetRelease(ctx context.Context, token, releaseID string) (*store.Release, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	value, err := b.svc.GetRelease(ctx, actor, releaseID)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (b *Backend) Rollback(ctx context.Context, token, idemKey, envID, releaseID string) error {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return err
	}
	_, err = b.svc.AcceptRollback(ctx, actor, idemKey, envID, releaseID)
	return err
}

// GetEnvVars returns the zero revision when no revision exists yet — the
// handler renders that as "no environment variables set".
func (b *Backend) GetEnvVars(ctx context.Context, token, envID string) (store.EnvironmentVariableRevision, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return store.EnvironmentVariableRevision{}, err
	}
	return b.svc.GetEnvironmentVariables(ctx, actor, envID)
}

// ── Aliases, certificates, domains ──────────────────────────────────────────

func (b *Backend) ListAliases(ctx context.Context, token, envID string) ([]store.Alias, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListAliases(ctx, actor, envID)
}

func (b *Backend) ListCertificates(ctx context.Context, token, envID string) ([]service.CertificateStatus, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListCertificates(ctx, actor, envID)
}

func (b *Backend) ListDomains(ctx context.Context, token, appID string) ([]store.Domain, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListDomains(ctx, actor, appID)
}

// ── Git connections ─────────────────────────────────────────────────────────

func (b *Backend) ListGitConnections(ctx context.Context, token, _ string) ([]store.GitConnection, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListGitConnections(ctx, actor)
}

// ── Registries ──────────────────────────────────────────────────────────────

func (b *Backend) ListRegistries(ctx context.Context, token, _ string) ([]store.Registry, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListRegistries(ctx, actor)
}

// ── Webhooks ────────────────────────────────────────────────────────────────

func (b *Backend) ListWebhooks(ctx context.Context, token, _ string) ([]store.Webhook, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListWebhooks(ctx, actor)
}

func (b *Backend) CreateWebhook(ctx context.Context, token, idemKey, _ string, input service.WebhookInput) (*store.Webhook, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	value, err := b.svc.CreateWebhook(ctx, actor, idemKey, input)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (b *Backend) DeleteWebhook(ctx context.Context, token, idemKey, whID string) error {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return err
	}
	return b.svc.DeleteWebhook(ctx, actor, idemKey, whID)
}

// ── API tokens ──────────────────────────────────────────────────────────────

func (b *Backend) ListAPITokens(ctx context.Context, token string) ([]store.APIToken, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListAPITokens(ctx, actor)
}

// CreateAPIToken returns the one-time plaintext secret; the UI displays it and
// lists tokens via ListAPITokens.
func (b *Backend) CreateAPIToken(ctx context.Context, token, idemKey, name string, scopes []string) (string, error) {
	principal, err := b.auth.AuthenticateToken(ctx, token)
	if err != nil {
		return "", ErrUnauthorized
	}
	if principal.Kind != auth.CredentialKindSession {
		return "", service.ErrForbidden
	}
	created, err := b.auth.CreateIdempotentAPIToken(ctx, principal, idemKey, auth.CreateTokenParams{Name: name, Scopes: scopes})
	if err != nil {
		return "", err
	}
	return created.Secret, nil
}

func (b *Backend) RevokeAPIToken(ctx context.Context, token, idemKey, tokenID string) error {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return err
	}
	_, err = b.svc.RevokeAPIToken(ctx, actor, idemKey, tokenID)
	return err
}

// ── Audit events ────────────────────────────────────────────────────────────

// auditPageSize matches the page size the UI requested from the REST API.
const auditPageSize = 50

// ListAuditEvents keeps real pagination (the audit page renders it), with an
// offset-based page token: producer and consumer are both this backend now, so
// the REST cursor format is no longer needed. The service bounds the org scan
// (maxAuditEventScan), same as before.
func (b *Backend) ListAuditEvents(ctx context.Context, token, _, pageToken string) ([]store.AuditEventRecord, string, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, "", err
	}
	offset := 0
	if pageToken != "" {
		offset, err = strconv.Atoi(pageToken)
		if err != nil || offset < 0 {
			return nil, "", service.ErrInvalidArgument
		}
	}
	values, err := b.svc.ListAuditEvents(ctx, actor, service.AuditEventFilter{})
	if err != nil {
		return nil, "", err
	}
	if offset > len(values) {
		offset = len(values)
	}
	page := values[offset:]
	next := ""
	if len(page) > auditPageSize {
		page = page[:auditPageSize]
		next = strconv.Itoa(offset + auditPageSize)
	}
	return page, next, nil
}

// ── Observability ───────────────────────────────────────────────────────────

func (b *Backend) QueryLogs(ctx context.Context, token, appID, envID, cursor, query string, limit int) ([]observability.LogEntry, string, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, "", err
	}
	page, err := b.svc.QueryApplicationLogs(ctx, actor, service.LogQueryInput{
		AppID: appID, EnvironmentID: envID, Cursor: cursor, Query: query, Limit: limit,
	})
	if err != nil {
		return nil, "", err
	}
	return page.Entries, page.NextCursor, nil
}

// ── Reconciliation ──────────────────────────────────────────────────────────

func (b *Backend) ListReconciliations(ctx context.Context, token, _ string) ([]store.ReconciliationRun, error) {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return nil, err
	}
	return b.svc.ListReconciliations(ctx, actor, nil, nil)
}

func (b *Backend) StartReconciliation(ctx context.Context, token, idemKey, orgID string, dryRun bool) error {
	actor, err := b.actor(ctx, token)
	if err != nil {
		return err
	}
	_, err = b.svc.CreateReconciliation(ctx, actor, idemKey, service.ReconciliationInput{
		ScopeKind: "org", ScopeID: orgID, DryRun: &dryRun,
	})
	return err
}

// ── Update-mask mechanics ───────────────────────────────────────────────────

// appInputFromStore lifts a stored app into a full update input — the in-process
// equivalent of the REST handler merging the masked proto over applicationProto(current).
func appInputFromStore(v store.App) service.AppInput {
	runtimeClass := v.RuntimeClass
	return service.AppInput{
		ProjectID: v.ProjectID, Name: v.Name, Slug: v.Slug, HostnameLabel: v.HostnameLabel,
		RegistryID: v.RegistryID, ImageRepository: v.ImageRepository, GitHubWorkflowPath: stringPtrField(v.GitHubWorkflowPath),
		ContainerPort: v.ContainerPort, HealthCheckPath: v.HealthCheckPath, ScalingMode: v.ScalingMode,
		MinReplicas: v.MinReplicas, MaxReplicas: v.MaxReplicas, HPATargetCPUPercent: v.HPATargetCPUPercent,
		KEDACronSchedules:             append([]store.KEDACronSchedule(nil), v.KEDACronSchedules...),
		KEDAHTTPTargetPendingRequests: v.KEDAHTTPTargetPendingRequests,
		CPURequestMillicores:          v.CPURequestMillicores, CPULimitMillicores: v.CPULimitMillicores,
		MemoryRequestMiB: v.MemoryRequestMiB, MemoryLimitMiB: v.MemoryLimitMiB,
		PreviewsEnabled: v.PreviewsEnabled, MaxPreviewEnvironments: v.MaxPreviewEnvironments,
		PreviewIdleTTLHours: v.PreviewIdleTTLHours, DeployTimeoutSeconds: v.DeployTimeoutSeconds,
		ReleaseRetentionCount: v.ReleaseRetentionCount, RuntimeClass: &runtimeClass,
	}
}

// applyAppField applies one field-mask path. The allowed set matches the REST
// UpdateApplication mask (keda_cron_schedules excluded: the UI never sent it
// and it has no scalar form value). The service validates the values.
func applyAppField(input *service.AppInput, path string, value any) error {
	switch path {
	case "name":
		input.Name = stringField(value)
	case "slug":
		input.Slug = stringField(value)
	case "hostname_label":
		input.HostnameLabel = stringField(value)
	case "health_check_path":
		input.HealthCheckPath = stringField(value)
	case "github_workflow_path":
		input.GitHubWorkflowPath = stringPtrField(value)
	case "image_repository":
		input.ImageRepository = stringPtrField(value)
	case "registry_id":
		input.RegistryID = stringPtrField(value)
	case "scaling_mode":
		input.ScalingMode = stringField(value)
	case "runtime_class":
		class := stringField(value)
		input.RuntimeClass = &class
	case "container_port":
		input.ContainerPort = intField(value)
	case "min_replicas":
		input.MinReplicas = intField(value)
	case "max_replicas":
		input.MaxReplicas = intField(value)
	case "hpa_target_cpu_percent":
		input.HPATargetCPUPercent = intField(value)
	case "keda_http_target_pending_requests":
		input.KEDAHTTPTargetPendingRequests = intField(value)
	case "cpu_request_millicores":
		input.CPURequestMillicores = intField(value)
	case "cpu_limit_millicores":
		input.CPULimitMillicores = intField(value)
	case "memory_request_mib":
		input.MemoryRequestMiB = intField(value)
	case "memory_limit_mib":
		input.MemoryLimitMiB = intField(value)
	case "max_preview_environments":
		input.MaxPreviewEnvironments = intField(value)
	case "preview_idle_ttl_hours":
		input.PreviewIdleTTLHours = intField(value)
	case "deploy_timeout_seconds":
		input.DeployTimeoutSeconds = intField(value)
	case "release_retention_count":
		input.ReleaseRetentionCount = intField(value)
	case "previews_enabled":
		input.PreviewsEnabled = boolField(value)
	default:
		return service.ErrInvalidArgument
	}
	return nil
}

func stringField(value any) string {
	v, _ := value.(string)
	return v
}

func stringPtrField(value any) *string {
	v := stringField(value)
	if v == "" {
		return nil
	}
	return &v
}

func intField(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func boolField(value any) bool {
	v, _ := value.(bool)
	return v
}
