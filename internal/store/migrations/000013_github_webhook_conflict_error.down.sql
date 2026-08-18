LOCK TABLE github_webhook_deliveries IN SHARE ROW EXCLUSIVE MODE;
SELECT assert_github_webhook_v13_down_safe();
DROP TRIGGER guard_github_webhook_v13_down ON schema_migrations;
DROP FUNCTION guard_github_webhook_v13_down();
DROP FUNCTION assert_github_webhook_v13_down_safe();

ALTER TABLE github_webhook_deliveries
    DROP CONSTRAINT github_webhook_deliveries_error_code_check;

ALTER TABLE github_webhook_deliveries
    ADD CONSTRAINT github_webhook_deliveries_error_code_check
    CHECK (error_code IS NULL OR error_code IN ('database_unavailable','github_unavailable','github_rate_limited','permission_revoked','malformed_response','invalid_event','fanout_limit','internal','attempts_exhausted'));
