package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shepard-labs/cyclopus/internal/adapters/kubernetes"
	"github.com/shepard-labs/cyclopus/internal/oci"
	"github.com/shepard-labs/cyclopus/internal/secretref"
	"github.com/shepard-labs/cyclopus/internal/store"
)

const maxNamespaceCredentialAttempts = 12

type RegistryNamespaceReconcilerConfig struct {
	ScanInterval, LeaseDuration, RequestTimeout, ShutdownGrace time.Duration
	Concurrency                                                int
	Owner, InstanceNamespace                                   string
}

type RegistryNamespaceReconcilerStore interface {
	RecoverExpiredNamespaceCredentialLeases(context.Context, int) (int64, error)
	RecoverExpiredNamespaceCredentialCleanupLeases(context.Context, int) (int64, error)
	ListNamespaceCredentialOrgIDs(context.Context, string, int) ([]string, error)
	ClaimNamespaceCredentials(context.Context, string, string, time.Duration, int) ([]store.NamespaceCredentialClaim, error)
	ClaimNamespaceCredentialCleanups(context.Context, string, string, time.Duration, int) ([]store.NamespaceCredentialCleanupClaim, error)
	CheckpointNamespaceCredential(context.Context, store.NamespaceCredentialClaim, store.LeaseToken, string) error
	CompleteNamespaceCredential(context.Context, store.NamespaceCredentialClaim, store.LeaseToken) (bool, error)
	CompleteNamespaceCredentialCleanup(context.Context, store.NamespaceCredentialCleanupClaim, store.LeaseToken) error
	RescheduleNamespaceCredential(context.Context, store.NamespaceCredentialClaim, store.LeaseToken, time.Duration) error
	RescheduleNamespaceCredentialCleanup(context.Context, store.NamespaceCredentialCleanupClaim, store.LeaseToken, time.Duration) error
	FailNamespaceCredential(context.Context, store.NamespaceCredentialClaim, store.LeaseToken, string, string) error
	FailNamespaceCredentialCleanup(context.Context, store.NamespaceCredentialCleanupClaim, store.LeaseToken, string) error
}

type RegistryNamespaceAdapter interface {
	ReadImagePullSecretIdentity(context.Context, string, string, int64, string, string) (kubernetes.ImagePullSecretIdentity, error)
	ReconcileImagePullSecret(context.Context, kubernetes.ImagePullSecretRequest) error
	DeleteImagePullSecret(context.Context, kubernetes.ImagePullSecretRequest) error
}

type registryNamespaceCleanupIdentityAdapter interface {
	ReadImagePullSecretCleanupIdentity(context.Context, string, string, int64, string, string) (kubernetes.ImagePullSecretIdentity, error)
}

type namespaceCredentialInFlightKey struct {
	orgID, environmentID string
}

type RegistryNamespaceReconciler struct {
	config       RegistryNamespaceReconcilerConfig
	logger       *slog.Logger
	store        RegistryNamespaceReconcilerStore
	adapter      RegistryNamespaceAdapter
	classify     func(error) (bool, oci.Code, bool)
	ready        atomic.Bool
	sem          chan struct{}
	wg           sync.WaitGroup
	mu           sync.Mutex
	scanMu       sync.Mutex
	cleanupFirst bool
	orgCursor    string
	inFlight     map[namespaceCredentialInFlightKey]struct{}
	process      context.Context
	cancel       context.CancelFunc
}

func NewRegistryNamespaceReconciler(config RegistryNamespaceReconcilerConfig, logger *slog.Logger, dataStore RegistryNamespaceReconcilerStore, adapter RegistryNamespaceAdapter) *RegistryNamespaceReconciler {
	return &RegistryNamespaceReconciler{config: config, logger: logger, store: dataStore, adapter: adapter,
		classify: kubernetes.RegistryCredentialErrorDisposition, sem: make(chan struct{}, config.Concurrency), inFlight: make(map[namespaceCredentialInFlightKey]struct{})}
}

func (r *RegistryNamespaceReconciler) Ready() bool { return r.ready.Load() }

func (r *RegistryNamespaceReconciler) Startup(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	r.process, r.cancel = context.WithCancel(context.WithoutCancel(ctx))
	if _, err := r.store.RecoverExpiredNamespaceCredentialLeases(ctx, 1000); err != nil {
		r.cancel()
		return err
	}
	if _, err := r.store.RecoverExpiredNamespaceCredentialCleanupLeases(ctx, 1000); err != nil {
		r.cancel()
		return err
	}
	if err := r.scan(ctx, false); err != nil {
		r.cancel()
		r.wg.Wait()
		return err
	}
	r.ready.Store(true)
	return nil
}

