package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	webhookOneID = "00000000-0000-0000-0000-0000000000b1"
	webhookTwoID = "00000000-0000-0000-0000-0000000000b2"
)

func seedWebhook(t *testing.T, pool *pgxpool.Pool, id, orgID string, events []string) {
	t.Helper()
	if events == nil {
		events = []string{}
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO webhooks(id,org_id,target_url,events,secret_ref)
		VALUES($1,$2,'https://hooks.example.com/webhook',$3,'k8s-secret://cyclopus/hooks#secret')`,
		id, orgID, events); err != nil {
		t.Fatal(err)
	}
}

func seedWebhookDelivery(t *testing.T, pool *pgxpool.Pool, id, orgID, webhookID, event string, attempt int) {
	t.Helper()
	raw := json.RawMessage(`{"event":"` + event + `","id":"` + id + `"}`)
	if _, err := pool.Exec(context.Background(), `INSERT INTO webhook_deliveries
		(id,org_id,webhook_id,event,payload,raw_body,target_url,secret_ref,attempt)
		VALUES($1,$2,$3,$4,$5::jsonb,$6,'https://hooks.example.com/webhook','k8s-secret://cyclopus/hooks#secret',$7)`,
		id, orgID, webhookID, event, string(raw), raw, attempt); err != nil {
		t.Fatal(err)
	}
}

func forceWebhookDeliveryDue(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE webhook_deliveries SET next_attempt_at=now()-interval '1 second'
		WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func assertWebhookNextAttemptDelay(t *testing.T, pool *pgxpool.Pool, id string, want time.Duration) {
	t.Helper()
	var seconds float64
	if err := pool.QueryRow(context.Background(), `SELECT extract(epoch FROM (next_attempt_at - now()))
		FROM webhook_deliveries WHERE id=$1`, id).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	if math.Abs(seconds-want.Seconds()) > 2 {
		t.Fatalf("next_attempt_at delay = %.1fs, want %s", seconds, want)
	}
}

func webhookDeliveryID(suffix string) string {
	return "00000000-0000-0000-0000-000000000" + suffix
}

func TestWebhookRetryDelayValues(t *testing.T) {
	delays := []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}
	for index, want := range delays {
		got, ok := WebhookRetryDelay(index + 1)
		if !ok || got != want {
			t.Fatalf("WebhookRetryDelay(%d) = %s, %v, want %s", index+1, got, ok, want)
		}
	}
	for _, attempt := range []int{-1, 0, 7, 8} {
		if got, ok := WebhookRetryDelay(attempt); ok {
			t.Fatalf("WebhookRetryDelay(%d) = %s, true, want no retry", attempt, got)
		}
	}
}

func TestWebhookRetryBackoffScheduleAndExhaustionIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedWebhook(t, pool, webhookOneID, orgOneID, []string{"deploy.succeeded"})
	deliveryID := webhookDeliveryID("c01")
	seedWebhookDelivery(t, pool, deliveryID, orgOneID, webhookOneID, "deploy.succeeded", 0)
	ctx := context.Background()

	delays := []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}
	for send := 1; send <= 7; send++ {
		delivery, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute)
		if err != nil {
			t.Fatalf("claim send %d error = %v", send, err)
		}
		if delivery.ID != deliveryID || delivery.Attempt != send-1 || delivery.Fence != int64(send) ||
			delivery.LeaseOwner != "worker-one" || delivery.LeaseExpiresAt.Before(time.Now()) {
			t.Fatalf("claim send %d = %+v", send, delivery)
		}
		if _, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-two", time.Minute); !errors.Is(err, ErrNotFound) {
			t.Fatalf("double claim send %d error = %v, want ErrNotFound", send, err)
		}
		outcome, err := dataStore.CompleteWebhookAttempt(ctx, delivery, WebhookAttemptResult{StatusCode: 500, Error: "receiver down"})
		if err != nil || outcome.EndpointDisabled {
			t.Fatalf("complete send %d = %+v, %v", send, outcome, err)
		}
		if send < 7 {
			assertWebhookNextAttemptDelay(t, pool, deliveryID, delays[send-1])
			var statusCode int
			var lastError string
			if err := pool.QueryRow(ctx, `SELECT last_status_code, last_error FROM webhook_deliveries WHERE id=$1`,
				deliveryID).Scan(&statusCode, &lastError); err != nil || statusCode != 500 || lastError != "receiver down" {
				t.Fatalf("send %d last status/error = %d/%q, %v", send, statusCode, lastError, err)
			}
			forceWebhookDeliveryDue(t, pool, deliveryID)
		}
	}

	var abandonedAt *time.Time
	var reason string
	var consecutive int
	var disabledAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT delivery.abandoned_at, delivery.abandoned_reason,
		endpoint.consecutive_failures, endpoint.disabled_at FROM webhook_deliveries delivery
		JOIN webhooks endpoint ON endpoint.id=delivery.webhook_id WHERE delivery.id=$1`,
		deliveryID).Scan(&abandonedAt, &reason, &consecutive, &disabledAt); err != nil {
		t.Fatal(err)
	}
	if abandonedAt == nil || reason != "retry_exhausted" || consecutive != 1 || disabledAt != nil {
		t.Fatalf("exhausted state = abandoned:%v reason:%q failures:%d disabled:%v", abandonedAt, reason, consecutive, disabledAt)
	}
	if _, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim after abandonment error = %v, want ErrNotFound", err)
	}
}

