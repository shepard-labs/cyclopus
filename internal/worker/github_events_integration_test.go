//go:build integration

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	githubadapter "github.com/shepard-labs/cyclopus/internal/adapters/github"
	"github.com/shepard-labs/cyclopus/internal/githubmetrics"
	"github.com/shepard-labs/cyclopus/internal/service"
	"github.com/shepard-labs/cyclopus/internal/store"
)

func TestGitHubEventDeliveryProcessorPersistsConflictPostgres18(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			testGitHubEventDeliveryProcessorPersistsConflictPostgres18(t, status)
		})
	}
}

func testGitHubEventDeliveryProcessorPersistsConflictPostgres18(t *testing.T, conflictStatus int) {
	databaseURL := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("POSTGRES_TEST_URL")
	}
	if databaseURL == "" {
		t.Fatal("CYCLOPUS_TEST_DATABASE_URL or POSTGRES_TEST_URL must point to PostgreSQL 18")
	}
	admin, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var serverVersion int
	if err := admin.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::integer`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 || serverVersion >= 190000 {
		t.Fatalf("PostgreSQL server_version_num=%d, want 18.x", serverVersion)
	}
	schema := fmt.Sprintf("github_worker_conflict_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsedURL, err := url.Parse(config.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	query := parsedURL.Query()
	query.Set("search_path", pgx.Identifier{schema}.Sanitize())
	parsedURL.RawQuery = query.Encode()
	testURL := parsedURL.String()
	if err := store.Migrate(t.Context(), store.MigrationConfig{URL: testURL, LockTimeout: 5 * time.Second, Migrations: store.Migrations}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var orgID string
	if err := pool.QueryRow(t.Context(), `INSERT INTO orgs(name,slug) VALUES('Conflict Worker','conflict-worker') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation,repository_reconciliation_generation)
		VALUES(42,$1,420,'conflict-worker','Organization','selected','active',1,1,1)`, orgID); err != nil {
		t.Fatal(err)
	}
	codec, err := store.NewAESGCMGitHubWebhookPayloadCodec("k1", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	dataStore := store.New(pool).WithGitHubWebhookPayloadCodec(codec)
	payload := []byte(`{"action":"created","installation":{"id":42}}`)
	digest := sha256.Sum256(payload)
	disposition, delivery, err := dataStore.ReceiveGitHubWebhook(t.Context(), store.ReceiveGitHubWebhookParams{
		DeliveryID: "40000000-0000-0000-0000-000000000013", Event: "installation", Action: "created", OrgID: orgID,
		PayloadDigest: hex.EncodeToString(digest[:]), SignatureKeyVersion: "legacy", Payload: payload, InstallationID: ptr(int64(42)),
	})
	if err != nil || disposition != store.GitHubWebhookAccepted {
		t.Fatalf("receive = %q %+v, %v", disposition, delivery, err)
	}
	processor, err := service.NewGitHubEventProcessor(dataStore, &persistentConflictClient{status: conflictStatus}, 10)
	if err != nil {
		t.Fatal(err)
	}
	configWorker := gitHubEventTestConfig()
	configWorker.Owner = "conflict-worker"
	runner := NewGitHubEventRunner(configWorker, discardLogger(), dataStore, processor)
	if err := runner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner.wg.Wait()
	var status, code string
	var attempts int
	var owner *string
	var expires *time.Time
	if err := pool.QueryRow(t.Context(), `SELECT status,attempts,error_code,lease_owner,lease_expires_at FROM github_webhook_deliveries WHERE id=$1`, delivery.ID).Scan(&status, &attempts, &code, &owner, &expires); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != 1 || code != "github_conflict" || owner != nil || expires != nil {
		t.Fatalf("persistent conflict state = %s/%d/%s owner=%v expires=%v", status, attempts, code, owner, expires)
	}
	claimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "retry-worker", time.Minute, 1)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("persistent conflict reclaim = %+v, %v", claimed, err)
	}
}

func TestGitHubEventDurableFanoutSurvivesWorkerRestartPostgres18(t *testing.T) {
	dataStore, pool := newGitHubEventIntegrationFixture(t, "github_worker_restart")
	seedGitHubEventFanoutApps(t, pool, 120, 20000)
	const ref = "refs/heads/feature/restart"
	before, after := strings.Repeat("0", 40), strings.Repeat("a", 40)
	delivery := receiveGitHubEventPush(t, dataStore, "40000000-0000-0000-0000-000000000020", ref, before, after)
	processor := newGitHubEventIntegrationProcessor(t, dataStore, ref, before, after, 120, 50)
	metrics := githubmetrics.New()

	claimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "delivery-worker", 20*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim delivery = %+v, %v", claimed, err)
	}
	outcome := processor.ProcessDelivery(t.Context(), claimed[0])
	if outcome.Disposition != service.GitHubEventComplete || outcome.Code != "fanout_staged" || outcome.IntentCount != 120 {
		t.Fatalf("ProcessDelivery() = %+v", outcome)
	}
	deliveryMetrics := &fakeGitHubMetrics{}
	claimed[0].Lease = integrationCurrentDeliveryLease(t, pool, delivery.ID)
	if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, delivery.ID); err != nil {
		t.Fatal(err)
	}
	claimed[0].Lease = integrationCurrentDeliveryLease(t, pool, delivery.ID)
	deliveryRunner := NewGitHubEventRunner(gitHubEventIntegrationConfig("delivery-worker", 120, 50), integrationDiscardLogger(), dataStore, &fakeGitHubEventProcessor{deliveryOutcome: outcome}, deliveryMetrics)
	enableTestWrites(deliveryRunner)
	runDeliveryDirect(deliveryRunner, t.Context(), claimed[0])
	if deliveryMetrics.deliveryResult != "processed" {
		t.Fatalf("delivery metric = %q", deliveryMetrics.deliveryResult)
	}
	metrics.Delivery("push", "", "processed", 0, outcome.IntentCount)
	if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET status='completed',result_code='fanout_staged',terminal_at=clock_timestamp(),retain_until=clock_timestamp()+interval '1 hour',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, delivery.ID); err != nil {
		t.Fatal(err)
	}
	assertGitHubEventDelivery(t, pool, delivery.ID, "completed", 1, "fanout_staged")
	assertGitHubEventFanout(t, pool, delivery.ID, "pending", 120, 0, 0)

	firstBatch := make(chan struct{}, 1)
	firstProcessor := &observedGitHubEventProcessor{GitHubEventProcessor: processor, fanoutDone: firstBatch}
	firstRunner := NewGitHubEventRunner(gitHubEventIntegrationConfig("fanout-worker-before-restart", 120, 50), integrationDiscardLogger(), fanoutOnlyGitHubEventStore{Store: dataStore}, firstProcessor, metrics)
	if err := firstRunner.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstBatch:
	case <-time.After(10 * time.Second):
		t.Fatal("first fanout batch did not finish")
	}
	firstRunner.wg.Wait()
	assertGitHubEventFanout(t, pool, delivery.ID, "pending", 120, 50, 0)

	restartCtx, cancelRestart := context.WithCancel(t.Context())
	restarted := NewGitHubEventRunner(gitHubEventIntegrationConfig("fanout-worker-after-restart", 120, 50), integrationDiscardLogger(), fanoutOnlyGitHubEventStore{Store: dataStore}, processor, metrics)
	if err := restarted.Startup(restartCtx); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- restarted.Run(restartCtx) }()
	waitForGitHubEventFanout(t, pool, delivery.ID, 120)
	time.Sleep(30 * time.Millisecond)
	cancelRestart()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}

	assertGitHubEventDelivery(t, pool, delivery.ID, "completed", 1, "fanout_staged")
	assertGitHubEventFanout(t, pool, delivery.ID, "completed", 120, 120, 1)
	assertGitHubEventIntentUniqueness(t, pool, delivery.ID, 120)
	assertGitHubEventMetrics(t, metrics, 1, 3)
}

func TestGitHubEventDeliveryRetryUsesExistingManifestStatusPostgres18(t *testing.T) {
	for _, test := range []struct {
		status           string
		inserted         int
		wantOutcome      service.GitHubEventOutcome
		wantDelivery     string
		wantDeliveryCode string
	}{
		{status: "completed", inserted: 120, wantOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "processed", IntentCount: 120}, wantDelivery: "completed", wantDeliveryCode: "processed"},
		{status: "superseded", inserted: 40, wantOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "superseded", IntentCount: 40}, wantDelivery: "completed", wantDeliveryCode: "ignored"},
		{status: "pending", wantOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "fanout_staged", IntentCount: 120}, wantDelivery: "completed", wantDeliveryCode: "fanout_staged"},
		{status: "leased", inserted: 40, wantOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "fanout_staged", IntentCount: 120}, wantDelivery: "completed", wantDeliveryCode: "fanout_staged"},
		{status: "failed", inserted: 40, wantOutcome: service.GitHubEventOutcome{Disposition: service.GitHubEventFail, Code: "internal", IntentCount: 40}, wantDelivery: "failed", wantDeliveryCode: "internal"},
	} {
		t.Run(test.status, func(t *testing.T) {
			dataStore, pool := newGitHubEventIntegrationFixture(t, "github_delivery_retry_"+test.status)
			seedGitHubEventFanoutApps(t, pool, 120, 25000)
			const ref = "refs/heads/feature/retry"
			before, after := strings.Repeat("0", 40), strings.Repeat("c", 40)
			delivery := receiveGitHubEventPush(t, dataStore, "40000000-0000-0000-0000-000000000030", ref, before, after)
			processor := newGitHubEventIntegrationProcessor(t, dataStore, ref, before, after, 120, 50)

			claimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "crashed-delivery", time.Minute, 1)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("initial claim = %+v, %v", claimed, err)
			}
			if outcome := processor.GitHubEventProcessor.ProcessDelivery(t.Context(), claimed[0]); outcome != (service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "fanout_staged", IntentCount: 120}) {
				t.Fatalf("initial ProcessDelivery() = %+v", outcome)
			}
			var candidatesBefore int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_routing_fanout_candidates candidate JOIN github_routing_fanout_manifests manifest ON manifest.id=candidate.manifest_id WHERE manifest.delivery_id=$1`, delivery.ID).Scan(&candidatesBefore); err != nil {
				t.Fatal(err)
			}
			leaseOwner, leaseExpiry := any(nil), any(nil)
			if test.status == "leased" {
				leaseOwner, leaseExpiry = "fanout-worker", time.Now().Add(time.Hour)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE github_routing_fanout_manifests SET status=$2,inserted_count=$3,terminal_at=CASE WHEN $2 IN ('completed','superseded','failed') THEN clock_timestamp() END,lease_owner=$4,lease_expires_at=$5,lease_epoch=CASE WHEN $2='leased' THEN 1 ELSE lease_epoch END,error_code=CASE WHEN $2='failed' THEN 'internal' END,error_detail=CASE WHEN $2='failed' THEN 'internal' END WHERE delivery_id=$1`, delivery.ID, test.status, test.inserted, leaseOwner, leaseExpiry); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, delivery.ID); err != nil {
				t.Fatal(err)
			}
			retried, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "retry-delivery", time.Minute, 1)
			if err != nil || len(retried) != 1 {
				t.Fatalf("retry claim = %+v, %v", retried, err)
			}
			outcome := processor.GitHubEventProcessor.ProcessDelivery(t.Context(), retried[0])
			if outcome != test.wantOutcome {
				t.Fatalf("retry ProcessDelivery() = %+v, want %+v", outcome, test.wantOutcome)
			}
			var candidatesAfter int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_routing_fanout_candidates candidate JOIN github_routing_fanout_manifests manifest ON manifest.id=candidate.manifest_id WHERE manifest.delivery_id=$1`, delivery.ID).Scan(&candidatesAfter); err != nil {
				t.Fatal(err)
			}
			wantCandidates := 120
			if test.status == "completed" || test.status == "superseded" || test.status == "failed" {
				wantCandidates = 0
			}
			if candidatesAfter != wantCandidates {
				t.Fatalf("candidate snapshots before/after = %d/%d, want after %d", candidatesBefore, candidatesAfter, wantCandidates)
			}

			runner := NewGitHubEventRunner(gitHubEventIntegrationConfig("retry-delivery", 120, 50), integrationDiscardLogger(), dataStore, &fakeGitHubEventProcessor{deliveryOutcome: outcome})
			enableTestWrites(runner)
			runDeliveryDirect(runner, t.Context(), retried[0])
			var status string
			var code string
			if err := pool.QueryRow(t.Context(), `SELECT status,coalesce(result_code,error_code) FROM github_webhook_deliveries WHERE id=$1`, delivery.ID).Scan(&status, &code); err != nil {
				t.Fatal(err)
			}
			if status != test.wantDelivery || code != test.wantDeliveryCode {
				t.Fatalf("terminal delivery = %s/%v, want %s/%s", status, code, test.wantDelivery, test.wantDeliveryCode)
			}
		})
	}
}

