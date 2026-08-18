package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	"github.com/shepard-labs/cyclopus/internal/secretref"
)

const envPrefix = "CYCLOPUS_"

const githubEventCallPinSafetyMargin = 5 * time.Second

// clusterIssuerNamePattern is a DNS-1123 subdomain (the shape of a Kubernetes
// object name such as a cert-manager ClusterIssuer): lowercase alphanumerics,
// '-' and '.' with alphanumeric start/end, <= 253 chars. An empty issuer is
// allowed (it disables the annotation) and is not checked against this pattern.
var clusterIssuerNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

const MaxSessionTTL = 7 * 24 * time.Hour

// sliceViperKeys is the single source of truth for which configuration keys are
// string slices. It is keyed by the viper key (e.g. "github.trusted_hosts") so
// that both the environment-variable path and the flag-binding path can share
// it. Slice-typed values arrive from env vars as a single comma-separated
// string and must be split; values from any source are normalized so the stored
// config never contains empty or whitespace-padded elements.
var sliceViperKeys = map[string]struct{}{
	"github.trusted_hosts":                     {},
	"kubernetes.network_egress_excluded_cidrs": {},
	"webhook.allowed_secret_refs":              {},
}

func isSliceKey(key string) bool {
	_, ok := sliceViperKeys[key]
	return ok
}

