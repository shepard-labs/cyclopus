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
	if err != nil || operation.Checkpoint != "activated" || operation.Status != "succeeded" {
		t.Fatalf("post-finish operation = %+v, %v", operation, err)
	}
	generation, err = dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, operation.CandidateGeneration)
	if err != nil || generation.ValidatedAt == nil || generation.State != "active" {
		t.Fatalf("post-finish generation = %+v, %v", generation, err)
	}
	registry, err := dataStore.GetRegistry(ctx, orgOneID, registryID)
	if err != nil || registry.State != "ready" || registry.Checkpoint != "activated" {
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

func seedNamespaceCredentialSaga(t *testing.T, pool *pgxpool.Pool, rotation bool, environments int) (*Store, RegistryCredentialOperation, string, string) {
	t.Helper()
	dataStore := New(pool)
	seedTenant(t, pool, tenantOne())
	seedRegistryCredentialActor(t, pool)
	registryID := "00000000-0000-7000-8000-000000000681"
	appID := "00000000-0000-7000-8000-000000000682"
	secretRef := "k8s-secret://cyclopus-system/credential-namespace#dockerconfigjson"
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	activeGeneration := int64(0)
	var previous any
	if rotation {
		activeGeneration, previous = 1, int64(1)
	}
	// The unconditional enforce_registry_credential_references BEFORE trigger
	// rejects generation references that do not exist yet, so the registry is
	// inserted bare and pointed at its generations once those rows exist.
	if _, err = tx.Exec(t.Context(), `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	if rotation {
		if _, err = tx.Exec(t.Context(), `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,
			candidate_generation,idempotency_key,terminal_at,credential_fingerprint,actor_user_id) VALUES('00000000-0000-7000-8000-000000000683',$1,$2,'create','succeeded','activated',
			1,'namespace-active',clock_timestamp(),decode(repeat('11',32),'hex'),$3)`, orgOneID, registryID, registryCredentialActorID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(t.Context(), `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,
			operation_id,validated_at,activated_at) VALUES($1,$2,1,'k8s-secret://cyclopus-system/credential-active#dockerconfigjson','active',
			'00000000-0000-7000-8000-000000000683',clock_timestamp(),clock_timestamp())`, orgOneID, registryID); err != nil {
			t.Fatal(err)
		}
	}
	candidate := activeGeneration + 1
	operationID := "00000000-0000-7000-8000-000000000684"
	kind := "create"
	if rotation {
		kind = "rotate"
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,
		candidate_generation,previous_generation,idempotency_key,credential_fingerprint,actor_user_id) VALUES($1,$2,$3,$4,'pending','namespaces_reconciling',$5,$6,'namespace-candidate',decode(repeat('22',32),'hex'),$7)`,
		operationID, orgOneID, registryID, kind, candidate, previous, registryCredentialActorID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,
		operation_id,validated_at) VALUES($1,$2,$3,$4,'pending',$5,clock_timestamp())`, orgOneID, registryID, candidate, secretRef, operationID); err != nil {
		t.Fatal(err)
	}
	if rotation {
		if _, err = tx.Exec(t.Context(), `UPDATE registries SET state='reconciling',
			checkpoint='namespaces_reconciling',active_generation=1,
			active_credential_secret_ref='k8s-secret://cyclopus-system/credential-active#dockerconfigjson',activated_at=clock_timestamp(),
			pending_generation=$3,pending_credential_secret_ref=$4,previous_generation=$5,
			previous_credential_secret_ref='k8s-secret://cyclopus-system/credential-active#dockerconfigjson',
			operation_id=$6 WHERE org_id=$1 AND id=$2`, orgOneID, registryID, candidate, secretRef, previous, operationID); err != nil {
			t.Fatal(err)
		}
	} else if _, err = tx.Exec(t.Context(), `UPDATE registries SET state='creating',
		checkpoint='namespaces_reconciling',pending_generation=$3,
		pending_credential_secret_ref=$4,operation_id=$5 WHERE org_id=$1 AND id=$2`, orgOneID, registryID, candidate, secretRef, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,github_workflow_path,production_branch,
		image_repository,image_visibility,registry_id,container_port,health_check_path) VALUES($1,$2,$3,'Namespace App','namespace-app',
		'namespace-app','.github/workflows/deploy.yml','main','acme/private','private',$4,8080,'/health')`, appID, orgOneID, projectOneID, registryID); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < environments; index++ {
		environmentID := fmt.Sprintf("00000000-0000-7000-8000-%012d", 685+index)
		kind := "production"
		var previewSlug *string
		if index > 0 {
			kind = "preview"
			slug := fmt.Sprintf("namespace-%d", index)
			previewSlug = &slug
		}
		if _, err = tx.Exec(t.Context(), `INSERT INTO environments(id,org_id,app_id,branch,kind,preview_slug,namespace,planned_hostname,lifecycle_status,
			required_registry_credential_generation,registry_credential_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'ready',$9,'pending')`,
			environmentID, orgOneID, appID, fmt.Sprintf("main-%d", index), kind, previewSlug, fmt.Sprintf("namespace-%d", index),
			fmt.Sprintf("namespace-%d.example.test", index), candidate); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	operation, err := dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operationID)
	if err != nil {
		t.Fatal(err)
	}
	return dataStore, operation, registryID, secretRef
}

func namespaceCredentialIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_, pool := newIntegrationStore(t)
	return pool
}

func claimCompleteNamespaceCredential(t *testing.T, dataStore *Store, owner string) (NamespaceCredentialClaim, bool) {
	t.Helper()
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, owner, time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("namespace claims = %+v, %v", claims, err)
	}
	claim := claims[0]
	if err := dataStore.CheckpointNamespaceCredential(t.Context(), claim, claim.Lease, "source_read"); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CheckpointNamespaceCredential(t.Context(), claim, claim.Lease, "namespace_secret_written"); err != nil {
		t.Fatal(err)
	}
	activated, err := dataStore.CompleteNamespaceCredential(t.Context(), claim, claim.Lease)
	if err != nil {
		t.Fatal(err)
	}
	return claim, activated
}

func TestNamespaceCredentialClaimCompleteAndActivationIntegration(t *testing.T) {
	dataStore, operation, registryID, secretRef := seedNamespaceCredentialSaga(t, namespaceCredentialIntegrationPool(t), false, 2)
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "namespace-one", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	claim := claims[0]
	if claim.RegistryID != registryID || claim.OperationID != operation.ID || claim.SecretRef != secretRef || claim.Checkpoint != "" {
		t.Fatalf("claim identity = %+v", claim)
	}
	if err := dataStore.CheckpointNamespaceCredential(t.Context(), claim, claim.Lease, "source_read"); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CheckpointNamespaceCredential(t.Context(), claim, claim.Lease, "namespace_secret_written"); err != nil {
		t.Fatal(err)
	}
	activated, err := dataStore.CompleteNamespaceCredential(t.Context(), claim, claim.Lease)
	if err != nil || activated {
		t.Fatalf("first complete = %t, %v", activated, err)
	}
	reconciliation, err := dataStore.GetNamespaceCredentialReconciliation(t.Context(), orgOneID, claim.EnvironmentID)
	if err != nil || reconciliation.State != "ready" || reconciliation.Checkpoint != "namespace_secret_verified" ||
		reconciliation.ReadyGeneration == nil || *reconciliation.ReadyGeneration != claim.Generation {
		t.Fatalf("completed namespace = %+v, %v", reconciliation, err)
	}
	_, activated = claimCompleteNamespaceCredential(t, dataStore, "namespace-two")
	if !activated {
		t.Fatal("last namespace did not activate candidate")
	}
	registry, err := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
	if err != nil || registry.State != "ready" || registry.Checkpoint != "activated" || registry.ActiveGeneration != claim.Generation {
		t.Fatalf("registry = %+v, %v", registry, err)
	}
}