func TestGitHubEventProcessorRejectsReclaimedDeliveryBeforeExternalCallPostgres18(t *testing.T) {
	dataStore, pool := newGitHubEventIntegrationFixture(t, "github_processor_reclaimed")
	const ref = "refs/heads/feature/reclaimed"
	before, after := strings.Repeat("0", 40), strings.Repeat("d", 40)
	delivery := receiveGitHubEventPush(t, dataStore, "40000000-0000-0000-0000-000000000031", ref, before, after)
	claimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "old-worker", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("old claim = %+v, %v", claimed, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, delivery.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "replacement-worker", time.Minute, 1)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Lease.Epoch != claimed[0].Lease.Epoch+1 {
		t.Fatalf("replacement claim = %+v, %v; old = %+v", reclaimed, err, claimed[0])
	}
	client := &recordingGitHubEventClient{sha: after, exists: true}
	processor, err := service.NewGitHubEventProcessor(dataStore, client, 10)
	if err != nil {
		t.Fatal(err)
	}
	outcome := processor.ProcessDelivery(t.Context(), claimed[0])
	if outcome != (service.GitHubEventOutcome{Disposition: service.GitHubEventLeaseLost, Code: "lease_lost"}) {
		t.Fatalf("stale ProcessDelivery() = %+v", outcome)
	}
	if calls := client.callCounts(); calls != [5]int{} {
		t.Fatalf("external calls installation/list/repository/ref/refresh = %v, want all zero", calls)
	}
	var status string
	var attempts int
	var result *string
	if err := pool.QueryRow(t.Context(), `SELECT status,attempts,result_code FROM github_webhook_deliveries WHERE id=$1`, delivery.ID).Scan(&status, &attempts, &result); err != nil {
		t.Fatal(err)
	}
	if status != "leased" || attempts != 2 || result != nil {
		t.Fatalf("reclaimed delivery = %s/%d/%v, want leased/2/nil", status, attempts, result)
	}
}

