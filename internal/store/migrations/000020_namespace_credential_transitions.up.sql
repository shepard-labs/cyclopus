ALTER TABLE environments
    ADD COLUMN registry_credential_cleanup_state text NOT NULL DEFAULT 'not_required',
    ADD COLUMN registry_credential_cleanup_registry_id uuid,
    ADD COLUMN registry_credential_cleanup_generation bigint,
    ADD COLUMN registry_credential_cleanup_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN registry_credential_cleanup_next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN registry_credential_cleanup_lease_owner text,
    ADD COLUMN registry_credential_cleanup_lease_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN registry_credential_cleanup_lease_expires_at timestamptz,
    ADD CONSTRAINT environments_registry_credential_cleanup_state_check
        CHECK (registry_credential_cleanup_state IN ('not_required','pending','complete','failed')),
    ADD CONSTRAINT environments_registry_credential_cleanup_target_check CHECK (
        (registry_credential_cleanup_state='not_required' AND registry_credential_cleanup_registry_id IS NULL AND registry_credential_cleanup_generation IS NULL)
        OR (registry_credential_cleanup_state<>'not_required' AND registry_credential_cleanup_registry_id IS NOT NULL AND registry_credential_cleanup_generation>0)),
    ADD CONSTRAINT environments_registry_credential_cleanup_attempts_check
        CHECK (registry_credential_cleanup_attempts BETWEEN 0 AND 12),
    ADD CONSTRAINT environments_registry_credential_cleanup_lease_epoch_check
        CHECK (registry_credential_cleanup_lease_epoch>=0),
    ADD CONSTRAINT environments_registry_credential_cleanup_lease_owner_check
        CHECK (registry_credential_cleanup_lease_owner IS NULL OR (btrim(registry_credential_cleanup_lease_owner)<>'' AND octet_length(registry_credential_cleanup_lease_owner)<=255)),
    ADD CONSTRAINT environments_registry_credential_cleanup_lease_pair_check
        CHECK ((registry_credential_cleanup_lease_owner IS NULL)=(registry_credential_cleanup_lease_expires_at IS NULL)),
    ADD CONSTRAINT environments_registry_credential_cleanup_lease_state_check
        CHECK (registry_credential_cleanup_lease_owner IS NULL OR registry_credential_cleanup_state='pending'),
    ADD CONSTRAINT environments_registry_credential_cleanup_generation_fk
        FOREIGN KEY (org_id,registry_credential_cleanup_registry_id,registry_credential_cleanup_generation)
        REFERENCES registry_credential_generations(org_id,registry_id,generation) DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX environments_registry_credential_cleanup_due
    ON environments(registry_credential_cleanup_next_attempt_at,created_at,id)
    WHERE registry_credential_cleanup_state='pending' AND registry_credential_cleanup_lease_owner IS NULL
      AND torn_down_at IS NULL AND registry_credential_cleanup_attempts<12;
CREATE INDEX environments_registry_credential_cleanup_leased
    ON environments(registry_credential_cleanup_lease_expires_at,id)
    WHERE registry_credential_cleanup_lease_owner IS NOT NULL;

CREATE OR REPLACE FUNCTION phase6_environment_credential_row_valid(value environments) RETURNS boolean
LANGUAGE sql STABLE SET search_path FROM CURRENT AS $$
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
            AND (value.ready_registry_credential_generation IS NULL OR value.ready_registry_credential_generation<=value.required_registry_credential_generation)
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
                  AND ((value.registry_credential_state='pending' AND generation.state IN ('pending','active','retired'))
                    OR (value.registry_credential_state='ready' AND generation.state='active')
                    OR (value.registry_credential_state='failed' AND generation.state IN ('active','retired'))))
        ELSE false END
    FROM apps app
    LEFT JOIN registries registry ON registry.org_id=app.org_id AND registry.id=app.registry_id
    WHERE app.org_id=value.org_id AND app.id=value.app_id
$$;

DROP TRIGGER apps_phase6_environments ON apps;
CREATE CONSTRAINT TRIGGER apps_phase6_environments
    AFTER UPDATE OF image_visibility,registry_id,deleted_at ON apps
    DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW EXECUTE FUNCTION enforce_phase6_app_environments();

