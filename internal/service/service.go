package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cyclopus-dev/cyclopus/internal/secretref"
	"github.com/cyclopus-dev/cyclopus/internal/store"
	"github.com/cyclopus-dev/cyclopus/internal/webhook"
	"github.com/google/uuid"
	cronparser "github.com/robfig/cron/v3"
)

var (
	ErrInvalidArgument = errors.New("service: invalid argument")
	ErrForbidden       = errors.New("service: forbidden")
	ErrConflict        = errors.New("service: conflict")
	ErrInProgress      = errors.New("service: operation in progress")
	slugPattern        = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	envKeyPattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,252}$`)
	commitPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	cronShapePattern   = regexp.MustCompile(`^\S+( \S+){4}$`)
	traceIDPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8,64}$`)
)

const (
	idempotencyLease     = time.Minute
	idempotencyRetention = 24 * time.Hour
)

// productionHostname assembles the default production hostname for an app as
// hostnameLabel + "." + suffix. Returns an error when suffix is empty, which
// happens when the operator has not configured a hostname suffix and must
// supply an explicit hostname at environment creation time.
func productionHostname(hostnameLabel, suffix string) (string, error) {
	if suffix == "" {
		return "", fmt.Errorf("kubernetes.preview_hostname_suffix is not configured; provide an explicit hostname")
	}
	if hostnameLabel == "" {
		return "", fmt.Errorf("hostname_label is required")
	}
	return hostnameLabel + "." + suffix, nil
}

type ActorContext struct {
	UserID   string
	OrgID    string
	Operator bool
}

type Service struct {
	store                    *store.Store
	now                      func() time.Time
	previewSlugs             PreviewSlugGenerator
	logger                   *slog.Logger
	telemetry                TelemetryProvider
	certReader               CertReader
	dnsResolver              DNSResolver
	ingressTarget            string
	hostnameProvider         string
	customDomainProvisioner  CustomDomainProvisioner
	certManagerClusterIssuer string
}

type Option func(*Service)

// WithTelemetry wires the observability provider used by the tenant-scoped
// logs/traces reads and the operator-level metrics read. When nil, those RPCs
// report telemetry as unavailable rather than panicking.
func WithTelemetry(provider TelemetryProvider) Option {
	return func(s *Service) {
		if provider != nil {
			s.telemetry = provider
		}
	}
}

// WithLogger wires the redacting structured logger the service uses to emit the
// stable §8.2 control-plane log events. The default is slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithDNSResolver overrides the DNS resolver used for custom-domain verification
// (§7.5). The default wraps net.DefaultResolver; tests inject a fake.
func WithDNSResolver(resolver DNSResolver) Option {
	return func(s *Service) {
		if resolver != nil {
			s.dnsResolver = resolver
		}
	}
}

// WithIngressTarget sets the expected CNAME target that subdomain custom domains
// must resolve to, and the apex A-record guidance value returned to callers
// (§7.5). This is the platform ingress hostname that custom subdomains CNAME to,
// a role distinct from the preview hostname suffix (though it defaults to it).
func WithIngressTarget(target string) Option {
	return func(s *Service) {
		s.ingressTarget = strings.ToLower(strings.TrimSpace(target))
	}
}

// WithHostnameSuffix sets the DNS suffix used to auto-generate production and
// preview environment hostnames (§7.5). Operators set this to their ingress
// IP in sslip.io format (e.g. "203-0-113-7.sslip.io") or to their own
// wildcard domain (e.g. "apps.example.com"). When empty, CreateProductionEnvironment
// requires the caller to supply an explicit hostname.
func WithHostnameSuffix(suffix string) Option {
	return func(s *Service) {
		s.hostnameProvider = strings.ToLower(strings.TrimSpace(suffix))
	}
}

// WithCertManagerClusterIssuer sets the cert-manager ClusterIssuer name the
// service passes to the CustomDomainProvisioner so a verified custom domain's
// Ingress is annotated for TLS issuance (§7.5). Empty means no issuer annotation
// (manual TLS / no cert-manager configured).
func WithCertManagerClusterIssuer(issuer string) Option {
	return func(s *Service) {
		s.certManagerClusterIssuer = strings.TrimSpace(issuer)
	}
}

