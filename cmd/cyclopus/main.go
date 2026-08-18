package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	githubadapter "github.com/shepard-labs/cyclopus/internal/adapters/github"
	kubernetesadapter "github.com/shepard-labs/cyclopus/internal/adapters/kubernetes"
	"github.com/shepard-labs/cyclopus/internal/adapters/observability"
	"github.com/shepard-labs/cyclopus/internal/auth"
	"github.com/shepard-labs/cyclopus/internal/config"
	"github.com/shepard-labs/cyclopus/internal/logging"
	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/server"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
	"github.com/shepard-labs/cyclopus/internal/ui"
	"github.com/shepard-labs/cyclopus/internal/uihandler"
	webhookdelivery "github.com/shepard-labs/cyclopus/internal/webhook"
	"github.com/shepard-labs/cyclopus/internal/worker"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if err := run(os.Args[1:], os.Environ()); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("cyclopus stopped", "error", err)
		os.Exit(1)
	}
}

func run(args, environ []string) error {
	cfg, err := config.Load(args, environ)
	if err != nil {
		return err
	}

	logger, err := logging.New(os.Stdout, cfg.Log.Level)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := store.Migrate(ctx, store.MigrationConfig{
		URL:         cfg.DB.URL,
		LockTimeout: cfg.DB.MigrationLockTimeout,
		Migrations:  store.Migrations,
	}); err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, cfg.DB.URL)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	owner := recoveryOwner()
	webhookCodec, err := newGitHubWebhookPayloadCodec(cfg.Webhook)
	if err != nil {
		return fmt.Errorf("construct GitHub webhook payload encryption keyring: %w", err)
	}
	dataStore := store.New(pool).WithGitHubWebhookPayloadCodec(webhookCodec)
	if err := auth.SetMaxConcurrentHashes(cfg.Auth.MaxConcurrentHashes); err != nil {
		return fmt.Errorf("configure password hash concurrency: %w", err)
	}
	authService, err := auth.NewService(dataStore, []byte(cfg.Auth.TokenPepper), []byte(cfg.Auth.BootstrapSecret))
	if err != nil {
		return fmt.Errorf("construct authentication service: %w", err)
	}
	kubernetesConfig, err := resolveKubernetesConfig(cfg.Kubernetes.Kubeconfig)
	if err != nil {
		return err
	}
	kubernetesClient, err := kubernetes.NewForConfig(kubernetesConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	webhookSecrets, err := kubernetesadapter.NewWebhookSecretAuthorizer(kubernetesClient, cfg.Kubernetes.InstanceNamespace, cfg.Webhook.ReleaseName, cfg.Webhook.AllowedSecretRefs)
	if err != nil {
		return fmt.Errorf("construct webhook Secret policy: %w", err)
	}
	managedSecrets, err := secretref.NewKubernetesManagedWriter(kubernetesClient, cfg.Kubernetes.InstanceNamespace)
	if err != nil {
		return err
	}
	githubOnboarding, err := service.NewGitHubOnboarding(dataStore, managedSecrets, service.GitHubOnboardingConfig{ExternalURL: cfg.ExternalURL,
		APIBaseURL: cfg.GitHub.APIBaseURL, UploadURL: cfg.GitHub.UploadURL, APIVersion: cfg.GitHub.APIVersion, RequestTimeout: cfg.GitHub.RequestTimeout,
		CredentialSecretRef: "k8s-secret://" + cfg.Kubernetes.InstanceNamespace + "/" + secretref.GitHubAppSecretName + "#" + secretref.GitHubPrivateKeyKey,
		SealingKey:          []byte(cfg.Auth.TokenPepper)}, nil)
	if err != nil {
		return err
	}
	githubClients, err := githubadapter.NewInstallationClientFactory(githubOnboarding, cfg.GitHub.APIBaseURL, cfg.GitHub.UploadURL, cfg.GitHub.APIVersion, nil)
	if err != nil {
		return fmt.Errorf("construct GitHub discovery clients: %w", err)
	}
	domainService := service.New(dataStore, service.WithLogger(logger), service.WithGitHubOnboarding(githubOnboarding),
		service.WithGitHubDiscovery(githubClients, []byte(cfg.Auth.TokenPepper)),
		service.WithIngressTarget(cfg.Kubernetes.CustomDomainCNAMETarget), service.WithHostnameSuffix(cfg.Kubernetes.PreviewHostnameSuffix),
		service.WithCertManagerClusterIssuer(cfg.Kubernetes.CertManagerClusterIssuer), service.WithWebhookSecretValidator(webhookSecrets))
	authenticator := server.NewAuthenticator(authService)
	services := server.NewServices(authService, domainService, cfg.Auth.SessionTTL)
	services.Authenticator = authenticator
	services.RateLimiter = server.NewAuthRateLimiter(cfg.Auth.RateLimitPerMinute)
	services.RequestBodyBytes = cfg.Auth.RequestBodyBytes
	services.GitHubBrowser = server.NewGitHubBrowser(domainService, authenticator, cfg.ExternalURL, []byte(cfg.Auth.TokenPepper))
	services.UIHandler = server.LoginContinuationMiddleware(uihandler.New(ui.NewBackend(domainService, authService, cfg.Auth.SessionTTL)))
	services.GitHubWebhook = server.NewGitHubWebhookHandler(cfg.Webhook, secretref.NewKubernetesResolver(kubernetesClient), dataStore)
	adapters, err := buildRecoveryAdapters(cfg, dataStore, productionAdapterConstructors(dataStore, owner, logger, githubOnboarding))
	if err != nil {
		return err
	}
	recovery := worker.New(worker.Config{
		ScanInterval:         cfg.Worker.ScanInterval,
		LeaseDuration:        cfg.Worker.LeaseDuration,
		MaxConcurrentDeploys: cfg.Worker.MaxConcurrentDeploys,
		Owner:                owner,
		RequestTimeout:       cfg.GitHub.RequestTimeout,
	}, logger, dataStore, adapters)
	eventProcessor, err := service.NewGitHubEventProcessor(dataStore, githubadapter.NewEventClient(githubClients), cfg.GitHubEventWorker.MaxIntents)
	if err != nil {
		return fmt.Errorf("construct GitHub event processor: %w", err)
	}
	eventRunner := worker.NewGitHubEventRunner(worker.GitHubEventConfig{
		ScanInterval: cfg.GitHubEventWorker.ScanInterval, DeliveryConcurrency: cfg.GitHubEventWorker.DeliveryConcurrency,
		IntentConcurrency: cfg.GitHubEventWorker.IntentConcurrency, LeaseDuration: cfg.GitHubEventWorker.LeaseDuration,
		ProcessingTimeout: cfg.GitHubEventWorker.ProcessingTimeout, MaxAttempts: cfg.GitHubEventWorker.MaxAttempts,
		InitialRetry: cfg.GitHubEventWorker.InitialRetry, MaxRetry: cfg.GitHubEventWorker.MaxRetry,
		PayloadRetention: cfg.GitHubEventWorker.PayloadRetention, CleanupInterval: cfg.GitHubEventWorker.CleanupInterval,
		CleanupBatch: cfg.GitHubEventWorker.CleanupBatch, ShutdownGrace: cfg.GitHubEventWorker.ShutdownGrace,
		MaxIntents: cfg.GitHubEventWorker.MaxIntents, Owner: owner,
	}, logger, dataStore, eventProcessor)
	if provider, err := buildTelemetryProvider(ctx, cfg.Observability, cfg.Kubernetes.InstanceNamespace, adapters); err != nil {
		return err
	} else if provider != nil {
		domainService.SetTelemetry(provider)
	}
	if reader := buildCertReader(adapters); reader != nil {
		domainService.SetCertReader(reader)
	}
	if provisioner := buildCustomDomainProvisioner(adapters); provisioner != nil {
		domainService.SetCustomDomainProvisioner(provisioner)
	}
	var ready atomic.Bool
	readiness := func(ctx context.Context) error {
		if !ready.Load() {
			return errors.New("startup recovery is incomplete")
		}
		return pool.Ping(ctx)
	}
	if err := githubOnboarding.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile GitHub App credentials: %w", err)
	}

	logger.Info("server starting", "address", cfg.HTTP.Address, "worker_owner", owner)
	apiServer, err := server.New(cfg.HTTP, cfg.GRPC, logger, readiness, services)
	if err != nil {
		return err
	}
	err = runLifecycle(ctx, apiServer, recovery, &ready, eventRunner)
	logger.Info("server stopped")
	return err
}