func TestGitHubEventRunnerRetriesTransactionalDatabaseFailuresPostgres18(t *testing.T) {
	for _, sqlstate := range []string{"40001", "40P01"} {
		t.Run(sqlstate, func(t *testing.T) {
			dataStore, pool := newGitHubEventIntegrationFixture(t, "github_delivery_tx_retry_"+sqlstate)
			seedGitHubEventFanoutApps(t, pool, 2, 40000)
			const ref = "refs/heads/feature/transaction-retry"
			before, after := strings.Repeat("0", 40), strings.Repeat("e", 40)
			delivery := receiveGitHubEventPush(t, dataStore, "40000000-0000-0000-0000-000000000032", ref, before, after)
			failed := false
			processorStore := &snapshotFailpointGitHubEventStore{Store: dataStore, failpoint: func(point string) error {
				if point == "after_first_intent" && !failed {
					failed = true
					return &pgconn.PgError{Code: sqlstate, Message: "injected transactional retry"}
				}
				return nil
			}}
			client := &recordingGitHubEventClient{sha: after, exists: true}
			processor, err := service.NewGitHubEventProcessorWithFanout(processorStore, client, 10, 10)
			if err != nil {
				t.Fatal(err)
			}
			config := gitHubEventIntegrationConfig("delivery-worker", 10, 10)
			config.InitialRetry, config.MaxRetry = time.Millisecond, time.Millisecond
			runner := NewGitHubEventRunner(config, integrationDiscardLogger(), dataStore, processor)
			enableTestWrites(runner)

			first := claimOneGitHubEventDelivery(t, dataStore, "delivery-worker")
			runDeliveryDirect(runner, t.Context(), first)
			assertGitHubEventDelivery(t, pool, delivery.ID, "completed", 1, "fanout_staged")
			assertGitHubEventSnapshotCounts(t, pool, delivery.ID, 1, 1, 2, 0)

			manifest := claimOneGitHubEventFanout(t, dataStore, "fanout-worker")
			fanoutRunner := NewGitHubEventRunner(config, integrationDiscardLogger(), dataStore, processor)
			enableTestWrites(fanoutRunner)
			runFanoutDirect(fanoutRunner, t.Context(), manifest)
			assertGitHubEventFanout(t, pool, delivery.ID, "completed", 2, 2, 1)
			assertGitHubEventIntentUniqueness(t, pool, delivery.ID, 2)
			assertGitHubEventSnapshotCounts(t, pool, delivery.ID, 1, 1, 0, 1)
			if calls := client.callCounts(); calls != [5]int{0, 0, 0, 1, 0} {
				t.Fatalf("external calls installation/list/repository/ref/refresh = %v, want one branch read only", calls)
			}
			claimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "unexpected-third-worker", time.Minute, 1)
			if err != nil || len(claimed) != 0 {
				t.Fatalf("unexpected third delivery claim = %+v, %v", claimed, err)
			}
		})
	}
}

