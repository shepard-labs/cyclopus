DO $migration$
DECLARE
    owning_schema text;
    unsafe boolean;
BEGIN
    SELECT namespace.nspname INTO STRICT owning_schema
    FROM pg_catalog.pg_trigger trigger
    JOIN pg_catalog.pg_class relation ON relation.oid=trigger.tgrelid
    JOIN pg_catalog.pg_namespace namespace ON namespace.oid=relation.relnamespace
    JOIN pg_catalog.pg_proc function ON function.oid=trigger.tgfoid AND function.pronamespace=relation.relnamespace
    WHERE trigger.tgname='git_connections_disconnect_fence'
      AND relation.relname='git_connections' AND function.proname='fence_disconnected_git_connection'
      AND namespace.nspname=pg_catalog.current_schema()
      AND relation.relowner=CURRENT_USER::pg_catalog.regrole
      AND function.proowner=CURRENT_USER::pg_catalog.regrole AND NOT trigger.tgisinternal;

    EXECUTE pg_catalog.format('LOCK TABLE %I.orchestration_jobs IN ACCESS EXCLUSIVE MODE', owning_schema);
    EXECUTE pg_catalog.format($query$
        SELECT EXISTS (
            SELECT 1 FROM %I.orchestration_jobs
            WHERE dispatch_call_authorized_at IS NOT NULL
               OR cancellation_attempts <> 0
               OR cancellation_error_code IS NOT NULL
               OR cancellation_error_message IS NOT NULL
               OR cancellation_state='no_run_observed'
        )
    $query$, owning_schema) INTO unsafe;
    IF unsafe THEN
        RAISE EXCEPTION '000022 downgrade would discard GitHub dispatch authorization or cancellation evidence'
            USING ERRCODE='check_violation';
    END IF;

    EXECUTE pg_catalog.format('DROP INDEX %I.orchestration_jobs_cancellation_due', owning_schema);
    EXECUTE pg_catalog.format($ddl$
        ALTER TABLE %I.orchestration_jobs
            DROP CONSTRAINT orchestration_jobs_cancellation_state_check,
            DROP CONSTRAINT orchestration_jobs_dispatch_call_pin,
            DROP COLUMN cancellation_error_message,
            DROP COLUMN cancellation_error_code,
            DROP COLUMN cancellation_next_attempt_at,
            DROP COLUMN cancellation_attempts,
            DROP COLUMN dispatch_call_expires_at,
            DROP COLUMN dispatch_call_owner,
            DROP COLUMN dispatch_call_authorized_at,
            ADD CONSTRAINT orchestration_jobs_cancellation_state_check CHECK
                (cancellation_state IN ('not_requested','requested','accepted','failed','not_supported'))
    $ddl$, owning_schema);

    EXECUTE pg_catalog.format($ddl$
        CREATE OR REPLACE FUNCTION %I.fence_disconnected_git_connection() RETURNS trigger
        LANGUAGE plpgsql SET search_path TO %I, pg_catalog AS $function$
        BEGIN
            IF OLD.state = 'active' AND NEW.state = 'disconnected' THEN
                PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(NEW.org_id::text, 1380270412));
                UPDATE environments environment
                   SET desired_generation = environment.desired_generation + 1,
                       pending_commit_sha = NULL,
                       in_progress_release_id = NULL,
                       lock_acquired_at = NULL
                  FROM apps app, projects project
                 WHERE project.org_id = NEW.org_id AND project.git_connection_id = NEW.id
                   AND app.org_id = project.org_id AND app.project_id = project.id
                   AND environment.org_id = app.org_id AND environment.app_id = app.id;
                UPDATE orchestration_jobs job
                   SET status = 'cancelled', pending_github_repository_id = NULL,
                       pending_github_workflow_path = NULL, pending_workflow_revision = NULL,
                       pending_dispatch_ref = NULL, pending_checkout_sha = NULL,
                       pending_desired_generation = NULL, pending_idempotency_key = NULL,
                       lease_owner = NULL, lease_expires_at = NULL, reconcile_after = NULL,
                       dispatch_deadline_at = NULL, run_deadline_at = NULL,
                       reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
                       deployment_claim_owner = NULL, deployment_claim_expires_at = NULL,
                       cancellation_state = 'requested',
                       cancellation_requested_at = COALESCE(cancellation_requested_at, pg_catalog.now()),
                       deployment_state = CASE WHEN deployment_state IN ('waiting_credential', 'ready', 'claimed', 'applying') THEN 'cancelled' ELSE deployment_state END,
                       terminal_error_code = 'git_connection_disconnected',
                       terminal_error_message = 'Git connection was disconnected',
                       terminal_at = pg_catalog.now(), updated_at = pg_catalog.now()
                  FROM environments environment, apps app, projects project
                 WHERE project.org_id = NEW.org_id AND project.git_connection_id = NEW.id
                   AND app.org_id = project.org_id AND app.project_id = project.id
                   AND environment.org_id = app.org_id AND environment.app_id = app.id
                   AND job.org_id = environment.org_id AND job.environment_id = environment.id
                   AND job.status IN ('queued', 'leased', 'dispatch_reconciling', 'dispatched', 'resolving_image', 'deploying');
            END IF;
            RETURN NEW;
        END;
        $function$
    $ddl$, owning_schema, owning_schema);
END
$migration$;