func newGitHubWebhookPayloadCodec(cfg config.Webhook) (store.GitHubWebhookPayloadCodec, error) {
	path := cfg.PayloadEncryptionKeyFile
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat keyring file: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("keyring file permissions must not allow group or world access")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keyring file: %w", err)
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return nil, fmt.Errorf("parse keyring file: %w", err)
	}
	var file struct {
		ActiveVersion string            `json:"active_version"`
		Keys          map[string]string `json:"keys"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse keyring file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse keyring file: trailing JSON value")
	}
	activeVersion := strings.TrimSpace(file.ActiveVersion)
	keys := make(map[string][]byte, len(file.Keys))
	for version, encoded := range file.Keys {
		version = strings.TrimSpace(version)
		if version == "" {
			return nil, fmt.Errorf("parse keyring file: key version must not be blank")
		}
		key, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("parse keyring file: key version %q is not standard base64", version)
		}
		if len(key) != 16 && len(key) != 24 && len(key) != 32 {
			return nil, fmt.Errorf("parse keyring file: key version %q must decode to 16, 24, or 32 bytes", version)
		}
		if _, exists := keys[version]; exists {
			return nil, fmt.Errorf("parse keyring file: duplicate key version %q", version)
		}
		keys[version] = key
	}
	return store.NewAESGCMGitHubWebhookPayloadKeyring(activeVersion, keys)
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return err
				}
				name := nameToken.(string)
				if _, exists := seen[name]; exists {
					return fmt.Errorf("duplicate field %q", name)
				}
				seen[name] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
	}
	return walk()
}

// buildTelemetryProvider assembles the OpenObserve and Beszel proxy clients from
// configuration, resolving any credential secret references through the same
// Kubernetes resolver used for webhook secrets. It returns nil (no error) when
// neither backend is configured, leaving telemetry RPCs to report the backend
// as unavailable.
func buildTelemetryProvider(ctx context.Context, cfg config.Observability, controlPlaneNamespace string, adapters worker.RecoveryAdapters) (service.TelemetryProvider, error) {
	if !cfg.LogsEnabled() && !cfg.MetricsEnabled() {
		return nil, nil
	}
	clientHolder, ok := adapters.Kubernetes.(interface {
		KubernetesClient() kubernetes.Interface
	})
	if !ok {
		return nil, errors.New("telemetry credential resolution requires a Kubernetes client")
	}
	resolver := secretref.NewKubernetesResolver(clientHolder.KubernetesClient())
	resolve := func(reference string) (string, error) {
		if reference == "" {
			return "", nil
		}
		parsed, err := secretref.Parse(reference)
		if err != nil || parsed.Namespace != controlPlaneNamespace {
			return "", fmt.Errorf("telemetry Secret reference must use control-plane namespace %q", controlPlaneNamespace)
		}
		value, err := resolver.Resolve(ctx, reference)
		if err != nil {
			return "", fmt.Errorf("resolve telemetry secret: %w", err)
		}
		return string(value), nil
	}
	var logs *observability.OpenObserveClient
	if cfg.LogsEnabled() {
		credential, err := resolve(cfg.OpenObserveCredentialSecret)
		if err != nil {
			return nil, err
		}
		logs, err = observability.NewOpenObserveClient(observability.OpenObserveConfig{
			BaseURL:      cfg.OpenObserveBaseURL,
			Organization: cfg.OpenObserveOrganization,
			LogStream:    cfg.OpenObserveLogStream,
			TraceStream:  cfg.OpenObserveTraceStream,
			Credential:   credential,
		})
		if err != nil {
			return nil, err
		}
	}
	var metrics *observability.BeszelClient
	if cfg.MetricsEnabled() {
		credential, err := resolve(cfg.BeszelCredentialSecret)
		if err != nil {
			return nil, err
		}
		metrics, err = observability.NewBeszelClient(observability.BeszelConfig{BaseURL: cfg.BeszelBaseURL, Credential: credential})
		if err != nil {
			return nil, err
		}
	}
	return observability.NewProvider(logs, metrics), nil
}

// buildCertReader constructs the read-only cert-manager Certificate reader from
// the Kubernetes adapter's dynamic client. It returns nil when the adapter does
// not expose a dynamic client (leaving the Certificate RPCs to report the
// capability as unavailable), and reads only through the dynamic client — no
// cert-manager Go module is imported.
func buildCertReader(adapters worker.RecoveryAdapters) service.CertReader {
	holder, ok := adapters.Kubernetes.(interface {
		DynamicClient() dynamic.Interface
	})
	if !ok {
		return nil
	}
	client := holder.DynamicClient()
	if client == nil {
		return nil
	}
	return kubernetesadapter.NewCertificateReader(client)
}

// buildCustomDomainProvisioner constructs the custom-domain Ingress provisioner
// from the Kubernetes adapter's typed client. It returns nil when the adapter
// does not expose a Kubernetes client (leaving custom-domain verification to
// succeed without provisioning routing), and uses only the typed networkingv1
// client — no cert-manager Go module.
func buildCustomDomainProvisioner(adapters worker.RecoveryAdapters) service.CustomDomainProvisioner {
	holder, ok := adapters.Kubernetes.(interface {
		KubernetesClient() kubernetes.Interface
	})
	if !ok {
		return nil
	}
	client := holder.KubernetesClient()
	if client == nil {
		return nil
	}
	return kubernetesadapter.NewCustomDomainProvisioner(client)
}

type adapterConstructors struct {
	github     func(config.GitHub) (worker.GitHubRecoveryAdapter, error)
	kubernetes func(config.Kubernetes) (worker.KubernetesRecoveryAdapter, error)
}

func buildRecoveryAdapters(cfg config.Config, dataStore *store.Store, constructors adapterConstructors) (worker.RecoveryAdapters, error) {
	var adapters worker.RecoveryAdapters
	if constructors.kubernetes == nil {
		return adapters, errors.New("Kubernetes adapter constructor is unavailable")
	}
	kubernetesRecovery, err := constructors.kubernetes(cfg.Kubernetes)
	if err != nil {
		return adapters, fmt.Errorf("construct Kubernetes recovery adapter: %w", err)
	}
	if kubernetesRecovery == nil {
		return adapters, errors.New("construct Kubernetes recovery adapter: constructor returned nil")
	}
	adapters.Kubernetes = kubernetesRecovery
	lifecycle, ok := kubernetesRecovery.(worker.KubernetesLifecycleAdapter)
	if !ok {
		return adapters, errors.New("Kubernetes lifecycle adapter is unavailable")
	}
	adapters.Lifecycle = lifecycle
	adapters.KubernetesErrorDisposition = kubernetesadapter.RecoveryErrorDisposition
	adapters.Capabilities.Kubernetes = true
	webhookClient, ok := kubernetesRecovery.(interface{ KubernetesClient() kubernetes.Interface })
	if !ok {
		return worker.RecoveryAdapters{}, errors.New("Kubernetes adapter does not expose a client for webhook delivery")
	}
	webhookNamespace := cfg.Kubernetes.InstanceNamespace
	if webhookNamespace == "" {
		webhookNamespace = "default"
	}
	webhookAuthorizer, err := kubernetesadapter.NewWebhookSecretAuthorizer(webhookClient.KubernetesClient(), webhookNamespace, cfg.Webhook.ReleaseName, cfg.Webhook.AllowedSecretRefs)
	if err != nil {
		return worker.RecoveryAdapters{}, fmt.Errorf("construct webhook Secret authorizer: %w", err)
	}
	adapters.Webhook = webhookdelivery.NewAuthorizedSender(dataStore, secretref.NewKubernetesResolver(webhookClient.KubernetesClient()), webhookAuthorizer, webhookNamespace)
	if reconciler, ok := kubernetesRecovery.(worker.ReconciliationAdapter); ok {
		adapters.Reconciliation = reconciler
	}

	if !cfg.GitHub.Enabled() {
		return adapters, nil
	}
	if constructors.github == nil {
		return worker.RecoveryAdapters{}, errors.New("GitHub adapter constructor is unavailable")
	}
	githubRecovery, err := constructors.github(cfg.GitHub)
	if err != nil {
		return worker.RecoveryAdapters{}, fmt.Errorf("construct GitHub recovery adapter: %w", err)
	}
	if githubRecovery == nil {
		return worker.RecoveryAdapters{}, errors.New("construct GitHub recovery adapter: constructor returned nil")
	}
	adapters.GitHub = githubRecovery
	adapters.GitHubErrorDisposition = githubadapter.ErrorDisposition
	adapters.Capabilities.GitHub = true
	return adapters, nil
}

func productionAdapterConstructors(dataStore *store.Store, owner string, logger *slog.Logger, credentials githubadapter.AppCredentialProvider) adapterConstructors {
	return adapterConstructors{
		github: func(cfg config.GitHub) (worker.GitHubRecoveryAdapter, error) {
			clients, err := githubadapter.NewInstallationClientFactory(credentials, cfg.APIBaseURL, cfg.UploadURL, cfg.APIVersion, nil)
			if err != nil {
				return nil, err
			}
			return githubadapter.New(githubadapter.Config{Owner: owner, DispatchObservationInterval: cfg.DispatchObservationInterval,
				RunPollInterval: cfg.RunPollInterval, RequestTimeout: cfg.RequestTimeout}, dataStore, clients)
		},
		kubernetes: func(cfg config.Kubernetes) (worker.KubernetesRecoveryAdapter, error) {
			clientConfig, err := resolveKubernetesConfig(cfg.Kubeconfig)
			if err != nil {
				return nil, err
			}
			client, err := kubernetes.NewForConfig(clientConfig)
			if err != nil {
				return nil, fmt.Errorf("create Kubernetes client: %w", err)
			}
			provisioner := client
			if cfg.Kubeconfig == "" {
				provisionerConfig := rest.CopyConfig(clientConfig)
				provisionerConfig.BearerToken = ""
				provisionerConfig.BearerTokenFile = "/var/run/secrets/cyclopus-provisioner/token"
				provisioner, err = kubernetes.NewForConfig(provisionerConfig)
				if err != nil {
					return nil, fmt.Errorf("create Kubernetes provisioner client: %w", err)
				}
			}
			dynamicClient, err := dynamic.NewForConfig(clientConfig)
			if err != nil {
				return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
			}
			return kubernetesadapter.NewAdapter(kubernetesadapter.Config{
				PollInterval: cfg.RecoveryPollInterval, ConflictRetries: cfg.ConflictRetries,
				PreviewHostnameSuffix:      cfg.PreviewHostnameSuffix,
				CertManagerClusterIssuer:   cfg.CertManagerClusterIssuer,
				NetworkEgressExcludedCIDRs: cfg.NetworkEgressExcludedCIDRs,
			}, client, dataStore, kubernetesadapter.WithProvisionerClient(provisioner), kubernetesadapter.WithDynamicClient(dynamicClient), kubernetesadapter.WithLogger(logger))
		},
	}
}

func resolveKubernetesConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load explicit kubeconfig: %w", err)
		}
		return cfg, nil
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load default Kubernetes client config: %w", err)
	}
	return cfg, nil
}

type lifecycleServer interface {
	Run(context.Context) error
}

type lifecycleWorker interface {
	Startup(context.Context) error
	Run(context.Context) error
}

func runLifecycle(ctx context.Context, api lifecycleServer, recovery lifecycleWorker, ready *atomic.Bool, additional ...lifecycleWorker) error {
	workers := append([]lifecycleWorker{recovery}, additional...)
	runCtx, cancel := context.WithCancel(ctx)
	defer func() {
		ready.Store(false)
		cancel()
	}()

	apiErr := make(chan error, 1)
	go func() {
		apiErr <- api.Run(runCtx)
		cancel()
	}()

	workerErr := make(chan error, len(workers))
	started := 0
	waitStarted := func() error {
		var err error
		for range started {
			err = errors.Join(err, <-workerErr)
		}
		return err
	}
	for index, worker := range workers {
		if worker == nil {
			ready.Store(false)
			cancel()
			apiRunErr := <-apiErr
			return errors.Join(fmt.Errorf("startup worker %d: unavailable", index), apiRunErr, waitStarted())
		}
		if err := worker.Startup(runCtx); err != nil {
			ready.Store(false)
			cancel()
			apiRunErr := <-apiErr
			return errors.Join(fmt.Errorf("startup worker %d: %w", index, err), apiRunErr, waitStarted())
		}
		started++
		go func(index int, worker lifecycleWorker) {
			if err := worker.Run(runCtx); err != nil {
				workerErr <- fmt.Errorf("worker %d: %w", index, err)
				cancel()
				return
			}
			workerErr <- nil
			cancel()
		}(index, worker)
	}
	ready.Store(true)

	select {
	case err := <-apiErr:
		ready.Store(false)
		cancel()
		var workerRunErr error
		for range workers {
			workerRunErr = errors.Join(workerRunErr, <-workerErr)
		}
		if err != nil {
			return err
		}
		return workerRunErr
	case err := <-workerErr:
		ready.Store(false)
		cancel()
		apiRunErr := <-apiErr
		for range len(workers) - 1 {
			err = errors.Join(err, <-workerErr)
		}
		if err != nil {
			return err
		}
		return apiRunErr
	}
}

func recoveryOwner() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "cyclopus"
	}
	return hostname + ":" + strconv.Itoa(os.Getpid())
}
