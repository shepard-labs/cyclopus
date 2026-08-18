ALTER TABLE github_webhook_deliveries
    DROP CONSTRAINT github_webhook_deliveries_error_code_check;

ALTER TABLE github_webhook_deliveries
    ADD CONSTRAINT github_webhook_deliveries_error_code_check
    CHECK (error_code IS NULL OR error_code IN ('database_unavailable','github_unavailable','github_rate_limited','permission_revoked','malformed_response','invalid_event','github_conflict','fanout_limit','internal','attempts_exhausted'));

CREATE FUNCTION assert_github_webhook_v13_down_safe() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM github_webhook_deliveries WHERE error_code = 'github_conflict') THEN
        RAISE EXCEPTION '000013 rollback blocked: webhook deliveries with github_conflict error codes exist; retain migration 000013 or clean up those rows before retrying'
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;

CREATE FUNCTION guard_github_webhook_v13_down() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE version=13 AND dirty=false) THEN
        PERFORM assert_github_webhook_v13_down_safe();
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER guard_github_webhook_v13_down BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT EXECUTE FUNCTION guard_github_webhook_v13_down();
