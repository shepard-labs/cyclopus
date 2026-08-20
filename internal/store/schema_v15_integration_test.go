package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schemaV15ActorID = "00000000-0000-0000-0000-000000000160"

func seedSchemaV15Actor(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO users(id,org_id,email,password_hash,display_name,operator)
		VALUES($1,$2,'schema-v15@example.test','hash','Schema V15 Admin',true)`, schemaV15ActorID, orgOneID); err != nil {
		t.Fatal(err)
	}
}

func TestEnablePhase6SchemaEpochIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedSchemaV15Actor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000141"
	operationID := "00000000-0000-0000-0000-000000000142"
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host,state,checkpoint,pending_generation,
		pending_credential_secret_ref,operation_id) VALUES($1,$2,'ghcr','ghcr.io','creating','secret_written',1,
		'k8s-secret://cyclopus-system/enable-ghcr#dockerconfigjson',$3)`, registryID, orgOneID, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,checkpoint,candidate_generation,idempotency_key,
		credential_fingerprint,actor_user_id) VALUES($1,$2,$3,'create','source_secret_written',1,'enable-phase6',decode(repeat('a',64),'hex'),$4)`, operationID, orgOneID, registryID, schemaV15ActorID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id)
		VALUES($1,$2,1,'k8s-secret://cyclopus-system/enable-ghcr#dockerconfigjson','pending',$3)`, orgOneID, registryID, operationID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.EnablePhase6SchemaEpoch(ctx); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	var checkpoint string
	if err := pool.QueryRow(ctx, `SELECT epoch.phase6_enabled,registry.checkpoint FROM schema_epoch epoch,registries registry
		WHERE epoch.singleton AND registry.id=$1`, registryID).Scan(&enabled, &checkpoint); err != nil || !enabled || checkpoint != "source_secret_written" {
		t.Fatalf("enabled/checkpoint = %t/%q, %v", enabled, checkpoint, err)
	}
}

