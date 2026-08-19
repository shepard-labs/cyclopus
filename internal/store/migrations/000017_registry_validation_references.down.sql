DO $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM registry_credential_validation_targets
        GROUP BY operation_id,repository HAVING count(*)>1
    ) THEN
        RAISE EXCEPTION '000017 downgrade would collapse multiple validation references for one operation/repository'
            USING ERRCODE='check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM registry_credential_operations WHERE status='awaiting_source') OR
       EXISTS (SELECT 1 FROM registry_credential_generations WHERE state='reserved') OR
       EXISTS (SELECT 1 FROM registries WHERE state='provisioning' OR checkpoint IN ('reserved','validation_complete','reconciling_namespaces')) OR
       EXISTS (SELECT 1 FROM registry_credential_validation_targets WHERE status='skipped') THEN
        RAISE EXCEPTION '000017 downgrade requires all v17-only credential states to be drained'
            USING ERRCODE='check_violation';
    END IF;
END $$;

DROP TRIGGER registry_credential_validation_reference ON registry_credential_validation_targets;
DROP FUNCTION enforce_registry_credential_validation_reference();
DROP INDEX registry_credential_validation_targets_due;
DROP INDEX registry_credential_validation_targets_leased;

ALTER TABLE registry_credential_validation_targets
    DROP CONSTRAINT registry_credential_validation_targets_pkey,
    DROP CONSTRAINT registry_credential_validation_targets_reference_check,
    DROP CONSTRAINT registry_credential_validation_targets_lease_owner_check,
    DROP CONSTRAINT registry_credential_validation_targets_lease_pair,
    DROP CONSTRAINT registry_credential_validation_targets_lease_state,
    DROP CONSTRAINT registry_credential_validation_targets_status_check,
    ADD CONSTRAINT registry_credential_validation_targets_status_check CHECK (status IN ('pending','validated','failed')),
    DROP COLUMN lease_owner,
    DROP COLUMN lease_epoch,
    DROP COLUMN lease_expires_at,
    DROP COLUMN reference,
    ADD PRIMARY KEY (operation_id,repository);

CREATE INDEX registry_credential_validation_targets_due
    ON registry_credential_validation_targets(next_attempt_at,operation_id,repository)
    WHERE status='pending' AND attempts<12;

DROP INDEX registry_credential_operations_awaiting_source;
ALTER TABLE registry_credential_operations
    DROP CONSTRAINT registry_credential_operations_target_reference_check,
    DROP COLUMN target_reference,
    DROP CONSTRAINT registry_credential_operations_status_check,
    ADD CONSTRAINT registry_credential_operations_status_check CHECK (status IN ('pending','leased','succeeded','failed','cancelled')),
    DROP CONSTRAINT registry_credential_operations_checkpoint_check,
    ADD CONSTRAINT registry_credential_operations_checkpoint_check CHECK (checkpoint IN (
        'pending_recorded','source_secret_write_started','source_secret_written','targets_validated',
        'namespaces_reconciling','namespaces_ready','activated','rollback_reconciling','cleanup_pending'));

DROP INDEX registry_credential_operations_active;
CREATE UNIQUE INDEX registry_credential_operations_active
    ON registry_credential_operations(org_id,registry_id) WHERE status IN ('pending','leased');

ALTER TABLE registry_credential_generations
    DROP CONSTRAINT registry_credential_generations_secret_ref_check,
    DROP CONSTRAINT registry_credential_generations_state_check,
    ADD CONSTRAINT registry_credential_generations_state_check CHECK (state IN ('pending','active','retired','failed','garbage_collecting','garbage_collected')),
    ALTER COLUMN secret_ref SET NOT NULL;

CREATE OR REPLACE FUNCTION protect_registry_credential_generation_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.org_id IS DISTINCT FROM OLD.org_id
       OR NEW.registry_id IS DISTINCT FROM OLD.registry_id
       OR NEW.generation IS DISTINCT FROM OLD.generation
       OR NEW.secret_ref IS DISTINCT FROM OLD.secret_ref
       OR NEW.operation_id IS DISTINCT FROM OLD.operation_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'registry credential generation identity is append-only'
            USING ERRCODE='integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

