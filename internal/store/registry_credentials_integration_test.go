//go:build integration

package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const registryCredentialActorID = "00000000-0000-0000-0000-000000000091"

func seedRegistryCredentialActor(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator) VALUES($1,$2,'registry@example.test','hash','Registry Admin',true)`, registryCredentialActorID, orgOneID); err != nil {
		t.Fatal(err)
	}
}

func credentialReserveParams(orgID, registryID, kind, key, repository, reference string) ReserveRegistryCredentialOperationParams {
	fingerprint := sha256.Sum256([]byte(key))
	return ReserveRegistryCredentialOperationParams{OrgID: orgID, RegistryID: registryID, Kind: kind, IdempotencyKey: key,
		TargetRepository: repository, TargetReference: reference, CredentialFingerprint: fingerprint,
		ActorUserID: registryCredentialActorID, AuditID: fmt.Sprintf("%08x-0000-7000-8000-%012x", fingerprint[:4], fingerprint[4:10])}
}

func TestRegistryCredentialReserveAttachAndIdempotencyIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000171"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	request := credentialReserveParams(orgOneID, registryID, "create", "reserve-one", "acme/private", commitSHA('a'))
	reserved, err := dataStore.ReserveRegistryCredentialOperation(ctx, request)
	if err != nil || reserved.Status != "awaiting_source" || reserved.Checkpoint != "reserved" {
		t.Fatalf("reserve = %+v, %v", reserved, err)
	}
	var fingerprint []byte
	var actorID, action string
	var after string
	if err := pool.QueryRow(ctx, `SELECT operation.credential_fingerprint,operation.actor_user_id::text,audit.action,audit.after::text
		FROM registry_credential_operations operation JOIN audit_events audit ON audit.target_id=operation.registry_id
		WHERE operation.id=$1 AND audit.actor_user_id=operation.actor_user_id`, reserved.ID).Scan(&fingerprint, &actorID, &action, &after); err != nil {
		t.Fatal(err)
	}
	if len(fingerprint) != 32 || actorID != registryCredentialActorID || action != "registry.credential.create_requested" {
		t.Fatalf("fingerprint/actor/audit = %d %q %q", len(fingerprint), actorID, action)
	}
	for _, forbidden := range []string{"credential_fingerprint", "secret_ref", "dockerconfigjson"} {
		if strings.Contains(after, forbidden) {
			t.Fatalf("audit after contains %q: %s", forbidden, after)
		}
	}
	replay, err := dataStore.ReserveRegistryCredentialOperation(ctx, request)
	if err != nil || replay.ID != reserved.ID {
		t.Fatalf("reserve replay = %+v, %v", replay, err)
	}
	request.TargetReference = commitSHA('b')
	if _, err := dataStore.ReserveRegistryCredentialOperation(ctx, request); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reserve replay = %v, want conflict", err)
	}
	request.TargetReference = commitSHA('a')
	request.CredentialFingerprint = sha256.Sum256([]byte("different credential"))
	if _, err := dataStore.ReserveRegistryCredentialOperation(ctx, request); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed credential replay = %v, want conflict", err)
	}
	secretRef := "k8s-secret://cyclopus-system/credential-one#dockerconfigjson"
	attached, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "reserve-one", SecretRef: secretRef})
	if err != nil || attached.Status != "pending" || attached.Checkpoint != "source_secret_written" {
		t.Fatalf("attach = %+v, %v", attached, err)
	}
	if _, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "reserve-one", SecretRef: secretRef}); err != nil {
		t.Fatalf("attach replay: %v", err)
	}
	if _, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "reserve-one", SecretRef: "k8s-secret://cyclopus-system/other#dockerconfigjson"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed attach replay = %v, want conflict", err)
	}
	generation, err := dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, 1)
	if err != nil || generation.State != "pending" || generation.SecretRef != secretRef {
		t.Fatalf("generation = %+v, %v", generation, err)
	}
}

// TestRegistryCredentialReserveAttachThenProcessIntegration walks the full
// reservation/attachment state machine into worker processing: reserve ->
// awaiting_source/reserved -> attach -> pending/source_secret_written ->
// claim -> target completion -> Finish reaches validation_complete.
func TestRegistryCredentialReserveAttachThenProcessIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000173"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	reserved, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "process-one", "acme/private", commitSHA('d')))
	if err != nil {
		t.Fatal(err)
	}
	// awaiting_source never reaches the worker: nothing pending to claim.
	if claims, err := dataStore.ClaimRegistryCredentialOperations(ctx, orgOneID, "worker-one", time.Minute, 10); err != nil || len(claims) != 0 {
		t.Fatalf("claim before attach = %+v, %v", claims, err)
	}
	secretRef := "k8s-secret://cyclopus-system/credential-process#dockerconfigjson"
	if _, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "process-one", SecretRef: secretRef}); err != nil {
		t.Fatal(err)
	}
	claims, err := dataStore.ClaimRegistryCredentialOperations(ctx, orgOneID, "worker-one", time.Minute, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim after attach = %+v, %v", claims, err)
	}
	operation := claims[0]
	if operation.Status != "leased" || operation.Checkpoint != "source_secret_written" || operation.Lease == nil {
		t.Fatalf("claimed operation = %+v", operation)
	}
	generation, err := dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, operation.CandidateGeneration)
	if err != nil || generation.SecretRef != secretRef || generation.State != "pending" {
		t.Fatalf("generation = %+v, %v", generation, err)
	}
	targets, err := dataStore.ClaimRegistryCredentialValidationTargets(ctx, operation, "worker-one", time.Minute, 1000)
	if err != nil || len(targets) != 1 || targets[0].Lease == nil {
		t.Fatalf("targets = %+v, %v", targets, err)
	}
	if err := dataStore.CompleteRegistryCredentialValidationTarget(ctx, targets[0], *targets[0].Lease); err != nil {
		t.Fatal(err)
	}
	complete, err := dataStore.FinishRegistryCredentialValidation(ctx, operation, *operation.Lease)
	if err != nil || !complete {
		t.Fatalf("finish = %t, %v", complete, err)
	}
	operation, err = dataStore.GetRegistryCredentialOperation(ctx, orgOneID, operation.ID)
	if err != nil || operation.Checkpoint != "validation_complete" || operation.Status != "pending" {
		t.Fatalf("post-finish operation = %+v, %v", operation, err)
	}
	generation, err = dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, operation.CandidateGeneration)
	if err != nil || generation.ValidatedAt == nil || generation.State != "pending" {
		t.Fatalf("post-finish generation = %+v, %v", generation, err)
	}
	registry, err := dataStore.GetRegistry(ctx, orgOneID, registryID)
	if err != nil || registry.State != "reconciling" || registry.Checkpoint != "reconciling_namespaces" {
		t.Fatalf("post-finish registry = %+v, %v", registry, err)
	}
}

// TestRegistryCredentialRestartAfterLeaseReclaimIntegration simulates a worker
// crash between target completion and Finish: the lease expires, the operation
// is reclaimed at source_secret_written, re-leased, and Finish drives the
// restart to validation_complete.
func TestRegistryCredentialRestartAfterLeaseReclaimIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000174"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	reserved, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "restart-one", "acme/private", commitSHA('e')))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "restart-one", SecretRef: "k8s-secret://cyclopus-system/credential-restart#dockerconfigjson"}); err != nil {
		t.Fatal(err)
	}
	claims, err := dataStore.ClaimRegistryCredentialOperations(ctx, orgOneID, "worker-one", time.Minute, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	operation := claims[0]
	targets, err := dataStore.ClaimRegistryCredentialValidationTargets(ctx, operation, "worker-one", time.Minute, 1000)
	if err != nil || len(targets) != 1 || targets[0].Lease == nil {
		t.Fatalf("targets = %+v, %v", targets, err)
	}
	if err := dataStore.CompleteRegistryCredentialValidationTarget(ctx, targets[0], *targets[0].Lease); err != nil {
		t.Fatal(err)
	}
	// Crash before Finish: operation lease expires and is reclaimed.
	if _, err := pool.Exec(ctx, `UPDATE registry_credential_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE org_id=$1 AND id=$2`, orgOneID, operation.ID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := dataStore.RecoverExpiredRegistryCredentialLeases(ctx, 10); err != nil || recovered != 1 {
		t.Fatalf("recover = %d, %v", recovered, err)
	}
	reclaims, err := dataStore.ClaimRegistryCredentialOperations(ctx, orgOneID, "worker-two", time.Minute, 10)
	if err != nil || len(reclaims) != 1 {
		t.Fatalf("reclaim = %+v, %v", reclaims, err)
	}
	restarted := reclaims[0]
	if restarted.Checkpoint != "source_secret_written" || restarted.Lease == nil || restarted.Lease.Owner != "worker-two" {
		t.Fatalf("restarted operation = %+v", restarted)
	}
	// Restart processing re-reads the Secret (persisted generation ref) and
	// finds no pending targets; Finish completes from the reclaim path.
	remaining, err := dataStore.ClaimRegistryCredentialValidationTargets(ctx, restarted, "worker-two", time.Minute, 1000)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining targets = %+v, %v", remaining, err)
	}
	complete, err := dataStore.FinishRegistryCredentialValidation(ctx, restarted, *restarted.Lease)
	if err != nil || !complete {
		t.Fatalf("restart finish = %t, %v", complete, err)
	}
	generation, err := dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, restarted.CandidateGeneration)
	if err != nil || generation.ValidatedAt == nil {
		t.Fatalf("generation = %+v, %v", generation, err)
	}
}

// TestRegistryCredentialPermanentTargetTerminatesIntegration covers the
// permanent-failure state machine edge: Finish returns complete=false yet
// terminalizes operation, generation, and registry.
func TestRegistryCredentialPermanentTargetTerminatesIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000175"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	reserved, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "permanent-one", "acme/private", commitSHA('f')))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "permanent-one", SecretRef: "k8s-secret://cyclopus-system/credential-permanent#dockerconfigjson"}); err != nil {
		t.Fatal(err)
	}
	claims, err := dataStore.ClaimRegistryCredentialOperations(ctx, orgOneID, "worker-one", time.Minute, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	operation := claims[0]
	targets, err := dataStore.ClaimRegistryCredentialValidationTargets(ctx, operation, "worker-one", time.Minute, 1000)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %+v, %v", targets, err)
	}
	if err := dataStore.FailRegistryCredentialValidationTarget(ctx, targets[0], *targets[0].Lease, "registry_auth_failed"); err != nil {
		t.Fatal(err)
	}
	complete, err := dataStore.FinishRegistryCredentialValidation(ctx, operation, *operation.Lease)
	if err != nil || complete {
		t.Fatalf("finish = %t, %v; want complete=false without error", complete, err)
	}
	operation, err = dataStore.GetRegistryCredentialOperation(ctx, orgOneID, operation.ID)
	if err != nil || operation.Status != "failed" || operation.FailureCode == nil || *operation.FailureCode != "credential_validation_failed" || operation.TerminalAt == nil {
		t.Fatalf("operation = %+v, %v", operation, err)
	}
	generation, err := dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, operation.CandidateGeneration)
	if err != nil || generation.State != "failed" {
		t.Fatalf("generation = %+v, %v", generation, err)
	}
	registry, err := dataStore.GetRegistry(ctx, orgOneID, registryID)
	if err != nil || registry.State != "failed" || registry.OperationID != nil {
		t.Fatalf("registry = %+v, %v", registry, err)
	}
	// Restart edge: a stale lease cannot re-finish the terminal operation.
	if _, err := dataStore.FinishRegistryCredentialValidation(ctx, operation, LeaseToken{Owner: "worker-one", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)}); err == nil {
		t.Fatal("stale finish after terminalization must fail")
	}
}

// TestRegistryCredentialPriorFailureFinishFailsIntegration covers the restart
// path where a target was already failed before this Process call: Finish
// terminalizes and the worker must return Failed rather than Retry.
func TestRegistryCredentialPriorFailureFinishFailsIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000176"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	reserved, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "prior-one", "acme/private", commitSHA('7')))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "prior-one", SecretRef: "k8s-secret://cyclopus-system/credential-prior#dockerconfigjson"}); err != nil {
		t.Fatal(err)
	}
	claims, err := dataStore.ClaimRegistryCredentialOperations(ctx, orgOneID, "worker-one", time.Minute, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	operation := claims[0]
	targets, err := dataStore.ClaimRegistryCredentialValidationTargets(ctx, operation, "worker-one", time.Minute, 1000)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %+v, %v", targets, err)
	}
	// First attempt: fail the target, then crash before Finish.
	if err := dataStore.FailRegistryCredentialValidationTarget(ctx, targets[0], *targets[0].Lease, "registry_auth_failed"); err != nil {
		t.Fatal(err)
	}
	// Restart attempt with the same lease still valid: no targets are claimed
	// (already failed), and Finish must terminalize with complete=false.
	remaining, err := dataStore.ClaimRegistryCredentialValidationTargets(ctx, operation, "worker-one", time.Minute, 1000)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining = %+v, %v", remaining, err)
	}
	complete, err := dataStore.FinishRegistryCredentialValidation(ctx, operation, *operation.Lease)
	if err != nil || complete {
		t.Fatalf("finish = %t, %v; want complete=false without error", complete, err)
	}
	operation, err = dataStore.GetRegistryCredentialOperation(ctx, orgOneID, operation.ID)
	if err != nil || operation.Status != "failed" || operation.FailureCode == nil || *operation.FailureCode != "credential_validation_failed" {
		t.Fatalf("operation = %+v, %v", operation, err)
	}
	target, err := dataStore.GetRegistryCredentialValidationTarget(ctx, orgOneID, operation.ID, "acme/private", commitSHA('7'))
	if err != nil || target.Status != "failed" || target.FailureCode == nil || *target.FailureCode != "registry_auth_failed" {
		t.Fatalf("target = %+v, %v", target, err)
	}
}

// TestRegistryCredentialTransientTargetRetriesIntegration covers transient
// failures: the target is retried with backoff and stays pending; Finish
// reports the operation cannot yet complete.
func TestRegistryCredentialTransientTargetRetriesIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000177"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	reserved, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "transient-one", "acme/private", commitSHA('8')))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "transient-one", SecretRef: "k8s-secret://cyclopus-system/credential-transient#dockerconfigjson"}); err != nil {
		t.Fatal(err)
	}
	claims, err := dataStore.ClaimRegistryCredentialOperations(ctx, orgOneID, "worker-one", time.Minute, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	operation := claims[0]
	targets, err := dataStore.ClaimRegistryCredentialValidationTargets(ctx, operation, "worker-one", time.Minute, 1000)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %+v, %v", targets, err)
	}
	if err := dataStore.RetryRegistryCredentialValidationTarget(ctx, targets[0], *targets[0].Lease, "registry_unavailable", time.Minute); err != nil {
		t.Fatal(err)
	}
	target, err := dataStore.GetRegistryCredentialValidationTarget(ctx, orgOneID, operation.ID, "acme/private", commitSHA('8'))
	if err != nil || target.Status != "pending" || target.FailureCode != nil || target.Lease != nil || !target.NextAttemptAt.After(time.Now()) {
		t.Fatalf("retried target = %+v, %v", target, err)
	}
	if _, err := dataStore.FinishRegistryCredentialValidation(ctx, operation, *operation.Lease); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("finish with pending target = %v, want failed precondition", err)
	}
	if err := dataStore.RetryRegistryCredentialOperation(ctx, operation, *operation.Lease, time.Second, "registry_unavailable"); err != nil {
		t.Fatal(err)
	}
	operation, err = dataStore.GetRegistryCredentialOperation(ctx, orgOneID, operation.ID)
	if err != nil || operation.Status != "pending" || operation.Lease != nil {
		t.Fatalf("retried operation = %+v, %v", operation, err)
	}
}

// TestRegistryCredentialAttemptTwelveExhaustionIntegration drives a target to
// the 12-attempt ceiling: recovery of the expired target lease terminalizes
// the whole operation with credential_operation_exhausted.
func TestRegistryCredentialAttemptTwelveExhaustionIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000178"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	reserved, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "exhaust-one", "acme/private", commitSHA('9')))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.AttachRegistryCredentialSource(ctx, AttachRegistryCredentialSourceParams{OrgID: orgOneID, OperationID: reserved.ID, IdempotencyKey: "exhaust-one", SecretRef: "k8s-secret://cyclopus-system/credential-exhaust#dockerconfigjson"}); err != nil {
		t.Fatal(err)
	}
	claims, err := dataStore.ClaimRegistryCredentialOperations(ctx, orgOneID, "worker-one", time.Minute, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	operation := claims[0]
	// Pin the target at 12 attempts with an expired lease still recorded.
	if _, err := pool.Exec(ctx, `UPDATE registry_credential_validation_targets SET attempts=12,lease_owner='worker-one',lease_expires_at=clock_timestamp()-interval '1 second' WHERE operation_id=$1`, operation.ID); err != nil {
		t.Fatal(err)
	}
	targets, err := dataStore.ClaimRegistryCredentialValidationTargets(ctx, operation, "worker-one", time.Minute, 1000)
	if err != nil || len(targets) != 0 {
		t.Fatalf("exhausted targets claimed = %+v, %v", targets, err)
	}
	if recovered, err := dataStore.RecoverExpiredRegistryCredentialValidationTargets(ctx, 10); err != nil || recovered != 1 {
		t.Fatalf("recover = %d, %v", recovered, err)
	}
	operation, err = dataStore.GetRegistryCredentialOperation(ctx, orgOneID, operation.ID)
	if err != nil || operation.Status != "failed" || operation.FailureCode == nil || *operation.FailureCode != "credential_operation_exhausted" {
		t.Fatalf("operation = %+v, %v", operation, err)
	}
	target, err := dataStore.GetRegistryCredentialValidationTarget(ctx, orgOneID, operation.ID, "acme/private", commitSHA('9'))
	if err != nil || target.Status != "skipped" {
		t.Fatalf("target = %+v, %v", target, err)
	}
	generation, err := dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, operation.CandidateGeneration)
	if err != nil || generation.State != "failed" {
		t.Fatalf("generation = %+v, %v", generation, err)
	}
}

func TestCheckpointRegistryCredentialSourceRejectsLegacyCheckpointsIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000179"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	reserved, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "checkpoint-one", "acme/private", commitSHA('a')))
	if err != nil {
		t.Fatal(err)
	}
	lease := LeaseToken{Owner: "worker-one", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)}
	if err := dataStore.CheckpointRegistryCredentialSource(ctx, reserved, lease, "source_secret_write_started"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("write_started checkpoint = %v, want invalid argument", err)
	}
	if err := dataStore.CheckpointRegistryCredentialSource(ctx, reserved, lease, "pending_recorded"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("pending_recorded checkpoint = %v, want invalid argument", err)
	}
}

func TestRegistryCredentialValidationReferenceConstraintsIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000180"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "reference-one", "acme/private", "not-a-reference")); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("reserve invalid reference = %v, want invalid argument", err)
	}
	if _, err := dataStore.GetRegistryCredentialValidationTarget(ctx, orgOneID, "00000000-0000-0000-0000-000000000999", "acme/private", "nope"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("get invalid reference = %v, want invalid argument", err)
	}
	var pgErr *pgconn.PgError
	_, err := pool.Exec(ctx, `INSERT INTO registry_credential_validation_targets(operation_id,org_id,registry_id,repository,reference) VALUES(uuidv7(),$1,$2,'acme/private','latest')`, orgOneID, registryID)
	if err == nil || !errors.As(err, &pgErr) {
		t.Fatalf("direct insert invalid reference = %v, want constraint violation", err)
	}
}

func TestRegistryCredentialAwaitingSourceRescueIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000172"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	operation, err := dataStore.ReserveRegistryCredentialOperation(ctx, credentialReserveParams(orgOneID, registryID, "create", "rescue-one", "acme/private", commitSHA('c')))
	if err != nil {
		t.Fatal(err)
	}
	if rescued, err := dataStore.RescueAwaitingRegistryCredentialSources(ctx, time.Now().Add(time.Minute), 1); err != nil || rescued != 1 {
		t.Fatalf("rescue = %d, %v", rescued, err)
	}
	operation, err = dataStore.GetRegistryCredentialOperation(ctx, orgOneID, operation.ID)
	if err != nil || operation.Status != "failed" || operation.FailureCode == nil {
		t.Fatalf("rescued operation = %+v, %v", operation, err)
	}
	registry, err := dataStore.GetRegistry(ctx, orgOneID, registryID)
	if err != nil || registry.State != "failed" || registry.OperationID != nil || registry.PendingGeneration != nil {
		t.Fatalf("rescued registry = %+v, %v", registry, err)
	}
}