func TestGitHubEventRunnerPersistsOneRetryAfterTransactionRetryExhaustionPostgres18(t *testing.T) {
	for _, sqlstate := range []string{"40001", "40P01"} {
		t.Run(sqlstate, func(t *testing.T) {
			dataStore, pool := newGitHubEventIntegrationFixture(t, "github_delivery_tx_exhaustion_"+sqlstate)
			seedGitHubEventFanoutApps(t, pool, 2, 41000)
			const ref = "refs/heads/feature/transaction-exhaustion"
			before, after := strings.Repeat("0", 40), strings.Repeat("f", 40)
			delivery := receiveGitHubEventPush(t, dataStore, "40000000-0000-0000-0000-000000000033", ref, before, after)
			var failures atomic.Int32
			processorStore := &snapshotFailpointGitHubEventStore{Store: dataStore, failpoint: func(point string) error {
				if point == "after_first_intent" {
					failures.Add(1)
					return &pgconn.PgError{Code: sqlstate, Message: "injected transactional retry exhaustion"}
				}
				return nil
			}}
			client := &recordingGitHubEventClient{sha: after, exists: true}
			processor, err := service.NewGitHubEventProcessorWithFanout(processorStore, client, 10, 10)
			if err != nil {
				t.Fatal(err)
			}
			config := gitHubEventIntegrationConfig("delivery-worker", 10, 10)
			config.InitialRetry, config.MaxRetry = time.Millisecond, time.Millisecond
			runner := NewGitHubEventRunner(config, integrationDiscardLogger(), dataStore, processor)
			enableTestWrites(runner)

			first := claimOneGitHubEventDelivery(t, dataStore, "delivery-worker")
			runDeliveryDirect(runner, t.Context(), first)
			if failures.Load() != 3 {
				t.Fatalf("transaction failures = %d, want 3", failures.Load())
			}
			if calls := client.callCounts(); calls != [5]int{0, 0, 0, 1, 0} {
				t.Fatalf("external calls installation/list/repository/ref/refresh = %v, want one branch read only", calls)
			}
			assertGitHubEventRetryState(t, pool, delivery.ID, 1, "database_unavailable")
			assertGitHubEventSnapshotCounts(t, pool, delivery.ID, 0, 0, 0, 0)
		})
	}
}

