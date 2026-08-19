DO $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM registry_credential_validation_targets target
        JOIN registry_credential_operations operation ON operation.id=target.operation_id
        WHERE operation.status NOT IN ('succeeded','failed','cancelled')
    ) THEN
        RAISE EXCEPTION '000017 cannot migrate nonterminal legacy validation targets; finish or cancel their operations first'
            USING ERRCODE='check_violation';
    END IF;
END $$;

ALTER TABLE registries
    DROP CONSTRAINT registries_check1,
    ADD CONSTRAINT registries_pending_credential_pair CHECK (
        (pending_generation IS NULL AND pending_credential_secret_ref IS NULL) OR pending_generation IS NOT NULL),
    DROP CONSTRAINT registries_state_check,
    ADD CONSTRAINT registries_state_check CHECK (state IN ('absent','provisioning','creating','ready','rotating','reconciling','failed','deleting')),
    DROP CONSTRAINT registries_checkpoint_check,
    ADD CONSTRAINT registries_checkpoint_check CHECK (checkpoint IS NULL OR checkpoint IN (
        'reserved','pending_recorded','source_secret_write_started','source_secret_written','targets_validated',
        'validation_complete','reconciling_namespaces','namespaces_reconciling','namespaces_ready','activated',
        'rollback_reconciling','cleanup_pending','secret_write_started','secret_written','verified'));

ALTER TABLE registry_credential_operations
    ADD COLUMN target_reference text,
    ADD CONSTRAINT registry_credential_operations_target_reference_check CHECK (
        target_reference IS NULL OR target_reference ~ '^[0-9a-f]{40}$' OR target_reference ~ '^sha256:[0-9a-f]{64}$'),
    DROP CONSTRAINT registry_credential_operations_status_check,
    ADD CONSTRAINT registry_credential_operations_status_check CHECK (status IN ('awaiting_source','pending','leased','succeeded','failed','cancelled')),
    DROP CONSTRAINT registry_credential_operations_checkpoint_check,
    ADD CONSTRAINT registry_credential_operations_checkpoint_check CHECK (checkpoint IN (
        'reserved','awaiting_source','source_secret_write_started','source_secret_written','targets_validated',
        'validation_complete','reconciling_namespaces','namespaces_reconciling','namespaces_ready','activated',
        'rollback_reconciling','cleanup_pending'));

ALTER TABLE registry_credential_generations
    ALTER COLUMN secret_ref DROP NOT NULL,
    DROP CONSTRAINT registry_credential_generations_secret_ref_check,
    DROP CONSTRAINT registry_credential_generations_state_check,
    ADD CONSTRAINT registry_credential_generations_state_check CHECK (state IN ('reserved','pending','active','retired','failed','garbage_collecting','garbage_collected')),
    ADD CONSTRAINT registry_credential_generations_secret_ref_check CHECK (
        (state='reserved' AND secret_ref IS NULL) OR
        (state='failed') OR
        (state NOT IN ('reserved','failed') AND secret_ref IS NOT NULL));

DROP INDEX registry_credential_operations_active;
CREATE UNIQUE INDEX registry_credential_operations_active
    ON registry_credential_operations(org_id,registry_id) WHERE status IN ('awaiting_source','pending','leased');
CREATE INDEX registry_credential_operations_awaiting_source
    ON registry_credential_operations(created_at,id) WHERE status='awaiting_source';

CREATE OR REPLACE FUNCTION protect_registry_credential_generation_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.org_id IS DISTINCT FROM OLD.org_id
       OR NEW.registry_id IS DISTINCT FROM OLD.registry_id
       OR NEW.generation IS DISTINCT FROM OLD.generation
       OR (OLD.secret_ref IS NOT NULL AND NEW.secret_ref IS DISTINCT FROM OLD.secret_ref)
       OR (OLD.secret_ref IS NULL AND NEW.secret_ref IS NOT NULL AND OLD.state<>'reserved')
       OR (OLD.secret_ref IS NULL AND NEW.secret_ref IS NULL AND NEW.state NOT IN ('reserved','failed'))
       OR NEW.operation_id IS DISTINCT FROM OLD.operation_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'registry credential generation identity is append-only'
            USING ERRCODE='integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

ALTER TABLE registry_credential_validation_targets
    ADD COLUMN reference text;

UPDATE registry_credential_validation_targets
SET reference='legacy';

ALTER TABLE registry_credential_validation_targets
    ALTER COLUMN reference SET NOT NULL,
    ADD COLUMN lease_owner text,
    ADD COLUMN lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    ADD COLUMN lease_expires_at timestamptz,
    DROP CONSTRAINT registry_credential_validation_targets_status_check,
    ADD CONSTRAINT registry_credential_validation_targets_status_check CHECK (status IN ('pending','validated','failed','skipped')),
    ADD CONSTRAINT registry_credential_validation_targets_reference_check CHECK (
        reference ~ '^[0-9a-f]{40}$' OR reference ~ '^sha256:[0-9a-f]{64}$' OR
        (reference='legacy' AND status IN ('validated','failed','skipped'))),
    ADD CONSTRAINT registry_credential_validation_targets_lease_owner_check CHECK (
        lease_owner IS NULL OR (btrim(lease_owner) <> '' AND octet_length(lease_owner) <= 255)),
    ADD CONSTRAINT registry_credential_validation_targets_lease_pair CHECK (
        (lease_owner IS NULL) = (lease_expires_at IS NULL)),
    ADD CONSTRAINT registry_credential_validation_targets_lease_state CHECK (
        lease_owner IS NULL OR status='pending'),
    DROP CONSTRAINT registry_credential_validation_targets_pkey,
    ADD PRIMARY KEY (operation_id,repository,reference);