func (r *RegistryNamespaceReconciler) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.cancel == nil {
		return errors.New("registry namespace reconciler Startup must run before Run")
	}
	ticker := time.NewTicker(r.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.ready.Store(false)
			r.cancel()
			return r.awaitShutdown()
		case <-ticker.C:
			if err := r.scan(ctx, true); err != nil && ctx.Err() == nil {
				r.logger.Error("registry namespace reconciliation scan failed", "error", err)
			}
		}
	}
}

func (r *RegistryNamespaceReconciler) validate() error {
	c := r.config
	if r.store == nil || r.adapter == nil || r.logger == nil || c.Owner == "" || c.InstanceNamespace == "" || c.ScanInterval <= 0 ||
		c.LeaseDuration <= 0 || c.RequestTimeout <= 0 || c.RequestTimeout >= c.LeaseDuration ||
		c.ShutdownGrace <= 0 || c.Concurrency < 1 || c.Concurrency > 100 {
		return errors.New("invalid registry namespace reconciler configuration")
	}
	return nil
}

func (r *RegistryNamespaceReconciler) scan(ctx context.Context, recover bool) error {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	if recover {
		if _, err := r.store.RecoverExpiredNamespaceCredentialLeases(ctx, 1000); err != nil {
			return err
		}
		if _, err := r.store.RecoverExpiredNamespaceCredentialCleanupLeases(ctx, 1000); err != nil {
			return err
		}
	}
	orgIDs, err := r.store.ListNamespaceCredentialOrgIDs(ctx, r.orgCursor, 1000)
	if err != nil {
		return err
	}
	if len(orgIDs) == 0 && r.orgCursor != "" {
		r.orgCursor = ""
		orgIDs, err = r.store.ListNamespaceCredentialOrgIDs(ctx, r.orgCursor, 1000)
		if err != nil {
			return err
		}
	}
	if len(orgIDs) == 0 {
		return nil
	}
	for cap(r.sem)-len(r.sem) > 0 {
		started := 0
		cleanupFirst := r.cleanupFirst
		r.cleanupFirst = !r.cleanupFirst
		for _, orgID := range orgIDs {
			if cap(r.sem)-len(r.sem) == 0 {
				return nil
			}
			r.orgCursor = orgID
			claimed, err := r.claimForOrg(ctx, orgID, cleanupFirst)
			if err != nil {
				return err
			}
			if claimed {
				started++
			}
		}
		if started == 0 {
			break
		}
	}
	return nil
}

func (r *RegistryNamespaceReconciler) claimForOrg(ctx context.Context, orgID string, cleanupFirst bool) (bool, error) {
	if cleanupFirst {
		claimed, err := r.claimCleanup(ctx, orgID)
		if err != nil || claimed {
			return claimed, err
		}
		return r.claimReconciliation(ctx, orgID)
	}
	claimed, err := r.claimReconciliation(ctx, orgID)
	if err != nil || claimed {
		return claimed, err
	}
	return r.claimCleanup(ctx, orgID)
}

func (r *RegistryNamespaceReconciler) claimCleanup(ctx context.Context, orgID string) (bool, error) {
	claims, err := r.store.ClaimNamespaceCredentialCleanups(ctx, orgID, r.config.Owner, r.config.LeaseDuration, 1)
	if err != nil || len(claims) == 0 {
		return false, err
	}
	claim := claims[0]
	if !r.markInFlight(claim.OrgID, claim.EnvironmentID) {
		err = r.store.RescheduleNamespaceCredentialCleanup(ctx, claim, claim.Lease, 0)
		if isNamespaceConvergenceError(err) {
			err = nil
		}
		return false, err
	}
	r.sem <- struct{}{}
	r.wg.Add(1)
	go r.processCleanupClaim(r.process, claim)
	return true, nil
}

func (r *RegistryNamespaceReconciler) claimReconciliation(ctx context.Context, orgID string) (bool, error) {
	claims, err := r.store.ClaimNamespaceCredentials(ctx, orgID, r.config.Owner, r.config.LeaseDuration, 1)
	if err != nil || len(claims) == 0 {
		return false, err
	}
	claim := claims[0]
	if !r.markInFlight(claim.OrgID, claim.EnvironmentID) {
		err = r.store.RescheduleNamespaceCredential(ctx, claim, claim.Lease, 0)
		if isNamespaceConvergenceError(err) {
			err = nil
		}
		return false, err
	}
	r.sem <- struct{}{}
	r.wg.Add(1)
	go r.processClaim(r.process, claim)
	return true, nil
}