func TestNamespaceCredentialExpiredWrittenReclaimsAndStaleLeaseRejectedIntegration(t *testing.T) {
	pool := namespaceCredentialIntegrationPool(t)
	dataStore, _, _, _ := seedNamespaceCredentialSaga(t, pool, false, 1)
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "namespace-old", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	old := claims[0]
	if err := dataStore.CheckpointNamespaceCredential(t.Context(), old, old.Lease, "source_read"); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CheckpointNamespaceCredential(t.Context(), old, old.Lease, "namespace_secret_written"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE environments SET registry_credential_lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, old.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := dataStore.RecoverExpiredNamespaceCredentialLeases(t.Context(), 10); err != nil || recovered != 1 {
		t.Fatalf("recover = %d, %v", recovered, err)
	}
	reclaims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "namespace-new", time.Minute, 1)
	if err != nil || len(reclaims) != 1 || reclaims[0].Checkpoint != "namespace_secret_written" {
		t.Fatalf("reclaims = %+v, %v", reclaims, err)
	}
	if _, err := dataStore.CompleteNamespaceCredential(t.Context(), old, old.Lease); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale completion = %v", err)
	}
	staleGeneration := reclaims[0]
	staleGeneration.Generation++
	if err := dataStore.CheckpointNamespaceCredential(t.Context(), staleGeneration, staleGeneration.Lease, "source_read"); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale generation = %v", err)
	}
}

func TestNamespaceCredentialPermanentFailureAndRotationRollbackIntegration(t *testing.T) {
	t.Run("first create", func(t *testing.T) {
		dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, namespaceCredentialIntegrationPool(t), false, 1)
		claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "namespace-fail", time.Minute, 1)
		if err != nil || len(claims) != 1 {
			t.Fatalf("claims = %+v, %v", claims, err)
		}
		if err := dataStore.FailNamespaceCredential(t.Context(), claims[0], claims[0].Lease, "kubernetes_ownership_conflict", "owned elsewhere"); err != nil {
			t.Fatal(err)
		}
		operation, _ = dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operation.ID)
		registry, _ := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
		if operation.Status != "failed" || operation.FailureCode == nil || *operation.FailureCode != "kubernetes_ownership_conflict" || registry.State != "failed" {
			t.Fatalf("operation/registry = %+v %+v", operation, registry)
		}
	})
	t.Run("rotation rollback", func(t *testing.T) {
		dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, namespaceCredentialIntegrationPool(t), true, 2)
		claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "namespace-fail", time.Minute, 1)
		if err != nil || len(claims) != 1 {
			t.Fatalf("claims = %+v, %v", claims, err)
		}
		if err := dataStore.FailNamespaceCredential(t.Context(), claims[0], claims[0].Lease, "kubernetes_unavailable", "api unavailable"); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 2; index++ {
			_, _ = claimCompleteNamespaceCredential(t, dataStore, fmt.Sprintf("rollback-%d", index))
		}
		operation, _ = dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operation.ID)
		registry, _ := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
		generation, _ := dataStore.GetRegistryCredentialGeneration(t.Context(), orgOneID, registryID, 2)
		if operation.Status != "failed" || operation.FailureCode == nil || *operation.FailureCode != "kubernetes_unavailable" ||
			registry.State != "ready" || registry.ActiveGeneration != 1 || registry.PendingGeneration != nil || generation.State != "failed" {
			t.Fatalf("rollback = operation=%+v registry=%+v candidate=%+v", operation, registry, generation)
		}
	})
}

func TestVisibilityPrivateDuringRollbackSelectsPreviousGenerationIntegration(t *testing.T) {
	pool := namespaceCredentialIntegrationPool(t)
	dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, pool, true, 1)
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "rollback-start", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	if err := dataStore.FailNamespaceCredential(t.Context(), claims[0], claims[0].Lease, "kubernetes_unavailable", ""); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `UPDATE apps SET image_visibility='public',registry_id=NULL WHERE org_id=$1 AND id=$2`, orgOneID, claims[0].AppID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `UPDATE environments SET required_registry_credential_generation=NULL,
		ready_registry_credential_generation=NULL,registry_credential_state='not_required',registry_credential_checkpoint=NULL,
		registry_credential_error_code=NULL,registry_credential_error_message=NULL,registry_credential_lease_owner=NULL,
		registry_credential_lease_expires_at=NULL WHERE org_id=$1 AND app_id=$2`, orgOneID, claims[0].AppID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), `SELECT transition_app_image_visibility($1,$2,'private',$3)`, orgOneID, claims[0].AppID, registryID); err != nil {
		t.Fatal(err)
	}
	reconciliation, err := dataStore.GetNamespaceCredentialReconciliation(t.Context(), orgOneID, claims[0].EnvironmentID)
	if err != nil || reconciliation.RequiredGeneration == nil || *reconciliation.RequiredGeneration != 1 {
		t.Fatalf("rollback private target = %+v, %v; operation=%s", reconciliation, err, operation.ID)
	}
}