func TestEnablePhase6SchemaEpochGuardAndHostileSearchPathIntegration(t *testing.T) {
	_, pool := newIntegrationStore(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `UPDATE schema_epoch SET phase6_enabled=true WHERE singleton`); err == nil {
		t.Fatal("direct Phase 6 enable succeeded")
	}
	var targetSchema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&targetSchema); err != nil {
		t.Fatal(err)
	}
	maliciousSchema := "malicious_phase6_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{maliciousSchema}.Sanitize()+`;
		CREATE TABLE `+pgx.Identifier{maliciousSchema, "schema_epoch"}.Sanitize()+`(singleton boolean PRIMARY KEY,phase6_enabled boolean NOT NULL);
		INSERT INTO `+pgx.Identifier{maliciousSchema, "schema_epoch"}.Sanitize()+` VALUES(true,false);
		CREATE TABLE `+pgx.Identifier{maliciousSchema, "phase6_hijack"}.Sanitize()+`(called boolean NOT NULL DEFAULT false);
		INSERT INTO `+pgx.Identifier{maliciousSchema, "phase6_hijack"}.Sanitize()+` DEFAULT VALUES;
		CREATE FUNCTION `+pgx.Identifier{maliciousSchema, "enable_phase6_schema_epoch"}.Sanitize()+`() RETURNS void LANGUAGE sql AS
		'UPDATE `+pgx.Identifier{maliciousSchema, "phase6_hijack"}.Sanitize()+` SET called=true'`); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET search_path=`+pgx.Identifier{maliciousSchema}.Sanitize()+`,`+pgx.Identifier{targetSchema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := New(conn).EnablePhase6SchemaEpoch(ctx); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("default public schema error = %v, want ErrFailedPrecondition", err)
	}
	var hijacked, maliciousEnabled bool
	if err := conn.QueryRow(ctx, `SELECT hijack.called,epoch.phase6_enabled FROM `+pgx.Identifier{maliciousSchema, "phase6_hijack"}.Sanitize()+` hijack,`+pgx.Identifier{maliciousSchema, "schema_epoch"}.Sanitize()+` epoch WHERE epoch.singleton`).Scan(&hijacked, &maliciousEnabled); err != nil || hijacked || maliciousEnabled {
		t.Fatalf("malicious schema changed by default store = called:%t enabled:%t, %v", hijacked, maliciousEnabled, err)
	}
	wrongStore, err := NewWithSchema(conn, "wrong_phase6_schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongStore.EnablePhase6SchemaEpoch(ctx); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("wrong configured schema error = %v, want ErrFailedPrecondition", err)
	}
	var enabled bool
	if err := conn.QueryRow(ctx, `SELECT phase6_enabled FROM `+pgx.Identifier{targetSchema, "schema_epoch"}.Sanitize()+` WHERE singleton`).Scan(&enabled); err != nil || enabled {
		t.Fatalf("target schema changed after wrong-schema call = %t, %v", enabled, err)
	}
	configuredStore, err := NewWithSchema(conn, targetSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := configuredStore.EnablePhase6SchemaEpoch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT phase6_enabled FROM `+pgx.Identifier{targetSchema, "schema_epoch"}.Sanitize()+` WHERE singleton`).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("real schema epoch enabled = %t, %v", enabled, err)
	}
	if err := conn.QueryRow(ctx, `SELECT hijack.called,epoch.phase6_enabled FROM `+pgx.Identifier{maliciousSchema, "phase6_hijack"}.Sanitize()+` hijack,`+pgx.Identifier{maliciousSchema, "schema_epoch"}.Sanitize()+` epoch WHERE epoch.singleton`).Scan(&hijacked, &maliciousEnabled); err != nil || hijacked || maliciousEnabled {
		t.Fatalf("malicious schema changed = called:%t enabled:%t, %v", hijacked, maliciousEnabled, err)
	}
}

func TestMigration000015LegacyResolvedJobIntegration(t *testing.T) {
	_, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	jobID := "00000000-0000-0000-0000-000000000148"
	if _, err := pool.Exec(ctx, `INSERT INTO orchestration_jobs(id,org_id,app_id,environment_id,github_repository_id,
		github_workflow_path,workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key,status,
		oci_resolution_state,image_tag_ref,resolved_image_digest,immutable_image_ref,oci_media_type,oci_resolved_at,deployment_state)
		SELECT $1,$2,$3,$4,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('a',40),'refs/heads/main',repeat('b',40),1,
		'legacy-resolved','deploying','resolved','ghcr.io/acme/app-one:'||repeat('b',40),'sha256:'||repeat('c',64),
		'ghcr.io/acme/app-one@sha256:'||repeat('c',64),'application/vnd.oci.image.manifest.v1+json',now(),'ready'
		FROM git_connections connection WHERE connection.id=$5`, jobID, orgOneID, appOneID, environmentOneID, repositoryOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET oci_annotation_status='legacy' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT enable_phase6_schema_epoch()`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET updated_at=now() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("untouched legacy evidence update: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET oci_attempts=1 WHERE id=$1`, jobID); err == nil {
		t.Fatal("legacy OCI attempts mutation succeeded after enable")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO orchestration_jobs(org_id,app_id,environment_id,github_repository_id,github_workflow_path,
		workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key,oci_annotation_status)
		SELECT $1,$2,$3,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('a',40),'refs/heads/main',repeat('b',40),1,
		'new-legacy','legacy' FROM git_connections connection WHERE connection.id=$4`, orgOneID, appOneID, environmentOneID, repositoryOneID); err == nil {
		t.Fatal("new legacy annotation status succeeded after enable")
	}
}

func TestEnablePhase6SchemaEpochRejectsWithoutPartialConversion(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	ctx := t.Context()
	compatibleID := "00000000-0000-0000-0000-000000000143"
	invalidID := "00000000-0000-0000-0000-000000000144"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host,state,checkpoint)
		VALUES($1,$3,'ghcr','ghcr.io','failed','secret_written'),($2,$3,'ghcr','ghcr.io','failed','verified')`,
		compatibleID, invalidID, orgOneID); err == nil {
		t.Fatal("failed registries without failure codes unexpectedly inserted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host,state,checkpoint,failure_code)
		VALUES($1,$3,'ghcr','ghcr.io','failed','secret_written','postgres_permanent'),
		($2,$4,'ghcr','ghcr.io','failed','verified','legacy_failure')`, compatibleID, invalidID, orgOneID, orgTwoID); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.EnablePhase6SchemaEpoch(ctx); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("enable error = %v, want ErrFailedPrecondition", err)
	}
	var enabled bool
	var first, second string
	if err := pool.QueryRow(ctx, `SELECT epoch.phase6_enabled,
		(SELECT checkpoint FROM registries WHERE id=$1),(SELECT checkpoint FROM registries WHERE id=$2)
		FROM schema_epoch epoch WHERE singleton`, compatibleID, invalidID).Scan(&enabled, &first, &second); err != nil {
		t.Fatal(err)
	}
	if enabled || first != "secret_written" || second != "verified" {
		t.Fatalf("failed enable partially changed state: enabled=%t checkpoints=%q/%q", enabled, first, second)
	}
}

func TestSchemaV15OperationGenerationIdentityIntegration(t *testing.T) {
	_, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedSchemaV15Actor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000145"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	insertPair := func(operationID string, candidate, generation int64) error {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,checkpoint,candidate_generation,idempotency_key,
			credential_fingerprint,actor_user_id) VALUES($1,$2,$3,'create','reserved',$4,$5,decode(repeat('a',64),'hex'),$6)`, operationID, orgOneID, registryID, candidate, operationID, schemaV15ActorID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id)
			VALUES($1,$2,$3,$5,'pending',$4)`, orgOneID, registryID, generation, operationID,
			fmt.Sprintf("k8s-secret://cyclopus-system/g-%d#dockerconfigjson", generation)); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := insertPair("00000000-0000-0000-0000-000000000146", 1, 1); err != nil {
		t.Fatalf("valid deferred pair: %v", err)
	}
	if err := insertPair("00000000-0000-0000-0000-000000000147", 2, 3); err == nil {
		t.Fatal("mismatched operation generation committed")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id)
		VALUES($1,$2,4,'k8s-secret://cyclopus-system/g-4#dockerconfigjson','pending',$3)`, orgOneID, registryID,
		"00000000-0000-0000-0000-000000000146"); err == nil {
		t.Fatal("additional generation for operation committed")
	}
}