func (r *RegistryNamespaceReconciler) processCleanupClaim(root context.Context, claim store.NamespaceCredentialCleanupClaim) {
	defer func() { <-r.sem; r.clearInFlight(claim.OrgID, claim.EnvironmentID); r.wg.Done() }()
	ctx, cancel := context.WithTimeout(root, r.config.RequestTimeout)
	defer cancel()
	reference, err := secretref.Parse(claim.SecretRef)
	expectedName, nameErr := kubernetes.RegistryCredentialSecretName(claim.OrgID, claim.Generation)
	if err != nil || nameErr != nil || reference.Namespace != r.config.InstanceNamespace || reference.Name != expectedName || reference.Key != kubernetes.RegistryCredentialKey {
		r.failCleanupClaim(root, claim, string(oci.CodeImagePullSecretReconcileFailed))
		return
	}
	identityReader, ok := r.adapter.(registryNamespaceCleanupIdentityAdapter)
	var identity kubernetes.ImagePullSecretIdentity
	if ok {
		identity, err = identityReader.ReadImagePullSecretCleanupIdentity(ctx, claim.OrgID, claim.RegistryID, claim.Generation, claim.SecretRef, claim.Namespace)
	} else {
		identity, err = r.adapter.ReadImagePullSecretIdentity(ctx, claim.OrgID, claim.RegistryID, claim.Generation, claim.SecretRef, claim.Namespace)
	}
	if err != nil {
		r.handleCleanupClaimError(root, claim, err)
		return
	}
	if identity.SourceSecretName != reference.Name {
		r.failCleanupClaim(root, claim, string(oci.CodeImagePullSecretReconcileFailed))
		return
	}
	request := kubernetes.ImagePullSecretRequest{OrgID: claim.OrgID, AppID: claim.AppID, EnvironmentID: claim.EnvironmentID,
		Namespace: claim.Namespace, NamespaceUID: identity.NamespaceUID, RegistryID: claim.RegistryID,
		SourceSecretName: identity.SourceSecretName, SourceUID: identity.SourceUID, Generation: claim.Generation}
	if err = r.adapter.DeleteImagePullSecret(ctx, request); err != nil {
		r.handleCleanupClaimError(root, claim, err)
		return
	}
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(root), r.config.RequestTimeout)
	defer writeCancel()
	r.handleCleanupStoreConvergence(claim, r.store.CompleteNamespaceCredentialCleanup(writeCtx, claim, claim.Lease))
}

func (r *RegistryNamespaceReconciler) processClaim(root context.Context, claim store.NamespaceCredentialClaim) {
	defer func() { <-r.sem; r.clearInFlight(claim.OrgID, claim.EnvironmentID); r.wg.Done() }()
	ctx, cancel := context.WithTimeout(root, r.config.RequestTimeout)
	defer cancel()
	reference, err := secretref.Parse(claim.SecretRef)
	expectedName, nameErr := kubernetes.RegistryCredentialSecretName(claim.OrgID, claim.Generation)
	if err != nil || nameErr != nil || reference.Namespace != r.config.InstanceNamespace || reference.Name != expectedName || reference.Key != kubernetes.RegistryCredentialKey {
		r.failClaim(root, claim, string(oci.CodeImagePullSecretReconcileFailed))
		return
	}
	identity, err := r.adapter.ReadImagePullSecretIdentity(ctx, claim.OrgID, claim.RegistryID, claim.Generation, claim.SecretRef, claim.Namespace)
	if err != nil {
		r.handleClaimError(root, claim, err)
		return
	}
	if identity.SourceSecretName != reference.Name {
		r.failClaim(root, claim, string(oci.CodeImagePullSecretReconcileFailed))
		return
	}
	if claim.Checkpoint != "namespace_secret_written" {
		if err = r.store.CheckpointNamespaceCredential(ctx, claim, claim.Lease, "source_read"); err != nil {
			r.handleStoreConvergence(claim, err)
			return
		}
	}
	request := kubernetes.ImagePullSecretRequest{OrgID: claim.OrgID, AppID: claim.AppID, EnvironmentID: claim.EnvironmentID,
		Namespace: claim.Namespace, NamespaceUID: identity.NamespaceUID, RegistryID: claim.RegistryID,
		SourceSecretName: identity.SourceSecretName, SourceUID: identity.SourceUID, Generation: claim.Generation}
	if err = r.adapter.ReconcileImagePullSecret(ctx, request); err != nil {
		r.handleClaimError(root, claim, err)
		return
	}
	if err = r.store.CheckpointNamespaceCredential(ctx, claim, claim.Lease, "namespace_secret_written"); err != nil {
		r.handleStoreConvergence(claim, err)
		return
	}
	// Reconcile performs a read-after-write verification. Calling it again here
	// binds fresh Kubernetes evidence immediately to the final database CAS.
	if err = r.adapter.ReconcileImagePullSecret(ctx, request); err != nil {
		r.handleClaimError(root, claim, err)
		return
	}
	_, err = r.store.CompleteNamespaceCredential(ctx, claim, claim.Lease)
	r.handleStoreConvergence(claim, err)
}