func TestVisibilityPublicLastRotationTargetActivatesCandidateIntegration(t *testing.T) {
	pool := namespaceCredentialIntegrationPool(t)
	dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, pool, true, 1)
	appID := "00000000-0000-7000-8000-000000000682"
	if _, err := pool.Exec(t.Context(), `SELECT transition_app_image_visibility($1,$2,'public',NULL)`, orgOneID, appID); err != nil {
		t.Fatal(err)
	}
	operation, err := dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operation.ID)
	if err != nil || operation.Status != "succeeded" {
		t.Fatalf("operation = %+v, %v", operation, err)
	}
	registry, err := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
	if err != nil || registry.State != "ready" || registry.ActiveGeneration != 2 || registry.OperationID != nil {
		t.Fatalf("registry = %+v, %v", registry, err)
	}
}

func TestRollbackPermanentFailureTerminalizesWithoutReseedingIntegration(t *testing.T) {
	dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, namespaceCredentialIntegrationPool(t), true, 2)
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "rollback-start", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	if err = dataStore.FailNamespaceCredential(t.Context(), claims[0], claims[0].Lease, "kubernetes_unavailable", ""); err != nil {
		t.Fatal(err)
	}
	rollback, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "rollback-fail", time.Minute, 1)
	if err != nil || len(rollback) != 1 || rollback[0].Generation != 1 {
		t.Fatalf("rollback claim = %+v, %v", rollback, err)
	}
	if err = dataStore.FailNamespaceCredential(t.Context(), rollback[0], rollback[0].Lease, "kubernetes_ownership_conflict", ""); err != nil {
		t.Fatal(err)
	}
	operation, _ = dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operation.ID)
	registry, _ := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
	if operation.Status != "failed" || operation.FailureCode == nil || *operation.FailureCode != "kubernetes_ownership_conflict" ||
		registry.State != "ready" || registry.ActiveGeneration != 1 || registry.OperationID != nil {
		t.Fatalf("terminal rollback = operation=%+v registry=%+v", operation, registry)
	}
	if claims, err = dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "rollback-again", time.Minute, 10); err != nil || len(claims) != 0 {
		t.Fatalf("reseeded rollback = %+v, %v", claims, err)
	}
}

func TestNamespaceCredentialFinalAttemptCrashRecoveryIntegration(t *testing.T) {
	pool := namespaceCredentialIntegrationPool(t)
	dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, pool, false, 1)
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "final-attempt", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim = %+v, %v", claims, err)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE environments SET registry_credential_attempts=12,
		registry_credential_lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, claims[0].EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := dataStore.RecoverExpiredNamespaceCredentialLeases(t.Context(), 10); err != nil || recovered != 1 {
		t.Fatalf("recover = %d, %v", recovered, err)
	}
	operation, _ = dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operation.ID)
	registry, _ := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
	if operation.Status != "failed" || registry.State != "failed" {
		t.Fatalf("final attempt = operation=%+v registry=%+v", operation, registry)
	}
}