func TestGitHubEventDurableFanoutTwoRunnersAtConfiguredLimitPostgres18(t *testing.T) {
	dataStore, pool := newGitHubEventIntegrationFixture(t, "github_worker_fanout_limit")
	seedGitHubEventFanoutApps(t, pool, 1000, 30000)
	const ref = "refs/heads/feature/runner-load"
	before, after := strings.Repeat("0", 40), strings.Repeat("b", 40)
	delivery := receiveGitHubEventPush(t, dataStore, "40000000-0000-0000-0000-000000000021", ref, before, after)
	processor := newGitHubEventIntegrationProcessor(t, dataStore, ref, before, after, 1000, 50)
	claimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), "load-delivery", 20*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim delivery = %+v, %v", claimed, err)
	}
	outcome := processor.ProcessDelivery(t.Context(), claimed[0])
	if outcome.Disposition != service.GitHubEventComplete || outcome.Code != "fanout_staged" || outcome.IntentCount != 1000 {
		t.Fatalf("ProcessDelivery() = %+v", outcome)
	}
	deliveryMetrics := &fakeGitHubMetrics{}
	claimed[0].Lease = integrationCurrentDeliveryLease(t, pool, delivery.ID)
	if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, delivery.ID); err != nil {
		t.Fatal(err)
	}
	claimed[0].Lease = integrationCurrentDeliveryLease(t, pool, delivery.ID)
	deliveryRunner := NewGitHubEventRunner(gitHubEventIntegrationConfig("load-delivery", 1000, 50), integrationDiscardLogger(), dataStore, &fakeGitHubEventProcessor{deliveryOutcome: outcome}, deliveryMetrics)
	enableTestWrites(deliveryRunner)
	runDeliveryDirect(deliveryRunner, t.Context(), claimed[0])
	if deliveryMetrics.deliveryResult != "processed" {
		t.Fatalf("delivery metric = %q", deliveryMetrics.deliveryResult)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE github_webhook_deliveries SET status='completed',result_code='fanout_staged',terminal_at=clock_timestamp(),retain_until=clock_timestamp()+interval '1 hour',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, delivery.ID); err != nil {
		t.Fatal(err)
	}
	assertGitHubEventDelivery(t, pool, delivery.ID, "completed", 1, "fanout_staged")

	metrics := githubmetrics.New()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	runners := []*GitHubEventRunner{
		NewGitHubEventRunner(gitHubEventIntegrationConfig("load-fanout-one", 1000, 50), integrationDiscardLogger(), fanoutOnlyGitHubEventStore{Store: dataStore}, processor, metrics),
		NewGitHubEventRunner(gitHubEventIntegrationConfig("load-fanout-two", 1000, 50), integrationDiscardLogger(), fanoutOnlyGitHubEventStore{Store: dataStore}, processor, metrics),
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(runners))
	for _, runner := range runners {
		if err := runner.Startup(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(runner *GitHubEventRunner) {
			defer wg.Done()
			errs <- runner.Run(ctx)
		}(runner)
	}
	waitForGitHubEventFanout(t, pool, delivery.ID, 1000)
	time.Sleep(30 * time.Millisecond)
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertGitHubEventFanout(t, pool, delivery.ID, "completed", 1000, 1000, 1)
	assertGitHubEventIntentUniqueness(t, pool, delivery.ID, 1000)
	assertGitHubEventMetrics(t, metrics, 0, 20)
}

type persistentConflictClient struct{ status int }

func (c *persistentConflictClient) GetInstallation(context.Context, int64) (service.GitHubEventInstallation, error) {
	return service.GitHubEventInstallation{}, &githubadapter.APIError{StatusCode: c.status}
}
func (*persistentConflictClient) ListInstallationRepositories(context.Context, int64) ([]service.GitHubEventRepository, error) {
	return nil, nil
}
func (*persistentConflictClient) GetRepository(context.Context, int64, int64) (service.GitHubEventRepository, error) {
	return service.GitHubEventRepository{}, nil
}
func (*persistentConflictClient) GetBranchRef(context.Context, int64, int64, string) (string, bool, error) {
	return "", false, nil
}
func (*persistentConflictClient) RefreshInstallationCredentials(context.Context, int64) error {
	return nil
}

type githubEventBranchClient struct {
	sha    string
	exists bool
}

type recordingGitHubEventClient struct {
	mu                                           sync.Mutex
	sha                                          string
	exists                                       bool
	installation, list, repository, ref, refresh int
}

func (c *recordingGitHubEventClient) GetInstallation(context.Context, int64) (service.GitHubEventInstallation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installation++
	return service.GitHubEventInstallation{}, nil
}
func (c *recordingGitHubEventClient) ListInstallationRepositories(context.Context, int64) ([]service.GitHubEventRepository, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list++
	return nil, nil
}
func (c *recordingGitHubEventClient) GetRepository(context.Context, int64, int64) (service.GitHubEventRepository, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repository++
	return service.GitHubEventRepository{}, nil
}
func (c *recordingGitHubEventClient) GetBranchRef(context.Context, int64, int64, string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ref++
	return c.sha, c.exists, nil
}
func (c *recordingGitHubEventClient) RefreshInstallationCredentials(context.Context, int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh++
	return nil
}
func (c *recordingGitHubEventClient) callCounts() [5]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return [5]int{c.installation, c.list, c.repository, c.ref, c.refresh}
}

type snapshotFailpointGitHubEventStore struct {
	*store.Store
	failpoint func(string) error
}

func (s *snapshotFailpointGitHubEventStore) SnapshotGitHubPush(ctx context.Context, params store.SnapshotGitHubPushParams) (store.SnapshotGitHubPushResult, error) {
	params.Failpoint = s.failpoint
	return s.Store.SnapshotGitHubPush(ctx, params)
}