func New(dataStore *store.Store, opts ...Option) *Service {
	s := &Service{store: dataStore, now: time.Now, previewSlugs: randomPreviewSlugGenerator{source: rand.Reader}, logger: slog.Default(), dnsResolver: stdlibDNSResolver{}}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func (s *Service) CurrentUser(ctx context.Context, actor ActorContext) (store.User, error) {
	if err := validateActor(actor); err != nil {
		return store.User{}, err
	}
	return s.store.GetCurrentUser(ctx, actor.OrgID, actor.UserID)
}
func (s *Service) CurrentOrganization(ctx context.Context, actor ActorContext) (store.Organization, error) {
	if err := validateActor(actor); err != nil {
		return store.Organization{}, err
	}
	return s.store.GetOrganization(ctx, actor.OrgID)
}
func (s *Service) ListProjects(ctx context.Context, actor ActorContext, includeDeleted bool) ([]store.Project, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListProjects(ctx, actor.OrgID, includeDeleted)
}
func (s *Service) GetProject(ctx context.Context, actor ActorContext, id string) (store.Project, error) {
	if err := validateActor(actor); err != nil {
		return store.Project{}, err
	}
	return s.store.GetProject(ctx, actor.OrgID, id)
}
func (s *Service) ListApps(ctx context.Context, actor ActorContext, projectID string, includeDeleted bool) ([]store.App, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListApps(ctx, actor.OrgID, projectID, includeDeleted)
}
func (s *Service) GetApp(ctx context.Context, actor ActorContext, id string) (store.App, error) {
	if err := validateActor(actor); err != nil {
		return store.App{}, err
	}
	return s.store.GetApp(ctx, actor.OrgID, id)
}
func (s *Service) ListEnvironments(ctx context.Context, actor ActorContext, appID string, includeTornDown bool) ([]store.Environment, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListEnvironments(ctx, actor.OrgID, appID, includeTornDown)
}
func (s *Service) GetEnvironment(ctx context.Context, actor ActorContext, id string) (store.Environment, error) {
	if err := validateActor(actor); err != nil {
		return store.Environment{}, err
	}
	return s.store.GetEnvironment(ctx, actor.OrgID, id)
}
func (s *Service) ListReleases(ctx context.Context, actor ActorContext, environmentID string, statuses []string) ([]store.Release, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListReleases(ctx, actor.OrgID, environmentID, statuses)
}
func (s *Service) GetRelease(ctx context.Context, actor ActorContext, id string) (store.Release, error) {
	if err := validateActor(actor); err != nil {
		return store.Release{}, err
	}
	return s.store.GetRelease(ctx, actor.OrgID, id)
}

type WebhookInput struct {
	TargetURL string
	Events    []string
	SecretRef string
	Enabled   bool
}

func (s *Service) GetWebhook(ctx context.Context, actor ActorContext, id string) (store.Webhook, error) {
	if err := validateActor(actor); err != nil {
		return store.Webhook{}, err
	}
	return s.store.GetWebhook(ctx, actor.OrgID, id)
}

func (s *Service) ListWebhooks(ctx context.Context, actor ActorContext) ([]store.Webhook, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListWebhooks(ctx, actor.OrgID)
}

func (s *Service) CreateWebhook(ctx context.Context, actor ActorContext, key string, input WebhookInput) (store.Webhook, error) {
	if err := validateActor(actor); err != nil {
		return store.Webhook{}, err
	}
	if err := validateWebhook(input); err != nil {
		return store.Webhook{}, err
	}
	var replay store.Webhook
	claimed, err := s.claim(ctx, actor, "webhook.create", key, input, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	value := store.Webhook{ID: newID(), OrgID: actor.OrgID, TargetURL: input.TargetURL, Events: append([]string(nil), input.Events...), SecretRef: input.SecretRef}
	body, _ := json.Marshal(value)
	idem := s.completion(actor, "webhook.create", key, input, body, 201)
	value, err = s.store.CreateWebhook(ctx, store.SaveWebhookParams{Webhook: value, Idempotency: idem, Audit: audit(actor)})
	return value, mapError(err)
}

func (s *Service) UpdateWebhook(ctx context.Context, actor ActorContext, key, id string, input WebhookInput) (store.Webhook, error) {
	if err := validateActor(actor); err != nil {
		return store.Webhook{}, err
	}
	if err := validateWebhook(input); err != nil {
		return store.Webhook{}, err
	}
	request := struct {
		ID    string
		Input WebhookInput
	}{id, input}
	var replay store.Webhook
	claimed, err := s.claim(ctx, actor, "webhook.update", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	value := store.Webhook{ID: id, OrgID: actor.OrgID, TargetURL: input.TargetURL, Events: append([]string(nil), input.Events...), SecretRef: input.SecretRef}
	body, _ := json.Marshal(value)
	idem := s.completion(actor, "webhook.update", key, request, body, 200)
	value, err = s.store.UpdateWebhook(ctx, store.SaveWebhookParams{Webhook: value, Idempotency: idem, Audit: audit(actor)}, input.Enabled)
	return value, mapError(err)
}

func (s *Service) DeleteWebhook(ctx context.Context, actor ActorContext, key, id string) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	request := struct{ ID string }{id}
	var replay struct{}
	claimed, err := s.claim(ctx, actor, "webhook.delete", key, request, &replay)
	if err != nil || !claimed {
		return err
	}
	idem := s.completion(actor, "webhook.delete", key, request, json.RawMessage(`{}`), 200)
	idem.OrgID = actor.OrgID
	return mapError(s.store.DeleteWebhook(ctx, actor.OrgID, id, idem, audit(actor)))
}

func validateWebhook(input WebhookInput) error {
	parsed, err := url.Parse(input.TargetURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return ErrInvalidArgument
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ErrInvalidArgument
	}
	if address := net.ParseIP(host); address != nil && !webhook.PublicIPAllowed(address) {
		return ErrInvalidArgument
	}
	if _, err := secretref.Parse(input.SecretRef); err != nil {
		return ErrInvalidArgument
	}
	allowed := make(map[string]bool, len(store.WebhookEvents))
	for _, event := range store.WebhookEvents {
		allowed[event] = true
	}
	seen := map[string]bool{}
	for _, event := range input.Events {
		if !allowed[event] || seen[event] {
			return ErrInvalidArgument
		}
		seen[event] = true
	}
	return nil
}

type ProjectInput struct {
	Name, Slug      string
	GitConnectionID *string
}

func (s *Service) CreateProject(ctx context.Context, actor ActorContext, key string, input ProjectInput) (store.Project, error) {
	if err := validateActor(actor); err != nil {
		return store.Project{}, err
	}
	if err := validateProject(input); err != nil {
		return store.Project{}, err
	}
	id := newID()
	response := store.Project{ID: id, OrgID: actor.OrgID, Name: input.Name, Slug: input.Slug, GitConnectionID: input.GitConnectionID}
	var replay store.Project
	claimed, err := s.claim(ctx, actor, "project.create", key, input, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	body, _ := json.Marshal(response)
	idem := s.completion(actor, "project.create", key, input, body, 201)
	value, err := s.store.CreateProject(ctx, store.SaveProjectParams{Project: response, Idempotency: idem, Audit: audit(actor)})
	return value, mapError(err)
}
func (s *Service) UpdateProject(ctx context.Context, actor ActorContext, key, id string, input ProjectInput) (store.Project, error) {
	if err := validateActor(actor); err != nil {
		return store.Project{}, err
	}
	if err := validateProject(input); err != nil {
		return store.Project{}, err
	}
	request := struct {
		ID    string
		Input ProjectInput
	}{id, input}
	var replay store.Project
	claimed, err := s.claim(ctx, actor, "project.update", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	value := store.Project{ID: id, OrgID: actor.OrgID, Name: input.Name, Slug: input.Slug, GitConnectionID: input.GitConnectionID}
	body, _ := json.Marshal(value)
	idem := s.completion(actor, "project.update", key, request, body, 200)
	value, err = s.store.UpdateProject(ctx, store.SaveProjectParams{Project: value, Idempotency: idem, Audit: audit(actor)})
	return value, mapError(err)
}

func (s *Service) DeleteProject(ctx context.Context, actor ActorContext, key, id string, externalCredentialsRevoked bool) (AcceptedOperation, error) {
	return s.deleteResource(ctx, actor, key, id, externalCredentialsRevoked, "project.delete", "project", s.store.TombstoneProject)
}

type AppInput struct {
	ProjectID, Name, Slug, HostnameLabel                                                     string
	RegistryID, ImageRepository, CIWorkflowID                                                *string
	ContainerPort                                                                            int
	HealthCheckPath, ScalingMode                                                             string
	MinReplicas, MaxReplicas, HPATargetCPUPercent                                            int
	KEDACronSchedules                                                                        []store.KEDACronSchedule
	KEDAHTTPTargetPendingRequests                                                            int
	CPURequestMillicores, CPULimitMillicores, MemoryRequestMiB, MemoryLimitMiB               int
	PreviewsEnabled                                                                          bool
	MaxPreviewEnvironments, PreviewIdleTTLHours, DeployTimeoutSeconds, ReleaseRetentionCount int
	RuntimeClass                                                                             *string
}

func DefaultAppInput() AppInput {
	return AppInput{ContainerPort: 8080, HealthCheckPath: "/", ScalingMode: "hpa", MinReplicas: 1, MaxReplicas: 3, HPATargetCPUPercent: 70, KEDAHTTPTargetPendingRequests: 100, CPURequestMillicores: 100, CPULimitMillicores: 1000, MemoryRequestMiB: 128, MemoryLimitMiB: 512, PreviewsEnabled: true, MaxPreviewEnvironments: 10, PreviewIdleTTLHours: 168, DeployTimeoutSeconds: 600, ReleaseRetentionCount: 5}
}

func normalizeAppInput(input AppInput) AppInput {
	if input.KEDAHTTPTargetPendingRequests == 0 {
		input.KEDAHTTPTargetPendingRequests = DefaultAppInput().KEDAHTTPTargetPendingRequests
	}
	return input
}

func (s *Service) CreateApp(ctx context.Context, actor ActorContext, key string, input AppInput) (store.App, error) {
	if err := validateActor(actor); err != nil {
		return store.App{}, err
	}
	input = normalizeAppInput(input)
	runtimeClass := "gvisor"
	if input.RuntimeClass != nil {
		if !actor.Operator && *input.RuntimeClass != runtimeClass {
			return store.App{}, ErrForbidden
		}
		runtimeClass = *input.RuntimeClass
	}
	if err := validateApp(input, runtimeClass); err != nil {
		return store.App{}, err
	}
	id := newID()
	request := input
	var replay store.App
	claimed, err := s.claim(ctx, actor, "app.create", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	value := appValue(actor.OrgID, id, input, runtimeClass)
	body, _ := json.Marshal(value)
	idem := s.completion(actor, "app.create", key, request, body, 201)
	value, err = s.store.CreateApp(ctx, store.SaveAppParams{App: value, Idempotency: idem, Audit: audit(actor)})
	return value, mapError(err)
}
func (s *Service) UpdateApp(ctx context.Context, actor ActorContext, key, id string, input AppInput) (store.App, error) {
	if err := validateActor(actor); err != nil {
		return store.App{}, err
	}
	input = normalizeAppInput(input)
	current, err := s.store.GetApp(ctx, actor.OrgID, id)
	if err != nil {
		return store.App{}, mapError(err)
	}
	runtimeClass := current.RuntimeClass
	if input.RuntimeClass != nil && *input.RuntimeClass != current.RuntimeClass {
		if !actor.Operator {
			return store.App{}, ErrForbidden
		}
		runtimeClass = *input.RuntimeClass
	}
	if err := validateApp(input, runtimeClass); err != nil {
		return store.App{}, err
	}
	request := struct {
		ID    string
		Input AppInput
	}{id, input}
	var replay store.App
	claimed, err := s.claim(ctx, actor, "app.update", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	value := appValue(actor.OrgID, id, input, runtimeClass)
	body, _ := json.Marshal(value)
	idem := s.completion(actor, "app.update", key, request, body, 200)
	value, err = s.store.UpdateApp(ctx, store.SaveAppParams{App: value, Idempotency: idem, Audit: audit(actor)})
	return value, mapError(err)
}

func (s *Service) DeleteApp(ctx context.Context, actor ActorContext, key, id string, externalCredentialsRevoked bool) (AcceptedOperation, error) {
	return s.deleteResource(ctx, actor, key, id, externalCredentialsRevoked, "app.delete", "app", s.store.TombstoneApp)
}

func (s *Service) DeleteOrganization(ctx context.Context, actor ActorContext, key string, externalCredentialsRevoked bool) (AcceptedOperation, error) {
	return s.deleteResource(ctx, actor, key, actor.OrgID, externalCredentialsRevoked, "org.delete", "org", s.store.TombstoneOrg)
}

func (s *Service) deleteResource(ctx context.Context, actor ActorContext, key, id string, externalCredentialsRevoked bool, scope, resourceType string, tombstone func(context.Context, store.TombstoneParams) error) (AcceptedOperation, error) {
	if err := validateActor(actor); err != nil {
		return AcceptedOperation{}, err
	}
	request := struct {
		ID                         string
		ExternalCredentialsRevoked bool
	}{id, externalCredentialsRevoked}
	var replay AcceptedOperation
	claimed, err := s.claim(ctx, actor, scope, key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	response := AcceptedOperation{OperationID: id, ResourceID: id, Status: "succeeded"}
	body, _ := json.Marshal(response)
	idem := s.completion(actor, scope, key, request, body, 200)
	err = tombstone(ctx, store.TombstoneParams{OrgID: actor.OrgID, ID: id, ExternalCredentialsRevoked: externalCredentialsRevoked, Audit: audit(actor), Idempotency: &idem})
	_ = resourceType
	return response, mapError(err)
}
func appValue(orgID, id string, i AppInput, runtimeClass string) store.App {
	return store.App{ID: id, OrgID: orgID, ProjectID: i.ProjectID, Name: i.Name, Slug: i.Slug, HostnameLabel: i.HostnameLabel, RegistryID: i.RegistryID, ImageRepository: i.ImageRepository, CIWorkflowID: i.CIWorkflowID, ContainerPort: i.ContainerPort, HealthCheckPath: i.HealthCheckPath, ScalingMode: i.ScalingMode, MinReplicas: i.MinReplicas, MaxReplicas: i.MaxReplicas, HPATargetCPUPercent: i.HPATargetCPUPercent, KEDACronSchedules: i.KEDACronSchedules, KEDAHTTPTargetPendingRequests: i.KEDAHTTPTargetPendingRequests, CPURequestMillicores: i.CPURequestMillicores, CPULimitMillicores: i.CPULimitMillicores, MemoryRequestMiB: i.MemoryRequestMiB, MemoryLimitMiB: i.MemoryLimitMiB, PreviewsEnabled: i.PreviewsEnabled, MaxPreviewEnvironments: i.MaxPreviewEnvironments, PreviewIdleTTLHours: i.PreviewIdleTTLHours, DeployTimeoutSeconds: i.DeployTimeoutSeconds, ReleaseRetentionCount: i.ReleaseRetentionCount, RuntimeClass: runtimeClass}
}

type AcceptedOperation struct {
	OperationID string `json:"operation_id,omitempty"`
	ResourceID  string `json:"resource_id"`
	Status      string `json:"status"`
}
type DeployInput struct {
	ImageRef       string
	GitCommitSHA   string
	ConfigSnapshot json.RawMessage
}

func (s *Service) AcceptDeploy(ctx context.Context, actor ActorContext, key, environmentID string, input DeployInput) (AcceptedOperation, error) {
	if err := validateActor(actor); err != nil {
		return AcceptedOperation{}, err
	}
	environment, err := s.store.GetEnvironment(ctx, actor.OrgID, environmentID)
	if err != nil {
		return AcceptedOperation{}, mapError(err)
	}
	if environment.LifecycleStatus != "ready" || environment.PlannedHostname == nil {
		return AcceptedOperation{}, store.ErrFailedPrecondition
	}
	app, err := s.store.GetApp(ctx, actor.OrgID, environment.AppID)
	if err != nil {
		return AcceptedOperation{}, mapError(err)
	}
	if err := ValidateImageRef(input.ImageRef, app.RegistryURL, app.ImageRepository); err != nil {
		return AcceptedOperation{}, err
	}
	request := struct {
		EnvironmentID string
		Input         DeployInput
	}{environmentID, input}
	var replay AcceptedOperation
	claimed, err := s.claim(ctx, actor, "deploy.accept", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	id := newID()
	if len(input.ConfigSnapshot) == 0 {
		input.ConfigSnapshot, err = deploymentSnapshot(environment, app, input.ImageRef, id)
		if err != nil {
			return AcceptedOperation{}, err
		}
	}
	snapshot, hash, err := normalizeSnapshot(input.ConfigSnapshot)
	if err != nil {
		return AcceptedOperation{}, err
	}
	response := AcceptedOperation{OperationID: id, ResourceID: id, Status: "queued"}
	body, _ := json.Marshal(response)
	idem := s.completion(actor, "deploy.accept", key, request, body, 202)
	var gitCommitSHA *string
	if strings.TrimSpace(input.GitCommitSHA) != "" {
		value := strings.TrimSpace(input.GitCommitSHA)
		gitCommitSHA = &value
	}
	_, err = s.store.AcceptRelease(ctx, store.AcceptReleaseParams{ID: id, OrgID: actor.OrgID, EnvironmentID: environmentID, ImageRef: input.ImageRef, GitCommitSHA: gitCommitSHA, Kind: "deploy", TriggeredByUserID: &actor.UserID, ConfigSnapshot: snapshot, ConfigHash: hash, InheritCurrentEnv: true, Idempotency: idem, Audit: audit(actor)})
	return response, mapError(err)
}
func (s *Service) AcceptRollback(ctx context.Context, actor ActorContext, key, environmentID, targetReleaseID string) (AcceptedOperation, error) {
	if err := validateActor(actor); err != nil {
		return AcceptedOperation{}, err
	}
	request := struct{ EnvironmentID, TargetReleaseID string }{environmentID, targetReleaseID}
	var replay AcceptedOperation
	claimed, err := s.claim(ctx, actor, "rollback.accept", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	id := newID()
	response := AcceptedOperation{OperationID: id, ResourceID: id, Status: "queued"}
	body, _ := json.Marshal(response)
	idem := s.completion(actor, "rollback.accept", key, request, body, 202)
	_, err = s.store.AcceptRelease(ctx, store.AcceptReleaseParams{ID: id, OrgID: actor.OrgID, EnvironmentID: environmentID, Kind: "rollback", TriggeredByUserID: &actor.UserID, RollbackTargetID: &targetReleaseID, Idempotency: idem, Audit: audit(actor)})
	return response, mapError(err)
}

type EnvVarInput struct{ Key, SecretRef string }

func (s *Service) GetEnvironmentVariables(ctx context.Context, actor ActorContext, environmentID string) (store.EnvironmentVariableRevision, error) {
	if err := validateActor(actor); err != nil {
		return store.EnvironmentVariableRevision{}, err
	}
	return s.store.GetCurrentEnvironmentVariableRevision(ctx, actor.OrgID, environmentID)
}

func (s *Service) SetEnvironmentVariables(ctx context.Context, actor ActorContext, key, environmentID string, inputs []EnvVarInput) (AcceptedOperation, error) {
	if err := validateActor(actor); err != nil {
		return AcceptedOperation{}, err
	}
	environment, err := s.store.GetEnvironment(ctx, actor.OrgID, environmentID)
	if err != nil {
		return AcceptedOperation{}, mapError(err)
	}
	if environment.TornDownAt != nil {
		return AcceptedOperation{}, store.ErrNotFound
	}
	variables, err := validateEnvironmentVariables(environment.Namespace, inputs)
	if err != nil {
		return AcceptedOperation{}, err
	}
	sort.Slice(variables, func(i, j int) bool { return variables[i].Key < variables[j].Key })
	request := struct {
		EnvironmentID string
		Variables     []store.EnvironmentVariableValue
	}{environmentID, variables}
	var replay AcceptedOperation
	claimed, err := s.claim(ctx, actor, "environment.env_vars.replace", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	releaseID := newID()
	response := AcceptedOperation{OperationID: releaseID, ResourceID: releaseID, Status: "queued"}
	body, _ := json.Marshal(response)
	idem := s.completion(actor, "environment.env_vars.replace", key, request, body, 202)
	_, err = s.store.ReplaceEnvVarsAndCreateRelease(ctx, store.ReplaceEnvVarsParams{OrgID: actor.OrgID, EnvironmentID: environmentID, ReleaseID: releaseID, Variables: variables, TriggeredByUserID: &actor.UserID, Idempotency: idem, Audit: audit(actor)})
	return response, mapError(err)
}

func (s *Service) ChangeEnvironmentVariable(ctx context.Context, actor ActorContext, key, environmentID string, input EnvVarInput) (AcceptedOperation, error) {
	if err := validateActor(actor); err != nil {
		return AcceptedOperation{}, err
	}
	environment, err := s.store.GetEnvironment(ctx, actor.OrgID, environmentID)
	if err != nil {
		return AcceptedOperation{}, mapError(err)
	}
	if environment.TornDownAt != nil {
		return AcceptedOperation{}, store.ErrNotFound
	}
	if _, err := validateEnvironmentVariables(environment.Namespace, []EnvVarInput{input}); err != nil {
		return AcceptedOperation{}, ErrInvalidArgument
	}
	request := struct {
		EnvironmentID string
		Input         EnvVarInput
	}{environmentID, input}
	var replay AcceptedOperation
	claimed, err := s.claim(ctx, actor, "environment.env_var.upsert", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	releaseID := newID()
	response := AcceptedOperation{OperationID: releaseID, ResourceID: releaseID, Status: "queued"}
	body, _ := json.Marshal(response)
	idem := s.completion(actor, "environment.env_var.upsert", key, request, body, 202)
	_, err = s.store.MutateEnvVarAndCreateRelease(ctx, store.MutateEnvVarParams{OrgID: actor.OrgID, EnvironmentID: environmentID, EnvVarID: newID(), Key: input.Key, SecretRef: input.SecretRef, ReleaseID: releaseID, TriggeredByUserID: &actor.UserID, Idempotency: idem, Audit: audit(actor)})
	return response, mapError(err)
}

func validateEnvironmentVariables(namespace string, inputs []EnvVarInput) ([]store.EnvironmentVariableValue, error) {
	seen := map[string]bool{}
	variables := make([]store.EnvironmentVariableValue, 0, len(inputs))
	for _, input := range inputs {
		parsed, err := secretref.Parse(input.SecretRef)
		if !envKeyPattern.MatchString(input.Key) || err != nil || parsed.Namespace != namespace || seen[input.Key] {
			return nil, ErrInvalidArgument
		}
		seen[input.Key] = true
		variables = append(variables, store.EnvironmentVariableValue{Key: input.Key, SecretRef: input.SecretRef, IsSecret: true})
	}
	return variables, nil
}

func (s *Service) ListAPITokens(ctx context.Context, actor ActorContext) ([]store.APIToken, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListAPITokens(ctx, actor.OrgID, actor.UserID)
}
func (s *Service) RevokeAPIToken(ctx context.Context, actor ActorContext, key, tokenID string) (AcceptedOperation, error) {
	if err := validateActor(actor); err != nil {
		return AcceptedOperation{}, err
	}
	request := struct{ TokenID string }{tokenID}
	var replay AcceptedOperation
	claimed, err := s.claim(ctx, actor, "api_token.revoke", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	response := AcceptedOperation{ResourceID: tokenID, Status: "accepted"}
	body, _ := json.Marshal(response)
	idem := s.completion(actor, "api_token.revoke", key, request, body, 200)
	err = s.store.AcceptRevokeAPIToken(ctx, store.AcceptRevokeAPITokenParams{OrgID: actor.OrgID, UserID: actor.UserID, TokenID: tokenID, RevokedAt: s.now(), Idempotency: idem, Audit: audit(actor)})
	return response, mapError(err)
}

type ReconciliationInput struct {
	ScopeKind, ScopeID             string
	ScopeAppID, ScopeEnvironmentID *string
	Categories                     []string
	DryRun                         *bool
	Prune                          bool
}

func (s *Service) CreateReconciliation(ctx context.Context, actor ActorContext, key string, input ReconciliationInput) (store.ReconciliationRun, error) {
	if err := validateActor(actor); err != nil {
		return store.ReconciliationRun{}, err
	}
	if input.ScopeKind != "org" && input.ScopeKind != "app" && input.ScopeKind != "environment" || strings.TrimSpace(input.ScopeID) == "" {
		return store.ReconciliationRun{}, ErrInvalidArgument
	}
	switch input.ScopeKind {
	case "org":
		if input.ScopeID != actor.OrgID || input.ScopeAppID != nil || input.ScopeEnvironmentID != nil {
			return store.ReconciliationRun{}, ErrInvalidArgument
		}
	case "app":
		if input.ScopeAppID == nil || *input.ScopeAppID != input.ScopeID || input.ScopeEnvironmentID != nil {
			return store.ReconciliationRun{}, ErrInvalidArgument
		}
		if _, err := s.store.GetApp(ctx, actor.OrgID, input.ScopeID); err != nil {
			return store.ReconciliationRun{}, mapError(err)
		}
	case "environment":
		if input.ScopeEnvironmentID == nil || *input.ScopeEnvironmentID != input.ScopeID {
			return store.ReconciliationRun{}, ErrInvalidArgument
		}
		environment, err := s.store.GetEnvironment(ctx, actor.OrgID, input.ScopeID)
		if err != nil {
			return store.ReconciliationRun{}, mapError(err)
		}
		input.ScopeAppID = &environment.AppID
	}
	seenCategories := make(map[string]struct{}, len(input.Categories))
	for _, category := range input.Categories {
		if !validReconciliationCategory(category) {
			return store.ReconciliationRun{}, ErrInvalidArgument
		}
		if _, exists := seenCategories[category]; exists {
			return store.ReconciliationRun{}, ErrInvalidArgument
		}
		seenCategories[category] = struct{}{}
	}
	if input.Prune && (input.DryRun == nil || *input.DryRun) {
		return store.ReconciliationRun{}, ErrInvalidArgument
	}
	request := input
	var replay store.ReconciliationRun
	claimed, err := s.claim(ctx, actor, "reconciliation.create", key, request, &replay)
	if err != nil || !claimed {
		return replay, err
	}
	id := newID()
	dryRun := true
	if input.DryRun != nil {
		dryRun = *input.DryRun
	}
	response := store.ReconciliationRun{ID: id, OrgID: actor.OrgID, ActorUserID: &actor.UserID, ScopeKind: input.ScopeKind, ScopeID: input.ScopeID, ScopeAppID: input.ScopeAppID, ScopeEnvironmentID: input.ScopeEnvironmentID, Categories: append([]string(nil), input.Categories...), DryRun: dryRun, Prune: input.Prune, Status: "queued"}
	body, _ := json.Marshal(response)
	idem := s.completion(actor, "reconciliation.create", key, request, body, 202)
	value, err := s.store.CreateReconciliationRun(ctx, store.CreateReconciliationRunParams{ID: id, OrgID: actor.OrgID, ActorUserID: &actor.UserID, ScopeKind: input.ScopeKind, ScopeID: input.ScopeID, ScopeAppID: input.ScopeAppID, ScopeEnvironmentID: input.ScopeEnvironmentID, Categories: input.Categories, DryRun: input.DryRun, Prune: input.Prune, Idempotency: &idem})
	return value, mapError(err)
}
func (s *Service) ReconciliationStatus(ctx context.Context, actor ActorContext, id string) (store.ReconciliationRun, error) {
	if err := validateActor(actor); err != nil {
		return store.ReconciliationRun{}, err
	}
	return s.store.GetReconciliationRun(ctx, actor.OrgID, id)
}

func (s *Service) ListReconciliations(ctx context.Context, actor ActorContext, appID, environmentID *string) ([]store.ReconciliationRun, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListReconciliationRuns(ctx, actor.OrgID, appID, environmentID)
}

func (s *Service) ListReconciliationEvidence(ctx context.Context, actor ActorContext, runID string, afterSequence int64, limit int) ([]store.ReconciliationEvidence, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	return s.store.ListReconciliationEvidence(ctx, actor.OrgID, runID, afterSequence, limit)
}

func validReconciliationCategory(category string) bool {
	switch category {
	case "database", "namespace", "workload", "network_policy", "ingress", "tls", "scaling", "environment_variables", "orchestration":
		return true
	default:
		return false
	}
}

func (s *Service) claim(ctx context.Context, actor ActorContext, scope, key string, request any, replay any) (bool, error) {
	if strings.TrimSpace(key) == "" {
		return false, ErrInvalidArgument
	}
	hash := requestHash(request)
	result, err := s.store.ClaimIdempotency(ctx, store.IdempotencyClaimParams{OrgID: actor.OrgID, Scope: scope, Key: key, RequestHash: hash, Owner: actor.UserID, LeaseDuration: idempotencyLease, Retention: idempotencyRetention})
	if err != nil {
		return false, mapError(err)
	}
	switch result.Disposition {
	case store.IdempotencyClaimed:
		return true, nil
	case store.IdempotencyReplay:
		if err := json.Unmarshal(result.Body, replay); err != nil {
			return false, fmt.Errorf("decode idempotency replay: %w", err)
		}
		return false, nil
	case store.IdempotencyConflict:
		return false, ErrConflict
	default:
		return false, ErrInProgress
	}
}
func (s *Service) completion(actor ActorContext, scope, key string, request any, body json.RawMessage, status int) store.CompleteIdempotencyParams {
	return store.CompleteIdempotencyParams{OrgID: actor.OrgID, Scope: scope, Key: key, RequestHash: requestHash(request), Owner: actor.UserID, Status: "completed", StatusCode: status, Body: body}
}

func validateActor(a ActorContext) error {
	if a.UserID == "" || a.OrgID == "" {
		return ErrForbidden
	}
	return nil
}
func validateProject(i ProjectInput) error {
	if strings.TrimSpace(i.Name) == "" || len(i.Name) > 200 || !slugPattern.MatchString(i.Slug) {
		return ErrInvalidArgument
	}
	return nil
}
func validateApp(i AppInput, runtimeClass string) error {
	if strings.TrimSpace(i.Name) == "" || !slugPattern.MatchString(i.Slug) || !slugPattern.MatchString(i.HostnameLabel) || i.ContainerPort < 1 || i.ContainerPort > 65535 || !strings.HasPrefix(i.HealthCheckPath, "/") {
		return ErrInvalidArgument
	}
	if runtimeClass != "gvisor" && runtimeClass != "runc" {
		return ErrInvalidArgument
	}
	if i.ScalingMode != "hpa" && i.ScalingMode != "keda_cron" && i.ScalingMode != "keda_http" {
		return ErrInvalidArgument
	}
	if i.MinReplicas < 0 || i.MaxReplicas < 1 || i.MaxReplicas < i.MinReplicas || (i.ScalingMode == "hpa" && i.MinReplicas < 1) || i.HPATargetCPUPercent < 1 || i.HPATargetCPUPercent > 100 {
		return ErrInvalidArgument
	}
	if err := validateKEDAAppConfig(i); err != nil {
		return err
	}
	if i.CPURequestMillicores <= 0 || i.CPULimitMillicores < i.CPURequestMillicores || i.MemoryRequestMiB <= 0 || i.MemoryLimitMiB < i.MemoryRequestMiB {
		return ErrInvalidArgument
	}
	if i.MaxPreviewEnvironments < 0 || i.MaxPreviewEnvironments > 1000 || i.PreviewIdleTTLHours <= 0 || i.PreviewIdleTTLHours > 8760 || i.DeployTimeoutSeconds <= 0 || i.DeployTimeoutSeconds > 86400 || i.ReleaseRetentionCount < 1 || i.ReleaseRetentionCount > 1000 {
		return ErrInvalidArgument
	}
	if (i.RegistryID == nil) != (i.ImageRepository == nil) {
		return ErrInvalidArgument
	}
	return nil
}
func validateKEDAAppConfig(i AppInput) error {
	if i.KEDAHTTPTargetPendingRequests < 1 {
		return ErrInvalidArgument
	}
	switch i.ScalingMode {
	case "keda_cron":
		if len(i.KEDACronSchedules) == 0 || len(i.KEDACronSchedules) > 16 {
			return ErrInvalidArgument
		}
		parser := cronparser.NewParser(cronparser.Minute | cronparser.Hour | cronparser.Dom | cronparser.Month | cronparser.Dow)
		seen := make(map[string]struct{}, len(i.KEDACronSchedules))
		for _, schedule := range i.KEDACronSchedules {
			if strings.TrimSpace(schedule.Timezone) == "" || len(schedule.Timezone) > 64 {
				return ErrInvalidArgument
			}
			if _, err := time.LoadLocation(schedule.Timezone); err != nil {
				return ErrInvalidArgument
			}
			if _, err := parser.Parse(schedule.Start); err != nil {
				return ErrInvalidArgument
			}
			if _, err := parser.Parse(schedule.End); err != nil {
				return ErrInvalidArgument
			}
			if schedule.Start == schedule.End {
				return ErrInvalidArgument
			}
			if schedule.DesiredReplicas < 1 {
				return ErrInvalidArgument
			}
			if int(schedule.DesiredReplicas) > i.MaxReplicas {
				return ErrInvalidArgument
			}
			key := schedule.Timezone + "\x00" + schedule.Start + "\x00" + schedule.End
			if _, exists := seen[key]; exists {
				return ErrInvalidArgument
			}
			seen[key] = struct{}{}
		}
	case "keda_http":
		if len(i.KEDACronSchedules) != 0 {
			return ErrInvalidArgument
		}
	}
	return nil
}

func ValidateImageRef(imageRef string, registryURL, imageRepository *string) error {
	if registryURL == nil || imageRepository == nil {
		return fmt.Errorf("%w: app registry is not configured", ErrInvalidArgument)
	}
	parts := strings.Split(imageRef, "@")
	if len(parts) != 2 || !digestPattern.MatchString(parts[1]) {
		return fmt.Errorf("%w: image must use a sha256 digest", ErrInvalidArgument)
	}
	u, err := url.Parse(*registryURL)
	if err != nil {
		return ErrInvalidArgument
	}
	host := u.Host
	if host == "" {
		host = u.Path
	}
	host = strings.TrimSuffix(strings.ToLower(host), "/")
	repository := strings.Trim(strings.TrimSpace(*imageRepository), "/")
	if strings.ToLower(parts[0]) != host+"/"+repository {
		return fmt.Errorf("%w: image registry or repository mismatch", ErrInvalidArgument)
	}
	return nil
}
func normalizeSnapshot(raw json.RawMessage) (json.RawMessage, string, error) {
	envelope, err := store.DecodeReleaseSnapshot(raw)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return store.EncodeReleaseSnapshot(envelope)
}
func requestHash(v any) string {
	encoded, _ := json.Marshal(v)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
func audit(actor ActorContext) *store.AuditEvent {
	return &store.AuditEvent{ID: newID(), ActorUserID: &actor.UserID}
}
func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err)
	}
	return id.String()
}
func deploymentSnapshot(environment store.Environment, app store.App, imageRef, releaseID string) (json.RawMessage, error) {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "cyclopus",
		"app.kubernetes.io/name":       app.Slug,
		"cyclopus.dev/environment-id":  environment.ID,
	}
	selector := map[string]string{"app.kubernetes.io/name": app.Slug, "cyclopus.dev/environment-id": environment.ID}
	replicas := int32(app.MinReplicas)
	scaling := store.ScalingIntent{Version: 1, Name: app.Slug}
	if environment.Kind == "preview" {
		replicas = 1
		scaling.Kind = "none"
		scaling.Name = ""
	} else if app.ScalingMode == "hpa" {
		scaling.Kind = "hpa"
		scaling.HPA = &store.HPAIntent{MinReplicas: int32(app.MinReplicas), MaxReplicas: int32(app.MaxReplicas), TargetCPUPercent: int32(app.HPATargetCPUPercent)}
	} else {
		scaling.Kind = "keda"
		mode := strings.TrimPrefix(app.ScalingMode, "keda_")
		scaling.KEDA = &store.KEDAIntent{Mode: mode, MinReplicas: int32(app.MinReplicas), MaxReplicas: int32(app.MaxReplicas), TargetCPUPercent: int32(app.HPATargetCPUPercent)}
		if mode == "cron" {
			schedules := make([]store.KEDACronScheduleIntent, len(app.KEDACronSchedules))
			for index, schedule := range app.KEDACronSchedules {
				schedules[index] = store.KEDACronScheduleIntent{Timezone: schedule.Timezone, Start: schedule.Start, End: schedule.End, DesiredReplicas: int32(schedule.DesiredReplicas)}
			}
			scaling.KEDA.CronSchedules = schedules
		} else {
			scaling.KEDA.HTTPTargetPendingRequests = int32(app.KEDAHTTPTargetPendingRequests)
		}
	}
	identity := func(name string) store.ObjectIdentity {
		return store.ObjectIdentity{Name: name, Namespace: environment.Namespace, Labels: labels}
	}
	envelope := store.ReleaseSnapshotEnvelope{Version: store.ReleaseSnapshotVersion, DesiredState: store.DesiredStateSnapshot{
		Version: store.DesiredStateSnapshotVersion,
		Deployment: store.DeploymentIntent{
			Version: 1, Identity: identity(app.Slug), Selector: selector, Replicas: replicas,
			Strategy:  store.RollingUpdateIntent{Type: "RollingUpdate", MaxUnavailable: 0, MaxSurge: 1},
			PodLabels: labels, PodAnnotations: map[string]string{
				"cyclopus.dev/release-id":  releaseID,
				"cyclopus.dev/config-hash": store.ConfigHashAnnotationTemplate,
			},
			RuntimeClassName: app.RuntimeClass, ProgressDeadlineSeconds: int32(app.DeployTimeoutSeconds),
			Container: store.ContainerIntent{Name: "app", Image: imageRef, Port: int32(app.ContainerPort),
				Resources:      store.ResourceIntent{CPURequestMillicores: int64(app.CPURequestMillicores), CPULimitMillicores: int64(app.CPULimitMillicores), MemoryRequestMiB: int64(app.MemoryRequestMiB), MemoryLimitMiB: int64(app.MemoryLimitMiB)},
				ReadinessProbe: store.ReadinessIntent{Path: app.HealthCheckPath, Port: int32(app.ContainerPort)}},
		},
		Service: store.ServiceIntent{Version: 1, Identity: identity(app.Slug), Selector: selector, Port: 80, TargetPort: int32(app.ContainerPort)},
		Ingress: store.IngressIntent{Version: 1, Identity: identity(app.Slug), Hostname: *environment.PlannedHostname, Path: "/", ServiceName: app.Slug, ServicePort: 80, TLS: store.TLSIntent{Enabled: true, SecretName: app.Slug + "-tls"}},
		Scaling: scaling,
	}}
	encoded, _, err := store.EncodeReleaseSnapshot(envelope)
	return encoded, err
}
func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.ErrNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrLeaseNotOwned):
		return ErrConflict
	case errors.Is(err, store.ErrAlreadyExists):
		return store.ErrAlreadyExists
	case errors.Is(err, store.ErrFailedPrecondition):
		return store.ErrFailedPrecondition
	case errors.Is(err, store.ErrInvalidArgument):
		return ErrInvalidArgument
	}
	return err
}