// splitSliceValue splits a comma-separated env value into individual elements,
// trimming whitespace around each and dropping empty elements so that
// "a, b," yields ["a","b"].
func splitSliceValue(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// normalizeSliceValues trims whitespace around each element and drops empty
// elements, canonicalizing a slice regardless of its source (env or flag).
func normalizeSliceValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

type Config struct {
	ExternalURL             string
	HTTP                    HTTP
	GRPC                    GRPC
	Metrics                 Metrics
	Auth                    Auth
	Log                     Log
	DB                      DB
	Worker                  Worker
	GitHubEventWorker       GitHubEventWorker
	GitHub                  GitHub
	Webhook                 Webhook
	Kubernetes              Kubernetes
	Observability           Observability
	InsecurePublicListeners bool
}

// Observability configures the server-side proxies to the OpenObserve
// (tenant-scoped logs/traces) and Beszel (operator-level metrics) backends.
// When OpenObserveBaseURL is empty, the logs/traces RPCs report the backend as
// unavailable rather than reaching anything; likewise MetricsEnabled gates the
// operator metrics RPC — which requires BOTH BeszelBaseURL and
// BeszelCredentialSecret, not the URL alone (a URL with no credential cannot
// authenticate, so the RPC would otherwise silently fail against Beszel
// instead of cleanly reporting "unavailable"). Credential secret references
// are resolved through internal/secretref at startup; plaintext credentials
// never sit in config.
type Observability struct {
	OpenObserveBaseURL          string
	OpenObserveOrganization     string
	OpenObserveLogStream        string
	OpenObserveTraceStream      string
	OpenObserveCredentialSecret string
	BeszelBaseURL               string
	BeszelCredentialSecret      string
}

type Auth struct {
	TokenPepper         string
	BootstrapSecret     string
	SessionTTL          time.Duration
	MaxConcurrentHashes int
	RateLimitPerMinute  int
	RequestBodyBytes    int64
}

type GRPC struct {
	Address      string
	TLSCertFile  string
	TLSKeyFile   string
	MaxBodyBytes int
}

type HTTP struct {
	Address           string
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	TLSCertFile       string
	TLSKeyFile        string
	MaxBodyBytes      int64
	MaxHeaderBytes    int
}

type Metrics struct {
	Address     string
	AllowPublic bool
}

type Log struct {
	Level string
}

type DB struct {
	URL                  string
	MigrationLockTimeout time.Duration
}

type Worker struct {
	ScanInterval         time.Duration
	LeaseDuration        time.Duration
	MaxConcurrentDeploys int
}

type GitHubEventWorker struct {
	ScanInterval        time.Duration
	DeliveryConcurrency int
	FanoutConcurrency   int
	IntentConcurrency   int
	LeaseDuration       time.Duration
	ProcessingTimeout   time.Duration
	MaxAttempts         int
	InitialRetry        time.Duration
	MaxRetry            time.Duration
	PayloadRetention    time.Duration
	CleanupInterval     time.Duration
	CleanupBatch        int
	ShutdownGrace       time.Duration
	MaxFanout           int
	FanoutBatch         int
	MaxIntents          int
}

type GitHub struct {
	APIBaseURL                  string
	UploadURL                   string
	APIVersion                  string
	DispatchObservationInterval time.Duration
	RunPollInterval             time.Duration
	RequestTimeout              time.Duration
	TrustedHosts                []string
}

type Webhook struct {
	AllowedSecretRefs        []string
	ReleaseName              string
	PayloadEncryptionKeyFile string
	GitHubActiveSecretRef    string
	GitHubActiveKeyVersion   string
	GitHubPreviousSecretRef  string
	GitHubPreviousKeyVersion string
	GitHubSecretOverlapUntil time.Time
	GitHubBodyLimit          int64
	GitHubMaxConcurrent      int
	GitHubBodyReadTimeout    time.Duration
	GitHubRateLimitEnabled   bool
	GitHubRateLimitPerMinute int
	GitHubRateLimitBurst     int
}

type Kubernetes struct {
	InstanceNamespace          string
	Kubeconfig                 string
	RecoveryPollInterval       time.Duration
	ConflictRetries            int
	PreviewHostnameSuffix      string
	CustomDomainCNAMETarget    string
	CertManagerClusterIssuer   string
	NetworkEgressExcludedCIDRs []string
}

func Load(args, environ []string) (Config, error) {
	flags := pflag.NewFlagSet("cyclopus", pflag.ContinueOnError)
	flags.SetInterspersed(true)
	configFile := flags.String("config", "", "path to a configuration file")
	flags.String("external-url", "", "canonical public HTTPS URL used for browser callbacks")
	flags.String("http-address", "", "HTTP listen address")
	flags.String("grpc-address", "", "gRPC listen address")
	flags.String("metrics-address", "", "operator metrics listen address")
	flags.Bool("metrics-allow-public", false, "acknowledge exposing operator metrics on a non-loopback address")
	flags.String("auth-token-pepper", "", "secret pepper used to hash API credentials")
	flags.String("auth-bootstrap-secret", "", "high-entropy one-time installation bootstrap secret")
	flags.Duration("auth-session-ttl", 0, "local login session lifetime")
	flags.Int("auth-max-concurrent-hashes", 0, "maximum concurrent password hash and verify operations")
	flags.Int("auth-rate-limit-per-minute", 0, "public authentication attempts per client/account per minute")
	flags.Int64("auth-request-body-bytes", 0, "maximum public authentication request body size")
	flags.Bool("insecure-public-listeners", false, "development only: allow plaintext listeners on non-loopback addresses")
	flags.String("http-tls-cert-file", "", "HTTP TLS certificate file")
	flags.String("http-tls-key-file", "", "HTTP TLS private key file")
	flags.Int64("http-max-body-bytes", 0, "maximum HTTP request body size")
	flags.String("grpc-tls-cert-file", "", "gRPC TLS certificate file")
	flags.String("grpc-tls-key-file", "", "gRPC TLS private key file")
	flags.Int("grpc-max-body-bytes", 0, "maximum gRPC request message size")
	flags.Duration("http-read-timeout", 0, "HTTP request read timeout")
	flags.Duration("http-read-header-timeout", 0, "HTTP request header timeout")
	flags.Duration("http-write-timeout", 0, "HTTP response write timeout")
	flags.Duration("http-idle-timeout", 0, "HTTP keep-alive idle timeout")
	flags.Duration("http-shutdown-timeout", 0, "HTTP graceful shutdown timeout")
	flags.String("log-level", "", "structured log level")
	flags.String("database-url", "", "PostgreSQL connection URL")
	flags.Duration("database-migration-lock-timeout", 0, "database migration advisory lock timeout")
	flags.Duration("worker-scan-interval", 0, "recovery worker scan interval")
	flags.Duration("worker-lease-duration", 0, "recovery worker lease duration")
	flags.Int("worker-max-concurrent-deploys", 0, "maximum concurrent Kubernetes deploy observations")
	flags.Duration("github-event-worker-scan-interval", 0, "GitHub event worker scan interval")
	flags.Int("github-event-worker-delivery-concurrency", 0, "maximum concurrent GitHub webhook deliveries")
	flags.Int("github-event-worker-fanout-concurrency", 0, "maximum concurrent GitHub routing fanout manifests")
	flags.Int("github-event-worker-intent-concurrency", 0, "maximum concurrent GitHub routing intents")
	flags.Duration("github-event-worker-lease-duration", 0, "GitHub event worker lease duration")
	flags.Duration("github-event-worker-processing-timeout", 0, "GitHub event processing timeout")
	flags.Int("github-event-worker-max-attempts", 0, "GitHub event maximum attempts (fixed by schema)")
	flags.Duration("github-event-worker-initial-retry", 0, "GitHub event initial retry delay")
	flags.Duration("github-event-worker-max-retry", 0, "GitHub event maximum retry delay")
	flags.Duration("github-event-worker-payload-retention", 0, "terminal GitHub webhook payload retention")
	flags.Duration("github-event-worker-cleanup-interval", 0, "terminal GitHub webhook payload cleanup interval")
	flags.Int("github-event-worker-cleanup-batch", 0, "terminal GitHub webhook payload cleanup batch size")
	flags.Duration("github-event-worker-shutdown-grace", 0, "GitHub event worker shutdown grace period")
	flags.Int("github-event-worker-max-intents", 0, "maximum routing intents generated by one delivery")
	flags.Int("github-event-worker-max-fanout", 0, "maximum total routing fan-out generated by one delivery")
	flags.Int("github-event-worker-fanout-batch", 0, "routing fan-out materialization batch size")
	flags.String("github-api-base-url", "", "GitHub REST API base URL")
	flags.String("github-upload-url", "", "GitHub uploads base URL")
	flags.String("github-api-version", "", "GitHub REST API version")
	flags.Duration("github-dispatch-observation-interval", 0, "GitHub workflow dispatch observation interval")
	flags.Duration("github-run-poll-interval", 0, "GitHub workflow run polling interval")
	flags.Duration("github-request-timeout", 0, "GitHub API request timeout")
	flags.StringSlice("github-trusted-hosts", nil, "trusted GitHub API and upload hosts")
	flags.StringSlice("webhook-allowed-secret-refs", nil, "exact Kubernetes Secret references allowed for webhook signing")
	flags.String("webhook-release-name", "", "Helm release name that owns webhook signing Secrets")
	flags.String("webhook-payload-encryption-key-file", "", "path to the GitHub webhook payload encryption keyring file")
	flags.String("webhook-github-active-secret-ref", "", "secret reference containing the active GitHub webhook HMAC key")
	flags.String("webhook-github-active-key-version", "", "immutable version label for the active GitHub webhook HMAC key")
	flags.String("webhook-github-previous-secret-ref", "", "secret reference containing the previous GitHub webhook HMAC key")
	flags.String("webhook-github-previous-key-version", "", "immutable version label for the previous GitHub webhook HMAC key")
	flags.String("webhook-github-secret-overlap-until", "", "RFC3339 end of the active/previous GitHub webhook key overlap")
	flags.Int64("webhook-github-body-limit", 0, "maximum raw GitHub webhook body size")
	flags.Int("webhook-github-max-concurrent", 0, "maximum concurrent GitHub webhook handlers")
	flags.Duration("webhook-github-body-read-timeout", 0, "GitHub webhook body read timeout")
	flags.Bool("webhook-github-rate-limit-enabled", false, "enable per-IP GitHub webhook rate limiting")
	flags.Int("webhook-github-rate-limit-per-minute", 0, "GitHub webhook requests allowed per source IP per minute")
	flags.Int("webhook-github-rate-limit-burst", 0, "GitHub webhook burst allowed per source IP")
	flags.Int("http-max-header-bytes", 0, "maximum HTTP request header bytes")
	flags.String("kubeconfig", "", "path to a Kubernetes client configuration file")
	flags.String("kubernetes-instance-namespace", "", "namespace containing Cyclopus-managed source credentials")
	flags.Duration("kubernetes-recovery-poll-interval", 0, "Kubernetes deployment recovery observation interval")
	flags.Int("kubernetes-conflict-retries", 0, "Kubernetes optimistic update conflict retries")
	flags.String("kubernetes-preview-hostname-suffix", "", "verified DNS suffix appended to generated preview slugs")
	flags.String("kubernetes-custom-domain-cname-target", "", "platform ingress hostname that custom subdomains CNAME to (§7.5); defaults to the preview hostname suffix when empty")
	flags.String("kubernetes-cert-manager-cluster-issuer", "", "cert-manager ClusterIssuer name annotated onto environment and custom-domain Ingresses so TLS certs are issued (§7.5); empty disables the annotation (manual TLS)")
	flags.StringSlice("kubernetes-network-egress-excluded-cidrs", nil, "additional CIDRs (e.g. node public IPs) appended to the default-deny-egress policy Except list")
	flags.String("observability-openobserve-base-url", "", "OpenObserve HTTPS base URL for proxied logs/traces queries")
	flags.String("observability-openobserve-organization", "", "OpenObserve organization slug")
	flags.String("observability-openobserve-log-stream", "", "OpenObserve stream holding application logs")
	flags.String("observability-openobserve-trace-stream", "", "OpenObserve stream holding application traces")
	flags.String("observability-openobserve-credential-secret", "", "secret reference for the OpenObserve basic-auth credential")
	flags.String("observability-beszel-base-url", "", "Beszel HTTPS base URL for operator-level metrics")
	flags.String("observability-beszel-credential-secret", "", "secret reference for the Beszel superuser identity:password credential")
	if err := flags.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}

	v := viper.New()
	setDefaults(v)
	if *configFile != "" {
		v.SetConfigFile(*configFile)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("read config file: %w", err)
		}
	}

	envValues := environment(environ)
	for key, value := range envValues {
		if isSliceKey(key) {
			v.Set(key, splitSliceValue(value))
			continue
		}
		v.Set(key, value)
	}
	flagKeys := map[string]string{
		"external-url":                                "external_url",
		"http-address":                                "http.address",
		"grpc-address":                                "grpc.address",
		"metrics-address":                             "metrics.address",
		"metrics-allow-public":                        "metrics.allow_public",
		"auth-token-pepper":                           "auth.token_pepper",
		"auth-bootstrap-secret":                       "auth.bootstrap_secret",
		"auth-session-ttl":                            "auth.session_ttl",
		"auth-max-concurrent-hashes":                  "auth.max_concurrent_hashes",
		"auth-rate-limit-per-minute":                  "auth.rate_limit_per_minute",
		"auth-request-body-bytes":                     "auth.request_body_bytes",
		"insecure-public-listeners":                   "insecure_public_listeners",
		"http-tls-cert-file":                          "http.tls_cert_file",
		"http-tls-key-file":                           "http.tls_key_file",
		"http-max-body-bytes":                         "http.max_body_bytes",
		"grpc-tls-cert-file":                          "grpc.tls_cert_file",
		"grpc-tls-key-file":                           "grpc.tls_key_file",
		"grpc-max-body-bytes":                         "grpc.max_body_bytes",
		"http-read-timeout":                           "http.read_timeout",
		"http-read-header-timeout":                    "http.read_header_timeout",
		"http-write-timeout":                          "http.write_timeout",
		"http-idle-timeout":                           "http.idle_timeout",
		"http-shutdown-timeout":                       "http.shutdown_timeout",
		"log-level":                                   "log.level",
		"database-url":                                "database.url",
		"database-migration-lock-timeout":             "database.migration_lock_timeout",
		"worker-scan-interval":                        "worker.scan_interval",
		"worker-lease-duration":                       "worker.lease_duration",
		"worker-max-concurrent-deploys":               "worker.max_concurrent_deploys",
		"github-event-worker-scan-interval":           "github_event_worker.scan_interval",
		"github-event-worker-delivery-concurrency":    "github_event_worker.delivery_concurrency",
		"github-event-worker-fanout-concurrency":      "github_event_worker.fanout_concurrency",
		"github-event-worker-intent-concurrency":      "github_event_worker.intent_concurrency",
		"github-event-worker-lease-duration":          "github_event_worker.lease_duration",
		"github-event-worker-processing-timeout":      "github_event_worker.processing_timeout",
		"github-event-worker-max-attempts":            "github_event_worker.max_attempts",
		"github-event-worker-initial-retry":           "github_event_worker.initial_retry",
		"github-event-worker-max-retry":               "github_event_worker.max_retry",
		"github-event-worker-payload-retention":       "github_event_worker.payload_retention",
		"github-event-worker-cleanup-interval":        "github_event_worker.cleanup_interval",
		"github-event-worker-cleanup-batch":           "github_event_worker.cleanup_batch",
		"github-event-worker-shutdown-grace":          "github_event_worker.shutdown_grace",
		"github-event-worker-max-intents":             "github_event_worker.max_intents",
		"github-event-worker-max-fanout":              "github_event_worker.max_fanout",
		"github-event-worker-fanout-batch":            "github_event_worker.fanout_batch",
		"github-api-base-url":                         "github.api_base_url",
		"github-upload-url":                           "github.upload_url",
		"github-api-version":                          "github.api_version",
		"github-dispatch-observation-interval":        "github.dispatch_observation_interval",
		"github-run-poll-interval":                    "github.run_poll_interval",
		"github-request-timeout":                      "github.request_timeout",
		"github-trusted-hosts":                        "github.trusted_hosts",
		"webhook-allowed-secret-refs":                 "webhook.allowed_secret_refs",
		"webhook-release-name":                        "webhook.release_name",
		"webhook-payload-encryption-key-file":         "webhook.payload_encryption_key_file",
		"webhook-github-active-secret-ref":            "webhook.github_active_secret_ref",
		"webhook-github-active-key-version":           "webhook.github_active_key_version",
		"webhook-github-previous-secret-ref":          "webhook.github_previous_secret_ref",
		"webhook-github-previous-key-version":         "webhook.github_previous_key_version",
		"webhook-github-secret-overlap-until":         "webhook.github_secret_overlap_until",
		"webhook-github-body-limit":                   "webhook.github_body_limit",
		"webhook-github-max-concurrent":               "webhook.github_max_concurrent",
		"webhook-github-body-read-timeout":            "webhook.github_body_read_timeout",
		"webhook-github-rate-limit-enabled":           "webhook.github_rate_limit_enabled",
		"webhook-github-rate-limit-per-minute":        "webhook.github_rate_limit_per_minute",
		"webhook-github-rate-limit-burst":             "webhook.github_rate_limit_burst",
		"http-max-header-bytes":                       "http.max_header_bytes",
		"kubeconfig":                                  "kubernetes.kubeconfig",
		"kubernetes-instance-namespace":               "kubernetes.instance_namespace",
		"kubernetes-recovery-poll-interval":           "kubernetes.recovery_poll_interval",
		"kubernetes-conflict-retries":                 "kubernetes.conflict_retries",
		"kubernetes-preview-hostname-suffix":          "kubernetes.preview_hostname_suffix",
		"kubernetes-custom-domain-cname-target":       "kubernetes.custom_domain_cname_target",
		"kubernetes-cert-manager-cluster-issuer":      "kubernetes.cert_manager_cluster_issuer",
		"kubernetes-network-egress-excluded-cidrs":    "kubernetes.network_egress_excluded_cidrs",
		"observability-openobserve-base-url":          "observability.openobserve_base_url",
		"observability-openobserve-organization":      "observability.openobserve_organization",
		"observability-openobserve-log-stream":        "observability.openobserve_log_stream",
		"observability-openobserve-trace-stream":      "observability.openobserve_trace_stream",
		"observability-openobserve-credential-secret": "observability.openobserve_credential_secret",
		"observability-beszel-base-url":               "observability.beszel_base_url",
		"observability-beszel-credential-secret":      "observability.beszel_credential_secret",
	}
	for flagName, key := range flagKeys {
		if !flags.Changed(flagName) {
			continue
		}
		if isSliceKey(key) {
			values, err := flags.GetStringSlice(flagName)
			if err != nil {
				return Config{}, fmt.Errorf("parse flag %s: %w", flagName, err)
			}
			v.Set(key, normalizeSliceValues(values))
			continue
		}
		v.Set(key, flags.Lookup(flagName).Value.String())
	}
	legacyMaxIntentsExplicit := v.InConfig("github_event_worker.max_intents") || flags.Changed("github-event-worker-max-intents")
	maxFanoutExplicit := v.InConfig("github_event_worker.max_fanout") || flags.Changed("github-event-worker-max-fanout")
	if _, ok := envValues["github_event_worker.max_intents"]; ok {
		legacyMaxIntentsExplicit = true
	}
	if _, ok := envValues["github_event_worker.max_fanout"]; ok {
		maxFanoutExplicit = true
	}

	// The custom-domain CNAME target (§7.5) defaults to the preview hostname
	// suffix when unset so operators need only configure one value unless their
	// custom-domain ingress endpoint differs from the preview parent suffix.
	customDomainTarget := v.GetString("kubernetes.custom_domain_cname_target")
	if strings.TrimSpace(customDomainTarget) == "" {
		customDomainTarget = v.GetString("kubernetes.preview_hostname_suffix")
	}

	cfg := Config{
		ExternalURL: v.GetString("external_url"),
		HTTP: HTTP{
			Address:           v.GetString("http.address"),
			ReadTimeout:       v.GetDuration("http.read_timeout"),
			ReadHeaderTimeout: v.GetDuration("http.read_header_timeout"),
			WriteTimeout:      v.GetDuration("http.write_timeout"),
			IdleTimeout:       v.GetDuration("http.idle_timeout"),
			ShutdownTimeout:   v.GetDuration("http.shutdown_timeout"),
			TLSCertFile:       v.GetString("http.tls_cert_file"),
			TLSKeyFile:        v.GetString("http.tls_key_file"),
			MaxBodyBytes:      v.GetInt64("http.max_body_bytes"),
			MaxHeaderBytes:    v.GetInt("http.max_header_bytes"),
		},
		GRPC: GRPC{Address: v.GetString("grpc.address"), TLSCertFile: v.GetString("grpc.tls_cert_file"),
			TLSKeyFile: v.GetString("grpc.tls_key_file"), MaxBodyBytes: v.GetInt("grpc.max_body_bytes")},
		Metrics: Metrics{Address: v.GetString("metrics.address"), AllowPublic: v.GetBool("metrics.allow_public")},
		Auth: Auth{
			TokenPepper: v.GetString("auth.token_pepper"), BootstrapSecret: v.GetString("auth.bootstrap_secret"),
			SessionTTL: v.GetDuration("auth.session_ttl"), MaxConcurrentHashes: v.GetInt("auth.max_concurrent_hashes"),
			RateLimitPerMinute: v.GetInt("auth.rate_limit_per_minute"), RequestBodyBytes: v.GetInt64("auth.request_body_bytes"),
		},
		Log: Log{Level: strings.ToLower(v.GetString("log.level"))},
		DB: DB{
			URL:                  v.GetString("database.url"),
			MigrationLockTimeout: v.GetDuration("database.migration_lock_timeout"),
		},
		Worker: Worker{
			ScanInterval:         v.GetDuration("worker.scan_interval"),
			LeaseDuration:        v.GetDuration("worker.lease_duration"),
			MaxConcurrentDeploys: v.GetInt("worker.max_concurrent_deploys"),
		},
		GitHubEventWorker: GitHubEventWorker{
			ScanInterval: v.GetDuration("github_event_worker.scan_interval"), DeliveryConcurrency: v.GetInt("github_event_worker.delivery_concurrency"),
			FanoutConcurrency: v.GetInt("github_event_worker.fanout_concurrency"), IntentConcurrency: v.GetInt("github_event_worker.intent_concurrency"), LeaseDuration: v.GetDuration("github_event_worker.lease_duration"),
			ProcessingTimeout: v.GetDuration("github_event_worker.processing_timeout"), MaxAttempts: v.GetInt("github_event_worker.max_attempts"),
			InitialRetry: v.GetDuration("github_event_worker.initial_retry"), MaxRetry: v.GetDuration("github_event_worker.max_retry"),
			PayloadRetention: v.GetDuration("github_event_worker.payload_retention"), CleanupInterval: v.GetDuration("github_event_worker.cleanup_interval"),
			CleanupBatch: v.GetInt("github_event_worker.cleanup_batch"), ShutdownGrace: v.GetDuration("github_event_worker.shutdown_grace"),
			MaxFanout: v.GetInt("github_event_worker.max_fanout"), FanoutBatch: v.GetInt("github_event_worker.fanout_batch"),
			MaxIntents: v.GetInt("github_event_worker.max_intents"),
		},
		GitHub: GitHub{
			APIBaseURL:                  v.GetString("github.api_base_url"),
			UploadURL:                   v.GetString("github.upload_url"),
			APIVersion:                  v.GetString("github.api_version"),
			DispatchObservationInterval: v.GetDuration("github.dispatch_observation_interval"),
			RunPollInterval:             v.GetDuration("github.run_poll_interval"),
			RequestTimeout:              v.GetDuration("github.request_timeout"),
			TrustedHosts:                normalizeSliceValues(v.GetStringSlice("github.trusted_hosts")),
		},
		Webhook: Webhook{
			AllowedSecretRefs:        normalizeSliceValues(v.GetStringSlice("webhook.allowed_secret_refs")),
			ReleaseName:              strings.TrimSpace(v.GetString("webhook.release_name")),
			PayloadEncryptionKeyFile: strings.TrimSpace(v.GetString("webhook.payload_encryption_key_file")),
			GitHubActiveSecretRef:    strings.TrimSpace(v.GetString("webhook.github_active_secret_ref")),
			GitHubActiveKeyVersion:   v.GetString("webhook.github_active_key_version"),
			GitHubPreviousSecretRef:  strings.TrimSpace(v.GetString("webhook.github_previous_secret_ref")),
			GitHubPreviousKeyVersion: v.GetString("webhook.github_previous_key_version"),
			GitHubSecretOverlapUntil: v.GetTime("webhook.github_secret_overlap_until"),
			GitHubBodyLimit:          v.GetInt64("webhook.github_body_limit"), GitHubMaxConcurrent: v.GetInt("webhook.github_max_concurrent"),
			GitHubBodyReadTimeout:  v.GetDuration("webhook.github_body_read_timeout"),
			GitHubRateLimitEnabled: v.GetBool("webhook.github_rate_limit_enabled"), GitHubRateLimitPerMinute: v.GetInt("webhook.github_rate_limit_per_minute"),
			GitHubRateLimitBurst: v.GetInt("webhook.github_rate_limit_burst"),
		},
		Kubernetes: Kubernetes{
			InstanceNamespace:          strings.TrimSpace(v.GetString("kubernetes.instance_namespace")),
			Kubeconfig:                 v.GetString("kubernetes.kubeconfig"),
			RecoveryPollInterval:       v.GetDuration("kubernetes.recovery_poll_interval"),
			ConflictRetries:            v.GetInt("kubernetes.conflict_retries"),
			PreviewHostnameSuffix:      v.GetString("kubernetes.preview_hostname_suffix"),
			CustomDomainCNAMETarget:    customDomainTarget,
			CertManagerClusterIssuer:   strings.TrimSpace(v.GetString("kubernetes.cert_manager_cluster_issuer")),
			NetworkEgressExcludedCIDRs: normalizeSliceValues(v.GetStringSlice("kubernetes.network_egress_excluded_cidrs")),
		},
		Observability: Observability{
			OpenObserveBaseURL:          v.GetString("observability.openobserve_base_url"),
			OpenObserveOrganization:     v.GetString("observability.openobserve_organization"),
			OpenObserveLogStream:        v.GetString("observability.openobserve_log_stream"),
			OpenObserveTraceStream:      v.GetString("observability.openobserve_trace_stream"),
			OpenObserveCredentialSecret: v.GetString("observability.openobserve_credential_secret"),
			BeszelBaseURL:               v.GetString("observability.beszel_base_url"),
			BeszelCredentialSecret:      v.GetString("observability.beszel_credential_secret"),
		},
		InsecurePublicListeners: v.GetBool("insecure_public_listeners"),
	}
	if cfg.Webhook.GitHubActiveSecretRef == "" {
		cfg.Webhook.GitHubActiveSecretRef = "k8s-secret://" + cfg.Kubernetes.InstanceNamespace + "/" + secretref.GitHubAppSecretName + "#" + secretref.GitHubWebhookKey
	}
	if cfg.Webhook.GitHubActiveKeyVersion == "" {
		cfg.Webhook.GitHubActiveKeyVersion = "managed"
	}
	if legacyMaxIntentsExplicit && !maxFanoutExplicit {
		cfg.GitHubEventWorker.MaxFanout = cfg.GitHubEventWorker.MaxIntents
		if cfg.GitHubEventWorker.FanoutBatch > cfg.GitHubEventWorker.MaxFanout {
			cfg.GitHubEventWorker.FanoutBatch = cfg.GitHubEventWorker.MaxFanout
		}
	}
	if normalized, err := normalizeExternalURL(cfg.ExternalURL); err == nil {
		cfg.ExternalURL = normalized
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("http.address", "127.0.0.1:8080")
	v.SetDefault("grpc.address", "127.0.0.1:9091")
	v.SetDefault("metrics.address", "127.0.0.1:9090")
	v.SetDefault("auth.session_ttl", "12h")
	v.SetDefault("auth.max_concurrent_hashes", 4)
	v.SetDefault("auth.rate_limit_per_minute", 10)
	v.SetDefault("auth.request_body_bytes", 65536)
	v.SetDefault("http.max_body_bytes", 1048576)
	v.SetDefault("http.max_header_bytes", 1048576)
	v.SetDefault("grpc.max_body_bytes", 1048576)
	v.SetDefault("http.read_timeout", "15s")
	v.SetDefault("http.read_header_timeout", "5s")
	v.SetDefault("http.write_timeout", "30s")
	v.SetDefault("http.idle_timeout", "60s")
	v.SetDefault("http.shutdown_timeout", "10s")
	v.SetDefault("log.level", "info")
	v.SetDefault("database.migration_lock_timeout", "30s")
	v.SetDefault("worker.scan_interval", "30s")
	v.SetDefault("worker.lease_duration", "60s")
	v.SetDefault("worker.max_concurrent_deploys", 4)
	v.SetDefault("github_event_worker.scan_interval", "1s")
	v.SetDefault("github_event_worker.delivery_concurrency", 4)
	v.SetDefault("github_event_worker.fanout_concurrency", 2)
	v.SetDefault("github_event_worker.intent_concurrency", 8)
	v.SetDefault("github_event_worker.lease_duration", "60s")
	v.SetDefault("github_event_worker.processing_timeout", "20s")
	v.SetDefault("github_event_worker.max_attempts", 12)
	v.SetDefault("github_event_worker.initial_retry", "5s")
	v.SetDefault("github_event_worker.max_retry", "15m")
	v.SetDefault("github_event_worker.payload_retention", "24h")
	v.SetDefault("github_event_worker.cleanup_interval", "1m")
	v.SetDefault("github_event_worker.cleanup_batch", 100)
	v.SetDefault("github_event_worker.shutdown_grace", "30s")
	v.SetDefault("github_event_worker.max_fanout", 10000)
	v.SetDefault("github_event_worker.fanout_batch", 100)
	v.SetDefault("github_event_worker.max_intents", 1000)
	v.SetDefault("github.api_base_url", "https://api.github.com/")
	v.SetDefault("github.upload_url", "https://uploads.github.com/")
	v.SetDefault("github.api_version", "2026-03-10")
	v.SetDefault("github.dispatch_observation_interval", "90s")
	v.SetDefault("github.run_poll_interval", "30s")
	v.SetDefault("github.request_timeout", "20s")
	v.SetDefault("github.trusted_hosts", []string{"api.github.com", "uploads.github.com"})
	v.SetDefault("webhook.github_body_limit", 1048576)
	v.SetDefault("webhook.github_max_concurrent", 4)
	v.SetDefault("webhook.github_body_read_timeout", "10s")
	v.SetDefault("webhook.github_rate_limit_enabled", true)
	v.SetDefault("webhook.github_rate_limit_per_minute", 120)
	v.SetDefault("webhook.github_rate_limit_burst", 30)
	v.SetDefault("kubernetes.instance_namespace", "default")
	v.SetDefault("kubernetes.recovery_poll_interval", "2s")
	v.SetDefault("kubernetes.conflict_retries", 5)
	v.SetDefault("kubernetes.preview_hostname_suffix", "preview.invalid")
	v.SetDefault("kubernetes.cert_manager_cluster_issuer", "letsencrypt")
}

