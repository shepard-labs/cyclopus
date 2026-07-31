package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

const envPrefix = "CYCLOPUS_"

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
	HTTP                    HTTP
	GRPC                    GRPC
	Auth                    Auth
	Log                     Log
	DB                      DB
	Worker                  Worker
	GitHub                  GitHub
	Kubernetes              Kubernetes
	Observability           Observability
	InsecurePublicListeners bool
}

// Observability configures the server-side proxies to the OpenObserve
// (tenant-scoped logs/traces) and Beszel (operator-level metrics) backends.
// When OpenObserveBaseURL is empty, the logs/traces RPCs report the backend as
// unavailable rather than reaching anything; likewise BeszelBaseURL gates the
// operator metrics RPC. Credential secret references are resolved through
// internal/secretref at startup; plaintext credentials never sit in config.
type Observability struct {
	OpenObserveBaseURL          string
	OpenObserveOrganization     string
	OpenObserveLogStream        string
	OpenObserveTraceStream      string
	OpenObserveCredentialSecret string
	BeszelBaseURL               string
	BeszelTokenSecret           string
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

type GitHub struct {
	AppID                       int64
	PrivateKeyFile              string
	APIBaseURL                  string
	UploadURL                   string
	APIVersion                  string
	DispatchObservationInterval time.Duration
	RunPollInterval             time.Duration
	RequestTimeout              time.Duration
	TrustedHosts                []string
}

type Kubernetes struct {
	Kubeconfig                 string
	RecoveryPollInterval       time.Duration
	ConflictRetries            int
	PreviewHostnameSuffix      string
	NetworkEgressExcludedCIDRs []string
}

func Load(args, environ []string) (Config, error) {
	flags := pflag.NewFlagSet("cyclopus", pflag.ContinueOnError)
	flags.SetInterspersed(true)
	configFile := flags.String("config", "", "path to a configuration file")
	flags.String("http-address", "", "HTTP listen address")
	flags.String("grpc-address", "", "gRPC listen address")
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
	flags.Int64("github-app-id", 0, "GitHub App ID")
	flags.String("github-private-key-file", "", "GitHub App private key file")
	flags.String("github-api-base-url", "", "GitHub REST API base URL")
	flags.String("github-upload-url", "", "GitHub uploads base URL")
	flags.String("github-api-version", "", "GitHub REST API version")
	flags.Duration("github-dispatch-observation-interval", 0, "GitHub workflow dispatch observation interval")
	flags.Duration("github-run-poll-interval", 0, "GitHub workflow run polling interval")
	flags.Duration("github-request-timeout", 0, "GitHub API request timeout")
	flags.StringSlice("github-trusted-hosts", nil, "trusted GitHub API and upload hosts")
	flags.String("kubeconfig", "", "path to a Kubernetes client configuration file")
	flags.Duration("kubernetes-recovery-poll-interval", 0, "Kubernetes deployment recovery observation interval")
	flags.Int("kubernetes-conflict-retries", 0, "Kubernetes optimistic update conflict retries")
	flags.String("kubernetes-preview-hostname-suffix", "", "verified DNS suffix appended to generated preview slugs")
	flags.StringSlice("kubernetes-network-egress-excluded-cidrs", nil, "additional CIDRs (e.g. node public IPs) appended to the default-deny-egress policy Except list")
	flags.String("observability-openobserve-base-url", "", "OpenObserve HTTPS base URL for proxied logs/traces queries")
	flags.String("observability-openobserve-organization", "", "OpenObserve organization slug")
	flags.String("observability-openobserve-log-stream", "", "OpenObserve stream holding application logs")
	flags.String("observability-openobserve-trace-stream", "", "OpenObserve stream holding application traces")
	flags.String("observability-openobserve-credential-secret", "", "secret reference for the OpenObserve basic-auth credential")
	flags.String("observability-beszel-base-url", "", "Beszel HTTPS base URL for operator-level metrics")
	flags.String("observability-beszel-token-secret", "", "secret reference for the Beszel PocketBase auth token")
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

	for key, value := range environment(environ) {
		if isSliceKey(key) {
			v.Set(key, splitSliceValue(value))
			continue
		}
		v.Set(key, value)
	}
	flagKeys := map[string]string{
		"http-address":                                "http.address",
		"grpc-address":                                "grpc.address",
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
		"github-app-id":                               "github.app_id",
		"github-private-key-file":                     "github.private_key_file",
		"github-api-base-url":                         "github.api_base_url",
		"github-upload-url":                           "github.upload_url",
		"github-api-version":                          "github.api_version",
		"github-dispatch-observation-interval":        "github.dispatch_observation_interval",
		"github-run-poll-interval":                    "github.run_poll_interval",
		"github-request-timeout":                      "github.request_timeout",
		"github-trusted-hosts":                        "github.trusted_hosts",
		"kubeconfig":                                  "kubernetes.kubeconfig",
		"kubernetes-recovery-poll-interval":           "kubernetes.recovery_poll_interval",
		"kubernetes-conflict-retries":                 "kubernetes.conflict_retries",
		"kubernetes-preview-hostname-suffix":          "kubernetes.preview_hostname_suffix",
		"kubernetes-network-egress-excluded-cidrs":    "kubernetes.network_egress_excluded_cidrs",
		"observability-openobserve-base-url":          "observability.openobserve_base_url",
		"observability-openobserve-organization":      "observability.openobserve_organization",
		"observability-openobserve-log-stream":        "observability.openobserve_log_stream",
		"observability-openobserve-trace-stream":      "observability.openobserve_trace_stream",
		"observability-openobserve-credential-secret": "observability.openobserve_credential_secret",
		"observability-beszel-base-url":               "observability.beszel_base_url",
		"observability-beszel-token-secret":           "observability.beszel_token_secret",
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

	cfg := Config{
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
		},
		GRPC: GRPC{Address: v.GetString("grpc.address"), TLSCertFile: v.GetString("grpc.tls_cert_file"),
			TLSKeyFile: v.GetString("grpc.tls_key_file"), MaxBodyBytes: v.GetInt("grpc.max_body_bytes")},
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
		GitHub: GitHub{
			AppID:                       v.GetInt64("github.app_id"),
			PrivateKeyFile:              v.GetString("github.private_key_file"),
			APIBaseURL:                  v.GetString("github.api_base_url"),
			UploadURL:                   v.GetString("github.upload_url"),
			APIVersion:                  v.GetString("github.api_version"),
			DispatchObservationInterval: v.GetDuration("github.dispatch_observation_interval"),
			RunPollInterval:             v.GetDuration("github.run_poll_interval"),
			RequestTimeout:              v.GetDuration("github.request_timeout"),
			TrustedHosts:                normalizeSliceValues(v.GetStringSlice("github.trusted_hosts")),
		},
		Kubernetes: Kubernetes{
			Kubeconfig:                 v.GetString("kubernetes.kubeconfig"),
			RecoveryPollInterval:       v.GetDuration("kubernetes.recovery_poll_interval"),
			ConflictRetries:            v.GetInt("kubernetes.conflict_retries"),
			PreviewHostnameSuffix:      v.GetString("kubernetes.preview_hostname_suffix"),
			NetworkEgressExcludedCIDRs: normalizeSliceValues(v.GetStringSlice("kubernetes.network_egress_excluded_cidrs")),
		},
		Observability: Observability{
			OpenObserveBaseURL:          v.GetString("observability.openobserve_base_url"),
			OpenObserveOrganization:     v.GetString("observability.openobserve_organization"),
			OpenObserveLogStream:        v.GetString("observability.openobserve_log_stream"),
			OpenObserveTraceStream:      v.GetString("observability.openobserve_trace_stream"),
			OpenObserveCredentialSecret: v.GetString("observability.openobserve_credential_secret"),
			BeszelBaseURL:               v.GetString("observability.beszel_base_url"),
			BeszelTokenSecret:           v.GetString("observability.beszel_token_secret"),
		},
		InsecurePublicListeners: v.GetBool("insecure_public_listeners"),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("http.address", "127.0.0.1:8080")
	v.SetDefault("grpc.address", "127.0.0.1:9090")
	v.SetDefault("auth.session_ttl", "12h")
	v.SetDefault("auth.max_concurrent_hashes", 4)
	v.SetDefault("auth.rate_limit_per_minute", 10)
	v.SetDefault("auth.request_body_bytes", 65536)
	v.SetDefault("http.max_body_bytes", 1048576)
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
	v.SetDefault("github.api_base_url", "https://api.github.com/")
	v.SetDefault("github.upload_url", "https://uploads.github.com/")
	v.SetDefault("github.api_version", "2026-03-10")
	v.SetDefault("github.dispatch_observation_interval", "90s")
	v.SetDefault("github.run_poll_interval", "30s")
	v.SetDefault("github.request_timeout", "20s")
	v.SetDefault("github.trusted_hosts", []string{"api.github.com", "uploads.github.com"})
	v.SetDefault("kubernetes.recovery_poll_interval", "2s")
	v.SetDefault("kubernetes.conflict_retries", 5)
	v.SetDefault("kubernetes.preview_hostname_suffix", "preview.invalid")
}

func environment(environ []string) map[string]string {
	keys := map[string]string{
		"HTTP_ADDRESS":                                "http.address",
		"GRPC_ADDRESS":                                "grpc.address",
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
		"GITHUB_APP_ID":                               "github.app_id",
		"GITHUB_PRIVATE_KEY_FILE":                     "github.private_key_file",
		"GITHUB_API_BASE_URL":                         "github.api_base_url",
		"GITHUB_UPLOAD_URL":                           "github.upload_url",
		"GITHUB_API_VERSION":                          "github.api_version",
		"GITHUB_DISPATCH_OBSERVATION_INTERVAL":        "github.dispatch_observation_interval",
		"GITHUB_RUN_POLL_INTERVAL":                    "github.run_poll_interval",
		"GITHUB_REQUEST_TIMEOUT":                      "github.request_timeout",
		"GITHUB_TRUSTED_HOSTS":                        "github.trusted_hosts",
		"KUBECONFIG":                                  "kubernetes.kubeconfig",
		"KUBERNETES_RECOVERY_POLL_INTERVAL":           "kubernetes.recovery_poll_interval",
		"KUBERNETES_CONFLICT_RETRIES":                 "kubernetes.conflict_retries",
		"KUBERNETES_PREVIEW_HOSTNAME_SUFFIX":          "kubernetes.preview_hostname_suffix",
		"KUBERNETES_NETWORK_EGRESS_EXCLUDED_CIDRS":    "kubernetes.network_egress_excluded_cidrs",
		"OBSERVABILITY_OPENOBSERVE_BASE_URL":          "observability.openobserve_base_url",
		"OBSERVABILITY_OPENOBSERVE_ORGANIZATION":      "observability.openobserve_organization",
		"OBSERVABILITY_OPENOBSERVE_LOG_STREAM":        "observability.openobserve_log_stream",
		"OBSERVABILITY_OPENOBSERVE_TRACE_STREAM":      "observability.openobserve_trace_stream",
		"OBSERVABILITY_OPENOBSERVE_CREDENTIAL_SECRET": "observability.openobserve_credential_secret",
		"OBSERVABILITY_BESZEL_BASE_URL":               "observability.beszel_base_url",
		"OBSERVABILITY_BESZEL_TOKEN_SECRET":           "observability.beszel_token_secret",
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
	return g.AppID > 0 && strings.TrimSpace(g.PrivateKeyFile) != "" &&
		strings.TrimSpace(g.APIBaseURL) != "" && strings.TrimSpace(g.UploadURL) != "" &&
		strings.TrimSpace(g.APIVersion) != "" && g.DispatchObservationInterval > 0 &&
		g.RunPollInterval > 0 && g.RequestTimeout > 0
}

func (c Config) Validate() error {
	var errs []error
	if _, _, err := net.SplitHostPort(c.HTTP.Address); err != nil {
		errs = append(errs, fmt.Errorf("http.address must be a host:port pair: %w", err))
	}
	if _, _, err := net.SplitHostPort(c.GRPC.Address); err != nil {
		errs = append(errs, fmt.Errorf("grpc.address must be a host:port pair: %w", err))
	}
	if c.GRPC.Address == c.HTTP.Address {
		errs = append(errs, fmt.Errorf("grpc.address and http.address must be different"))
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
	githubConfigured := c.GitHub.AppID != 0 || strings.TrimSpace(c.GitHub.PrivateKeyFile) != ""
	if githubConfigured {
		if c.GitHub.AppID <= 0 {
			errs = append(errs, fmt.Errorf("github.app_id must be greater than zero"))
		}
		if strings.TrimSpace(c.GitHub.PrivateKeyFile) == "" {
			errs = append(errs, fmt.Errorf("github.private_key_file is required"))
		}
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
	if c.Kubernetes.RecoveryPollInterval <= 0 {
		errs = append(errs, fmt.Errorf("kubernetes.recovery_poll_interval must be greater than zero"))
	}
	if c.Kubernetes.ConflictRetries <= 0 {
		errs = append(errs, fmt.Errorf("kubernetes.conflict_retries must be greater than zero"))
	}
	if suffix := strings.TrimSpace(c.Kubernetes.PreviewHostnameSuffix); suffix == "" || len(suffix) > 253 || strings.ContainsAny(suffix, "/:@") {
		errs = append(errs, fmt.Errorf("kubernetes.preview_hostname_suffix must be a DNS suffix"))
	}
	for _, cidr := range c.Kubernetes.NetworkEgressExcludedCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			errs = append(errs, fmt.Errorf("kubernetes.network_egress_excluded_cidrs entry %q is not a valid CIDR", cidr))
		}
	}
	errs = append(errs, c.Observability.validate()...)
	return errors.Join(errs...)
}

// LogsEnabled reports whether OpenObserve-backed logs/traces proxying is
// configured.
func (o Observability) LogsEnabled() bool {
	return strings.TrimSpace(o.OpenObserveBaseURL) != ""
}

// MetricsEnabled reports whether Beszel-backed operator metrics are configured.
func (o Observability) MetricsEnabled() bool {
	return strings.TrimSpace(o.BeszelBaseURL) != ""
}

func (o Observability) validate() []error {
	var errs []error
	if o.LogsEnabled() {
		if u, err := url.ParseRequestURI(o.OpenObserveBaseURL); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf("observability.openobserve_base_url must be an absolute HTTPS URL"))
		}
		if strings.TrimSpace(o.OpenObserveOrganization) == "" {
			errs = append(errs, fmt.Errorf("observability.openobserve_organization is required when OpenObserve is enabled"))
		}
	}
	if o.MetricsEnabled() {
		if u, err := url.ParseRequestURI(o.BeszelBaseURL); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf("observability.beszel_base_url must be an absolute HTTPS URL"))
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
