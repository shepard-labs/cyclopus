package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cyclopus.json")
	contents := `{
  "http": {
    "address": "127.0.0.1:7000",
    "read_timeout": "20s"
  },
  "log": {"level": "debug"}
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(
		[]string{"--config", path, "--http-address", "127.0.0.1:9000"},
		[]string{"CYCLOPUS_HTTP_ADDRESS=127.0.0.1:8000", "CYCLOPUS_LOG_LEVEL=warn", "CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus", "CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef", "CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef"},
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTP.Address != "127.0.0.1:9000" {
		t.Fatalf("address = %q, want flag value", cfg.HTTP.Address)
	}
	if cfg.GRPC.Address != "127.0.0.1:9090" {
		t.Fatalf("gRPC address = %q, want default value", cfg.GRPC.Address)
	}
	if cfg.HTTP.ReadTimeout != 20*time.Second {
		t.Fatalf("read timeout = %s, want file value", cfg.HTTP.ReadTimeout)
	}
	if cfg.Log.Level != "warn" {
		t.Fatalf("log level = %q, want environment value", cfg.Log.Level)
	}
	if cfg.HTTP.WriteTimeout != 30*time.Second {
		t.Fatalf("write timeout = %s, want default value", cfg.HTTP.WriteTimeout)
	}
	if cfg.DB.URL != "postgres://localhost/cyclopus" {
		t.Fatalf("database URL = %q, want environment value", cfg.DB.URL)
	}
	if cfg.DB.MigrationLockTimeout != 30*time.Second {
		t.Fatalf("migration lock timeout = %s, want default value", cfg.DB.MigrationLockTimeout)
	}
	if cfg.Worker.ScanInterval != 30*time.Second || cfg.Worker.LeaseDuration != time.Minute {
		t.Fatalf("worker config = %+v, want default values", cfg.Worker)
	}
	if cfg.Auth.MaxConcurrentHashes != 4 || cfg.Auth.RateLimitPerMinute != 10 || cfg.Auth.RequestBodyBytes != 65536 {
		t.Fatalf("auth limits = %+v, want default values", cfg.Auth)
	}
	if cfg.GitHub.APIBaseURL != "https://api.github.com/" || cfg.GitHub.UploadURL != "https://uploads.github.com/" ||
		cfg.GitHub.APIVersion != "2026-03-10" || cfg.GitHub.DispatchObservationInterval != 90*time.Second ||
		cfg.GitHub.RunPollInterval != 30*time.Second || cfg.GitHub.RequestTimeout != 20*time.Second {
		t.Fatalf("github config = %+v, want default values", cfg.GitHub)
	}
	if cfg.Kubernetes.RecoveryPollInterval != 2*time.Second || cfg.Kubernetes.ConflictRetries != 5 {
		t.Fatalf("kubernetes config = %+v, want default values", cfg.Kubernetes)
	}
}

func TestLoadGitHubConfigPrecedence(t *testing.T) {
	cfg, err := Load(
		[]string{"--github-app-id", "42", "--github-api-version", "2026-03-10", "--github-run-poll-interval", "5s"},
		[]string{
			"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
			"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
			"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
			"CYCLOPUS_GITHUB_APP_ID=21",
			"CYCLOPUS_GITHUB_PRIVATE_KEY_FILE=/run/secrets/github.pem",
			"CYCLOPUS_GITHUB_RUN_POLL_INTERVAL=10s",
		},
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHub.AppID != 42 || cfg.GitHub.PrivateKeyFile != "/run/secrets/github.pem" || cfg.GitHub.RunPollInterval != 5*time.Second {
		t.Fatalf("github config = %+v", cfg.GitHub)
	}
}

func TestLoadGitHubPartialConfigValidation(t *testing.T) {
	_, err := Load(nil, []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_GITHUB_APP_ID=42",
	})
	if err == nil || !strings.Contains(err.Error(), "github.private_key_file is required") {
		t.Fatalf("Load() error = %v, want private key requirement", err)
	}
}

func TestLoadKubernetesRecoveryConfigPrecedence(t *testing.T) {
	cfg, err := Load(
		[]string{"--kubeconfig", "/flag/kubeconfig", "--kubernetes-conflict-retries", "9"},
		[]string{"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus", "CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef", "CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef", "CYCLOPUS_KUBECONFIG=/env/kubeconfig", "CYCLOPUS_KUBERNETES_RECOVERY_POLL_INTERVAL=250ms", "CYCLOPUS_KUBERNETES_CONFLICT_RETRIES=7"},
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Kubernetes.Kubeconfig != "/flag/kubeconfig" || cfg.Kubernetes.RecoveryPollInterval != 250*time.Millisecond || cfg.Kubernetes.ConflictRetries != 9 {
		t.Fatalf("kubernetes config = %+v", cfg.Kubernetes)
	}
}

func TestGitHubEnabledOnlyWithCompleteConfig(t *testing.T) {
	cfg, err := Load(nil, []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		"CYCLOPUS_GITHUB_APP_ID=42",
		"CYCLOPUS_GITHUB_PRIVATE_KEY_FILE=/run/secrets/github.pem",
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.GitHub.Enabled() {
		t.Fatal("GitHub.Enabled() = false for complete configuration")
	}
	cfg.GitHub.PrivateKeyFile = ""
	if cfg.GitHub.Enabled() {
		t.Fatal("GitHub.Enabled() = true for incomplete configuration")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := Load(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "database.url is required") {
		t.Fatalf("Load() error = %v, want required database URL", err)
	}
}

func TestLoadRequiresBootstrapSecretOfAtLeast32Bytes(t *testing.T) {
	_, err := Load(nil, []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=0123456789abcdef0123456789abcde",
	})
	if err == nil || !strings.Contains(err.Error(), "auth.bootstrap_secret must contain at least 32 bytes") {
		t.Fatalf("Load() error = %v, want bootstrap secret length error", err)
	}
}

func TestLoadValidation(t *testing.T) {
	_, err := Load(
		[]string{"--http-address", "invalid", "--grpc-address", "also-invalid", "--http-read-timeout", "0s", "--log-level", "verbose", "--worker-scan-interval", "0s", "--github-api-base-url", "relative", "--github-dispatch-observation-interval", "0s", "--kubernetes-recovery-poll-interval", "0s", "--kubernetes-conflict-retries", "-1"},
		[]string{"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus", "CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef", "CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef"},
	)
	if err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
	for _, want := range []string{"http.address", "grpc.address", "read_timeout", "log.level", "worker.scan_interval", "github.api_base_url", "github.dispatch_observation_interval", "kubernetes.recovery_poll_interval", "kubernetes.conflict_retries"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoadRejectsUntrustedOrInsecureGitHubURLsAndLongTimeout(t *testing.T) {
	_, err := Load([]string{
		"--worker-lease-duration", "10s",
		"--github-api-base-url", "http://evil.example/",
		"--github-upload-url", "https://uploads.evil.example/",
		"--github-request-timeout", "10s",
	}, []string{"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus", "CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef", "CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef"})
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	for _, want := range []string{"HTTPS", "trusted_hosts", "shorter than worker.lease_duration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoadRejectsPublicPlaintextAndLongSession(t *testing.T) {
	_, err := Load([]string{"--http-address", "0.0.0.0:8080", "--auth-session-ttl", "169h"}, []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	})
	if err == nil || !strings.Contains(err.Error(), "plaintext listener must be loopback") || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadAllowsSevenDaySessionTTL(t *testing.T) {
	cfg, err := Load([]string{"--auth-session-ttl", "168h"}, []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	})
	if err != nil || cfg.Auth.SessionTTL != 7*24*time.Hour {
		t.Fatalf("Load() = %+v, %v", cfg.Auth, err)
	}
}

func TestLoadAllowsExplicitDevelopmentPublicPlaintext(t *testing.T) {
	cfg, err := Load([]string{"--http-address", "0.0.0.0:8080", "--insecure-public-listeners"}, []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	})
	if err != nil || !cfg.InsecurePublicListeners {
		t.Fatalf("Load() = %+v, %v", cfg, err)
	}
}

func TestLoadListenerSecurityDefaults(t *testing.T) {
	cfg, err := Load(nil, []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTP.Address != "127.0.0.1:8080" || cfg.GRPC.Address != "127.0.0.1:9090" {
		t.Fatalf("listener defaults = HTTP %q, gRPC %q", cfg.HTTP.Address, cfg.GRPC.Address)
	}
	if cfg.InsecurePublicListeners {
		t.Fatal("insecure_public_listeners defaulted to true")
	}
}

func TestLoadRejectsPublicPlaintextForEachListener(t *testing.T) {
	baseEnv := []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "HTTP", args: []string{"--http-address", "0.0.0.0:8080"}, want: "http.address plaintext listener"},
		{name: "gRPC", args: []string{"--grpc-address", "[::]:9090"}, want: "grpc.address plaintext listener"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(test.args, baseEnv)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadRequiresCompleteTLSPairs(t *testing.T) {
	baseEnv := []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "HTTP certificate only", args: []string{"--http-tls-cert-file", "server.crt"}, want: "http TLS certificate and key"},
		{name: "HTTP key only", args: []string{"--http-tls-key-file", "server.key"}, want: "http TLS certificate and key"},
		{name: "gRPC certificate only", args: []string{"--grpc-tls-cert-file", "server.crt"}, want: "grpc TLS certificate and key"},
		{name: "gRPC key only", args: []string{"--grpc-tls-key-file", "server.key"}, want: "grpc TLS certificate and key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(test.args, baseEnv)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadAllowsPublicTLSListeners(t *testing.T) {
	cfg, err := Load([]string{
		"--http-address", "0.0.0.0:8443",
		"--http-tls-cert-file", "http.crt",
		"--http-tls-key-file", "http.key",
		"--grpc-address", "[::]:9443",
		"--grpc-tls-cert-file", "grpc.crt",
		"--grpc-tls-key-file", "grpc.key",
	}, []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTP.TLSCertFile != "http.crt" || cfg.HTTP.TLSKeyFile != "http.key" ||
		cfg.GRPC.TLSCertFile != "grpc.crt" || cfg.GRPC.TLSKeyFile != "grpc.key" {
		t.Fatalf("TLS config = HTTP %+v, gRPC %+v", cfg.HTTP, cfg.GRPC)
	}
}