func environment(environ []string) map[string]string {
	keys := map[string]string{
		"EXTERNAL_URL":                                "external_url",
		"HTTP_ADDRESS":                                "http.address",
		"GRPC_ADDRESS":                                "grpc.address",
		"METRICS_ADDRESS":                             "metrics.address",
		"METRICS_ALLOW_PUBLIC":                        "metrics.allow_public",
		"AUTH_TOKEN_PEPPER":                           "auth.token_pepper",
		"AUTH_BOOTSTRAP_SECRET":                       "auth.bootstrap_secret",
		"AUTH_SESSION_TTL":                            "auth.session_ttl",
		"AUTH_MAX_CONCURRENT_HASHES":                  "auth.max_concurrent_hashes",
		"AUTH_RATE_LIMIT_PER_MINUTE":                  "auth.rate_limit_per_minute",
		"AUTH_REQUEST_BODY_BYTES":                     "auth.request_body_bytes",
		"INSECURE_PUBLIC_LISTENERS":                   "insecure_public_listeners",
		"HTTP_TLS_CERT_FILE":                          "http.tls_cert_file",
		"HTTP_TLS_KEY_FILE":                           "http.tls_key_file",
		"HTTP_MAX_BODY_BYTES":                         "http.max_body_bytes",
		"GRPC_TLS_CERT_FILE":                          "grpc.tls_cert_file",
		"GRPC_TLS_KEY_FILE":                           "grpc.tls_key_file",
		"GRPC_MAX_BODY_BYTES":                         "grpc.max_body_bytes",
		"HTTP_READ_TIMEOUT":                           "http.read_timeout",
		"HTTP_READ_HEADER_TIMEOUT":                    "http.read_header_timeout",
		"HTTP_WRITE_TIMEOUT":                          "http.write_timeout",
		"HTTP_IDLE_TIMEOUT":                           "http.idle_timeout",
		"HTTP_SHUTDOWN_TIMEOUT":                       "http.shutdown_timeout",
		"LOG_LEVEL":                                   "log.level",
		"DATABASE_URL":                                "database.url",
		"DATABASE_MIGRATION_LOCK_TIMEOUT":             "database.migration_lock_timeout",
		"WORKER_SCAN_INTERVAL":                        "worker.scan_interval",
		"WORKER_LEASE_DURATION":                       "worker.lease_duration",
		"WORKER_MAX_CONCURRENT_DEPLOYS":               "worker.max_concurrent_deploys",
		"GITHUB_EVENT_WORKER_SCAN_INTERVAL":           "github_event_worker.scan_interval",
		"GITHUB_EVENT_WORKER_DELIVERY_CONCURRENCY":    "github_event_worker.delivery_concurrency",
		"GITHUB_EVENT_WORKER_FANOUT_CONCURRENCY":      "github_event_worker.fanout_concurrency",
		"GITHUB_EVENT_WORKER_INTENT_CONCURRENCY":      "github_event_worker.intent_concurrency",
		"GITHUB_EVENT_WORKER_LEASE_DURATION":          "github_event_worker.lease_duration",
		"GITHUB_EVENT_WORKER_PROCESSING_TIMEOUT":      "github_event_worker.processing_timeout",
		"GITHUB_EVENT_WORKER_MAX_ATTEMPTS":            "github_event_worker.max_attempts",
		"GITHUB_EVENT_WORKER_INITIAL_RETRY":           "github_event_worker.initial_retry",
		"GITHUB_EVENT_WORKER_MAX_RETRY":               "github_event_worker.max_retry",
		"GITHUB_EVENT_WORKER_PAYLOAD_RETENTION":       "github_event_worker.payload_retention",
		"GITHUB_EVENT_WORKER_CLEANUP_INTERVAL":        "github_event_worker.cleanup_interval",
		"GITHUB_EVENT_WORKER_CLEANUP_BATCH":           "github_event_worker.cleanup_batch",
		"GITHUB_EVENT_WORKER_SHUTDOWN_GRACE":          "github_event_worker.shutdown_grace",
		"GITHUB_EVENT_WORKER_MAX_INTENTS":             "github_event_worker.max_intents",
		"GITHUB_EVENT_WORKER_MAX_FANOUT":              "github_event_worker.max_fanout",
		"GITHUB_EVENT_WORKER_FANOUT_BATCH":            "github_event_worker.fanout_batch",
		"GITHUB_API_BASE_URL":                         "github.api_base_url",
		"GITHUB_UPLOAD_URL":                           "github.upload_url",
		"GITHUB_API_VERSION":                          "github.api_version",
		"GITHUB_DISPATCH_OBSERVATION_INTERVAL":        "github.dispatch_observation_interval",
		"GITHUB_RUN_POLL_INTERVAL":                    "github.run_poll_interval",
		"GITHUB_REQUEST_TIMEOUT":                      "github.request_timeout",
		"GITHUB_TRUSTED_HOSTS":                        "github.trusted_hosts",
		"WEBHOOK_ALLOWED_SECRET_REFS":                 "webhook.allowed_secret_refs",
		"WEBHOOK_RELEASE_NAME":                        "webhook.release_name",
		"WEBHOOK_PAYLOAD_ENCRYPTION_KEY_FILE":         "webhook.payload_encryption_key_file",
		"WEBHOOK_GITHUB_ACTIVE_SECRET_REF":            "webhook.github_active_secret_ref",
		"WEBHOOK_GITHUB_ACTIVE_KEY_VERSION":           "webhook.github_active_key_version",
		"WEBHOOK_GITHUB_PREVIOUS_SECRET_REF":          "webhook.github_previous_secret_ref",
		"WEBHOOK_GITHUB_PREVIOUS_KEY_VERSION":         "webhook.github_previous_key_version",
		"WEBHOOK_GITHUB_SECRET_OVERLAP_UNTIL":         "webhook.github_secret_overlap_until",
		"WEBHOOK_GITHUB_BODY_LIMIT":                   "webhook.github_body_limit",
		"WEBHOOK_GITHUB_MAX_CONCURRENT":               "webhook.github_max_concurrent",
		"WEBHOOK_GITHUB_BODY_READ_TIMEOUT":            "webhook.github_body_read_timeout",
		"WEBHOOK_GITHUB_RATE_LIMIT_ENABLED":           "webhook.github_rate_limit_enabled",
		"WEBHOOK_GITHUB_RATE_LIMIT_PER_MINUTE":        "webhook.github_rate_limit_per_minute",
		"WEBHOOK_GITHUB_RATE_LIMIT_BURST":             "webhook.github_rate_limit_burst",
		"HTTP_MAX_HEADER_BYTES":                       "http.max_header_bytes",
		"KUBECONFIG":                                  "kubernetes.kubeconfig",
		"KUBERNETES_INSTANCE_NAMESPACE":               "kubernetes.instance_namespace",
		"KUBERNETES_RECOVERY_POLL_INTERVAL":           "kubernetes.recovery_poll_interval",
		"KUBERNETES_CONFLICT_RETRIES":                 "kubernetes.conflict_retries",
		"KUBERNETES_PREVIEW_HOSTNAME_SUFFIX":          "kubernetes.preview_hostname_suffix",
		"KUBERNETES_CUSTOM_DOMAIN_CNAME_TARGET":       "kubernetes.custom_domain_cname_target",
		"KUBERNETES_CERT_MANAGER_CLUSTER_ISSUER":      "kubernetes.cert_manager_cluster_issuer",
		"KUBERNETES_NETWORK_EGRESS_EXCLUDED_CIDRS":    "kubernetes.network_egress_excluded_cidrs",
		"OBSERVABILITY_OPENOBSERVE_BASE_URL":          "observability.openobserve_base_url",
		"OBSERVABILITY_OPENOBSERVE_ORGANIZATION":      "observability.openobserve_organization",
		"OBSERVABILITY_OPENOBSERVE_LOG_STREAM":        "observability.openobserve_log_stream",
		"OBSERVABILITY_OPENOBSERVE_TRACE_STREAM":      "observability.openobserve_trace_stream",
		"OBSERVABILITY_OPENOBSERVE_CREDENTIAL_SECRET": "observability.openobserve_credential_secret",
		"OBSERVABILITY_BESZEL_BASE_URL":               "observability.beszel_base_url",
		"OBSERVABILITY_BESZEL_CREDENTIAL_SECRET":      "observability.beszel_credential_secret",
	}
	values := make(map[string]string)
	for _, item := range environ {
		name, value, ok := strings.Cut(item, "=")
		if !ok || !strings.HasPrefix(name, envPrefix) {
			continue
		}
		if key, exists := keys[strings.TrimPrefix(name, envPrefix)]; exists {
			values[key] = value
		}
	}
	return values
}