func (r *RegistryNamespaceReconciler) failClaim(root context.Context, claim store.NamespaceCredentialClaim, code string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(root), r.config.RequestTimeout)
	defer cancel()
	r.handleStoreConvergence(claim, r.store.FailNamespaceCredential(writeCtx, claim, claim.Lease, code, ""))
}

func (r *RegistryNamespaceReconciler) failCleanupClaim(root context.Context, claim store.NamespaceCredentialCleanupClaim, code string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(root), r.config.RequestTimeout)
	defer cancel()
	r.handleCleanupStoreConvergence(claim, r.store.FailNamespaceCredentialCleanup(writeCtx, claim, claim.Lease, code))
}

func (r *RegistryNamespaceReconciler) handleClaimError(root context.Context, claim store.NamespaceCredentialClaim, err error) {
	if root.Err() != nil || isNamespaceConvergenceError(err) {
		return
	}
	retryable, code, classified := r.classify(err)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(root), r.config.RequestTimeout)
	defer cancel()
	if !classified {
		retryable, code = true, oci.CodeKubernetesUnavailable
	}
	var writeErr error
	if retryable && claim.Attempts < maxNamespaceCredentialAttempts {
		writeErr = r.store.RescheduleNamespaceCredential(writeCtx, claim, claim.Lease, namespaceCredentialRetryDelay(int(claim.Lease.Epoch), r.config.LeaseDuration))
	} else {
		writeErr = r.store.FailNamespaceCredential(writeCtx, claim, claim.Lease, namespaceCredentialFailureCode(code), "")
	}
	r.handleStoreConvergence(claim, writeErr)
}

func (r *RegistryNamespaceReconciler) handleCleanupClaimError(root context.Context, claim store.NamespaceCredentialCleanupClaim, err error) {
	if root.Err() != nil || isNamespaceConvergenceError(err) {
		return
	}
	retryable, code, classified := r.classify(err)
	if !classified {
		retryable, code = true, oci.CodeKubernetesUnavailable
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(root), r.config.RequestTimeout)
	defer cancel()
	var writeErr error
	if retryable && claim.Attempts < maxNamespaceCredentialAttempts {
		writeErr = r.store.RescheduleNamespaceCredentialCleanup(writeCtx, claim, claim.Lease, namespaceCredentialRetryDelay(claim.Attempts, r.config.LeaseDuration))
	} else {
		writeErr = r.store.FailNamespaceCredentialCleanup(writeCtx, claim, claim.Lease, namespaceCredentialFailureCode(code))
	}
	r.handleCleanupStoreConvergence(claim, writeErr)
}

func namespaceCredentialFailureCode(code oci.Code) string {
	switch code {
	case oci.CodeImagePullSecretReconcileFailed, oci.CodeKubernetesOwnershipConflict, oci.CodeKubernetesUnavailable:
		return string(code)
	default:
		return string(oci.CodeImagePullSecretReconcileFailed)
	}
}

func namespaceCredentialRetryDelay(attempt int, leaseDuration time.Duration) time.Duration {
	delay := time.Second
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	limit := leaseDuration / 2
	if limit > 30*time.Second {
		limit = 30 * time.Second
	}
	if delay > limit {
		delay = limit
	}
	return delay
}

func isNamespaceConvergenceError(err error) bool {
	return errors.Is(err, store.ErrLeaseNotOwned) || errors.Is(err, store.ErrFailedPrecondition) ||
		errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound)
}

func (r *RegistryNamespaceReconciler) handleStoreConvergence(claim store.NamespaceCredentialClaim, err error) {
	if err == nil || isNamespaceConvergenceError(err) {
		return
	}
	r.logger.Warn("registry namespace reconciliation store write failed", "org_id", claim.OrgID,
		"environment_id", claim.EnvironmentID, "error", err)
}

func (r *RegistryNamespaceReconciler) handleCleanupStoreConvergence(claim store.NamespaceCredentialCleanupClaim, err error) {
	if err == nil || isNamespaceConvergenceError(err) {
		return
	}
	r.logger.Warn("registry namespace cleanup store write failed", "org_id", claim.OrgID,
		"environment_id", claim.EnvironmentID, "error", err)
}

func (r *RegistryNamespaceReconciler) markInFlight(orgID, environmentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := namespaceCredentialInFlightKey{orgID: orgID, environmentID: environmentID}
	if _, exists := r.inFlight[key]; exists {
		return false
	}
	r.inFlight[key] = struct{}{}
	return true
}

func (r *RegistryNamespaceReconciler) clearInFlight(orgID, environmentID string) {
	r.mu.Lock()
	delete(r.inFlight, namespaceCredentialInFlightKey{orgID: orgID, environmentID: environmentID})
	r.mu.Unlock()
}

func (r *RegistryNamespaceReconciler) awaitShutdown() error {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	timer := time.NewTimer(r.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		r.logger.Warn("registry namespace reconciler shutdown", "result", "grace_exhausted")
		return nil
	}
}