func (*githubEventBranchClient) GetInstallation(context.Context, int64) (service.GitHubEventInstallation, error) {
	return service.GitHubEventInstallation{}, nil
}
func (*githubEventBranchClient) ListInstallationRepositories(context.Context, int64) ([]service.GitHubEventRepository, error) {
	return nil, nil
}
func (*githubEventBranchClient) GetRepository(context.Context, int64, int64) (service.GitHubEventRepository, error) {
	return service.GitHubEventRepository{}, nil
}
func (c *githubEventBranchClient) GetBranchRef(context.Context, int64, int64, string) (string, bool, error) {
	return c.sha, c.exists, nil
}
func (*githubEventBranchClient) RefreshInstallationCredentials(context.Context, int64) error {
	return nil
}

type observedGitHubEventProcessor struct {
	GitHubEventProcessor
	fanoutDone chan<- struct{}
}

type fanoutOnlyGitHubEventStore struct{ *store.Store }

func (fanoutOnlyGitHubEventStore) ClaimGitHubRoutingIntents(context.Context, string, time.Duration, int) ([]store.GitHubRoutingIntent, error) {
	return nil, nil
}

type integrationGitHubEventProcessor struct {
	*service.GitHubEventProcessor
	dataStore              *store.Store
	ref, before, after     string
	maxFanout, fanoutBatch int
}

func (p *integrationGitHubEventProcessor) ProcessDelivery(ctx context.Context, delivery store.GitHubWebhookDelivery) service.GitHubEventOutcome {
	result, err := p.dataStore.SnapshotGitHubPush(ctx, store.SnapshotGitHubPushParams{DeliveryID: delivery.ID, DeliveryLease: *delivery.Lease, OrgID: delivery.OrgID, InstallationID: *delivery.InstallationID, RepositoryID: *delivery.RepositoryID, RepositoryNodeID: *delivery.RepositoryNodeID, Ref: p.ref, BeforeSHA: p.before, CommitSHA: p.after, AuthoritativeSHA: p.after, EventKind: "push", RefExists: true, ReceivedAt: delivery.ReceivedAt, ObservationSequence: delivery.IngestionSequence, MaxFanout: p.maxFanout, FanoutBatch: p.fanoutBatch})
	if err != nil {
		return service.GitHubEventOutcome{Disposition: service.GitHubEventFail, Code: "invalid_event"}
	}
	return service.GitHubEventOutcome{Disposition: service.GitHubEventComplete, Code: "fanout_staged", IntentCount: result.TotalCount}
}

func (p *observedGitHubEventProcessor) ProcessFanout(ctx context.Context, manifest store.GitHubRoutingFanoutManifest) service.GitHubEventOutcome {
	outcome := p.GitHubEventProcessor.ProcessFanout(ctx, manifest)
	p.fanoutDone <- struct{}{}
	return outcome
}