func (g GitHub) Enabled() bool {
	return strings.TrimSpace(g.APIBaseURL) != "" && strings.TrimSpace(g.UploadURL) != "" &&
		strings.TrimSpace(g.APIVersion) != "" && g.DispatchObservationInterval > 0 &&
		g.RunPollInterval > 0 && g.RequestTimeout > 0
}

func (c Config) Validate() error {
	var errs []error
	externalURL, err := normalizeExternalURL(c.ExternalURL)
	if err != nil {
		errs = append(errs, err)
	} else {
		c.ExternalURL = externalURL
	}
	if _, _, err := net.SplitHostPort(c.HTTP.Address); err != nil {
		errs = append(errs, fmt.Errorf("http.address must be a host:port pair: %w", err))
	}
	if _, _, err := net.SplitHostPort(c.GRPC.Address); err != nil {
		errs = append(errs, fmt.Errorf("grpc.address must be a host:port pair: %w", err))
	}
	if _, _, err := net.SplitHostPort(c.Metrics.Address); err != nil {
		errs = append(errs, fmt.Errorf("metrics.address must be a host:port pair: %w", err))
	}
	if c.GRPC.Address == c.HTTP.Address || c.Metrics.Address == c.HTTP.Address || c.Metrics.Address == c.GRPC.Address {
		errs = append(errs, fmt.Errorf("http.address, grpc.address, and metrics.address must be different"))
	}
	if !c.Metrics.AllowPublic && !loopbackAddress(c.Metrics.Address) {
		errs = append(errs, fmt.Errorf("metrics.address must be loopback unless metrics.allow_public is enabled"))
	}
	if len(c.Auth.TokenPepper) < 32 {
		errs = append(errs, fmt.Errorf("auth.token_pepper must contain at least 32 bytes"))
	}
	if len(c.Auth.BootstrapSecret) < 32 {
		errs = append(errs, fmt.Errorf("auth.bootstrap_secret must contain at least 32 bytes"))
	}
	if c.Auth.SessionTTL <= 0 {
		errs = append(errs, fmt.Errorf("auth.session_ttl must be greater than zero"))
	} else if c.Auth.SessionTTL > MaxSessionTTL {
		errs = append(errs, fmt.Errorf("auth.session_ttl must not exceed %s", MaxSessionTTL))
	}
	if c.Auth.MaxConcurrentHashes <= 0 || c.Auth.MaxConcurrentHashes > 64 {
		errs = append(errs, fmt.Errorf("auth.max_concurrent_hashes must be between 1 and 64"))
	}
	if c.Auth.RateLimitPerMinute <= 0 {
		errs = append(errs, fmt.Errorf("auth.rate_limit_per_minute must be greater than zero"))
	}
	if c.Auth.RequestBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("auth.request_body_bytes must be greater than zero"))
	}
	if c.HTTP.MaxBodyBytes <= 0 || c.GRPC.MaxBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("listener body size limits must be greater than zero"))
	}
	if c.HTTP.MaxHeaderBytes <= 0 {
		errs = append(errs, fmt.Errorf("http.max_header_bytes must be greater than zero"))
	}
	if err := validateTLSFiles("http", c.HTTP.TLSCertFile, c.HTTP.TLSKeyFile); err != nil {
		errs = append(errs, err)
	}
	if err := validateTLSFiles("grpc", c.GRPC.TLSCertFile, c.GRPC.TLSKeyFile); err != nil {
		errs = append(errs, err)
	}
	if !c.InsecurePublicListeners {
		if c.HTTP.TLSCertFile == "" && !loopbackAddress(c.HTTP.Address) {
			errs = append(errs, fmt.Errorf("http.address plaintext listener must be loopback unless insecure_public_listeners is enabled"))
		}
		if c.GRPC.TLSCertFile == "" && !loopbackAddress(c.GRPC.Address) {
			errs = append(errs, fmt.Errorf("grpc.address plaintext listener must be loopback unless insecure_public_listeners is enabled"))
		}
	}
	durations := map[string]time.Duration{
		"http.read_timeout":        c.HTTP.ReadTimeout,
		"http.read_header_timeout": c.HTTP.ReadHeaderTimeout,
		"http.write_timeout":       c.HTTP.WriteTimeout,
		"http.idle_timeout":        c.HTTP.IdleTimeout,
		"http.shutdown_timeout":    c.HTTP.ShutdownTimeout,
	}
	for name, duration := range durations {
		if duration <= 0 {
			errs = append(errs, fmt.Errorf("%s must be greater than zero", name))
		}
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level must be one of debug, info, warn, error"))
	}
	if strings.TrimSpace(c.DB.URL) == "" {
		errs = append(errs, fmt.Errorf("database.url is required"))
	}
	if c.DB.MigrationLockTimeout <= 0 {
		errs = append(errs, fmt.Errorf("database.migration_lock_timeout must be greater than zero"))
	}
	if c.Worker.ScanInterval <= 0 {
		errs = append(errs, fmt.Errorf("worker.scan_interval must be greater than zero"))
	}
	if c.Worker.LeaseDuration <= 0 {
		errs = append(errs, fmt.Errorf("worker.lease_duration must be greater than zero"))
	}
	if c.Worker.MaxConcurrentDeploys <= 0 || c.Worker.MaxConcurrentDeploys > 32 {
		errs = append(errs, fmt.Errorf("worker.max_concurrent_deploys must be between 1 and 32"))
	}
	ew := c.GitHubEventWorker
	if ew.MaxFanout == 0 {
		ew.MaxFanout = ew.MaxIntents
	}
	if ew.ScanInterval <= 0 || ew.LeaseDuration <= 0 || ew.ProcessingTimeout <= 0 || ew.InitialRetry <= 0 || ew.MaxRetry <= 0 || ew.PayloadRetention < 0 || ew.CleanupInterval <= 0 || ew.ShutdownGrace <= 0 {
		errs = append(errs, fmt.Errorf("github_event_worker durations must be positive (payload retention may be zero)"))
	}
	if ew.DeliveryConcurrency < 1 || ew.DeliveryConcurrency > 100 || ew.FanoutConcurrency < 1 || ew.FanoutConcurrency > 100 || ew.IntentConcurrency < 1 || ew.IntentConcurrency > 100 {
		errs = append(errs, fmt.Errorf("github_event_worker concurrency must be between 1 and 100"))
	}
	if ew.ProcessingTimeout >= ew.LeaseDuration {
		errs = append(errs, fmt.Errorf("github_event_worker.processing_timeout must be shorter than lease_duration"))
	}
	if ew.MaxAttempts != 12 {
		errs = append(errs, fmt.Errorf("github_event_worker.max_attempts must be 12 to match the database schema"))
	}
	if ew.InitialRetry > ew.MaxRetry || ew.MaxRetry > 15*time.Minute {
		errs = append(errs, fmt.Errorf("github_event_worker retry delays must be ordered and max_retry must not exceed 15m"))
	}
	if ew.PayloadRetention > 30*24*time.Hour || ew.CleanupBatch < 1 || ew.CleanupBatch > 1000 || ew.MaxIntents < 1 || ew.MaxIntents > 10000 || ew.MaxFanout < 1 || ew.MaxFanout > 10000 || ew.FanoutBatch < 1 || ew.FanoutBatch > ew.MaxFanout {
		errs = append(errs, fmt.Errorf("github_event_worker retention, cleanup batch, or fanout limits are out of bounds"))
	}
	trustedHosts := make(map[string]struct{}, len(c.GitHub.TrustedHosts))
	for _, host := range c.GitHub.TrustedHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" || strings.ContainsAny(host, "/:@") {
			errs = append(errs, fmt.Errorf("github.trusted_hosts must contain hostnames only"))
			continue
		}
		trustedHosts[host] = struct{}{}
	}
	for name, rawURL := range map[string]string{
		"github.api_base_url": c.GitHub.APIBaseURL,
		"github.upload_url":   c.GitHub.UploadURL,
	} {
		u, err := url.ParseRequestURI(rawURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be an absolute HTTPS URL", name))
		} else if _, trusted := trustedHosts[strings.ToLower(u.Hostname())]; !trusted {
			errs = append(errs, fmt.Errorf("%s host must be listed in github.trusted_hosts", name))
		}
	}
	if strings.TrimSpace(c.GitHub.APIVersion) == "" {
		errs = append(errs, fmt.Errorf("github.api_version is required"))
	}
	if c.GitHub.DispatchObservationInterval <= 0 {
		errs = append(errs, fmt.Errorf("github.dispatch_observation_interval must be greater than zero"))
	}
	if c.GitHub.RunPollInterval <= 0 {
		errs = append(errs, fmt.Errorf("github.run_poll_interval must be greater than zero"))
	}
	if c.GitHub.RequestTimeout <= 0 {
		errs = append(errs, fmt.Errorf("github.request_timeout must be greater than zero"))
	}
	if c.GitHub.RequestTimeout >= c.Worker.LeaseDuration {
		errs = append(errs, fmt.Errorf("github.request_timeout must be shorter than worker.lease_duration"))
	}
	if c.GitHubEventWorker.LeaseDuration < githubEventCallPinSafetyMargin || c.GitHub.RequestTimeout > c.GitHubEventWorker.LeaseDuration-githubEventCallPinSafetyMargin {
		errs = append(errs, fmt.Errorf("github.request_timeout plus a %s call pin safety margin must not exceed github_event_worker.lease_duration", githubEventCallPinSafetyMargin))
	}
	seenWebhookRefs := make(map[string]struct{}, len(c.Webhook.AllowedSecretRefs))
	for _, value := range c.Webhook.AllowedSecretRefs {
		reference, err := secretref.Parse(value)
		if err != nil {
			errs = append(errs, fmt.Errorf("webhook.allowed_secret_refs entry %q must be a valid Kubernetes Secret reference: %w", value, err))
			continue
		}
		if reference.Namespace != c.Kubernetes.InstanceNamespace {
			errs = append(errs, fmt.Errorf("webhook.allowed_secret_refs entry %q must reference the Kubernetes control-plane namespace %q", value, c.Kubernetes.InstanceNamespace))
		}
		if !strings.HasPrefix(reference.Name, "cyclopus-webhook-") {
			errs = append(errs, fmt.Errorf("webhook.allowed_secret_refs entry %q must use a cyclopus-webhook- Secret name", value))
		}
		if _, exists := seenWebhookRefs[value]; exists {
			errs = append(errs, fmt.Errorf("webhook.allowed_secret_refs entry %q is duplicated", value))
		}
		seenWebhookRefs[value] = struct{}{}
	}
	if len(c.Webhook.AllowedSecretRefs) > 0 && strings.TrimSpace(c.Webhook.ReleaseName) == "" {
		errs = append(errs, fmt.Errorf("webhook.release_name is required when webhook.allowed_secret_refs is configured"))
	}
	if c.Webhook.PayloadEncryptionKeyFile == "" {
		errs = append(errs, fmt.Errorf("webhook.payload_encryption_key_file is required"))
	}
	if c.Webhook.GitHubActiveSecretRef == "" || !validKeyVersion(c.Webhook.GitHubActiveKeyVersion) {
		errs = append(errs, fmt.Errorf("webhook GitHub active secret reference and key version are required"))
	} else if _, err := secretref.Parse(c.Webhook.GitHubActiveSecretRef); err != nil {
		errs = append(errs, fmt.Errorf("webhook.github_active_secret_ref must be a valid Kubernetes Secret reference"))
	}
	if c.Webhook.GitHubPreviousKeyVersion != "" && !validKeyVersion(c.Webhook.GitHubPreviousKeyVersion) {
		errs = append(errs, fmt.Errorf("webhook GitHub key versions must be nonempty, unpadded, and at most 128 bytes"))
	}
	if (c.Webhook.GitHubPreviousSecretRef == "") != (c.Webhook.GitHubPreviousKeyVersion == "") || (c.Webhook.GitHubPreviousSecretRef != "" && c.Webhook.GitHubSecretOverlapUntil.IsZero()) {
		errs = append(errs, fmt.Errorf("webhook GitHub previous secret requires its version and a bounded overlap end"))
	} else if c.Webhook.GitHubPreviousSecretRef != "" {
		if _, err := secretref.Parse(c.Webhook.GitHubPreviousSecretRef); err != nil {
			errs = append(errs, fmt.Errorf("webhook.github_previous_secret_ref must be a valid Kubernetes Secret reference"))
		}
	}
	if c.Webhook.GitHubBodyLimit <= 0 || c.Webhook.GitHubMaxConcurrent <= 0 || c.Webhook.GitHubMaxConcurrent > 64 || c.Webhook.GitHubBodyReadTimeout <= 0 {
		errs = append(errs, fmt.Errorf("webhook GitHub ingress limits must be positive and concurrency at most 64"))
	}
	if !c.Webhook.GitHubRateLimitEnabled || c.Webhook.GitHubRateLimitPerMinute <= 0 || c.Webhook.GitHubRateLimitBurst <= 0 || c.Webhook.GitHubRateLimitBurst > c.Webhook.GitHubRateLimitPerMinute {
		errs = append(errs, fmt.Errorf("webhook GitHub rate limiting must be enabled with positive rate and burst not exceeding the per-minute rate"))
	}
	if c.Kubernetes.RecoveryPollInterval <= 0 {
		errs = append(errs, fmt.Errorf("kubernetes.recovery_poll_interval must be greater than zero"))
	}
	if namespace := strings.TrimSpace(c.Kubernetes.InstanceNamespace); namespace == "" || len(namespace) > 63 || !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(namespace) {
		errs = append(errs, fmt.Errorf("kubernetes.instance_namespace must be a DNS label"))
	}
	if c.Kubernetes.ConflictRetries <= 0 {
		errs = append(errs, fmt.Errorf("kubernetes.conflict_retries must be greater than zero"))
	}
	if suffix := strings.TrimSpace(c.Kubernetes.PreviewHostnameSuffix); suffix == "" || len(suffix) > 253 || strings.ContainsAny(suffix, "/:@") {
		errs = append(errs, fmt.Errorf("kubernetes.preview_hostname_suffix must be a DNS suffix"))
	} else {
		lower := strings.ToLower(suffix)
		for _, blocked := range []string{"sslip.io", "nip.io"} {
			if strings.HasSuffix(lower, "."+blocked) || lower == blocked {
				slog.Warn("kubernetes.preview_hostname_suffix uses a service that is absent from the Public Suffix List; "+
					"Let's Encrypt certificate quota is shared globally — configure your own domain for production use",
					"suffix", suffix, "service", blocked)
				break
			}
		}
	}
	if target := strings.TrimSpace(c.Kubernetes.CustomDomainCNAMETarget); target == "" || len(target) > 253 || strings.ContainsAny(target, "/:@") {
		errs = append(errs, fmt.Errorf("kubernetes.custom_domain_cname_target must be a DNS suffix"))
	}
	// An empty cert-manager cluster issuer is valid: it disables the issuer
	// annotation so installs without cert-manager (or using manual TLS) keep the
	// prior behavior. A non-empty value must be a valid Kubernetes object name.
	if issuer := strings.TrimSpace(c.Kubernetes.CertManagerClusterIssuer); issuer != "" && (len(issuer) > 253 || !clusterIssuerNamePattern.MatchString(issuer)) {
		errs = append(errs, fmt.Errorf("kubernetes.cert_manager_cluster_issuer must be a valid Kubernetes resource name"))
	}
	for _, cidr := range c.Kubernetes.NetworkEgressExcludedCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			errs = append(errs, fmt.Errorf("kubernetes.network_egress_excluded_cidrs entry %q is not a valid CIDR", cidr))
		}
	}
	errs = append(errs, c.Observability.validate(c.Kubernetes.InstanceNamespace)...)
	return errors.Join(errs...)
}

func normalizeExternalURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("external_url is required")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("external_url must be an absolute HTTPS URL without userinfo, query, or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackExternalHost(u.Hostname()) {
			return "", fmt.Errorf("external_url must be an absolute HTTPS URL without userinfo, query, or fragment")
		}
	default:
		return "", fmt.Errorf("external_url must be an absolute HTTPS URL without userinfo, query, or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("external_url path must be empty or /")
	}
	u.Path, u.RawPath = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopbackExternalHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// LogsEnabled reports whether OpenObserve-backed logs/traces proxying is
// configured.
func (o Observability) LogsEnabled() bool {
	return strings.TrimSpace(o.OpenObserveBaseURL) != ""
}

// MetricsEnabled reports whether Beszel-backed operator metrics are fully
// configured: a base URL alone is not enough, since unauthenticated calls to
// Beszel's PocketBase API fail. Until a credential is also wired in, metrics
// stay disabled and the RPC reports the backend as unavailable rather than
// reaching Beszel unauthenticated.
func (o Observability) MetricsEnabled() bool {
	return o.beszelConfigured() && strings.TrimSpace(o.BeszelCredentialSecret) != ""
}

// beszelConfigured reports whether a Beszel base URL is set, independent of
// whether a credential has been wired in yet. It exists so the URL's format is
// still validated even before the credential exists (see validate below),
// rather than validation silently going quiet in the same window that
// MetricsEnabled reports the feature as not-yet-usable.
func (o Observability) beszelConfigured() bool {
	return strings.TrimSpace(o.BeszelBaseURL) != ""
}

