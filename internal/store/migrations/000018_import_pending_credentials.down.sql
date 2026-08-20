DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM registry_credential_operations
               WHERE kind='import_create' AND (status IN ('awaiting_import','pending','leased') OR checkpoint='validation_complete')) OR
       EXISTS (SELECT 1 FROM environments environment
               JOIN apps app ON app.org_id=environment.org_id AND app.id=environment.app_id
               JOIN registry_credential_generations generation
                 ON generation.org_id=app.org_id AND generation.registry_id=app.registry_id
                AND generation.generation=environment.required_registry_credential_generation
               WHERE environment.required_registry_credential_generation IS NOT NULL
                 AND generation.state IN ('reserved','pending','failed')) OR
       EXISTS (SELECT 1 FROM environments WHERE registry_credential_checkpoint='activated') THEN
        RAISE EXCEPTION '000018 downgrade requires nonterminal imports and non-active or activated credential bindings to be drained'
            USING ERRCODE='check_violation';
    END IF;
END $$;

DROP INDEX registry_credential_operations_awaiting_import;
DROP INDEX registry_credential_operations_active;
CREATE UNIQUE INDEX registry_credential_operations_active
    ON registry_credential_operations(org_id,registry_id) WHERE status IN ('awaiting_source','pending','leased');
ALTER TABLE registry_credential_operations
    DROP CONSTRAINT registry_credential_operations_status_check,
    ADD CONSTRAINT registry_credential_operations_status_check CHECK (status IN ('awaiting_source','pending','leased','succeeded','failed','cancelled'));

ALTER TABLE environments DROP CONSTRAINT environments_registry_credential_checkpoint_check;
ALTER TABLE environments ADD CONSTRAINT environments_registry_credential_checkpoint_check CHECK (
    registry_credential_checkpoint IS NULL OR registry_credential_checkpoint IN ('source_read','namespace_secret_written','namespace_secret_verified'));

CREATE OR REPLACE FUNCTION environment_credential_row_valid(value environments) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT CASE value.registry_credential_state
        WHEN 'not_required' THEN value.required_registry_credential_generation IS NULL
            AND value.ready_registry_credential_generation IS NULL
            AND value.registry_credential_checkpoint IS NULL
            AND value.registry_credential_error_code IS NULL
            AND value.registry_credential_error_message IS NULL
        WHEN 'pending' THEN value.required_registry_credential_generation IS NOT NULL
            AND value.ready_registry_credential_generation IS NULL
            AND (value.registry_credential_checkpoint IS NULL OR value.registry_credential_checkpoint IN ('source_read','namespace_secret_written'))
            AND value.registry_credential_error_code IS NULL
            AND value.registry_credential_error_message IS NULL
        WHEN 'ready' THEN value.required_registry_credential_generation IS NOT NULL
            AND value.ready_registry_credential_generation=value.required_registry_credential_generation
            AND value.registry_credential_checkpoint='namespace_secret_verified'
            AND value.registry_credential_error_code IS NULL
            AND value.registry_credential_error_message IS NULL
        WHEN 'failed' THEN value.required_registry_credential_generation IS NOT NULL
            AND (value.ready_registry_credential_generation IS NULL
                 OR value.ready_registry_credential_generation<=value.required_registry_credential_generation)
            AND value.registry_credential_error_code IN ('image_pull_secret_reconcile_failed','kubernetes_ownership_conflict','kubernetes_unavailable','postgres_permanent')
        ELSE false END
       AND CASE app.image_visibility
        WHEN 'public' THEN value.registry_credential_state='not_required'
        WHEN 'private' THEN app.deleted_at IS NULL AND registry.id IS NOT NULL
            AND value.required_registry_credential_generation>0
            AND (value.ready_registry_credential_generation IS NULL OR value.ready_registry_credential_generation<=value.required_registry_credential_generation)
            AND EXISTS (SELECT 1 FROM registry_credential_generations generation
                WHERE generation.org_id=registry.org_id AND generation.registry_id=registry.id
                  AND generation.generation=value.required_registry_credential_generation
                  AND generation.state IN ('active','retired'))
        ELSE false END
    FROM apps app
    LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
    WHERE app.org_id=value.org_id AND app.id=value.app_id
$$;

CREATE OR REPLACE FUNCTION assert_registry_generation_consistency(target_org uuid,target_registry uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE registry_row registries;
BEGIN
    SELECT * INTO registry_row FROM registries WHERE org_id=target_org AND id=target_registry;
    IF NOT FOUND THEN RETURN; END IF;
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
