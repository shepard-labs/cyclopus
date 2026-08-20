LOCK TABLE apps,environments IN SHARE ROW EXCLUSIVE MODE;
SELECT assert_namespace_credential_transitions_v20_down_safe();

DROP TRIGGER guard_namespace_credential_transitions_v20_down ON schema_migrations;
DROP FUNCTION guard_namespace_credential_transitions_v20_down();
DROP FUNCTION assert_namespace_credential_transitions_v20_down_safe();

DROP FUNCTION transition_app_image_visibility(uuid,uuid,text,uuid);
DROP FUNCTION converge_registry_credential_operation_without_namespaces(uuid,uuid);
DROP TRIGGER apps_phase6_environments ON apps;
CREATE TRIGGER apps_phase6_environments AFTER UPDATE OF image_visibility,registry_id,deleted_at ON apps
    FOR EACH ROW EXECUTE FUNCTION enforce_phase6_app_environments();

CREATE OR REPLACE FUNCTION phase6_environment_credential_row_valid(value environments) RETURNS boolean
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
            AND value.registry_credential_checkpoint IN ('activated','namespace_secret_verified')
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
                  AND ((value.registry_credential_state='pending' AND generation.state='pending')
                    OR (value.registry_credential_state='ready' AND generation.state='active')
                    OR (value.registry_credential_state='failed' AND generation.state IN ('active','retired'))))
        ELSE false END
    FROM apps app
    LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
    WHERE app.org_id=value.org_id AND app.id=value.app_id
$$;

DROP INDEX environments_registry_credential_cleanup_due;
DROP INDEX environments_registry_credential_cleanup_leased;
ALTER TABLE environments
    DROP CONSTRAINT environments_registry_credential_cleanup_generation_fk,
    DROP CONSTRAINT environments_registry_credential_cleanup_state_check,
    DROP CONSTRAINT environments_registry_credential_cleanup_target_check,
    DROP CONSTRAINT environments_registry_credential_cleanup_attempts_check,
    DROP CONSTRAINT environments_registry_credential_cleanup_lease_epoch_check,
    DROP CONSTRAINT environments_registry_credential_cleanup_lease_owner_check,
    DROP CONSTRAINT environments_registry_credential_cleanup_lease_pair_check,
    DROP CONSTRAINT environments_registry_credential_cleanup_lease_state_check,
    DROP COLUMN registry_credential_cleanup_state,
    DROP COLUMN registry_credential_cleanup_registry_id,
    DROP COLUMN registry_credential_cleanup_generation,
    DROP COLUMN registry_credential_cleanup_attempts,
    DROP COLUMN registry_credential_cleanup_next_attempt_at,
    DROP COLUMN registry_credential_cleanup_lease_owner,
    DROP COLUMN registry_credential_cleanup_lease_epoch,
    DROP COLUMN registry_credential_cleanup_lease_expires_at;
