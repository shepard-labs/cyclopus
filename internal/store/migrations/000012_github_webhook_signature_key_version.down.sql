LOCK TABLE github_webhook_deliveries IN SHARE ROW EXCLUSIVE MODE;
SELECT assert_github_webhook_v12_down_safe();
DROP TRIGGER guard_github_webhook_v12_down ON schema_migrations;
DROP FUNCTION guard_github_webhook_v12_down();
DROP FUNCTION assert_github_webhook_v12_down_safe();

ALTER TABLE github_webhook_deliveries
    DROP CONSTRAINT IF EXISTS github_webhook_deliveries_signature_key_version_check,
    DROP COLUMN IF EXISTS signature_key_version;
