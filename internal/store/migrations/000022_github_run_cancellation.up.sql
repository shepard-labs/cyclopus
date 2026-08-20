DO $migration$
DECLARE
    owning_schema text;
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

    EXECUTE pg_catalog.format($ddl$
        ALTER TABLE %I.orchestration_jobs
            DROP CONSTRAINT orchestration_jobs_cancellation_state_check,
            ADD COLUMN dispatch_call_authorized_at timestamptz,
            ADD COLUMN dispatch_call_owner text CHECK (dispatch_call_owner IS NULL OR (pg_catalog.btrim(dispatch_call_owner) <> '' AND pg_catalog.octet_length(dispatch_call_owner) <= 255)),
            ADD COLUMN dispatch_call_expires_at timestamptz,
            ADD COLUMN cancellation_attempts integer NOT NULL DEFAULT 0 CHECK (cancellation_attempts BETWEEN 0 AND 12),
            ADD COLUMN cancellation_next_attempt_at timestamptz NOT NULL DEFAULT pg_catalog.now(),
            ADD COLUMN cancellation_error_code text CHECK (cancellation_error_code IS NULL OR pg_catalog.octet_length(cancellation_error_code) <= 200),
            ADD COLUMN cancellation_error_message text CHECK (cancellation_error_message IS NULL OR pg_catalog.octet_length(cancellation_error_message) <= 8192),
            ADD CONSTRAINT orchestration_jobs_dispatch_call_pin
                CHECK ((dispatch_call_owner IS NULL) = (dispatch_call_expires_at IS NULL)
                    AND (dispatch_call_authorized_at IS NULL OR dispatch_call_owner IS NOT NULL)),
            ADD CONSTRAINT orchestration_jobs_cancellation_state_check CHECK
                (cancellation_state IN ('not_requested','requested','accepted','failed','not_supported','no_run_observed'))
    $ddl$, owning_schema);

    EXECUTE pg_catalog.format($ddl$
        CREATE INDEX orchestration_jobs_cancellation_due
            ON %I.orchestration_jobs(cancellation_next_attempt_at,created_at,id)
            WHERE status='cancelled' AND cancellation_state IN ('requested','failed')
    $ddl$, owning_schema);

    EXECUTE pg_catalog.format($ddl$
        CREATE OR REPLACE FUNCTION %I.fence_disconnected_git_connection() RETURNS trigger
        LANGUAGE plpgsql SET search_path TO %I, pg_catalog AS $function$
        BEGIN
            IF OLD.state = 'active' AND NEW.state = 'disconnected' THEN
                PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(NEW.org_id::text, 1380270412));
                IF EXISTS (SELECT 1 FROM orchestration_jobs job
                    JOIN apps app ON app.org_id=job.org_id AND app.id=job.app_id
                    JOIN projects project ON project.org_id=app.org_id AND project.id=app.project_id
                    WHERE project.org_id=NEW.org_id AND project.git_connection_id=NEW.id
                      AND job.dispatch_call_owner IS NOT NULL AND job.dispatch_call_expires_at>pg_catalog.now()) THEN
                    RAISE EXCEPTION 'GitHub workflow dispatch call is in progress' USING ERRCODE='serialization_failure';
                END IF;
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
                       lease_owner = NULL, lease_expires_at = NULL,
                       reconciliation_owner = NULL, reconciliation_lease_expires_at = NULL,
                       deployment_claim_owner = NULL, deployment_claim_expires_at = NULL,
                       cancellation_state = 'requested',
                       cancellation_requested_at = COALESCE(cancellation_requested_at, pg_catalog.now()),
                       cancellation_next_attempt_at = pg_catalog.now(),
                       cancellation_error_code = NULL, cancellation_error_message = NULL,
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