func TestWebhookRetrySuccessResetsConsecutiveFailuresIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedWebhook(t, pool, webhookOneID, orgOneID, nil)
	deliveryID := webhookDeliveryID("c02")
	seedWebhookDelivery(t, pool, deliveryID, orgOneID, webhookOneID, "deploy.succeeded", 0)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE webhooks SET consecutive_failures=3 WHERE id=$1`, webhookOneID); err != nil {
		t.Fatal(err)
	}

	delivery, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.PersistWebhookSignature(ctx, delivery, 1700000456, "sha256="+strings.Repeat("cd", 32)); err != nil {
		t.Fatal(err)
	}
	outcome, err := dataStore.CompleteWebhookAttempt(ctx, delivery, WebhookAttemptResult{StatusCode: 200})
	if err != nil || !outcome.Delivered || outcome.EndpointDisabled {
		t.Fatalf("CompleteWebhookAttempt() = %+v, %v", outcome, err)
	}
	var deliveredAt *time.Time
	var statusCode int
	var consecutive int
	if err := pool.QueryRow(ctx, `SELECT delivery.delivered_at, delivery.last_status_code, endpoint.consecutive_failures
		FROM webhook_deliveries delivery JOIN webhooks endpoint ON endpoint.id=delivery.webhook_id WHERE delivery.id=$1`,
		deliveryID).Scan(&deliveredAt, &statusCode, &consecutive); err != nil {
		t.Fatal(err)
	}
	if deliveredAt == nil || statusCode != 200 || consecutive != 0 {
		t.Fatalf("success state = delivered:%v status:%d failures:%d", deliveredAt, statusCode, consecutive)
	}
	if _, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim after delivery error = %v, want ErrNotFound", err)
	}
}

func TestWebhookRetryEndpointDisabledAfterTwentyExhaustedDeliveriesIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedWebhook(t, pool, webhookOneID, orgOneID, nil)
	ctx := context.Background()

	for index := 1; index <= 20; index++ {
		deliveryID := webhookDeliveryID(fmt.Sprintf("c%02d", 10+index))
		seedWebhookDelivery(t, pool, deliveryID, orgOneID, webhookOneID, "deploy.failed", 6)
		delivery, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute)
		if err != nil || delivery.Attempt != 6 {
			t.Fatalf("claim %d = %+v, %v", index, delivery, err)
		}
		outcome, err := dataStore.CompleteWebhookAttempt(ctx, delivery, WebhookAttemptResult{StatusCode: 503, Error: "unavailable"})
		if err != nil {
			t.Fatalf("complete %d error = %v", index, err)
		}
		if !outcome.Abandoned || outcome.Reason != "retry_exhausted" {
			t.Fatalf("complete %d = %+v, want abandoned retry_exhausted", index, outcome)
		}
		if index < 20 && outcome.EndpointDisabled {
			t.Fatalf("endpoint disabled after %d exhausted deliveries", index)
		}
		if index == 20 && !outcome.EndpointDisabled {
			t.Fatal("endpoint not disabled after 20 exhausted deliveries")
		}
	}
	var consecutive int
	var disabledAt time.Time
	if err := pool.QueryRow(ctx, `SELECT consecutive_failures, disabled_at FROM webhooks WHERE id=$1`,
		webhookOneID).Scan(&consecutive, &disabledAt); err != nil {
		t.Fatal(err)
	}
	if consecutive != 20 || disabledAt.IsZero() {
		t.Fatalf("endpoint state = failures:%d disabled:%v", consecutive, disabledAt)
	}

	repeatID := webhookDeliveryID("c31")
	seedWebhookDelivery(t, pool, repeatID, orgOneID, webhookOneID, "deploy.failed", 6)
	orgIDs, err := dataStore.ListWebhookOrgIDs(ctx)
	if err != nil || len(orgIDs) != 1 || orgIDs[0] != orgOneID {
		t.Fatalf("ListWebhookOrgIDs() = %v, %v", orgIDs, err)
	}
	if _, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim for disabled endpoint error = %v, want ErrNotFound", err)
	}
	var abandonedAt *time.Time
	var reason string
	if err := pool.QueryRow(ctx, `SELECT abandoned_at, abandoned_reason FROM webhook_deliveries WHERE id=$1`,
		repeatID).Scan(&abandonedAt, &reason); err != nil {
		t.Fatal(err)
	}
	if abandonedAt == nil || reason != "endpoint_disabled" {
		t.Fatalf("disabled endpoint pending delivery = abandoned:%v reason:%q", abandonedAt, reason)
	}
	if orgIDs, err := dataStore.ListWebhookOrgIDs(ctx); err != nil || len(orgIDs) != 0 {
		t.Fatalf("ListWebhookOrgIDs() after sweep = %v, %v", orgIDs, err)
	}

	// A delivery still under an active lease is not abandoned by the disabled-endpoint sweep.
	if _, err := pool.Exec(ctx, `UPDATE webhooks SET disabled_at=NULL WHERE id=$1`, webhookOneID); err != nil {
		t.Fatal(err)
	}
	leasedID := webhookDeliveryID("c33")
	seedWebhookDelivery(t, pool, leasedID, orgOneID, webhookOneID, "deploy.failed", 0)
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET lease_owner='other-worker',lease_expires_at=now()+interval '10 minutes' WHERE id=$1`, leasedID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhooks SET disabled_at=now() WHERE id=$1`, webhookOneID); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim with actively leased delivery error = %v, want ErrNotFound", err)
	}
	var leasedAbandonedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT abandoned_at FROM webhook_deliveries WHERE id=$1`, leasedID).Scan(&leasedAbandonedAt); err != nil {
		t.Fatal(err)
	}
	if leasedAbandonedAt != nil {
		t.Fatalf("actively leased delivery abandoned by disabled sweep: %v", leasedAbandonedAt)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, leasedID); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim with expired-lease delivery error = %v, want ErrNotFound", err)
	}
	if err := pool.QueryRow(ctx, `SELECT abandoned_at FROM webhook_deliveries WHERE id=$1`, leasedID).Scan(&leasedAbandonedAt); err != nil {
		t.Fatal(err)
	}
	if leasedAbandonedAt == nil {
		t.Fatal("expired-lease delivery was not abandoned by disabled sweep")
	}

	// Manual re-enable (operator action) resets the disable; the next exhausted
	// delivery disables the endpoint again with a fresh timestamp.
	if _, err := pool.Exec(ctx, `UPDATE webhooks SET disabled_at=NULL,consecutive_failures=19 WHERE id=$1`, webhookOneID); err != nil {
		t.Fatal(err)
	}
	reenabledID := webhookDeliveryID("c32")
	seedWebhookDelivery(t, pool, reenabledID, orgOneID, webhookOneID, "deploy.failed", 6)
	delivery, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute)
	if err != nil || delivery.ID != reenabledID {
		t.Fatalf("re-enabled claim = %+v, %v", delivery, err)
	}
	outcome, err := dataStore.CompleteWebhookAttempt(ctx, delivery, WebhookAttemptResult{StatusCode: 500, Error: "still down"})
	if err != nil || !outcome.EndpointDisabled {
		t.Fatalf("re-disable = %+v, %v", outcome, err)
	}
	var secondDisabledAt time.Time
	if err := pool.QueryRow(ctx, `SELECT disabled_at FROM webhooks WHERE id=$1`, webhookOneID).Scan(&secondDisabledAt); err != nil {
		t.Fatal(err)
	}
	if secondDisabledAt.Equal(disabledAt) {
		t.Fatalf("disabled_at was not refreshed after manual re-enable: %v", secondDisabledAt)
	}
}

