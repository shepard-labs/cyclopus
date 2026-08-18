LOCK TABLE github_routing_fanout_manifests, github_routing_fanout_candidates IN SHARE ROW EXCLUSIVE MODE;
SELECT assert_github_routing_fanout_v14_down_safe();
DROP TRIGGER guard_github_routing_fanout_v14_down ON schema_migrations;
DROP FUNCTION guard_github_routing_fanout_v14_down();
DROP FUNCTION assert_github_routing_fanout_v14_down_safe();
DROP TABLE github_routing_fanout_candidates;
DROP TABLE github_routing_fanout_manifests;
ALTER TABLE github_webhook_deliveries DROP CONSTRAINT github_webhook_deliveries_result_code_check;
ALTER TABLE github_webhook_deliveries ADD CONSTRAINT github_webhook_deliveries_result_code_check
    CHECK (result_code IS NULL OR result_code IN ('processed','ignored','stale_push','production_branch_deleted','exhausted'));
