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
	"github.com/cyclopus-dev/cyclopus/internal/auth"
	"github.com/cyclopus-dev/cyclopus/internal/config"
	"github.com/cyclopus-dev/cyclopus/internal/logging"
	"github.com/cyclopus-dev/cyclopus/internal/server"
	"github.com/cyclopus-dev/cyclopus/internal/service"
	"github.com/cyclopus-dev/cyclopus/internal/store"
	"github.com/cyclopus-dev/cyclopus/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
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
	domainService := service.New(dataStore)
	authenticator := server.NewAuthenticator(authService)
	services := server.NewServices(authService, domainService, cfg.Auth.SessionTTL)
	services.Authenticator = authenticator
	services.RateLimiter = server.NewAuthRateLimiter(cfg.Auth.RateLimitPerMinute)
	services.RequestBodyBytes = cfg.Auth.RequestBodyBytes
	adapters, err := buildRecoveryAdapters(cfg, productionAdapterConstructors(dataStore, owner))
	if err != nil {
		return err
	}
	recovery := worker.New(worker.Config{
		ScanInterval:   cfg.Worker.ScanInterval,
		LeaseDuration:  cfg.Worker.LeaseDuration,
		Owner:          owner,
		RequestTimeout: cfg.GitHub.RequestTimeout,
	}, logger, dataStore, adapters)
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

type adapterConstructors struct {
	github     func(config.GitHub) (worker.GitHubRecoveryAdapter, error)
	kubernetes func(config.Kubernetes) (worker.KubernetesRecoveryAdapter, error)
}

func buildRecoveryAdapters(cfg config.Config, constructors adapterConstructors) (worker.RecoveryAdapters, error) {
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
	adapters.KubernetesErrorDisposition = kubernetesadapter.RecoveryErrorDisposition
	adapters.Capabilities.Kubernetes = true

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

func productionAdapterConstructors(dataStore *store.Store, owner string) adapterConstructors {
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
			return kubernetesadapter.NewRecoveryAdapter(kubernetesadapter.Config{
				PollInterval: cfg.RecoveryPollInterval, ConflictRetries: cfg.ConflictRetries,
			}, client, dataStore)
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
	go func() { workerErr <- recovery.Run(runCtx) }()

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