func TestWebhookRetryCrashClaimCyclesDoNotConsumeSendBudgetIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedWebhook(t, pool, webhookOneID, orgOneID, nil)
	deliveryID := webhookDeliveryID("c70")
	seedWebhookDelivery(t, pool, deliveryID, orgOneID, webhookOneID, "deploy.succeeded", 0)
	ctx := context.Background()

	// Seven claim-then-crash cycles (no send recorded) must not exhaust the
	// retry budget: the delivery remains claimable and unabandoned.
	for cycle := 1; cycle <= 7; cycle++ {
		delivery, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "crashed-worker", time.Minute)
		if err != nil || delivery.Attempt != 0 || delivery.Fence != int64(cycle) {
			t.Fatalf("crash-cycle claim %d = %+v, %v", cycle, delivery, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, deliveryID); err != nil {
			t.Fatal(err)
		}
	}
	var abandonedAt *time.Time
	var attempt int
	if err := pool.QueryRow(ctx, `SELECT abandoned_at, attempt FROM webhook_deliveries WHERE id=$1`,
		deliveryID).Scan(&abandonedAt, &attempt); err != nil {
		t.Fatal(err)
	}
	if abandonedAt != nil || attempt != 0 {
		t.Fatalf("crash cycles consumed send budget: abandoned=%v attempt=%d", abandonedAt, attempt)
	}

	// The first real send still follows the initial retry delay.
	delivery, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "worker-one", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.CompleteWebhookAttempt(ctx, delivery, WebhookAttemptResult{StatusCode: 500, Error: "receiver down"}); err != nil {
		t.Fatal(err)
	}
	assertWebhookNextAttemptDelay(t, pool, deliveryID, 10*time.Second)
}