func TestSchemaV15DeferredGenerationRotationAndEnvironmentDependenciesIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000149"
	operationOne := "00000000-0000-0000-0000-00000000014a"
	operationTwo := "00000000-0000-0000-0000-00000000014b"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host,state,checkpoint,active_generation,active_credential_secret_ref,activated_at)
		VALUES($1,$2,'ghcr','ghcr.io','ready','activated',1,'k8s-secret://cyclopus-system/g1#dockerconfigjson',now())`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,candidate_generation,idempotency_key,terminal_at)
		VALUES($1,$2,$3,'create','succeeded','activated',1,'g1',now())`, operationOne, orgOneID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id,validated_at,activated_at)
		VALUES($1,$2,1,'k8s-secret://cyclopus-system/g1#dockerconfigjson','active',$3,now(),now())`, orgOneID, registryID, operationOne); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE apps SET registry_id=$2,image_repository='acme/private',image_visibility='private',github_workflow_path=NULL,production_branch=NULL WHERE id=$1`, appOneID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET required_registry_credential_generation=1,ready_registry_credential_generation=1,
		registry_credential_state='ready',registry_credential_checkpoint='namespace_secret_verified' WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.EnablePhase6SchemaEpoch(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,candidate_generation,previous_generation,idempotency_key,terminal_at)
		VALUES($1,$2,$3,'rotate','succeeded','activated',2,1,'g2',now())`, operationTwo, orgOneID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE registry_credential_generations SET state='retired',retired_at=now() WHERE registry_id=$1 AND generation=1`, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id,validated_at,activated_at)
		VALUES($1,$2,2,'k8s-secret://cyclopus-system/g2#dockerconfigjson','active',$3,now(),now())`, orgOneID, registryID, operationTwo); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE environments SET required_registry_credential_generation=2,ready_registry_credential_generation=2,
		registry_credential_checkpoint='activated' WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE registries SET active_generation=2,active_credential_secret_ref='k8s-secret://cyclopus-system/g2#dockerconfigjson' WHERE id=$1`, registryID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatalf("valid deferred rotation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE apps SET image_visibility='public',registry_id=NULL WHERE id=$1`, appOneID); err == nil {
		t.Fatal("app mutation invalidating live environment succeeded")
	}
	if _, err := pool.Exec(ctx, `UPDATE registries SET active_generation=1,active_credential_secret_ref='k8s-secret://cyclopus-system/g1#dockerconfigjson' WHERE id=$1`, registryID); err == nil {
		t.Fatal("registry pointer to retired generation succeeded")
	}
}