func TestRegistryCredentialGCExcludesNamespaceCleanupTargetsIntegration(t *testing.T) {
	pool := namespaceCredentialIntegrationPool(t)
	dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, pool, false, 1)
	gcTx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer gcTx.Rollback(t.Context())
	if _, err = gcTx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	// Transition app to public first so the environment can go to not_required.
	if _, err = gcTx.Exec(t.Context(), `UPDATE apps SET image_visibility='public',registry_id=NULL WHERE org_id=$1 AND registry_id=$2`, orgOneID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = gcTx.Exec(t.Context(), `UPDATE environments SET required_registry_credential_generation=NULL,
		registry_credential_state='not_required',registry_credential_cleanup_state='pending',
		registry_credential_cleanup_registry_id=$2,registry_credential_cleanup_generation=1 WHERE org_id=$1`, orgOneID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = gcTx.Exec(t.Context(), `UPDATE registry_credential_operations SET status='failed',failure_code='postgres_permanent',
		terminal_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = gcTx.Exec(t.Context(), `UPDATE registry_credential_generations SET state='failed' WHERE registry_id=$1 AND generation=1`, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = gcTx.Exec(t.Context(), `UPDATE registries SET state='failed',checkpoint=NULL,pending_generation=NULL,
		pending_credential_secret_ref=NULL,operation_id=NULL,failure_code='postgres_permanent' WHERE id=$1`, registryID); err != nil {
		t.Fatal(err)
	}
	if err = gcTx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidates, err := dataStore.ListRegistryCredentialGarbageCollectionCandidates(t.Context(), time.Now().Add(-time.Hour), 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("GC candidates = %+v, %v", candidates, err)
	}
}

func TestNamespaceCredentialTeardownInvalidatesLeaseIntegration(t *testing.T) {
	dataStore, _, _, _ := seedNamespaceCredentialSaga(t, namespaceCredentialIntegrationPool(t), false, 1)
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "namespace-teardown", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	claim := claims[0]
	if err := dataStore.InvalidateNamespaceCredentialLeaseForTeardown(t.Context(), orgOneID, claim.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CheckpointNamespaceCredential(t.Context(), claim, claim.Lease, "source_read"); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("invalidated checkpoint = %v", err)
	}
}

func leaseNamespaceCredentialCleanupForTeardown(t *testing.T, pool *pgxpool.Pool, environmentID, registryID string, generation int64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `UPDATE environments SET registry_credential_cleanup_state='pending',
		registry_credential_cleanup_registry_id=$2,registry_credential_cleanup_generation=$3,
		registry_credential_cleanup_lease_owner='cleanup-worker',registry_credential_cleanup_lease_expires_at=clock_timestamp()+interval '1 minute'
		WHERE org_id=$1 AND id=$4`, orgOneID, registryID, generation, environmentID); err != nil {
		t.Fatal(err)
	}
}

func assertNamespaceCredentialTeardownLeasesInvalidated(t *testing.T, pool *pgxpool.Pool, environmentID string) {
	t.Helper()
	var reconciliationOwner, cleanupOwner *string
	var reconciliationEpoch, cleanupEpoch int64
	var cleanupState string
	if err := pool.QueryRow(t.Context(), `SELECT registry_credential_lease_owner,registry_credential_lease_epoch,
		registry_credential_cleanup_lease_owner,registry_credential_cleanup_lease_epoch,registry_credential_cleanup_state
		FROM environments WHERE org_id=$1 AND id=$2`, orgOneID, environmentID).Scan(
		&reconciliationOwner, &reconciliationEpoch, &cleanupOwner, &cleanupEpoch, &cleanupState); err != nil {
		t.Fatal(err)
	}
	if reconciliationOwner != nil || cleanupOwner != nil || reconciliationEpoch < 2 || cleanupEpoch < 1 || cleanupState != "complete" {
		t.Fatalf("teardown leases = reconciliation owner/epoch %v/%d cleanup owner/epoch/state %v/%d/%s",
			reconciliationOwner, reconciliationEpoch, cleanupOwner, cleanupEpoch, cleanupState)
	}
}

func TestNamespaceCredentialLastTargetTeardownActivatesCandidateIntegration(t *testing.T) {
	pool := namespaceCredentialIntegrationPool(t)
	dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, pool, false, 1)
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "candidate-teardown", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	claim := claims[0]
	leaseNamespaceCredentialCleanupForTeardown(t, pool, claim.EnvironmentID, registryID, claim.Generation)
	if err = dataStore.InvalidateNamespaceCredentialLeaseForTeardown(t.Context(), orgOneID, claim.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	assertNamespaceCredentialTeardownLeasesInvalidated(t, pool, claim.EnvironmentID)
	operation, err = dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operation.ID)
	if err != nil || operation.Status != "succeeded" || operation.Checkpoint != "activated" {
		t.Fatalf("operation = %+v, %v", operation, err)
	}
	registry, err := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
	if err != nil || registry.State != "ready" || registry.ActiveGeneration != claim.Generation || registry.OperationID != nil {
		t.Fatalf("registry = %+v, %v", registry, err)
	}
}

func TestNamespaceCredentialLastTargetTeardownFinalizesRollbackIntegration(t *testing.T) {
	pool := namespaceCredentialIntegrationPool(t)
	dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, pool, true, 1)
	candidate, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "candidate-failure", time.Minute, 1)
	if err != nil || len(candidate) != 1 {
		t.Fatalf("candidate claim = %+v, %v", candidate, err)
	}
	if err = dataStore.FailNamespaceCredential(t.Context(), candidate[0], candidate[0].Lease, "kubernetes_unavailable", ""); err != nil {
		t.Fatal(err)
	}
	rollback, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "rollback-teardown", time.Minute, 1)
	if err != nil || len(rollback) != 1 || rollback[0].Generation != 1 {
		t.Fatalf("rollback claim = %+v, %v", rollback, err)
	}
	claim := rollback[0]
	leaseNamespaceCredentialCleanupForTeardown(t, pool, claim.EnvironmentID, registryID, claim.Generation)
	if err = dataStore.InvalidateNamespaceCredentialLeaseForTeardown(t.Context(), orgOneID, claim.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	assertNamespaceCredentialTeardownLeasesInvalidated(t, pool, claim.EnvironmentID)
	operation, err = dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operation.ID)
	if err != nil || operation.Status != "failed" || operation.Checkpoint != "rollback_reconciling" {
		t.Fatalf("operation = %+v, %v", operation, err)
	}
	registry, err := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
	if err != nil || registry.State != "ready" || registry.ActiveGeneration != 1 || registry.PendingGeneration != nil || registry.OperationID != nil {
		t.Fatalf("registry = %+v, %v", registry, err)
	}
}

func TestNamespaceCredentialNonLastTargetTeardownDoesNotActivateCandidateIntegration(t *testing.T) {
	pool := namespaceCredentialIntegrationPool(t)
	dataStore, operation, registryID, _ := seedNamespaceCredentialSaga(t, pool, false, 2)
	claims, err := dataStore.ClaimNamespaceCredentials(t.Context(), orgOneID, "non-last-teardown", time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	claim := claims[0]
	leaseNamespaceCredentialCleanupForTeardown(t, pool, claim.EnvironmentID, registryID, claim.Generation)
	if err = dataStore.InvalidateNamespaceCredentialLeaseForTeardown(t.Context(), orgOneID, claim.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	assertNamespaceCredentialTeardownLeasesInvalidated(t, pool, claim.EnvironmentID)
	operation, err = dataStore.GetRegistryCredentialOperation(t.Context(), orgOneID, operation.ID)
	if err != nil || operation.Status != "pending" || operation.Checkpoint != "namespaces_reconciling" {
		t.Fatalf("operation = %+v, %v", operation, err)
	}
	registry, err := dataStore.GetRegistry(t.Context(), orgOneID, registryID)
	if err != nil || registry.State != "creating" || registry.ActiveGeneration != 0 || registry.PendingGeneration == nil {
		t.Fatalf("registry = %+v, %v", registry, err)
	}
}

func seedNamespaceCredentialCleanup(t *testing.T, pool *pgxpool.Pool, environments int) (*Store, string, string) {
	t.Helper()
	dataStore, _, registryID, _ := seedNamespaceCredentialSaga(t, pool, false, environments)
	for index := 0; index < environments; index++ {
		claim, _ := claimCompleteNamespaceCredential(t, dataStore, fmt.Sprintf("cleanup-ready-%d", index))
		_ = claim
	}
	appID := "00000000-0000-7000-8000-000000000682"
	if _, err := pool.Exec(t.Context(), `SELECT transition_app_image_visibility($1,$2,'public',NULL)`, orgOneID, appID); err != nil {
		t.Fatal(err)
	}
	return dataStore, appID, registryID
}

func TestNamespaceCredentialCleanupLifecycleIntegration(t *testing.T) {
	t.Run("complete advances app", func(t *testing.T) {
		pool := namespaceCredentialIntegrationPool(t)
		dataStore, appID, registryID := seedNamespaceCredentialCleanup(t, pool, 2)
		orgIDs, err := dataStore.ListNamespaceCredentialOrgIDs(t.Context(), "", 10)
		if err != nil || len(orgIDs) != 1 || orgIDs[0] != orgOneID {
			t.Fatalf("cleanup orgs = %v, %v", orgIDs, err)
		}
		claims, err := dataStore.ClaimNamespaceCredentialCleanups(t.Context(), orgOneID, "cleanup-one", time.Minute, 1)
		if err != nil || len(claims) != 1 || claims[0].RegistryID != registryID || claims[0].OrgID != orgOneID {
			t.Fatalf("first cleanup claim = %+v, %v", claims, err)
		}
		if err := dataStore.CompleteNamespaceCredentialCleanup(t.Context(), claims[0], claims[0].Lease); err != nil {
			t.Fatal(err)
		}
		var state, checkpoint string
		if err := pool.QueryRow(t.Context(), `SELECT cleanup_state,COALESCE(cleanup_checkpoint,'') FROM apps WHERE org_id=$1 AND id=$2`, orgOneID, appID).Scan(&state, &checkpoint); err != nil || state != "cleanup_requested" {
			t.Fatalf("app after first cleanup = %s/%s, %v", state, checkpoint, err)
		}
		claims, err = dataStore.ClaimNamespaceCredentialCleanups(t.Context(), orgOneID, "cleanup-two", time.Minute, 1)
		if err != nil || len(claims) != 1 {
			t.Fatalf("second cleanup claim = %+v, %v", claims, err)
		}
		if err := dataStore.CompleteNamespaceCredentialCleanup(t.Context(), claims[0], claims[0].Lease); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT cleanup_state,COALESCE(cleanup_checkpoint,'') FROM apps WHERE org_id=$1 AND id=$2`, orgOneID, appID).Scan(&state, &checkpoint); err != nil || state != "cleaned" || checkpoint != "namespace_credentials_removed" {
			t.Fatalf("completed app cleanup = %s/%s, %v", state, checkpoint, err)
		}
	})

	t.Run("retry exhaustion stale and cross org fences", func(t *testing.T) {
		pool := namespaceCredentialIntegrationPool(t)
		dataStore, _, _ := seedNamespaceCredentialCleanup(t, pool, 1)
		if claims, err := dataStore.ClaimNamespaceCredentialCleanups(t.Context(), orgTwoID, "wrong-org", time.Minute, 1); err != nil || len(claims) != 0 {
			t.Fatalf("cross-org claims = %+v, %v", claims, err)
		}
		claims, err := dataStore.ClaimNamespaceCredentialCleanups(t.Context(), orgOneID, "cleanup-retry", time.Minute, 1)
		if err != nil || len(claims) != 1 {
			t.Fatalf("cleanup claim = %+v, %v", claims, err)
		}
		claim := claims[0]
		if err := dataStore.RescheduleNamespaceCredentialCleanup(t.Context(), claim, claim.Lease, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if err := dataStore.CompleteNamespaceCredentialCleanup(t.Context(), claim, claim.Lease); !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("stale cleanup completion = %v", err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE environments SET registry_credential_cleanup_attempts=11,
			registry_credential_cleanup_next_attempt_at=clock_timestamp() WHERE org_id=$1 AND id=$2`, orgOneID, claim.EnvironmentID); err != nil {
			t.Fatal(err)
		}
		claims, err = dataStore.ClaimNamespaceCredentialCleanups(t.Context(), orgOneID, "cleanup-final", time.Minute, 1)
		if err != nil || len(claims) != 1 || claims[0].Attempts != 12 {
			t.Fatalf("final cleanup claim = %+v, %v", claims, err)
		}
		if err := dataStore.FailNamespaceCredentialCleanup(t.Context(), claims[0], claims[0].Lease, "kubernetes_unavailable"); err != nil {
			t.Fatal(err)
		}
		var environmentState, appState, code string
		if err := pool.QueryRow(t.Context(), `SELECT environment.registry_credential_cleanup_state,app.cleanup_state,
			COALESCE(app.cleanup_error_code,'') FROM environments environment JOIN apps app ON app.org_id=environment.org_id
			AND app.id=environment.app_id WHERE environment.org_id=$1 AND environment.id=$2`, orgOneID, claim.EnvironmentID).Scan(&environmentState, &appState, &code); err != nil || environmentState != "failed" || appState != "cleanup_failed" || code != "kubernetes_unavailable" {
			t.Fatalf("cleanup failure = %s/%s/%s, %v", environmentState, appState, code, err)
		}
	})
}