func TestWebhookRetryConcurrentDoubleClaimUsesSkipLockedIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedWebhook(t, pool, webhookOneID, orgOneID, nil)
	seedWebhookDelivery(t, pool, webhookDeliveryID("c40"), orgOneID, webhookOneID, "deploy.succeeded", 0)

	results := make(chan error, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := dataStore.ClaimWebhookDelivery(context.Background(), orgOneID, "worker", time.Minute)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	claimed, notFound := 0, 0
	for err := range results {
		if err == nil {
			claimed++
		} else if errors.Is(err, ErrNotFound) {
			notFound++
		} else {
			t.Fatal(err)
		}
	}
	if claimed != 1 || notFound != 1 {
		t.Fatalf("concurrent claims = claimed:%d not-found:%d, want 1 each", claimed, notFound)
	}
}

func TestWebhookRetryExpiredLeaseReclaimIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedWebhook(t, pool, webhookOneID, orgOneID, nil)
	deliveryID := webhookDeliveryID("c50")
	seedWebhookDelivery(t, pool, deliveryID, orgOneID, webhookOneID, "deploy.succeeded", 0)
	ctx := context.Background()

	stale, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "crashed-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	recovered, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "replacement-worker", time.Minute)
	if err != nil {
		t.Fatalf("expired lease reclaim error = %v", err)
	}
	if recovered.ID != stale.ID || recovered.Attempt != stale.Attempt || recovered.Fence != stale.Fence+1 ||
		recovered.LeaseOwner != "replacement-worker" {
		t.Fatalf("reclaimed delivery = %+v, stale = %+v", recovered, stale)
	}
	if _, err := dataStore.CompleteWebhookAttempt(ctx, stale, WebhookAttemptResult{StatusCode: 200}); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale lease completion error = %v, want ErrLeaseNotOwned", err)
	}
	if err := dataStore.PersistWebhookSignature(ctx, recovered, 1700000999, "sha256="+strings.Repeat("ef", 32)); err != nil {
		t.Fatal(err)
	}
	if outcome, err := dataStore.CompleteWebhookAttempt(ctx, recovered, WebhookAttemptResult{StatusCode: 200}); err != nil || !outcome.Delivered || outcome.EndpointDisabled {
		t.Fatalf("recovered lease completion = %+v, %v", outcome, err)
	}
}