CREATE FUNCTION converge_registry_credential_operation_without_namespaces(target_org uuid,target_registry uuid)
RETURNS void LANGUAGE plpgsql SECURITY INVOKER SET search_path FROM CURRENT AS $$
DECLARE
    operation_row registry_credential_operations;
    registry_row registries;
    candidate_ref text;
BEGIN
    SELECT * INTO registry_row FROM registries WHERE org_id=target_org AND id=target_registry FOR UPDATE;
    IF NOT FOUND OR registry_row.operation_id IS NULL THEN RETURN; END IF;
    SELECT * INTO operation_row FROM registry_credential_operations
      WHERE org_id=target_org AND id=registry_row.operation_id AND registry_id=target_registry FOR UPDATE;
    IF NOT FOUND OR operation_row.status<>'pending' OR operation_row.checkpoint NOT IN ('namespaces_reconciling','rollback_reconciling') THEN RETURN; END IF;
    IF EXISTS (SELECT 1 FROM environments environment JOIN apps app
        ON app.org_id=environment.org_id AND app.id=environment.app_id
        WHERE environment.org_id=target_org AND app.registry_id=target_registry
          AND app.image_visibility='private' AND app.deleted_at IS NULL
          AND environment.torn_down_at IS NULL AND environment.lifecycle_status<>'teardown_requested') THEN RETURN; END IF;

    IF operation_row.checkpoint='namespaces_reconciling' THEN
        SELECT secret_ref INTO candidate_ref FROM registry_credential_generations
          WHERE org_id=target_org AND registry_id=target_registry
            AND generation=operation_row.candidate_generation AND operation_id=operation_row.id
            AND state='pending' AND validated_at IS NOT NULL FOR UPDATE;
        IF NOT FOUND THEN RAISE EXCEPTION 'candidate generation cannot be activated' USING ERRCODE='check_violation'; END IF;
        IF operation_row.previous_generation IS NOT NULL THEN
            UPDATE registry_credential_generations SET state='retired',retired_at=clock_timestamp()
              WHERE org_id=target_org AND registry_id=target_registry
                AND generation=operation_row.previous_generation AND state='active';
            IF NOT FOUND THEN RAISE EXCEPTION 'previous generation cannot be retired' USING ERRCODE='check_violation'; END IF;
        END IF;
        UPDATE registry_credential_generations SET state='active',activated_at=clock_timestamp()
          WHERE org_id=target_org AND registry_id=target_registry
            AND generation=operation_row.candidate_generation AND operation_id=operation_row.id AND state='pending';
        IF NOT FOUND THEN RAISE EXCEPTION 'candidate generation activation lost' USING ERRCODE='check_violation'; END IF;
        UPDATE registry_credential_operations SET status='succeeded',checkpoint='activated',terminal_at=clock_timestamp(),
          next_attempt_at='infinity',lease_owner=NULL,lease_expires_at=NULL,failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp()
          WHERE org_id=target_org AND id=operation_row.id AND status='pending' AND checkpoint='namespaces_reconciling';
        IF NOT FOUND THEN RAISE EXCEPTION 'credential operation activation lost' USING ERRCODE='check_violation'; END IF;
        UPDATE registries SET state='ready',checkpoint='activated',active_generation=operation_row.candidate_generation,
          active_credential_secret_ref=candidate_ref,pending_generation=NULL,pending_credential_secret_ref=NULL,
          previous_generation=operation_row.previous_generation,
          previous_credential_secret_ref=CASE WHEN operation_row.previous_generation IS NULL THEN NULL ELSE active_credential_secret_ref END,
          operation_id=NULL,activated_at=clock_timestamp(),failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp()
          WHERE org_id=target_org AND id=target_registry AND operation_id=operation_row.id
            AND pending_generation=operation_row.candidate_generation;
        IF NOT FOUND THEN RAISE EXCEPTION 'registry activation lost' USING ERRCODE='check_violation'; END IF;
    ELSE
        UPDATE registry_credential_generations SET state='failed'
          WHERE org_id=target_org AND registry_id=target_registry
            AND generation=operation_row.candidate_generation AND operation_id=operation_row.id AND state='pending';
        IF NOT FOUND THEN RAISE EXCEPTION 'rollback candidate terminalization lost' USING ERRCODE='check_violation'; END IF;
        UPDATE registry_credential_operations SET status='failed',failure_code=COALESCE(registry_row.failure_message,'image_pull_secret_reconcile_failed'),
          failure_message=NULL,terminal_at=clock_timestamp(),next_attempt_at='infinity',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
          WHERE org_id=target_org AND id=operation_row.id AND status='pending' AND checkpoint='rollback_reconciling';
        IF NOT FOUND THEN RAISE EXCEPTION 'rollback operation terminalization lost' USING ERRCODE='check_violation'; END IF;
        UPDATE registries SET state='ready',checkpoint='activated',pending_generation=NULL,pending_credential_secret_ref=NULL,
          operation_id=NULL,previous_generation=NULL,previous_credential_secret_ref=NULL,
          failure_code=NULL,failure_message=NULL,updated_at=clock_timestamp()
          WHERE org_id=target_org AND id=target_registry AND operation_id=operation_row.id
            AND checkpoint='rollback_reconciling' AND active_generation=operation_row.previous_generation
            AND pending_generation=operation_row.candidate_generation;
        IF NOT FOUND THEN RAISE EXCEPTION 'registry rollback terminalization lost' USING ERRCODE='check_violation'; END IF;
    END IF;