DROP INDEX registry_credential_validation_targets_due;
CREATE INDEX registry_credential_validation_targets_due
    ON registry_credential_validation_targets(next_attempt_at,operation_id,repository,reference)
    WHERE status='pending' AND lease_owner IS NULL AND attempts<12;
CREATE INDEX registry_credential_validation_targets_leased
    ON registry_credential_validation_targets(lease_expires_at,operation_id,repository,reference)
    WHERE lease_owner IS NOT NULL;

CREATE FUNCTION enforce_registry_credential_validation_reference() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.reference='legacy' AND (TG_OP='INSERT' OR NEW.status NOT IN ('validated','failed','skipped')) THEN
        RAISE EXCEPTION 'legacy registry validation references are terminal history only'
            USING ERRCODE='check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER registry_credential_validation_reference
    BEFORE INSERT OR UPDATE OF reference,status ON registry_credential_validation_targets
    FOR EACH ROW EXECUTE FUNCTION enforce_registry_credential_validation_reference();

CREATE OR REPLACE FUNCTION phase6_registry_row_valid(value registries) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT value.reconciliation_generation <= value.active_generation
       AND (value.previous_generation IS NULL OR value.previous_generation<=value.active_generation)
       AND CASE value.state
        WHEN 'absent' THEN value.active_generation=0 AND
            ((value.checkpoint IS NULL AND value.pending_generation IS NULL AND value.operation_id IS NULL)
             OR (value.checkpoint='reserved' AND value.pending_generation IS NOT NULL
                 AND value.pending_credential_secret_ref IS NULL AND value.operation_id IS NOT NULL))
        WHEN 'provisioning' THEN value.checkpoint IN ('source_secret_written','targets_validated','validation_complete','reconciling_namespaces','namespaces_reconciling','namespaces_ready')
            AND value.active_generation=0 AND value.pending_generation IS NOT NULL AND value.pending_credential_secret_ref IS NOT NULL
        WHEN 'creating' THEN value.checkpoint IN ('pending_recorded','source_secret_write_started','source_secret_written','targets_validated','validation_complete','reconciling_namespaces','namespaces_reconciling','namespaces_ready')
            AND value.active_generation=0 AND value.pending_generation IS NOT NULL
        WHEN 'ready' THEN value.checkpoint IN ('activated','cleanup_pending','reserved')
            AND value.active_generation>0 AND value.active_credential_secret_ref IS NOT NULL
            AND ((value.checkpoint='reserved' AND value.pending_generation IS NOT NULL AND value.pending_credential_secret_ref IS NULL AND value.operation_id IS NOT NULL)
                 OR (value.checkpoint<>'reserved' AND value.pending_generation IS NULL))
        WHEN 'rotating' THEN value.checkpoint IN ('pending_recorded','source_secret_write_started','source_secret_written','targets_validated','validation_complete','reconciling_namespaces','namespaces_reconciling','namespaces_ready')
            AND value.pending_generation IS NOT NULL AND value.pending_credential_secret_ref IS NOT NULL
        WHEN 'reconciling' THEN value.checkpoint IN ('reconciling_namespaces','namespaces_reconciling','namespaces_ready','rollback_reconciling')
            AND value.active_generation>0
        WHEN 'failed' THEN value.failure_code IN ('registry_auth_failed','credential_validation_failed','credential_operation_exhausted','image_pull_secret_reconcile_failed','kubernetes_ownership_conflict','kubernetes_unavailable','postgres_permanent')
            AND value.active_generation=0 AND value.pending_generation IS NULL AND value.pending_credential_secret_ref IS NULL AND value.operation_id IS NULL
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
        IF registry_row.active_credential_secret_ref IS NOT NULL OR EXISTS (
            SELECT 1 FROM registry_credential_generations WHERE org_id=target_org AND registry_id=target_registry AND state='active') THEN
            RAISE EXCEPTION 'zero active registry generation has active credential state' USING ERRCODE='check_violation';
        END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM registry_credential_generations generation
        WHERE generation.org_id=target_org AND generation.registry_id=target_registry
          AND generation.generation=registry_row.active_generation AND generation.state='active'
          AND generation.secret_ref=registry_row.active_credential_secret_ref) THEN
        RAISE EXCEPTION 'registry active generation and credential row disagree' USING ERRCODE='check_violation';
    END IF;
    IF registry_row.pending_generation IS NULL THEN
        IF registry_row.pending_credential_secret_ref IS NOT NULL THEN
            RAISE EXCEPTION 'pending credential reference has no generation' USING ERRCODE='check_violation';
        END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM registry_credential_generations generation
        JOIN registry_credential_operations operation ON operation.id=generation.operation_id
         AND operation.org_id=generation.org_id AND operation.registry_id=generation.registry_id
         AND operation.candidate_generation=generation.generation
        WHERE generation.org_id=target_org AND generation.registry_id=target_registry
          AND generation.generation=registry_row.pending_generation
          AND ((generation.state='reserved' AND generation.secret_ref IS NULL AND registry_row.pending_credential_secret_ref IS NULL AND operation.status='awaiting_source')
               OR (generation.state='pending' AND generation.secret_ref=registry_row.pending_credential_secret_ref AND operation.status IN ('pending','leased')))
          AND operation.id=registry_row.operation_id) THEN
        RAISE EXCEPTION 'registry pending generation, operation, and credential row disagree' USING ERRCODE='check_violation';
    END IF;
END
$$;