func newGitHubEventIntegrationFixture(t *testing.T, prefix string) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("CYCLOPUS_TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("POSTGRES_TEST_URL")
	}
	if databaseURL == "" {
		t.Fatal("CYCLOPUS_TEST_DATABASE_URL or POSTGRES_TEST_URL must point to PostgreSQL 18")
	}
	admin, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	var serverVersion int
	if err := admin.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::integer`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 || serverVersion >= 190000 {
		t.Fatalf("PostgreSQL server_version_num=%d, want 18.x", serverVersion)
	}
	schema := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE") })
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsedURL, err := url.Parse(config.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	query := parsedURL.Query()
	query.Set("search_path", pgx.Identifier{schema}.Sanitize())
	parsedURL.RawQuery = query.Encode()
	testURL := parsedURL.String()
	if err := store.Migrate(t.Context(), store.MigrationConfig{URL: testURL, LockTimeout: 5 * time.Second, Migrations: store.Migrations}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	queries := []string{
		`INSERT INTO orgs(id,name,slug) VALUES('10000000-0000-0000-0000-000000000001','Worker Fanout','worker-fanout')`,
		`INSERT INTO github_installations(github_installation_id,org_id,account_id,account_login,account_type,repository_selection,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(90001,'10000000-0000-0000-0000-000000000001',90002,'acme','Organization','selected','active',1,1)`,
		`INSERT INTO github_repositories(github_repository_id,org_id,github_installation_id,node_id,owner_login,name,full_name,visibility,default_branch,access_state,reconciliation_generation,last_api_confirmed_generation) VALUES(91001,'10000000-0000-0000-0000-000000000001',90001,'R_node','acme','hooks','acme/hooks','private','main','active',1,1)`,
		`INSERT INTO git_connections(id,org_id,github_repository_id,state,access_state) VALUES('10000000-0000-0000-0000-000000000009','10000000-0000-0000-0000-000000000001',91001,'active','active')`,
		`INSERT INTO projects(id,org_id,name,slug,git_connection_id) VALUES('10000000-0000-0000-0000-000000000010','10000000-0000-0000-0000-000000000001','Hooks','hooks','10000000-0000-0000-0000-000000000009')`,
		`INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch) VALUES('10000000-0000-0000-0000-000000000002','10000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000010','A','a','a','ghcr.io/acme/a','.github/workflows/deploy.yml','main'),('10000000-0000-0000-0000-000000000003','10000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000010','B','b','b','ghcr.io/acme/b','.github/workflows/deploy.yml','main')`,
		`INSERT INTO environments(id,org_id,app_id,branch,kind,namespace,planned_hostname) VALUES('10000000-0000-0000-0000-000000000004','10000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000002','main','production','hooks-production','hooks.example.test')`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	codec, err := store.NewAESGCMGitHubWebhookPayloadCodec("k1", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return store.New(pool).WithGitHubWebhookPayloadCodec(codec), pool
}

func claimOneGitHubEventDelivery(t *testing.T, dataStore *store.Store, owner string) store.GitHubWebhookDelivery {
	t.Helper()
	claimed, err := dataStore.ClaimGitHubWebhookDeliveries(t.Context(), owner, 20*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim delivery = %+v, %v", claimed, err)
	}
	return claimed[0]
}

func claimOneGitHubEventFanout(t *testing.T, dataStore *store.Store, owner string) store.GitHubRoutingFanoutManifest {
	t.Helper()
	claimed, err := dataStore.ClaimGitHubRoutingFanoutManifests(t.Context(), owner, 20*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim fanout = %+v, %v", claimed, err)
	}
	return claimed[0]
}

func assertGitHubEventRetryState(t *testing.T, pool *pgxpool.Pool, id string, wantAttempts int, wantCode string) {
	t.Helper()
	var status, code string
	var attempts int
	var owner *string
	if err := pool.QueryRow(t.Context(), `SELECT status,attempts,error_code,lease_owner FROM github_webhook_deliveries WHERE id=$1`, id).Scan(&status, &attempts, &code, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != wantAttempts || code != wantCode || owner != nil {
		t.Fatalf("retry state = %s/%d/%s owner=%v, want pending/%d/%s owner=nil", status, attempts, code, owner, wantAttempts, wantCode)
	}
}

func assertGitHubEventSnapshotCounts(t *testing.T, pool *pgxpool.Pool, deliveryID string, observations, manifests, candidates, audits int) {
	t.Helper()
	var gotObservations, gotManifests, gotCandidates, gotAudits int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_ref_transition_observations WHERE delivery_id=$1`, deliveryID).Scan(&gotObservations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_routing_fanout_manifests WHERE delivery_id=$1`, deliveryID).Scan(&gotManifests); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM github_routing_fanout_candidates candidate JOIN github_routing_fanout_manifests manifest ON manifest.id=candidate.manifest_id WHERE manifest.delivery_id=$1`, deliveryID).Scan(&gotCandidates); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events`).Scan(&gotAudits); err != nil {
		t.Fatal(err)
	}
	if gotObservations != observations || gotManifests != manifests || gotCandidates != candidates || gotAudits != audits {
		t.Fatalf("snapshot observations/manifests/candidates/audits = %d/%d/%d/%d, want %d/%d/%d/%d", gotObservations, gotManifests, gotCandidates, gotAudits, observations, manifests, candidates, audits)
	}
}

func seedGitHubEventFanoutApps(t *testing.T, pool *pgxpool.Pool, count, offset int) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO apps(id,org_id,project_id,name,slug,hostname_label,image_repository,github_workflow_path,production_branch,previews_enabled)
		SELECT ('50000000-0000-0000-0000-'||lpad((n+$1)::text,12,'0'))::uuid,'10000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000010','Fanout '||n,'fanout-'||(n+$1),'fanout-'||(n+$1),'ghcr.io/acme/fanout-'||(n+$1),'.github/workflows/deploy.yml','main',true FROM generate_series(1,$2) n`, offset, count); err != nil {
		t.Fatal(err)
	}
}