END
$$;

CREATE FUNCTION transition_app_image_visibility(target_org uuid,target_app uuid,target_visibility text,target_registry uuid)
RETURNS void LANGUAGE plpgsql SECURITY INVOKER SET search_path FROM CURRENT AS $$
DECLARE
    app_row apps;
    selected_generation bigint;
BEGIN
    IF target_visibility NOT IN ('public','private') THEN
        RAISE EXCEPTION 'invalid application image visibility' USING ERRCODE='invalid_parameter_value';
    END IF;
    SELECT * INTO app_row FROM apps WHERE org_id=target_org AND id=target_app AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'application not found' USING ERRCODE='no_data_found'; END IF;
    IF app_row.image_visibility=target_visibility AND app_row.registry_id IS NOT DISTINCT FROM target_registry THEN RETURN; END IF;
    IF app_row.image_visibility=target_visibility THEN
        RAISE EXCEPTION 'registry binding cannot be changed by visibility transition' USING ERRCODE='check_violation';
    END IF;
    IF target_visibility='private' THEN
        IF target_registry IS NULL THEN RAISE EXCEPTION 'private application requires registry' USING ERRCODE='check_violation'; END IF;
        SELECT CASE
            WHEN operation.status='pending' AND operation.checkpoint='namespaces_reconciling' AND candidate.state='pending'
                 AND registry.pending_generation=operation.candidate_generation THEN operation.candidate_generation
            WHEN operation.status='pending' AND operation.checkpoint='rollback_reconciling'
                 AND operation.previous_generation IS NOT NULL AND active.state='active'
                 AND registry.active_generation=operation.previous_generation THEN operation.previous_generation
            WHEN registry.active_generation>0 AND active.state='active' THEN registry.active_generation
          END INTO selected_generation
        FROM registries registry
        LEFT JOIN registry_credential_operations operation ON operation.org_id=registry.org_id AND operation.id=registry.operation_id AND operation.registry_id=registry.id
        LEFT JOIN registry_credential_generations candidate ON candidate.org_id=operation.org_id AND candidate.registry_id=operation.registry_id AND candidate.generation=operation.candidate_generation
        LEFT JOIN registry_credential_generations active ON active.org_id=registry.org_id AND active.registry_id=registry.id AND active.generation=registry.active_generation
        WHERE registry.org_id=target_org AND registry.id=target_registry FOR UPDATE OF registry;
        IF selected_generation IS NULL THEN RAISE EXCEPTION 'registry has no reconcilable generation' USING ERRCODE='check_violation'; END IF;
    ELSE
        IF target_registry IS NOT NULL THEN RAISE EXCEPTION 'public application cannot bind registry' USING ERRCODE='check_violation'; END IF;
    END IF;

    SET CONSTRAINTS apps_phase6_environments DEFERRED;
    IF target_visibility='private' THEN
        UPDATE apps SET image_visibility='private',registry_id=target_registry,desired_generation=desired_generation+1,
            cleanup_state='active',cleanup_checkpoint=NULL,cleanup_error_code=NULL,cleanup_error_message=NULL
        WHERE org_id=target_org AND id=target_app;
        UPDATE environments SET required_registry_credential_generation=selected_generation,
            ready_registry_credential_generation=NULL,registry_credential_state='pending',registry_credential_checkpoint=NULL,
            registry_credential_error_code=NULL,registry_credential_error_message=NULL,registry_credential_attempts=0,
            registry_credential_next_attempt_at=clock_timestamp(),registry_credential_lease_owner=NULL,
            registry_credential_lease_expires_at=NULL,registry_credential_lease_epoch=registry_credential_lease_epoch+1,
            registry_credential_cleanup_state='not_required',registry_credential_cleanup_registry_id=NULL,
            registry_credential_cleanup_generation=NULL,registry_credential_cleanup_attempts=0,
            registry_credential_cleanup_lease_owner=NULL,registry_credential_cleanup_lease_expires_at=NULL,
            registry_credential_cleanup_lease_epoch=registry_credential_cleanup_lease_epoch+1
        WHERE org_id=target_org AND app_id=target_app AND torn_down_at IS NULL;
    ELSE
        UPDATE apps SET image_visibility='public',registry_id=NULL,desired_generation=desired_generation+1,
            cleanup_state='cleanup_requested',cleanup_checkpoint=NULL,cleanup_error_code=NULL,cleanup_error_message=NULL
        WHERE org_id=target_org AND id=target_app;
        UPDATE environments SET registry_credential_cleanup_state='pending',
            registry_credential_cleanup_registry_id=app_row.registry_id,
            registry_credential_cleanup_generation=COALESCE(ready_registry_credential_generation,required_registry_credential_generation),
            registry_credential_cleanup_attempts=0,registry_credential_cleanup_next_attempt_at=clock_timestamp(),
            registry_credential_cleanup_lease_owner=NULL,registry_credential_cleanup_lease_expires_at=NULL,
            registry_credential_cleanup_lease_epoch=registry_credential_cleanup_lease_epoch+1,
            required_registry_credential_generation=NULL,ready_registry_credential_generation=NULL,
            registry_credential_state='not_required',registry_credential_checkpoint=NULL,
            registry_credential_error_code=NULL,registry_credential_error_message=NULL,
            registry_credential_attempts=0,registry_credential_lease_owner=NULL,registry_credential_lease_expires_at=NULL,
            registry_credential_lease_epoch=registry_credential_lease_epoch+1
        WHERE org_id=target_org AND app_id=target_app AND torn_down_at IS NULL;
        PERFORM converge_registry_credential_operation_without_namespaces(target_org,app_row.registry_id);
    END IF;
    SET CONSTRAINTS apps_phase6_environments IMMEDIATE;
    PERFORM assert_phase6_dependent_environments(target_org,target_app);
END
$$;

CREATE FUNCTION assert_namespace_credential_transitions_v20_down_safe() RETURNS void
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM environments WHERE registry_credential_cleanup_state<>'not_required'
        OR registry_credential_cleanup_registry_id IS NOT NULL OR registry_credential_cleanup_generation IS NOT NULL
        OR registry_credential_cleanup_attempts<>0 OR registry_credential_cleanup_lease_epoch<>0
        OR registry_credential_cleanup_lease_owner IS NOT NULL) THEN
        RAISE EXCEPTION '000020 downgrade would discard namespace credential cleanup state or evidence'
            USING ERRCODE='check_violation';
    END IF;
END
$$;
CREATE FUNCTION guard_namespace_credential_transitions_v20_down() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE version=20 AND dirty=false) THEN
        PERFORM assert_namespace_credential_transitions_v20_down_safe();
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER guard_namespace_credential_transitions_v20_down
    BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT
    EXECUTE FUNCTION guard_namespace_credential_transitions_v20_down();