func TestSchemaV15RoundTripIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedTenant(t, pool, tenantTwo())
	seedSchemaV15Actor(t, pool)
	ctx := t.Context()
	now := time.Date(2026, 8, 19, 12, 34, 56, 123456000, time.UTC)
	registryID := "00000000-0000-0000-0000-000000000151"
	operationID := "00000000-0000-0000-0000-000000000152"
	secretRef := "k8s-secret://cyclopus-system/ghcr-1#dockerconfigjson"
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host,state,checkpoint,
		active_generation,active_credential_secret_ref,pending_generation,pending_credential_secret_ref,
		previous_generation,previous_credential_secret_ref,reconciliation_generation,operation_id,
		lease_owner,lease_epoch,lease_expires_at,attempts,next_attempt_at,activated_at,failure_code,failure_message,
		created_at,updated_at) VALUES($1,$2,'ghcr','ghcr.io','failed','cleanup_pending',3,$3,4,$4,2,$5,9,$6,
		'registry-worker',7,$7,5,$8,$9,'postgres_permanent','registry failure',$10,$11)`, registryID, orgOneID,
		secretRef, "k8s-secret://cyclopus-system/ghcr-2#dockerconfigjson",
		"k8s-secret://cyclopus-system/ghcr-0#dockerconfigjson", operationID, now.Add(time.Hour), now.Add(-time.Hour),
		now.Add(-2*time.Hour), now.Add(-3*time.Hour), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,
			candidate_generation,previous_generation,target_repository,idempotency_key,lease_owner,lease_epoch,
			lease_expires_at,attempts,next_attempt_at,failure_code,failure_message,created_at,updated_at,terminal_at)
			VALUES($1,$2,$3,'rotate','failed','cleanup_pending',4,3,'acme/private','operation-key',NULL,11,NULL,6,$4,
			'postgres_permanent','operation failure',$5,$6,$7)`, operationID, orgOneID, registryID, now.Add(-time.Minute),
		now.Add(-4*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_operations(id,org_id,registry_id,kind,status,checkpoint,
		candidate_generation,idempotency_key,failure_code,terminal_at) VALUES
		('00000000-0000-0000-0000-000000000157',$1,$2,'create','failed','targets_validated',2,'generation-2','postgres_permanent',$3),
		('00000000-0000-0000-0000-000000000158',$1,$2,'create','failed','targets_validated',3,'generation-3','postgres_permanent',$3)`,
		orgOneID, registryID, now); err != nil {
		t.Fatal(err)
	}
	for generation, state := range map[int]string{2: "garbage_collected", 3: "active", 4: "failed"} {
		validated, activated, retired, collected := any(now.Add(-6*time.Hour)), any(now.Add(-5*time.Hour)), any(nil), any(nil)
		if generation >= 3 && state == "retired" {
			retired = now.Add(-3 * time.Hour)
		}
		if generation == 2 {
			retired, collected = now.Add(-4*time.Hour), now.Add(-2*time.Hour)
		}
		if generation == 4 {
			validated, activated, retired = nil, nil, nil
		}
		generationOperationID := operationID
		if generation == 2 {
			generationOperationID = "00000000-0000-0000-0000-000000000157"
		} else if generation == 3 {
			generationOperationID = "00000000-0000-0000-0000-000000000158"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,
				operation_id,created_at,validated_at,activated_at,retired_at,garbage_collected_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, orgOneID, registryID, generation,
			fmt.Sprintf("k8s-secret://cyclopus-system/ghcr-%d#dockerconfigjson", generation), state, generationOperationID,
			now.Add(-7*time.Hour), validated, activated, retired, collected); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registry_credential_validation_targets(operation_id,org_id,registry_id,
		repository,reference,status,attempts,next_attempt_at,failure_code,validated_at)
		VALUES($1,$2,$3,'acme/private','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','validated',4,$4,NULL,$5),($1,$2,$3,'acme/null','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','failed',0,$4,'registry_auth_failed',NULL)`,
		operationID, orgOneID, registryID, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE apps SET registry_id=$2,image_repository='acme/private',image_visibility='private',
		github_workflow_path=NULL,production_branch=NULL,desired_generation=17 WHERE id=$1`,
		appOneID, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET required_registry_credential_generation=3,ready_registry_credential_generation=3,
		registry_credential_state='ready',registry_credential_checkpoint='namespace_secret_verified',
		registry_credential_attempts=3,registry_credential_next_attempt_at=$2,registry_credential_lease_epoch=8,
		desired_generation=17 WHERE id=$1`,
		environmentOneID, now); err != nil {
		t.Fatal(err)
	}
	jobID := "00000000-0000-0000-0000-000000000153"
	if _, err := pool.Exec(ctx, `INSERT INTO orchestration_jobs(id,org_id,app_id,environment_id,github_repository_id,
		github_workflow_path,workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key,status,
		oci_resolution_state,oci_lease_owner,oci_lease_epoch,oci_lease_expires_at,oci_attempts,oci_next_attempt_at,
		oci_deadline_at,oci_error_code,resolved_registry_id,resolved_registry_generation,oci_config_digest,oci_config_media_type,oci_source,
		oci_revision,oci_annotation_status,created_at,updated_at)
		SELECT $1,$2,$3,$4,connection.github_repository_id,'.github/workflows/deploy.yml',$5,'refs/heads/main',$6,17,
		'oci-roundtrip','resolving_image','polling','oci-worker',13,$7,9,$8,$9,'registry_unavailable',$14,3,
		'sha256:'||repeat('a',64),'application/vnd.oci.image.config.v1+json','https://github.com/acme/private',$10,
		'mismatched',$11,$12 FROM git_connections connection WHERE connection.id=$13`, jobID, orgOneID, appOneID,
		environmentOneID, commitSHA('b'), commitSHA('c'), now.Add(time.Minute), now.Add(-time.Minute), now.Add(time.Hour),
		commitSHA('d'), now.Add(-2*time.Hour), now.Add(-time.Hour), repositoryOneID, registryID); err != nil {
		t.Fatal(err)
	}
	releaseID := "00000000-0000-0000-0000-000000000154"
	if _, err := pool.Exec(ctx, `INSERT INTO releases(id,org_id,environment_id,image_ref,git_commit_sha,kind,
		config_snapshot,config_hash,status,registry_credential_registry_id,registry_credential_generation,created_at)
		VALUES($1,$2,$3,'ghcr.io/acme/private@sha256:'||repeat('e',64),$4,'deploy',
		'{"version":"2","desired_state":{"version":"1","deployment":{},"service":{},"ingress":{},"scaling":{}}}',
		repeat('f',64),'live',$6,3,$5)`,
		releaseID, orgOneID, environmentOneID, commitSHA('e'), now, registryID); err != nil {
		t.Fatal(err)
	}

	registry, err := dataStore.GetRegistry(ctx, orgOneID, registryID)
	if err != nil || registry.PreviousGeneration == nil || *registry.PreviousGeneration != 2 || !registry.HasPreviousCredential ||
		registry.OperationID == nil || registry.LeaseOwner == nil || registry.LeaseEpoch != 7 || registry.LeaseExpiresAt == nil ||
		registry.Attempts != 5 || !registry.NextAttemptAt.Equal(now.Add(-time.Hour)) || registry.ActivatedAt == nil {
		t.Fatalf("registry round trip = %+v, %v", registry, err)
	}
	defaultRegistryID := "00000000-0000-0000-0000-000000000155"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, defaultRegistryID, orgTwoID); err != nil {
		t.Fatal(err)
	}
	defaultRegistry, err := dataStore.GetRegistry(ctx, orgTwoID, defaultRegistryID)
	if err != nil || defaultRegistry.PendingGeneration != nil || defaultRegistry.PreviousGeneration != nil ||
		defaultRegistry.OperationID != nil || defaultRegistry.LeaseOwner != nil || defaultRegistry.LeaseExpiresAt != nil ||
		defaultRegistry.ActivatedAt != nil || defaultRegistry.FailureCode != nil || defaultRegistry.FailureMessage != nil ||
		defaultRegistry.LeaseEpoch != 0 || defaultRegistry.Attempts != 0 {
		t.Fatalf("default registry round trip = %+v, %v", defaultRegistry, err)
	}
	operation, err := dataStore.GetRegistryCredentialOperation(ctx, orgOneID, operationID)
	if err != nil || operation.PreviousGeneration == nil || operation.TargetRepository == nil || operation.Lease != nil ||
		operation.FailureCode == nil || operation.FailureMessage == nil || operation.TerminalAt == nil || operation.Attempts != 6 {
		t.Fatalf("operation round trip = %+v, %v", operation, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE registry_credential_operations SET status='leased',lease_owner='operation-worker',
		lease_epoch=12,lease_expires_at=$2,failure_code=NULL,failure_message=NULL,terminal_at=NULL,
		credential_fingerprint=decode(repeat('a',64),'hex'),actor_user_id=$3 WHERE id=$1`, operationID, now.Add(time.Hour), schemaV15ActorID); err != nil {
		t.Fatal(err)
	}
	leasedOperation, err := dataStore.GetRegistryCredentialOperation(ctx, orgOneID, operationID)
	if err != nil || leasedOperation.Lease == nil || leasedOperation.Lease.Owner != "operation-worker" || leasedOperation.Lease.Epoch != 12 || !leasedOperation.Lease.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("leased operation round trip = %+v, %v", leasedOperation, err)
	}
	generation, err := dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, 2)
	if err != nil || generation.ValidatedAt == nil || generation.ActivatedAt == nil || generation.RetiredAt == nil || generation.GarbageCollectedAt == nil {
		t.Fatalf("generation round trip = %+v, %v", generation, err)
	}
	nullGeneration, err := dataStore.GetRegistryCredentialGeneration(ctx, orgOneID, registryID, 4)
	if err != nil || nullGeneration.ValidatedAt != nil || nullGeneration.ActivatedAt != nil || nullGeneration.RetiredAt != nil || nullGeneration.GarbageCollectedAt != nil {
		t.Fatalf("null generation round trip = %+v, %v", nullGeneration, err)
	}
	target, err := dataStore.GetRegistryCredentialValidationTarget(ctx, orgOneID, operationID, "acme/private", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || target.ValidatedAt == nil || target.FailureCode != nil || target.Attempts != 4 {
		t.Fatalf("validation target round trip = %+v, %v", target, err)
	}
	nullTarget, err := dataStore.GetRegistryCredentialValidationTarget(ctx, orgOneID, operationID, "acme/null", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || nullTarget.ValidatedAt != nil || nullTarget.FailureCode == nil || *nullTarget.FailureCode != "registry_auth_failed" {
		t.Fatalf("null validation target round trip = %+v, %v", nullTarget, err)
	}
	namespace, err := dataStore.GetNamespaceCredentialReconciliation(ctx, orgOneID, environmentOneID)
	if err != nil || namespace.RequiredGeneration == nil || namespace.ReadyGeneration == nil || namespace.Lease != nil ||
		namespace.State != "ready" || namespace.Checkpoint != "namespace_secret_verified" || namespace.Attempts != 3 {
		t.Fatalf("namespace round trip = %+v, %v", namespace, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET registry_credential_state='pending',ready_registry_credential_generation=NULL,
		registry_credential_checkpoint=NULL,registry_credential_lease_owner='namespace-worker',registry_credential_lease_epoch=9,
		registry_credential_lease_expires_at=$2 WHERE id=$1`, environmentOneID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	leasedNamespace, err := dataStore.GetNamespaceCredentialReconciliation(ctx, orgOneID, environmentOneID)
	if err != nil || leasedNamespace.Lease == nil || leasedNamespace.Lease.Owner != "namespace-worker" || leasedNamespace.Checkpoint != "" {
		t.Fatalf("leased namespace round trip = %+v, %v", leasedNamespace, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET registry_credential_state='ready',ready_registry_credential_generation=3,
		registry_credential_checkpoint='namespace_secret_verified',registry_credential_lease_owner=NULL,
		registry_credential_lease_expires_at=NULL,registry_credential_lease_epoch=8 WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	job, err := dataStore.GetOrchestrationJob(ctx, orgOneID, jobID)
	if err != nil || job.OCI.Lease == nil || job.OCI.Lease.Owner != "oci-worker" || job.OCI.Lease.Epoch != 13 ||
		job.OCI.ConfigDigest == nil || job.OCI.ConfigMediaType == nil || job.OCI.Source == nil || job.OCI.Revision == nil ||
		job.OCI.ResolvedRegistryGeneration == nil || job.OCI.Deadline == nil || job.OCI.ErrorCode == nil ||
		job.OCI.Attempts != 9 || job.OCI.AnnotationStatus != "mismatched" {
		t.Fatalf("orchestration OCI round trip = %+v, %v", job, err)
	}
	claim, err := dataStore.GetImageResolutionClaim(ctx, orgOneID, jobID)
	if err != nil || claim.Registry == nil || claim.Registry.ID != registryID || claim.Lease.Owner != "oci-worker" || !claim.Deadline.Equal(now.Add(time.Hour)) {
		t.Fatalf("image resolution claim round trip = %+v, %v", claim, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orchestration_jobs SET oci_lease_owner=NULL,oci_lease_expires_at=NULL,
		oci_config_digest=NULL,oci_config_media_type=NULL,oci_source=NULL,oci_revision=NULL,oci_annotation_status='pending',
		resolved_registry_id=NULL,resolved_registry_generation=NULL,oci_deadline_at=NULL,oci_error_code=NULL WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	nullJob, err := dataStore.GetOrchestrationJob(ctx, orgOneID, jobID)
	if err != nil || nullJob.OCI.Lease != nil || nullJob.OCI.ConfigDigest != nil || nullJob.OCI.ConfigMediaType != nil ||
		nullJob.OCI.Source != nil || nullJob.OCI.Revision != nil || nullJob.OCI.ResolvedRegistryGeneration != nil ||
		nullJob.OCI.Deadline != nil || nullJob.OCI.ErrorCode != nil || nullJob.OCI.AnnotationStatus != "pending" {
		t.Fatalf("null OCI round trip = %+v, %v", nullJob, err)
	}
	environment, err := dataStore.GetEnvironment(ctx, orgOneID, environmentOneID)
	if err != nil || environment.RequiredRegistryCredentialGeneration == nil || environment.ReadyRegistryCredentialGeneration == nil ||
		environment.RegistryCredentialState != "ready" || environment.RegistryCredentialCheckpoint != "namespace_secret_verified" ||
		environment.RegistryCredentialAttempts != 3 || environment.RegistryCredentialLeaseEpoch != 8 || environment.RegistryCredentialLeaseOwner != nil {
		t.Fatalf("environment round trip = %+v, %v", environment, err)
	}
	release, err := dataStore.GetRelease(ctx, orgOneID, releaseID)
	if err != nil || release.RegistryCredentialRegistryID == nil || *release.RegistryCredentialRegistryID != registryID || release.RegistryCredentialGeneration == nil || *release.RegistryCredentialGeneration != 3 {
		t.Fatalf("release round trip = %+v, %v", release, err)
	}
	nullReleaseID := "00000000-0000-0000-0000-000000000156"
	if _, err := pool.Exec(ctx, `INSERT INTO releases(id,org_id,environment_id,image_ref,kind,config_snapshot,config_hash,status)
		VALUES($1,$2,$3,'ghcr.io/acme/public@sha256:'||repeat('a',64),'deploy',$4,
		repeat('a',64),'pending')`, nullReleaseID, orgTwoID, environmentTwoID,
		testReleaseSnapshot("ghcr.io/acme/public@sha256:"+strings.Repeat("a", 64))); err != nil {
		t.Fatal(err)
	}
	nullRelease, err := dataStore.GetRelease(ctx, orgTwoID, nullReleaseID)
	if err != nil || nullRelease.RegistryCredentialGeneration != nil {
		t.Fatalf("null release round trip = %+v, %v", nullRelease, err)
	}
	privatePendingID := "00000000-0000-0000-0000-000000000159"
	privateLiveID := "00000000-0000-0000-0000-00000000015a"
	validSnapshot := testReleaseSnapshot("ghcr.io/acme/private@sha256:" + strings.Repeat("e", 64))
	if _, err := pool.Exec(ctx, `UPDATE releases SET status='failed' WHERE id=$1`, releaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE registries SET state='ready',checkpoint='activated',pending_generation=NULL,
		pending_credential_secret_ref=NULL,operation_id=NULL,failure_code=NULL,failure_message=NULL WHERE id=$1`, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO releases(id,org_id,environment_id,image_ref,kind,config_snapshot,config_hash,status,
		registry_credential_registry_id,registry_credential_generation) VALUES
		($1,$3,$4,'ghcr.io/acme/private@sha256:'||repeat('e',64),'deploy',$5,repeat('1',64),'live',$6,3),
		($2,$3,$4,'ghcr.io/acme/private@sha256:'||repeat('e',64),'deploy',$5,repeat('2',64),'pending',$6,3)`,
		privateLiveID, privatePendingID, orgOneID, environmentOneID, validSnapshot, registryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET in_progress_release_id=$1,lock_acquired_at=clock_timestamp() WHERE id=$2`,
		privatePendingID, environmentOneID); err != nil {
		t.Fatal(err)
	}
	privateClaim, err := dataStore.ClaimPendingDeploy(ctx, orgOneID, nil)
	if err != nil || privateClaim.FailedRelease.RegistryCredentialRegistryID == nil || *privateClaim.FailedRelease.RegistryCredentialRegistryID != registryID ||
		privateClaim.FailedRelease.RegistryCredentialGeneration == nil || *privateClaim.FailedRelease.RegistryCredentialGeneration != 3 ||
		privateClaim.PreviousRelease == nil || privateClaim.PreviousRelease.RegistryCredentialRegistryID == nil ||
		*privateClaim.PreviousRelease.RegistryCredentialRegistryID != registryID || privateClaim.PreviousRelease.RegistryCredentialGeneration == nil ||
		*privateClaim.PreviousRelease.RegistryCredentialGeneration != 3 {
		t.Fatalf("private deploy snapshots = %+v, %v", privateClaim, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET lock_acquired_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, environmentOneID); err != nil {
		t.Fatal(err)
	}
	privateRecovery, err := dataStore.ClaimExpiredDeployLease(ctx, orgOneID, environmentOneID)
	if err != nil || privateRecovery.FailedRelease.RegistryCredentialRegistryID == nil || *privateRecovery.FailedRelease.RegistryCredentialRegistryID != registryID ||
		privateRecovery.FailedRelease.RegistryCredentialGeneration == nil || *privateRecovery.FailedRelease.RegistryCredentialGeneration != 3 ||
		privateRecovery.PreviousRelease == nil || privateRecovery.PreviousRelease.RegistryCredentialRegistryID == nil ||
		*privateRecovery.PreviousRelease.RegistryCredentialRegistryID != registryID || privateRecovery.PreviousRelease.RegistryCredentialGeneration == nil ||
		*privateRecovery.PreviousRelease.RegistryCredentialGeneration != 3 {
		t.Fatalf("private recovery snapshots = %+v, %v", privateRecovery, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET in_progress_release_id=$1,lock_acquired_at=clock_timestamp() WHERE id=$2`, nullReleaseID, environmentTwoID); err != nil {
		t.Fatal(err)
	}
	publicClaim, err := dataStore.ClaimPendingDeploy(ctx, orgTwoID, nil)
	if err != nil || publicClaim.FailedRelease.RegistryCredentialRegistryID != nil || publicClaim.FailedRelease.RegistryCredentialGeneration != nil {
		t.Fatalf("public deploy snapshot = %+v, %v", publicClaim, err)
	}
	publicNamespace, err := dataStore.GetNamespaceCredentialReconciliation(ctx, orgTwoID, environmentTwoID)
	if err != nil || publicNamespace.RegistryID != nil || publicNamespace.State != "not_required" {
		t.Fatalf("public namespace round trip = %+v, %v", publicNamespace, err)
	}
	defaultEnvironment, err := dataStore.GetEnvironment(ctx, orgOneID, environmentOneID)
	if err != nil || defaultEnvironment.RegistryCredentialErrorCode != nil || defaultEnvironment.RegistryCredentialErrorMessage != nil || defaultEnvironment.RegistryCredentialLeaseExpiresAt != nil {
		t.Fatalf("environment nullable fields = %+v, %v", defaultEnvironment, err)
	}
}

func TestSchemaV15CandidatePlansIntegration(t *testing.T) {
	_, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedSchemaV15Actor(t, pool)
	ctx := t.Context()
	registryID := "00000000-0000-0000-0000-000000000161"
	if _, err := pool.Exec(ctx, `INSERT INTO registries(id,org_id,provider,registry_host) VALUES($1,$2,'ghcr','ghcr.io')`, registryID, orgOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO environments(id,org_id,app_id,branch,kind,preview_slug,namespace,planned_hostname,registry_credential_state,
		required_registry_credential_generation,registry_credential_attempts,registry_credential_next_attempt_at)
		SELECT uuidv7(),$1,$2,'plan-'||g,'preview','plan-'||g,'plan-'||g,'plan-'||g||'.example.test','pending',1,
		CASE WHEN g<=100 THEN 1 ELSE 12 END,
		CASE WHEN g%2=0 THEN clock_timestamp()-interval '1 second' ELSE clock_timestamp()+interval '1 day' END
		FROM generate_series(1,12000) g`, orgOneID, appOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET registry_credential_lease_owner='seed',registry_credential_lease_epoch=1,
		registry_credential_lease_expires_at=clock_timestamp()-interval '1 second' WHERE branch LIKE 'plan-%' AND right(branch,1) IN ('1','3','5','7','9')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO orchestration_jobs(org_id,app_id,environment_id,github_repository_id,
		github_workflow_path,workflow_revision,dispatch_ref,desired_checkout_sha,desired_generation,idempotency_key,status,
		oci_resolution_state,oci_attempts,oci_next_attempt_at,oci_lease_owner,oci_lease_epoch,oci_lease_expires_at)
		SELECT $1,$2,environment.id,connection.github_repository_id,'.github/workflows/deploy.yml',repeat('a',40),
		'refs/heads/main',repeat('b',40),1,'plan-'||environment.id,'resolving_image','polling',1,
		clock_timestamp()-interval '1 second',CASE WHEN row_number() OVER ()%2=0 THEN NULL ELSE 'seed' END,
		CASE WHEN row_number() OVER ()%2=0 THEN 0 ELSE 1 END,
		CASE WHEN row_number() OVER ()%2=0 THEN NULL ELSE clock_timestamp()-interval '1 second' END
		FROM environments environment,git_connections connection WHERE environment.org_id=$1 AND environment.branch LIKE 'plan-%'
		AND connection.id=$3`, orgOneID, appOneID, repositoryOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO orgs(id,name,slug)
		SELECT ('10000000-0000-0000-0000-'||lpad(g::text,12,'0'))::uuid,'Plan '||g,'plan-'||g
		FROM generate_series(1,12000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP INDEX users_singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,org_id,email,password_hash,display_name,operator)
		SELECT ('20000000-0000-0000-0000-'||lpad(g::text,12,'0'))::uuid,
		('10000000-0000-0000-0000-'||lpad(g::text,12,'0'))::uuid,'plan-'||g||'@example.test','hash','Plan Admin',true
		FROM generate_series(1,12000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO registries(org_id,provider,registry_host,state,checkpoint,attempts,next_attempt_at,
		lease_owner,lease_epoch,lease_expires_at)
		SELECT ('10000000-0000-0000-0000-'||lpad(g::text,12,'0'))::uuid,'ghcr','ghcr.io',
		CASE WHEN g%2=0 THEN 'creating' ELSE 'reconciling' END,CASE WHEN g%2=0 THEN 'source_secret_written' ELSE 'reconciling_namespaces' END,CASE WHEN g<=100 THEN 1 ELSE 12 END,
		clock_timestamp()-interval '1 second',CASE WHEN g%2=0 THEN NULL ELSE 'seed' END,
		CASE WHEN g%2=0 THEN 0 ELSE 1 END,CASE WHEN g%2=0 THEN NULL ELSE clock_timestamp()-interval '1 second' END
		FROM generate_series(1,12000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `WITH operations AS (
		INSERT INTO registry_credential_operations(org_id,registry_id,kind,status,checkpoint,candidate_generation,idempotency_key,
		attempts,next_attempt_at,lease_owner,lease_epoch,lease_expires_at,credential_fingerprint,actor_user_id)
		SELECT registry.org_id,registry.id,'create',CASE WHEN row_number() OVER ()%2=0 THEN 'pending' ELSE 'leased' END,'source_secret_written',1,
		'plan-'||registry.id,CASE WHEN row_number() OVER ()<=100 THEN 1 ELSE 12 END,clock_timestamp()-interval '1 second',
		CASE WHEN row_number() OVER ()%2=0 THEN NULL ELSE 'seed' END,
		CASE WHEN row_number() OVER ()%2=0 THEN 0 ELSE 1 END,
		CASE WHEN row_number() OVER ()%2=0 THEN NULL ELSE clock_timestamp()-interval '1 second' END,
		decode(repeat('a',64),'hex'),('20000000-0000-0000-0000-'||right(registry.org_id::text,12))::uuid
		FROM registries registry WHERE registry.org_id<>$1 RETURNING id,org_id,registry_id)
		INSERT INTO registry_credential_generations(org_id,registry_id,generation,secret_ref,state,operation_id)
		SELECT org_id,registry_id,1,'k8s-secret://cyclopus-system/g-'||replace(registry_id::text,'-','')||'#dockerconfigjson',
		'failed',id FROM operations`, orgOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO registry_credential_validation_targets(operation_id,org_id,registry_id,repository,
		reference,status,attempts,next_attempt_at) SELECT id,org_id,registry_id,'acme/repo','sha256:'||replace(id::text,'-','')||'00000000000000000000000000000000','pending',attempts,clock_timestamp()-interval '1 second'
		FROM registry_credential_operations`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE registry_credential_generations SET state='retired',validated_at=created_at,
		activated_at=created_at,retired_at=created_at WHERE registry_id<>$1`, registryID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"registries", "registry_credential_operations", "registry_credential_generations", "registry_credential_validation_targets", "environments", "orchestration_jobs"} {
		if _, err := pool.Exec(ctx, "ANALYZE "+table); err != nil {
			t.Fatal(err)
		}
	}
	queries := []struct{ name, index, table, query string }{
		{"registry due", "registries_operation_due", "registries", registryDueCandidateQuery},
		{"registry expired", "registries_operation_leased", "registries", registryExpiredCandidateQuery},
		{"OCI due", "orchestration_jobs_oci_due", "orchestration_jobs", ociDueCandidateQuery},
		{"OCI expired", "orchestration_jobs_oci_leased", "orchestration_jobs", ociExpiredCandidateQuery},
		{"operation due", "registry_credential_operations_due", "registry_credential_operations", registryOperationDueCandidateQuery},
		{"operation expired", "registry_credential_operations_leased", "registry_credential_operations", registryOperationExpiredCandidateQuery},
		{"validation due", "registry_credential_validation_targets_due", "registry_credential_validation_targets", registryValidationTargetDueCandidateQuery},
		{"namespace due", "environments_registry_credential_due", "environments", namespaceCredentialDueCandidateQuery},
		{"namespace expired", "environments_registry_credential_leased", "environments", namespaceCredentialExpiredCandidateQuery},
		{"generation GC", "registry_credential_generations_cleanup", "registry_credential_generations", registryGenerationGCCandidateQuery},
	}
	for _, item := range queries {
		query := item.query
		if item.name == "generation GC" {
			query = strings.ReplaceAll(query, "$1", "clock_timestamp()")
			query = strings.ReplaceAll(query, "$2", "100")
		} else {
			query = strings.ReplaceAll(query, "$1", "100")
		}
		assertTargetPlanUsesIndex(t, pool, item.name, item.index, item.table, query)
	}
}

func assertTargetPlanUsesIndex(t *testing.T, pool *pgxpool.Pool, name, index, table, query string) {
	t.Helper()
	rows, err := pool.Query(t.Context(), "EXPLAIN (COSTS OFF) "+query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	if !strings.Contains(plan, index) || strings.Contains(plan, "Seq Scan on "+table) {
		t.Fatalf("%s plan did not use %s without target-table Seq Scan:\n%s", name, index, plan)
	}
}

func TestSchemaV15QueryShapesAreStable(t *testing.T) {
	queries := []string{registryDueCandidateQuery, registryExpiredCandidateQuery, ociDueCandidateQuery,
		ociExpiredCandidateQuery, registryOperationDueCandidateQuery, registryOperationExpiredCandidateQuery,
		registryValidationTargetDueCandidateQuery, namespaceCredentialDueCandidateQuery,
		namespaceCredentialExpiredCandidateQuery, registryGenerationGCCandidateQuery}
	for _, query := range queries {
		if !strings.Contains(query, "ORDER BY") || !strings.Contains(query, "LIMIT") {
			t.Fatalf("unbounded candidate query: %s", query)
		}
	}
}