func receiveGitHubEventPush(t *testing.T, dataStore *store.Store, deliveryID, ref, before, after string) store.GitHubWebhookDelivery {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"ref":%q,"before":%q,"after":%q,"installation":{"id":90001},"repository":{"id":91001,"node_id":"R_node"}}`, ref, before, after))
	digest := sha256.Sum256(payload)
	installationID, repositoryID, repositoryNodeID := int64(90001), int64(91001), "R_node"
	disposition, delivery, err := dataStore.ReceiveGitHubWebhook(t.Context(), store.ReceiveGitHubWebhookParams{DeliveryID: deliveryID, Event: "push", OrgID: "10000000-0000-0000-0000-000000000001", PayloadDigest: hex.EncodeToString(digest[:]), SignatureKeyVersion: "hmac-v1", Payload: payload, InstallationID: &installationID, RepositoryID: &repositoryID, RepositoryNodeID: &repositoryNodeID})
	if err != nil || disposition != store.GitHubWebhookAccepted {
		t.Fatalf("receive push = %q %+v, %v", disposition, delivery, err)
	}
	return delivery
}

func newGitHubEventIntegrationProcessor(t *testing.T, dataStore *store.Store, ref, before, after string, maxFanout, fanoutBatch int) *integrationGitHubEventProcessor {
	t.Helper()
	processor, err := service.NewGitHubEventProcessorWithFanout(dataStore, &githubEventBranchClient{sha: after, exists: true}, maxFanout, fanoutBatch)
	if err != nil {
		t.Fatal(err)
	}
	return &integrationGitHubEventProcessor{GitHubEventProcessor: processor, dataStore: dataStore, ref: ref, before: before, after: after, maxFanout: maxFanout, fanoutBatch: fanoutBatch}
}

func gitHubEventIntegrationConfig(owner string, maxFanout, fanoutBatch int) GitHubEventConfig {
	return GitHubEventConfig{ScanInterval: 10 * time.Millisecond, LeaseDuration: 20 * time.Second, ProcessingTimeout: 10 * time.Second, InitialRetry: 10 * time.Millisecond, MaxRetry: time.Second, PayloadRetention: time.Hour, CleanupInterval: time.Minute, ShutdownGrace: 10 * time.Second, DeliveryConcurrency: 1, FanoutConcurrency: 1, IntentConcurrency: 1, MaxAttempts: 12, CleanupBatch: 100, MaxFanout: maxFanout, FanoutBatch: fanoutBatch, MaxIntents: maxFanout, Owner: owner}
}

func assertGitHubEventDelivery(t *testing.T, pool *pgxpool.Pool, id, wantStatus string, wantAttempts int, wantResult string) {
	t.Helper()
	var status, result string
	var attempts int
	if err := pool.QueryRow(t.Context(), `SELECT status,attempts,result_code FROM github_webhook_deliveries WHERE id=$1`, id).Scan(&status, &attempts, &result); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || attempts != wantAttempts || result != wantResult {
		t.Fatalf("delivery state = %s/%d/%s, want %s/%d/%s", status, attempts, result, wantStatus, wantAttempts, wantResult)
	}
}

func integrationCurrentDeliveryLease(t *testing.T, pool *pgxpool.Pool, id string) *store.LeaseToken {
	t.Helper()
	var lease store.LeaseToken
	if err := pool.QueryRow(t.Context(), `SELECT lease_owner,lease_epoch,lease_expires_at FROM github_webhook_deliveries WHERE id=$1`, id).Scan(&lease.Owner, &lease.Epoch, &lease.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	return &lease
}

func assertGitHubEventFanout(t *testing.T, pool *pgxpool.Pool, deliveryID, wantStatus string, wantTotal, wantInserted, wantAttempts int) {
	t.Helper()
	var status string
	var total, inserted, attempts int
	if err := pool.QueryRow(t.Context(), `SELECT status,total_count,inserted_count,attempts FROM github_routing_fanout_manifests WHERE delivery_id=$1`, deliveryID).Scan(&status, &total, &inserted, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || total != wantTotal || inserted != wantInserted || attempts != wantAttempts {
		t.Fatalf("fanout state = %s/%d/%d attempts=%d, want %s/%d/%d attempts=%d", status, total, inserted, attempts, wantStatus, wantTotal, wantInserted, wantAttempts)
	}
}

func waitForGitHubEventFanout(t *testing.T, pool *pgxpool.Pool, deliveryID string, want int) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		var inserted int
		if err := pool.QueryRow(t.Context(), `SELECT status,inserted_count FROM github_routing_fanout_manifests WHERE delivery_id=$1`, deliveryID).Scan(&status, &inserted); err != nil {
			t.Fatal(err)
		}
		if status == "completed" && inserted == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var status string
	var inserted, attempts int
	var code, detail *string
	if err := pool.QueryRow(t.Context(), `SELECT status,inserted_count,attempts,error_code,error_detail FROM github_routing_fanout_manifests WHERE delivery_id=$1`, deliveryID).Scan(&status, &inserted, &attempts, &code, &detail); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("fanout for delivery %s did not complete with %d intents: status=%s inserted=%d attempts=%d code=%v detail=%v", deliveryID, want, status, inserted, attempts, code, detail)
}

func assertGitHubEventIntentUniqueness(t *testing.T, pool *pgxpool.Pool, deliveryID string, want int) {
	t.Helper()
	var total, unique int
	if err := pool.QueryRow(t.Context(), `SELECT count(*),count(DISTINCT app_id) FROM github_routing_intents WHERE delivery_id=$1`, deliveryID).Scan(&total, &unique); err != nil {
		t.Fatal(err)
	}
	if total != want || unique != want {
		t.Fatalf("intent total/unique = %d/%d, want %d/%d", total, unique, want, want)
	}
}

func assertGitHubEventMetrics(t *testing.T, metrics *githubmetrics.Metrics, wantDeliveries, wantFanouts int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	wants := []string{
		fmt.Sprintf(`cyclopus_github_fanout_total{event="push",result="processed"} %d`, wantFanouts),
		`cyclopus_github_leases{kind="delivery",state="active"} 0`,
		`cyclopus_github_leases{kind="delivery",state="delayed"} 0`,
		`cyclopus_github_leases{kind="fanout",state="active"} 0`,
		`cyclopus_github_leases{kind="fanout",state="delayed"} 0`,
		`cyclopus_github_leases{kind="intent",state="active"} 0`,
		`cyclopus_github_leases{kind="intent",state="delayed"} 0`,
	}
	if wantDeliveries > 0 {
		wants = append(wants, fmt.Sprintf(`cyclopus_github_delivery_total{event="push",action="",result="processed"} %d`, wantDeliveries))
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics do not contain %q:\n%s", want, body)
		}
	}
}

func integrationDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr[T any](value T) *T { return &value }
