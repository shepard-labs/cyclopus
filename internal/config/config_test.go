package config

import (
	"encoding/base64"
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
		withWebhookEncryption([]string{"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example", "CYCLOPUS_HTTP_ADDRESS=127.0.0.1:8000", "CYCLOPUS_LOG_LEVEL=warn", "CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus", "CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef", "CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef"}),
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
	if cfg.Worker.ScanInterval != 30*time.Second || cfg.Worker.LeaseDuration != time.Minute || cfg.Worker.MaxConcurrentDeploys != 4 {
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
	if cfg.Kubernetes.RecoveryPollInterval != 2*time.Second || cfg.Kubernetes.ConflictRetries != 5 || cfg.Kubernetes.PreviewHostnameSuffix != "preview.invalid" {
		t.Fatalf("kubernetes config = %+v, want default values", cfg.Kubernetes)
	}
	if cfg.Kubernetes.CertManagerClusterIssuer != "letsencrypt" {
		t.Fatalf("cert-manager cluster issuer = %q, want default letsencrypt", cfg.Kubernetes.CertManagerClusterIssuer)
	}
}

func TestLoadWebhookAllowedSecretRefs(t *testing.T) {
	cfg, err := Load(nil, append(baseValidEnv,
		"CYCLOPUS_WEBHOOK_RELEASE_NAME=release",
		"CYCLOPUS_WEBHOOK_ALLOWED_SECRET_REFS=k8s-secret://cyclopus/cyclopus-webhook-primary#secret,k8s-secret://cyclopus/cyclopus-webhook-secondary#key"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Webhook.AllowedSecretRefs) != 2 {
		t.Fatalf("AllowedSecretRefs = %v", cfg.Webhook.AllowedSecretRefs)
	}
}

func TestLoadWebhookPayloadEncryptionKeyring(t *testing.T) {
	path := writeWebhookKeyring(t, 0o600, `{"active_version":"k2","keys":{"k1":"`+base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))+`","k2":"`+base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))+`"}}`)
	cfg, err := Load(nil, append(baseValidEnvWithoutWebhook(), "CYCLOPUS_WEBHOOK_PAYLOAD_ENCRYPTION_KEY_FILE="+path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Webhook.PayloadEncryptionKeyFile != path {
		t.Fatalf("webhook encryption config = %+v", cfg.Webhook)
	}
}

func TestLoadRejectsInvalidWebhookPayloadEncryptionKeyring(t *testing.T) {
	_, err := Load(nil, baseValidEnvWithoutWebhook())
	if err == nil || !strings.Contains(err.Error(), "webhook.payload_encryption_key_file") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsInvalidWebhookAllowedSecretRefs(t *testing.T) {
	for _, value := range []string{
		"bad",
		"k8s-secret://other/cyclopus-webhook-primary#secret",
		"k8s-secret://cyclopus/arbitrary#secret",
		"k8s-secret://cyclopus/cyclopus-webhook-primary#secret,k8s-secret://cyclopus/cyclopus-webhook-primary#secret",
	} {
		_, err := Load(nil, append(baseValidEnv, "CYCLOPUS_WEBHOOK_RELEASE_NAME=release", "CYCLOPUS_WEBHOOK_ALLOWED_SECRET_REFS="+value))
		if err == nil || !strings.Contains(err.Error(), "webhook.allowed_secret_refs") {
			t.Fatalf("Load(%q) error = %v", value, err)
		}
	}
}

func TestLoadCertManagerClusterIssuerPrecedence(t *testing.T) {
	// Flag overrides env, mirroring the other kubernetes fields.
	cfg, err := Load(
		[]string{"--kubernetes-cert-manager-cluster-issuer", "letsencrypt-prod"},
		append(baseValidEnv, "CYCLOPUS_KUBERNETES_CERT_MANAGER_CLUSTER_ISSUER=letsencrypt-staging"),
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Kubernetes.CertManagerClusterIssuer != "letsencrypt-prod" {
		t.Fatalf("cert-manager cluster issuer = %q, want flag value letsencrypt-prod", cfg.Kubernetes.CertManagerClusterIssuer)
	}
}

func TestLoadCertManagerClusterIssuerFromEnv(t *testing.T) {
	cfg, err := Load(
		nil,
		append(baseValidEnv, "CYCLOPUS_KUBERNETES_CERT_MANAGER_CLUSTER_ISSUER=letsencrypt-staging"),
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Kubernetes.CertManagerClusterIssuer != "letsencrypt-staging" {
		t.Fatalf("cert-manager cluster issuer = %q, want env value letsencrypt-staging", cfg.Kubernetes.CertManagerClusterIssuer)
	}
}

func TestLoadCertManagerClusterIssuerEmptyAllowed(t *testing.T) {
	// An explicitly empty issuer disables the annotation and must validate.
	cfg, err := Load(
		[]string{"--kubernetes-cert-manager-cluster-issuer", ""},
		baseValidEnv,
	)
	if err != nil {
		t.Fatalf("Load() with empty issuer error = %v", err)
	}
	if cfg.Kubernetes.CertManagerClusterIssuer != "" {
		t.Fatalf("cert-manager cluster issuer = %q, want empty", cfg.Kubernetes.CertManagerClusterIssuer)
	}
}

func TestLoadCertManagerClusterIssuerRejectsInvalid(t *testing.T) {
	_, err := Load(
		[]string{"--kubernetes-cert-manager-cluster-issuer", "Bad Issuer!"},
		baseValidEnv,
	)
	if err == nil || !strings.Contains(err.Error(), "kubernetes.cert_manager_cluster_issuer must be a valid Kubernetes resource name") {
		t.Fatalf("Load() error = %v, want cert-manager cluster issuer validation", err)
	}
}

func TestLoadGitHubConfigPrecedence(t *testing.T) {
	cfg, err := Load(
		[]string{"--external-url", "https://cyclopus.example/", "--github-api-version", "2026-03-10", "--github-run-poll-interval", "5s"},
		withWebhookEncryption([]string{
			"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
			"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
			"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
			"CYCLOPUS_EXTERNAL_URL=https://env.example",
			"CYCLOPUS_GITHUB_RUN_POLL_INTERVAL=10s",
		}),
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ExternalURL != "https://cyclopus.example" || cfg.GitHub.RunPollInterval != 5*time.Second {
		t.Fatalf("external URL/GitHub config = %q %+v", cfg.ExternalURL, cfg.GitHub)
	}
}

func TestLoadExternalURLValidationAndNormalization(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
		valid             bool
	}{
		{name: "canonical", value: " https://cyclopus.example/ ", want: "https://cyclopus.example", valid: true},
		{name: "path rejected", value: "https://cyclopus.example/control/"},
		{name: "http", value: "http://cyclopus.example"},
		{name: "http loopback allowed", value: "http://127.0.0.1:8081", want: "http://127.0.0.1:8081", valid: true},
		{name: "http localhost allowed", value: "http://localhost:8081", want: "http://localhost:8081", valid: true},
		{name: "http loopback with slash", value: "http://127.0.0.1:8081/", want: "http://127.0.0.1:8081", valid: true},
		{name: "userinfo", value: "https://admin@cyclopus.example"},
		{name: "query", value: "https://cyclopus.example?next=evil"},
		{name: "fragment", value: "https://cyclopus.example/#evil"},
		{name: "malformed host", value: "https://bad host/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := append(append([]string(nil), baseValidEnv...), "CYCLOPUS_EXTERNAL_URL="+test.value)
			cfg, err := Load(nil, env)
			if !test.valid {
				if err == nil || !strings.Contains(err.Error(), "external_url") {
					t.Fatalf("Load() error = %v, want external_url rejection", err)
				}
				return
			}
			if err != nil || cfg.ExternalURL != test.want {
				t.Fatalf("Load() external URL = %q, %v, want %q", cfg.ExternalURL, err, test.want)
			}
		})
	}
}

func TestLoadKubernetesRecoveryConfigPrecedence(t *testing.T) {
	cfg, err := Load(
		[]string{"--kubeconfig", "/flag/kubeconfig", "--kubernetes-conflict-retries", "9", "--kubernetes-preview-hostname-suffix", "flag.example.test"},
		append(baseValidEnv, "CYCLOPUS_KUBECONFIG=/env/kubeconfig", "CYCLOPUS_KUBERNETES_RECOVERY_POLL_INTERVAL=250ms", "CYCLOPUS_KUBERNETES_CONFLICT_RETRIES=7", "CYCLOPUS_KUBERNETES_PREVIEW_HOSTNAME_SUFFIX=env.example.test"),
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Kubernetes.Kubeconfig != "/flag/kubeconfig" || cfg.Kubernetes.RecoveryPollInterval != 250*time.Millisecond || cfg.Kubernetes.ConflictRetries != 9 || cfg.Kubernetes.PreviewHostnameSuffix != "flag.example.test" {
		t.Fatalf("kubernetes config = %+v", cfg.Kubernetes)
	}
}

func TestLoadCustomDomainCNAMETargetDefaultsToPreviewSuffix(t *testing.T) {
	cfg, err := Load(
		[]string{"--kubernetes-preview-hostname-suffix", "203-0-113-7.example.test"},
		baseValidEnv,
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Kubernetes.CustomDomainCNAMETarget != "203-0-113-7.example.test" {
		t.Fatalf("custom domain CNAME target = %q, want fallback to preview suffix", cfg.Kubernetes.CustomDomainCNAMETarget)
	}
	if cfg.Kubernetes.CustomDomainCNAMETarget != cfg.Kubernetes.PreviewHostnameSuffix {
		t.Fatalf("custom domain CNAME target = %q, want equal to preview suffix %q", cfg.Kubernetes.CustomDomainCNAMETarget, cfg.Kubernetes.PreviewHostnameSuffix)
	}
}

func TestLoadCustomDomainCNAMETargetOverride(t *testing.T) {
	cfg, err := Load(
		[]string{"--kubernetes-preview-hostname-suffix", "203-0-113-7.example.test", "--kubernetes-custom-domain-cname-target", "ingress.example.test"},
		baseValidEnv,
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Kubernetes.CustomDomainCNAMETarget != "ingress.example.test" {
		t.Fatalf("custom domain CNAME target = %q, want explicit override", cfg.Kubernetes.CustomDomainCNAMETarget)
	}
	if cfg.Kubernetes.PreviewHostnameSuffix != "203-0-113-7.example.test" {
		t.Fatalf("preview suffix = %q, want unaffected by override", cfg.Kubernetes.PreviewHostnameSuffix)
	}
}

func TestLoadCustomDomainCNAMETargetRejectsInvalid(t *testing.T) {
	_, err := Load(
		[]string{"--kubernetes-custom-domain-cname-target", "bad/target"},
		baseValidEnv,
	)
	if err == nil || !strings.Contains(err.Error(), "kubernetes.custom_domain_cname_target must be a DNS suffix") {
		t.Fatalf("Load() error = %v, want custom domain CNAME target validation", err)
	}
}

func TestLoadNetworkEgressExcludedCIDRsParses(t *testing.T) {
	cfg, err := Load(
		[]string{"--kubernetes-network-egress-excluded-cidrs", "203.0.113.5/32,198.51.100.0/24"},
		baseValidEnv,
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"203.0.113.5/32", "198.51.100.0/24"}
	got := cfg.Kubernetes.NetworkEgressExcludedCIDRs
	if len(got) != len(want) {
		t.Fatalf("NetworkEgressExcludedCIDRs = %v, want %v", got, want)
	}
	for i, cidr := range want {
		if got[i] != cidr {
			t.Fatalf("NetworkEgressExcludedCIDRs[%d] = %q, want %q", i, got[i], cidr)
		}
	}
}

func TestLoadRejectsInvalidNetworkEgressExcludedCIDR(t *testing.T) {
	_, err := Load(
		[]string{"--kubernetes-network-egress-excluded-cidrs", "203.0.113.5"},
		baseValidEnv,
	)
	if err == nil || !strings.Contains(err.Error(), "kubernetes.network_egress_excluded_cidrs") {
		t.Fatalf("Load() error = %v, want invalid CIDR error", err)
	}
}

func TestLoadNetworkEgressExcludedCIDRsFromEnvSplits(t *testing.T) {
	cfg, err := Load(nil, withWebhookEncryption([]string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		"CYCLOPUS_KUBERNETES_NETWORK_EGRESS_EXCLUDED_CIDRS=203.0.113.0/24,198.51.100.0/24",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"203.0.113.0/24", "198.51.100.0/24"}
	got := cfg.Kubernetes.NetworkEgressExcludedCIDRs
	if len(got) != len(want) {
		t.Fatalf("NetworkEgressExcludedCIDRs = %v, want %v", got, want)
	}
	for i, cidr := range want {
		if got[i] != cidr {
			t.Fatalf("NetworkEgressExcludedCIDRs[%d] = %q, want %q", i, got[i], cidr)
		}
	}
}

func TestLoadTrustedHostsFromEnvSplits(t *testing.T) {
	cfg, err := Load(nil, withWebhookEncryption([]string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		"CYCLOPUS_GITHUB_TRUSTED_HOSTS=api.github.com,uploads.github.com",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"api.github.com", "uploads.github.com"}
	got := cfg.GitHub.TrustedHosts
	if len(got) != len(want) {
		t.Fatalf("TrustedHosts = %v, want %v", got, want)
	}
	for i, host := range want {
		if got[i] != host {
			t.Fatalf("TrustedHosts[%d] = %q, want %q", i, got[i], host)
		}
	}
}

func TestLoadSingleValueEnvSliceYieldsSingleElement(t *testing.T) {
	cfg, err := Load(nil, withWebhookEncryption([]string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		"CYCLOPUS_KUBERNETES_NETWORK_EGRESS_EXCLUDED_CIDRS=203.0.113.0/24",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := cfg.Kubernetes.NetworkEgressExcludedCIDRs
	if len(got) != 1 || got[0] != "203.0.113.0/24" {
		t.Fatalf("NetworkEgressExcludedCIDRs = %v, want [203.0.113.0/24]", got)
	}
}

func TestLoadUnsetEnvSliceDoesNotClobberFlag(t *testing.T) {
	cfg, err := Load(
		[]string{"--kubernetes-network-egress-excluded-cidrs", "203.0.113.5/32"},
		withWebhookEncryption([]string{
			"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
			"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
			"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
			"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		}),
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := cfg.Kubernetes.NetworkEgressExcludedCIDRs
	if len(got) != 1 || got[0] != "203.0.113.5/32" {
		t.Fatalf("NetworkEgressExcludedCIDRs = %v, want flag value [203.0.113.5/32]", got)
	}
	// The default trusted hosts must survive when neither flag nor env sets them.
	wantHosts := []string{"api.github.com", "uploads.github.com"}
	if len(cfg.GitHub.TrustedHosts) != len(wantHosts) {
		t.Fatalf("TrustedHosts = %v, want defaults %v", cfg.GitHub.TrustedHosts, wantHosts)
	}
}

func TestLoadNetworkEgressExcludedCIDRsTrimsWhitespaceAndEmpties(t *testing.T) {
	cfg, err := Load(nil, withWebhookEncryption([]string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		"CYCLOPUS_KUBERNETES_NETWORK_EGRESS_EXCLUDED_CIDRS= 203.0.113.0/24 , 198.51.100.0/24 ,",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"203.0.113.0/24", "198.51.100.0/24"}
	got := cfg.Kubernetes.NetworkEgressExcludedCIDRs
	if len(got) != len(want) {
		t.Fatalf("NetworkEgressExcludedCIDRs = %v, want %v", got, want)
	}
	for i, cidr := range want {
		if got[i] != cidr {
			t.Fatalf("NetworkEgressExcludedCIDRs[%d] = %q, want %q (no whitespace/empties)", i, got[i], cidr)
		}
	}
}

func TestLoadNetworkEgressExcludedCIDRsFlagTrimsWhitespace(t *testing.T) {
	cfg, err := Load(
		[]string{"--kubernetes-network-egress-excluded-cidrs", " 203.0.113.0/24 , 198.51.100.0/24 "},
		withWebhookEncryption([]string{
			"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
			"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
			"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
			"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		}),
	)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"203.0.113.0/24", "198.51.100.0/24"}
	got := cfg.Kubernetes.NetworkEgressExcludedCIDRs
	if len(got) != len(want) {
		t.Fatalf("NetworkEgressExcludedCIDRs = %v, want %v", got, want)
	}
	for i, cidr := range want {
		if got[i] != cidr {
			t.Fatalf("NetworkEgressExcludedCIDRs[%d] = %q, want %q (flag path trimmed)", i, got[i], cidr)
		}
	}
}

func TestGitHubEnabledUsesRuntimeOnboardingConfiguration(t *testing.T) {
	cfg, err := Load(nil, withWebhookEncryption([]string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.GitHub.Enabled() {
		t.Fatal("GitHub.Enabled() = false for valid runtime configuration")
	}
	cfg.GitHub.RequestTimeout = 0
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
		[]string{"--http-address", "invalid", "--grpc-address", "also-invalid", "--http-read-timeout", "0s", "--log-level", "verbose", "--worker-scan-interval", "0s", "--worker-max-concurrent-deploys", "0", "--github-api-base-url", "relative", "--github-dispatch-observation-interval", "0s", "--kubernetes-recovery-poll-interval", "0s", "--kubernetes-conflict-retries", "-1"},
		baseValidEnv,
	)
	if err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
	for _, want := range []string{"http.address", "grpc.address", "read_timeout", "log.level", "worker.scan_interval", "worker.max_concurrent_deploys", "github.api_base_url", "github.dispatch_observation_interval", "kubernetes.recovery_poll_interval", "kubernetes.conflict_retries"} {
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
	}, baseValidEnv)
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
	cfg, err := Load([]string{"--auth-session-ttl", "168h"}, withWebhookEncryption([]string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	}))
	if err != nil || cfg.Auth.SessionTTL != 7*24*time.Hour {
		t.Fatalf("Load() = %+v, %v", cfg.Auth, err)
	}
}

func TestLoadAllowsExplicitDevelopmentPublicPlaintext(t *testing.T) {
	cfg, err := Load([]string{"--http-address", "0.0.0.0:8080", "--insecure-public-listeners"}, withWebhookEncryption([]string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	}))
	if err != nil || !cfg.InsecurePublicListeners {
		t.Fatalf("Load() = %+v, %v", cfg, err)
	}
}

func TestLoadListenerSecurityDefaults(t *testing.T) {
	cfg, err := Load(nil, withWebhookEncryption([]string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	}))
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
	}, withWebhookEncryption([]string{
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTP.TLSCertFile != "http.crt" || cfg.HTTP.TLSKeyFile != "http.key" ||
		cfg.GRPC.TLSCertFile != "grpc.crt" || cfg.GRPC.TLSKeyFile != "grpc.key" {
		t.Fatalf("TLS config = HTTP %+v, gRPC %+v", cfg.HTTP, cfg.GRPC)
	}
}

var baseValidEnv = withWebhookEncryption([]string{
	"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
	"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
	"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
	"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
	"CYCLOPUS_KUBERNETES_INSTANCE_NAMESPACE=cyclopus",
})

func withWebhookEncryption(environ []string) []string {
	file, err := os.CreateTemp("", "cyclopus-webhook-keyring-*.json")
	if err != nil {
		panic(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if err := os.WriteFile(file.Name(), []byte(`{"active_version":"k1","keys":{"k1":"`+encoded+`"}}`), 0o600); err != nil {
		panic(err)
	}
	return append(environ, "CYCLOPUS_WEBHOOK_PAYLOAD_ENCRYPTION_KEY_FILE="+file.Name())
}

func baseValidEnvWithoutWebhook() []string {
	return []string{
		"CYCLOPUS_DATABASE_URL=postgres://localhost/cyclopus",
		"CYCLOPUS_AUTH_TOKEN_PEPPER=0123456789abcdef0123456789abcdef",
		"CYCLOPUS_AUTH_BOOTSTRAP_SECRET=bootstrap-0123456789abcdef0123456789abcdef",
		"CYCLOPUS_EXTERNAL_URL=https://cyclopus.example",
		"CYCLOPUS_KUBERNETES_INSTANCE_NAMESPACE=cyclopus",
	}
}

func writeWebhookKeyring(t *testing.T, mode os.FileMode, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMetricsEnabledRequiresURLAndCredential(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantErr     bool
		wantErrSub  string
		wantEnabled bool
	}{
		{
			name:        "neither set",
			args:        nil,
			wantEnabled: false,
		},
		{
			name:        "URL only: still disabled, URL still validated",
			args:        []string{"--observability-beszel-base-url", "https://beszel.example"},
			wantEnabled: false,
		},
		{
			name:        "credential only, no URL: disabled",
			args:        []string{"--observability-beszel-credential-secret", "k8s-secret://cyclopus/beszel-auth#credential"},
			wantEnabled: false,
		},
		{
			name: "URL and credential: enabled",
			args: []string{
				"--observability-beszel-base-url", "https://beszel.example",
				"--observability-beszel-credential-secret", "k8s-secret://cyclopus/beszel-auth#credential",
			},
			wantEnabled: true,
		},
		{
			name:       "malformed URL with no credential is still rejected",
			args:       []string{"--observability-beszel-base-url", "not-a-url"},
			wantErr:    true,
			wantErrSub: "observability.beszel_base_url must be an absolute https:// URL",
		},
		{
			name:       "plaintext URL to an external host is still rejected",
			args:       []string{"--observability-beszel-base-url", "http://beszel.example", "--observability-beszel-credential-secret", "k8s-secret://cyclopus/beszel-auth#credential"},
			wantErr:    true,
			wantErrSub: "observability.beszel_base_url must be an absolute https:// URL",
		},
		{
			// §8 keeps Beszel in-cluster only, so the installer points the API
			// at its ClusterIP Service. That address is plaintext by design.
			name: "plaintext in-cluster .svc URL with a credential is accepted",
			args: []string{
				"--observability-beszel-base-url", "http://beszel.cyclopus.svc:8090",
				"--observability-beszel-credential-secret", "k8s-secret://cyclopus/beszel-auth#credential",
			},
			wantEnabled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(tc.args, baseValidEnv)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("Load() error = %v, want substring %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got := cfg.Observability.MetricsEnabled(); got != tc.wantEnabled {
				t.Fatalf("MetricsEnabled() = %v, want %v", got, tc.wantEnabled)
			}
		})
	}
}

func TestTelemetrySecretRefsMustUseControlPlaneNamespace(t *testing.T) {
	for _, reference := range []string{"not-a-ref", "k8s-secret://other/openobserve#credential"} {
		_, err := Load([]string{
			"--observability-openobserve-base-url", "https://openobserve.example",
			"--observability-openobserve-organization", "cyclopus",
			"--observability-openobserve-credential-secret", reference,
		}, baseValidEnv)
		if err == nil || !strings.Contains(err.Error(), "observability.openobserve_credential_secret") {
			t.Fatalf("Load() error = %v for %q, want telemetry Secret ref rejection", err, reference)
		}
	}
}
