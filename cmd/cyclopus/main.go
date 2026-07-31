package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"

	githubadapter "github.com/cyclopus-dev/cyclopus/internal/adapters/github"
	kubernetesadapter "github.com/cyclopus-dev/cyclopus/internal/adapters/kubernetes"
	"github.com/cyclopus-dev/cyclopus/internal/adapters/observability"
	"github.com/cyclopus-dev/cyclopus/internal/auth"
	"github.com/cyclopus-dev/cyclopus/internal/config"
	"github.com/cyclopus-dev/cyclopus/internal/logging"
	"github.com/cyclopus-dev/cyclopus/internal/secretref"
	"github.com/cyclopus-dev/cyclopus/internal/server"
	"github.com/cyclopus-dev/cyclopus/internal/service"
	"github.com/cyclopus-dev/cyclopus/internal/store"
	webhookdelivery "github.com/cyclopus-dev/cyclopus/internal/webhook"
	"github.com/cyclopus-dev/cyclopus/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
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
	dataStore := store.New(pool)
	if err := auth.SetMaxConcurrentHashes(cfg.Auth.MaxConcurrentHashes); err != nil {
		return fmt.Errorf("configure password hash concurrency: %w", err)
	}
	authService, err := auth.NewService(dataStore, []byte(cfg.Auth.TokenPepper), []byte(cfg.Auth.BootstrapSecret))
	if err != nil {
		return fmt.Errorf("construct authentication service: %w", err)
	}
	domainService := service.New(dataStore, service.WithLogger(logger),
		service.WithIngressTarget(cfg.Kubernetes.CustomDomainCNAMETarget),
		service.WithHostnameSuffix(cfg.Kubernetes.PreviewHostnameSuffix),
		service.WithCertManagerClusterIssuer(cfg.Kubernetes.CertManagerClusterIssuer))
	authenticator := server.NewAuthenticator(authService)
	services := server.NewServices(authService, domainService, cfg.Auth.SessionTTL)
	services.Authenticator = authenticator
	services.RateLimiter = server.NewAuthRateLimiter(cfg.Auth.RateLimitPerMinute)
	services.RequestBodyBytes = cfg.Auth.RequestBodyBytes
	adapters, err := buildRecoveryAdapters(cfg, dataStore, productionAdapterConstructors(dataStore, owner, logger))
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
	if provider, err := buildTelemetryProvider(ctx, cfg.Observability, adapters); err != nil {
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

	logger.Info("server starting", "address", cfg.HTTP.Address, "worker_owner", owner)
	apiServer, err := server.New(cfg.HTTP, cfg.GRPC, logger, readiness, services)
	if err != nil {
		return err
	}
	err = runLifecycle(ctx, apiServer, recovery, &ready)
	logger.Info("server stopped")
	return err
}

// buildTelemetryProvider assembles the OpenObserve and Beszel proxy clients from
// configuration, resolving any credential secret references through the same
// Kubernetes resolver used for webhook secrets. It returns nil (no error) when
// neither backend is configured, leaving telemetry RPCs to report the backend
// as unavailable.
func buildTelemetryProvider(ctx context.Context, cfg config.Observability, adapters worker.RecoveryAdapters) (service.TelemetryProvider, error) {
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
		token, err := resolve(cfg.BeszelTokenSecret)
		if err != nil {
			return nil, err
		}
		metrics, err = observability.NewBeszelClient(observability.BeszelConfig{BaseURL: cfg.BeszelBaseURL, Token: token})
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
	adapters.Webhook = webhookdelivery.NewSender(dataStore, secretref.NewKubernetesResolver(webhookClient.KubernetesClient()))
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

func productionAdapterConstructors(dataStore *store.Store, owner string, logger *slog.Logger) adapterConstructors {
	return adapterConstructors{
		github: func(cfg config.GitHub) (worker.GitHubRecoveryAdapter, error) {
			clients, err := githubadapter.NewInstallationClientFactory(cfg.AppID, cfg.PrivateKeyFile,
				cfg.APIBaseURL, cfg.UploadURL, cfg.APIVersion, nil)
			if err != nil {
				return nil, err
			}
			return githubadapter.New(githubadapter.Config{
				Owner: owner, DispatchObservationInterval: cfg.DispatchObservationInterval,
				RunPollInterval: cfg.RunPollInterval, RequestTimeout: cfg.RequestTimeout,
			}, dataStore, clients)
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
			dynamicClient, err := dynamic.NewForConfig(clientConfig)
			if err != nil {
				return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
			}
			return kubernetesadapter.NewAdapter(kubernetesadapter.Config{
				PollInterval: cfg.RecoveryPollInterval, ConflictRetries: cfg.ConflictRetries,
				PreviewHostnameSuffix:      cfg.PreviewHostnameSuffix,
				CertManagerClusterIssuer:   cfg.CertManagerClusterIssuer,
				NetworkEgressExcludedCIDRs: cfg.NetworkEgressExcludedCIDRs,
			}, client, dataStore, kubernetesadapter.WithDynamicClient(dynamicClient), kubernetesadapter.WithLogger(logger))
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

func runLifecycle(ctx context.Context, api lifecycleServer, recovery lifecycleWorker, ready *atomic.Bool) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer func() {
		ready.Store(false)
		cancel()
	}()

	apiErr := make(chan error, 1)
	go func() { apiErr <- api.Run(runCtx) }()

	if err := recovery.Startup(runCtx); err != nil {
		ready.Store(false)
		cancel()
		<-apiErr
		return fmt.Errorf("startup recovery: %w", err)
	}
	ready.Store(true)

	workerErr := make(chan error, 1)
	workerStarted := make(chan struct{})
	go func() {
		close(workerStarted)
		workerErr <- recovery.Run(runCtx)
	}()
	<-workerStarted

	select {
	case err := <-apiErr:
		ready.Store(false)
		cancel()
		workerRunErr := <-workerErr
		if err != nil {
			return err
		}
		return workerRunErr
	case err := <-workerErr:
		ready.Store(false)
		cancel()
		apiRunErr := <-apiErr
		if err != nil {
			return fmt.Errorf("recovery worker: %w", err)
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
