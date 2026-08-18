ALTER TABLE github_webhook_deliveries
    ADD COLUMN signature_key_version text NOT NULL DEFAULT 'legacy';

ALTER TABLE github_webhook_deliveries
    ADD CONSTRAINT github_webhook_deliveries_signature_key_version_check
    CHECK (signature_key_version = btrim(signature_key_version)
		AND signature_key_version ~ '[^[:space:]]'
        AND length(signature_key_version) BETWEEN 1 AND 128);

ALTER TABLE github_webhook_deliveries
    ALTER COLUMN signature_key_version DROP DEFAULT;

CREATE FUNCTION assert_github_webhook_v12_down_safe() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM github_webhook_deliveries WHERE signature_key_version <> 'legacy') THEN
        RAISE EXCEPTION '000012 rollback blocked: non-legacy webhook signature key versions exist; retain migration 000012 or archive the version metadata before retrying'
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;

CREATE FUNCTION guard_github_webhook_v12_down() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE version=12 AND dirty=false) THEN
        PERFORM assert_github_webhook_v12_down_safe();
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER guard_github_webhook_v12_down BEFORE TRUNCATE ON schema_migrations FOR EACH STATEMENT EXECUTE FUNCTION guard_github_webhook_v12_down();