ALTER TABLE registries
    DROP CONSTRAINT registries_pending_credential_pair,
    ADD CONSTRAINT registries_check1 CHECK ((pending_generation IS NULL) = (pending_credential_secret_ref IS NULL)),
    DROP CONSTRAINT registries_state_check,
    ADD CONSTRAINT registries_state_check CHECK (state IN ('absent','creating','ready','rotating','reconciling','failed','deleting')),
    DROP CONSTRAINT registries_checkpoint_check,
    ADD CONSTRAINT registries_checkpoint_check CHECK (checkpoint IS NULL OR checkpoint IN (
        'pending_recorded','source_secret_write_started','source_secret_written','targets_validated',
        'namespaces_reconciling','namespaces_ready','activated','rollback_reconciling','cleanup_pending',
        'secret_write_started','secret_written','verified'));

CREATE OR REPLACE FUNCTION phase6_registry_row_valid(value registries) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT value.reconciliation_generation <= value.active_generation
       AND (value.previous_generation IS NULL OR value.previous_generation<=value.active_generation)
       AND CASE value.state
        WHEN 'absent' THEN value.checkpoint IS NULL AND value.active_generation=0 AND value.pending_generation IS NULL
        WHEN 'creating' THEN value.checkpoint IN ('pending_recorded','source_secret_write_started','source_secret_written','targets_validated','namespaces_reconciling','namespaces_ready')
            AND value.active_generation=0 AND value.pending_generation IS NOT NULL
        WHEN 'ready' THEN value.checkpoint IN ('activated','cleanup_pending')
            AND value.active_generation>0 AND value.active_credential_secret_ref IS NOT NULL AND value.pending_generation IS NULL
        WHEN 'rotating' THEN value.checkpoint IN ('pending_recorded','source_secret_write_started','source_secret_written','targets_validated','namespaces_reconciling','namespaces_ready')
            AND value.pending_generation IS NOT NULL
        WHEN 'reconciling' THEN value.checkpoint IN ('namespaces_reconciling','namespaces_ready','rollback_reconciling') AND value.active_generation>0
        WHEN 'failed' THEN value.failure_code IN ('registry_auth_failed','credential_validation_failed','credential_operation_exhausted','image_pull_secret_reconcile_failed','kubernetes_ownership_conflict','kubernetes_unavailable','postgres_permanent')
        WHEN 'deleting' THEN value.checkpoint='cleanup_pending'
        ELSE false END
$$;

CREATE OR REPLACE FUNCTION assert_phase6_registry_generation_consistency(target_org uuid,target_registry uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE registry_row registries;
BEGIN
    SELECT * INTO registry_row FROM registries WHERE org_id=target_org AND id=target_registry;
    IF NOT FOUND OR NOT EXISTS (SELECT 1 FROM schema_epoch WHERE singleton AND phase6_enabled) THEN RETURN; END IF;
    IF registry_row.active_generation=0 THEN
        IF registry_row.active_credential_secret_ref IS NOT NULL OR EXISTS (SELECT 1 FROM registry_credential_generations WHERE org_id=target_org AND registry_id=target_registry AND state='active') THEN
            RAISE EXCEPTION 'zero active registry generation has active credential state' USING ERRCODE='check_violation';
        END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM registry_credential_generations generation WHERE generation.org_id=target_org AND generation.registry_id=target_registry AND generation.generation=registry_row.active_generation AND generation.state='active' AND generation.secret_ref=registry_row.active_credential_secret_ref) THEN
        RAISE EXCEPTION 'registry active generation and credential row disagree' USING ERRCODE='check_violation';
    END IF;
    IF registry_row.pending_generation IS NULL THEN
        IF registry_row.pending_credential_secret_ref IS NOT NULL THEN RAISE EXCEPTION 'pending credential reference has no generation' USING ERRCODE='check_violation'; END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM registry_credential_generations generation JOIN registry_credential_operations operation ON operation.id=generation.operation_id AND operation.org_id=generation.org_id AND operation.registry_id=generation.registry_id AND operation.candidate_generation=generation.generation WHERE generation.org_id=target_org AND generation.registry_id=target_registry AND generation.generation=registry_row.pending_generation AND generation.state='pending' AND generation.secret_ref=registry_row.pending_credential_secret_ref AND operation.status IN ('pending','leased') AND operation.id=registry_row.operation_id) THEN
        RAISE EXCEPTION 'registry pending generation, operation, and credential row disagree' USING ERRCODE='check_violation';
    END IF;
END
$$;