func TestWebhookRecoveryReclaimPreservesPersistedSignatureIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedWebhook(t, pool, webhookOneID, orgOneID, nil)
	deliveryID := webhookDeliveryID("c60")
	seedWebhookDelivery(t, pool, deliveryID, orgOneID, webhookOneID, "deploy.succeeded", 0)
	ctx := context.Background()

	claimed, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "crashed-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.FirstSendTimestamp != nil || claimed.Signature != nil {
		t.Fatalf("first claim carried signature state: %+v", claimed)
	}
	timestamp := int64(1700000123)
	signature := "sha256=" + strings.Repeat("ab", 32)
	if err := dataStore.PersistWebhookSignature(ctx, claimed, timestamp, signature); err != nil {
		t.Fatalf("PersistWebhookSignature() error = %v", err)
	}
	if err := dataStore.PersistWebhookSignature(ctx, claimed, timestamp+1, signature); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("second PersistWebhookSignature() error = %v, want ErrLeaseNotOwned", err)
	}
	staleFence := claimed
	staleFence.Fence--
	if err := dataStore.PersistWebhookSignature(ctx, staleFence, timestamp, signature); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("stale fence PersistWebhookSignature() error = %v, want ErrLeaseNotOwned", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	recovered, err := dataStore.ClaimWebhookDelivery(ctx, orgOneID, "replacement-worker", time.Minute)
	if err != nil {
		t.Fatalf("reclaim error = %v", err)
	}
	if recovered.ID != claimed.ID || recovered.Attempt != claimed.Attempt || recovered.Fence != claimed.Fence+1 ||
		recovered.FirstSendTimestamp == nil || *recovered.FirstSendTimestamp != timestamp ||
		recovered.Signature == nil || *recovered.Signature != signature {
		t.Fatalf("reclaimed delivery = %+v", recovered)
	}
	if string(recovered.RawBody) != `{"event":"deploy.succeeded","id":"`+deliveryID+`"}` {
		t.Fatalf("reclaimed raw body = %q", recovered.RawBody)
	}
	if outcome, err := dataStore.CompleteWebhookAttempt(ctx, recovered, WebhookAttemptResult{StatusCode: 204}); err != nil || !outcome.Delivered || outcome.EndpointDisabled {
		t.Fatalf("recovered completion = %+v, %v", outcome, err)
	}
	var persistedTimestamp int64
	var persistedSignature string
	if err := pool.QueryRow(ctx, `SELECT first_send_timestamp, signature FROM webhook_deliveries WHERE id=$1`,
		deliveryID).Scan(&persistedTimestamp, &persistedSignature); err != nil {
		t.Fatal(err)
	}
	if persistedTimestamp != timestamp || persistedSignature != signature {
		t.Fatalf("persisted signature state = %d/%q", persistedTimestamp, persistedSignature)
	}
}