func (o Observability) validate(controlPlaneNamespace string) []error {
	var errs []error
	if o.LogsEnabled() {
		if err := observability.ValidateBackendBaseURL(o.OpenObserveBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("observability.openobserve_base_url %w", err))
		}
		if strings.TrimSpace(o.OpenObserveOrganization) == "" {
			errs = append(errs, fmt.Errorf("observability.openobserve_organization is required when OpenObserve is enabled"))
		}
	}
	if o.beszelConfigured() {
		if err := observability.ValidateBackendBaseURL(o.BeszelBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("observability.beszel_base_url %w", err))
		}
		if strings.TrimSpace(o.BeszelCredentialSecret) == "" {
			slog.Warn("observability.beszel_base_url is set but observability.beszel_credential_secret is empty; "+
				"operator metrics stay disabled until a Beszel credential is configured",
				"beszel_base_url", o.BeszelBaseURL)
		}
	}
	for name, value := range map[string]string{
		"observability.openobserve_credential_secret": o.OpenObserveCredentialSecret,
		"observability.beszel_credential_secret":      o.BeszelCredentialSecret,
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		reference, err := secretref.Parse(value)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s must be a valid Kubernetes Secret reference: %w", name, err))
			continue
		}
		if reference.Namespace != controlPlaneNamespace {
			errs = append(errs, fmt.Errorf("%s must reference the Kubernetes control-plane namespace %q", name, controlPlaneNamespace))
		}
	}
	return errs
}

func validateTLSFiles(listener, certFile, keyFile string) error {
	if (strings.TrimSpace(certFile) == "") != (strings.TrimSpace(keyFile) == "") {
		return fmt.Errorf("%s TLS certificate and key must be supplied together", listener)
	}
	return nil
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validKeyVersion(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value
}