func TestWebhookRecoveryClaimValidationIntegration(t *testing.T) {
	dataStore := New(nil)
	if _, err := dataStore.ClaimWebhookDelivery(context.Background(), orgOneID, "worker", 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ClaimWebhookDelivery(0 lease) error = %v, want ErrInvalidArgument", err)
	}
}

func TestWebhookEnqueueEventIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	seedWebhook(t, pool, webhookOneID, orgOneID, []string{"deploy.succeeded"})
	seedWebhook(t, pool, webhookTwoID, orgOneID, nil)
	disabledWebhookID := "00000000-0000-0000-0000-0000000000b3"
	seedWebhook(t, pool, disabledWebhookID, orgOneID, []string{"deploy.succeeded"})
	if _, err := pool.Exec(context.Background(), `UPDATE webhooks SET disabled_at=now() WHERE id=$1`, disabledWebhookID); err != nil {
		t.Fatal(err)
	}
	otherEventWebhookID := "00000000-0000-0000-0000-0000000000b4"
	seedWebhook(t, pool, otherEventWebhookID, orgOneID, []string{"environment.created"})

	ctx := context.Background()
	err := dataStore.withTx(ctx, func(tx pgx.Tx) error {
		return enqueueWebhookEvent(ctx, tx, orgOneID, "deploy.succeeded", environmentOneID, stringPointer(releaseOneID), map[string]any{"reason": "test"})
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, `SELECT id::text, webhook_id::text, event, raw_body FROM webhook_deliveries ORDER BY webhook_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct{ id, webhookID, event string }
	events := map[string]row{}
	for rows.Next() {
		var value row
		var raw []byte
		if err := rows.Scan(&value.id, &value.webhookID, &value.event, &raw); err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["id"] != value.id || payload["event"] != "deploy.succeeded" ||
			payload["organization_id"] != orgOneID || payload["environment_id"] != environmentOneID ||
			payload["release_id"] != releaseOneID || payload["timestamp"] == nil {
			t.Fatalf("payload = %v for delivery %s", payload, value.id)
		}
		events[value.webhookID] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[webhookOneID].event != "deploy.succeeded" || events[webhookTwoID].event != "deploy.succeeded" {
		t.Fatalf("enqueued deliveries = %+v, want subscribed plus all-events endpoints only", events)
	}
	if orgIDs, err := dataStore.ListWebhookOrgIDs(ctx); err != nil || len(orgIDs) != 1 || orgIDs[0] != orgOneID {
		t.Fatalf("ListWebhookOrgIDs() = %v, %v", orgIDs, err)
	}
}

func TestWebhookStoreCRUDIntegration(t *testing.T) {
	dataStore, pool := newIntegrationStore(t)
	seedTenant(t, pool, tenantOne())
	ctx := context.Background()

	claim := func(scope, key string) CompleteIdempotencyParams {
		t.Helper()
		hash := requestHash(key)
		if _, err := dataStore.ClaimIdempotency(ctx, IdempotencyClaimParams{
			OrgID: orgOneID, Scope: scope, Key: key, RequestHash: hash,
			Owner: "owner", LeaseDuration: time.Minute, Retention: time.Hour,
		}); err != nil {
			t.Fatal(err)
		}
		return CompleteIdempotencyParams{OrgID: orgOneID, Scope: scope, Key: key, RequestHash: hash, Owner: "owner",
			Status: "completed", StatusCode: 200, Body: json.RawMessage(`{"ok":true}`)}
	}

	created, err := dataStore.CreateWebhook(ctx, SaveWebhookParams{
		Webhook: Webhook{ID: webhookOneID, OrgID: orgOneID, TargetURL: "https://hooks.example.com/one",
			Events: []string{"deploy.succeeded"}, SecretRef: "k8s-secret://cyclopus/hooks#one"},
		Idempotency: claim("webhook.create", "create-1"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000a1", "webhook.created", webhookOneID),
	})
	if err != nil || created.ID != webhookOneID || created.DisabledAt != nil || created.ConsecutiveFailures != 0 {
		t.Fatalf("CreateWebhook() = %+v, %v", created, err)
	}
	fetched, err := dataStore.GetWebhook(ctx, orgOneID, webhookOneID)
	if err != nil || fetched.TargetURL != "https://hooks.example.com/one" || len(fetched.Events) != 1 {
		t.Fatalf("GetWebhook() = %+v, %v", fetched, err)
	}
	listed, err := dataStore.ListWebhooks(ctx, orgOneID)
	if err != nil || len(listed) != 1 || listed[0].ID != webhookOneID {
		t.Fatalf("ListWebhooks() = %+v, %v", listed, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE webhooks SET consecutive_failures=7 WHERE id=$1`, webhookOneID); err != nil {
		t.Fatal(err)
	}
	updated, err := dataStore.UpdateWebhook(ctx, SaveWebhookParams{
		Webhook: Webhook{ID: webhookOneID, OrgID: orgOneID, TargetURL: "https://hooks.example.com/two",
			Events: []string{"deploy.failed", "rollback.completed"}, SecretRef: "k8s-secret://cyclopus/hooks#two"},
		Idempotency: claim("webhook.update", "update-1"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000a2", "webhook.updated", webhookOneID),
	}, false)
	if err != nil || updated.DisabledAt == nil || updated.TargetURL != "https://hooks.example.com/two" || len(updated.Events) != 2 {
		t.Fatalf("UpdateWebhook(disable) = %+v, %v", updated, err)
	}
	if updated.ConsecutiveFailures != 7 {
		t.Fatalf("UpdateWebhook(disable) consecutive failures = %d, want 7 preserved", updated.ConsecutiveFailures)
	}
	reenabled, err := dataStore.UpdateWebhook(ctx, SaveWebhookParams{
		Webhook: Webhook{ID: webhookOneID, OrgID: orgOneID, TargetURL: "https://hooks.example.com/two",
			Events: []string{"deploy.failed", "rollback.completed"}, SecretRef: "k8s-secret://cyclopus/hooks#two"},
		Idempotency: claim("webhook.update", "update-2"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000a3", "webhook.updated", webhookOneID),
	}, true)
	if err != nil || reenabled.DisabledAt != nil || reenabled.ConsecutiveFailures != 0 {
		t.Fatalf("UpdateWebhook(enable) = %+v, %v", reenabled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhooks SET consecutive_failures=5 WHERE id=$1`, webhookOneID); err != nil {
		t.Fatal(err)
	}
	touched, err := dataStore.UpdateWebhook(ctx, SaveWebhookParams{
		Webhook: Webhook{ID: webhookOneID, OrgID: orgOneID, TargetURL: "https://hooks.example.com/three",
			Events: []string{"deploy.failed"}, SecretRef: "k8s-secret://cyclopus/hooks#three"},
		Idempotency: claim("webhook.update", "update-3"),
		Audit:       testAudit("00000000-0000-0000-0000-0000000000a5", "webhook.updated", webhookOneID),
	}, true)
	if err != nil || touched.DisabledAt != nil || touched.TargetURL != "https://hooks.example.com/three" {
		t.Fatalf("UpdateWebhook(enabled edit) = %+v, %v", touched, err)
	}
	if touched.ConsecutiveFailures != 5 {
		t.Fatalf("UpdateWebhook(enabled edit) consecutive failures = %d, want 5 preserved", touched.ConsecutiveFailures)
	}

	if err := dataStore.DeleteWebhook(ctx, orgOneID, webhookOneID, claim("webhook.delete", "delete-1"),
		testAudit("00000000-0000-0000-0000-0000000000a4", "webhook.deleted", webhookOneID)); err != nil {
		t.Fatalf("DeleteWebhook() error = %v", err)
	}
	if _, err := dataStore.GetWebhook(ctx, orgOneID, webhookOneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetWebhook() after delete error = %v, want ErrNotFound", err)
	}
	assertCount(t, pool, "audit_events", 5)
}
