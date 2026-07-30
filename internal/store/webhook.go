package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var WebhookEvents = []string{
	"deploy.started", "deploy.succeeded", "deploy.failed", "rollback.completed",
	"environment.created", "environment.torn_down",
}

type Webhook struct {
	ID, OrgID, TargetURL, SecretRef string
	Events                          []string
	ConsecutiveFailures             int
	DisabledAt                      *time.Time
	CreatedAt, UpdatedAt            time.Time
}

type SaveWebhookParams struct {
	Webhook     Webhook
	Idempotency CompleteIdempotencyParams
	Audit       *AuditEvent
}

func (s *Store) CreateWebhook(ctx context.Context, p SaveWebhookParams) (Webhook, error) {
	var value Webhook
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanWebhook(tx.QueryRow(ctx, `INSERT INTO webhooks(id,org_id,target_url,events,secret_ref)
			SELECT $1,$2,$3,$4,$5 WHERE EXISTS(SELECT 1 FROM orgs WHERE id=$2 AND deleted_at IS NULL)
			RETURNING id::text,org_id::text,target_url,events,secret_ref,consecutive_failures,disabled_at,created_at,updated_at`,
			p.Webhook.ID, p.Webhook.OrgID, p.Webhook.TargetURL, p.Webhook.Events, p.Webhook.SecretRef), &value); err != nil {
			return mapMutationError("create webhook", err)
		}
		if err := finishAcceptedMutation(ctx, tx, value.OrgID, "webhook", value.ID, &p.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, value.OrgID, p.Audit, "webhook.created", "webhook", value.ID, nil, after)
	})
	return value, err
}

func (s *Store) UpdateWebhook(ctx context.Context, p SaveWebhookParams, enabled bool) (Webhook, error) {
	var value Webhook
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(webhooks) FROM webhooks WHERE org_id=$1 AND id=$2 FOR UPDATE`, p.Webhook.OrgID, p.Webhook.ID).Scan(&before); err != nil {
			return mapMutationError("lock webhook", err)
		}
		if err := scanWebhook(tx.QueryRow(ctx, `UPDATE webhooks SET target_url=$3,events=$4,secret_ref=$5,
			disabled_at=CASE WHEN $6 THEN NULL ELSE COALESCE(disabled_at,now()) END,
			consecutive_failures=CASE WHEN $6 AND disabled_at IS NOT NULL THEN 0 ELSE consecutive_failures END,updated_at=now()
			WHERE org_id=$1 AND id=$2 RETURNING id::text,org_id::text,target_url,events,secret_ref,
			consecutive_failures,disabled_at,created_at,updated_at`, p.Webhook.OrgID, p.Webhook.ID,
			p.Webhook.TargetURL, p.Webhook.Events, p.Webhook.SecretRef, enabled), &value); err != nil {
			return mapMutationError("update webhook", err)
		}
		if err := finishAcceptedMutation(ctx, tx, value.OrgID, "webhook", value.ID, &p.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, value.OrgID, p.Audit, "webhook.updated", "webhook", value.ID, before, after)
	})
	return value, err
}

func (s *Store) DeleteWebhook(ctx context.Context, orgID, id string, idem CompleteIdempotencyParams, audit *AuditEvent) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(webhooks) FROM webhooks WHERE org_id=$1 AND id=$2 FOR UPDATE`, orgID, id).Scan(&before); err != nil {
			return mapMutationError("lock webhook", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM webhooks WHERE org_id=$1 AND id=$2`, orgID, id); err != nil {
			return fmt.Errorf("delete webhook: %w", err)
		}
		if err := completeIdempotency(ctx, tx, idem); err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, orgID, audit, "webhook.deleted", "webhook", id, before, nil)
	})
}

func (s *Store) GetWebhook(ctx context.Context, orgID, id string) (Webhook, error) {
	var value Webhook
	err := scanWebhook(s.db.QueryRow(ctx, webhookSelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &value)
	return value, resourceQueryError("get webhook", err)
}

func (s *Store) ListWebhooks(ctx context.Context, orgID string) ([]Webhook, error) {
	rows, err := s.db.Query(ctx, webhookSelect+` WHERE org_id=$1 ORDER BY created_at,id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	defer rows.Close()
	var values []Webhook
	for rows.Next() {
		var value Webhook
		if err := scanWebhook(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

const webhookSelect = `SELECT id::text,org_id::text,target_url,events,secret_ref,consecutive_failures,disabled_at,created_at,updated_at FROM webhooks`

func scanWebhook(row rowScanner, value *Webhook) error {
	return row.Scan(&value.ID, &value.OrgID, &value.TargetURL, &value.Events, &value.SecretRef,
		&value.ConsecutiveFailures, &value.DisabledAt, &value.CreatedAt, &value.UpdatedAt)
}

type WebhookDelivery struct {
	ID, OrgID, WebhookID, Event, TargetURL, SecretRef string
	RawBody                                           []byte
	FirstSendTimestamp                                *int64
	Signature                                         *string
	Attempt                                           int
	Fence                                             int64
	LeaseOwner                                        string
	LeaseExpiresAt                                    time.Time
}

func (s *Store) ListWebhookOrgIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT delivery.org_id::text FROM webhook_deliveries delivery
		JOIN webhooks endpoint ON endpoint.org_id=delivery.org_id AND endpoint.id=delivery.webhook_id
		WHERE delivery.delivered_at IS NULL AND delivery.abandoned_at IS NULL
		AND (endpoint.disabled_at IS NOT NULL OR delivery.next_attempt_at<=now() OR delivery.lease_expires_at<=now())
		ORDER BY delivery.org_id::text`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) ClaimWebhookDelivery(ctx context.Context, orgID, owner string, lease time.Duration) (WebhookDelivery, error) {
	if err := requirePositiveDuration("webhook lease duration", lease); err != nil {
		return WebhookDelivery{}, err
	}
	var value WebhookDelivery
	notFound := false
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries delivery SET abandoned_at=now(),abandoned_reason='endpoint_disabled',
			next_attempt_at=now(),lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
			FROM webhooks endpoint WHERE delivery.org_id=$1 AND endpoint.org_id=delivery.org_id
			AND endpoint.id=delivery.webhook_id AND endpoint.disabled_at IS NOT NULL
			AND delivery.delivered_at IS NULL AND delivery.abandoned_at IS NULL
			AND (delivery.lease_expires_at IS NULL OR delivery.lease_expires_at <= now())`, orgID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `WITH candidate AS (SELECT delivery.id FROM webhook_deliveries delivery
			JOIN webhooks endpoint ON endpoint.org_id=delivery.org_id AND endpoint.id=delivery.webhook_id
			WHERE delivery.org_id=$1 AND endpoint.disabled_at IS NULL AND delivery.delivered_at IS NULL
			AND delivery.abandoned_at IS NULL AND delivery.next_attempt_at<=now()
			AND (delivery.lease_owner IS NULL OR delivery.lease_expires_at<=now())
			ORDER BY delivery.next_attempt_at,delivery.created_at,delivery.id FOR UPDATE OF delivery SKIP LOCKED LIMIT 1),
			claimed AS (UPDATE webhook_deliveries delivery SET lease_owner=$2,
			lease_expires_at=now()+make_interval(secs=>$3),fence=delivery.fence+1,
			updated_at=now() FROM candidate WHERE delivery.id=candidate.id RETURNING delivery.*)
			SELECT id::text,org_id::text,webhook_id::text,event,target_url,secret_ref,raw_body,
			first_send_timestamp,signature,attempt,fence,lease_owner,lease_expires_at FROM claimed`,
			orgID, owner, lease.Seconds()).Scan(&value.ID, &value.OrgID, &value.WebhookID, &value.Event,
			&value.TargetURL, &value.SecretRef, &value.RawBody, &value.FirstSendTimestamp, &value.Signature,
			&value.Attempt, &value.Fence, &value.LeaseOwner, &value.LeaseExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			notFound = true
			return nil
		}
		return err
	})
	if notFound {
		return WebhookDelivery{}, ErrNotFound
	}
	return value, err
}

func (s *Store) PersistWebhookSignature(ctx context.Context, delivery WebhookDelivery, timestamp int64, signature string) error {
	tag, err := s.db.Exec(ctx, `UPDATE webhook_deliveries SET first_send_timestamp=$6,signature=$7,updated_at=now()
		WHERE org_id=$1 AND id=$2 AND lease_owner=$3 AND fence=$4 AND lease_expires_at>now()
		AND attempt=$5 AND first_send_timestamp IS NULL AND signature IS NULL`, delivery.OrgID, delivery.ID,
		delivery.LeaseOwner, delivery.Fence, delivery.Attempt, timestamp, signature)
	if err != nil {
		return err
	}
	return rowsAffected(tag, ErrLeaseNotOwned)
}

var webhookRetryDelays = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

func WebhookRetryDelay(attempt int) (time.Duration, bool) {
	if attempt < 1 || attempt > len(webhookRetryDelays) {
		return 0, false
	}
	return webhookRetryDelays[attempt-1], true
}

type WebhookAttemptResult struct {
	StatusCode int
	Error      string
}

type WebhookDeliveryOutcome struct {
	Delivered        bool
	Abandoned        bool
	Reason           string
	EndpointDisabled bool
}

func (s *Store) CompleteWebhookAttempt(ctx context.Context, delivery WebhookDelivery, result WebhookAttemptResult) (outcome WebhookDeliveryOutcome, err error) {
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if result.StatusCode >= 200 && result.StatusCode < 300 {
			tag, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET delivered_at=now(),next_attempt_at=now(),attempt=attempt+1,
				last_status_code=$6,last_error=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
				WHERE org_id=$1 AND id=$2 AND lease_owner=$3 AND fence=$4 AND attempt=$5 AND lease_expires_at>now()`,
				delivery.OrgID, delivery.ID, delivery.LeaseOwner, delivery.Fence, delivery.Attempt, result.StatusCode)
			if err != nil {
				return err
			}
			if err := rowsAffected(tag, ErrLeaseNotOwned); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE webhooks SET consecutive_failures=0,updated_at=now() WHERE org_id=$1 AND id=$2`, delivery.OrgID, delivery.WebhookID)
			if err != nil {
				return err
			}
			outcome.Delivered = true
			return nil
		}
		if delay, retry := WebhookRetryDelay(delivery.Attempt + 1); retry {
			tag, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at=now()+make_interval(secs=>$6),attempt=attempt+1,
				last_status_code=$7,last_error=$8,lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
				WHERE org_id=$1 AND id=$2 AND lease_owner=$3 AND fence=$4 AND attempt=$5 AND lease_expires_at>now()`,
				delivery.OrgID, delivery.ID, delivery.LeaseOwner, delivery.Fence, delivery.Attempt,
				delay.Seconds(), nullableStatus(result.StatusCode), result.Error)
			if err != nil {
				return err
			}
			return rowsAffected(tag, ErrLeaseNotOwned)
		}
		tag, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET abandoned_at=now(),abandoned_reason='retry_exhausted',
			next_attempt_at=now(),attempt=attempt+1,last_status_code=$6,last_error=$7,lease_owner=NULL,lease_expires_at=NULL,updated_at=now()
			WHERE org_id=$1 AND id=$2 AND lease_owner=$3 AND fence=$4 AND attempt=$5 AND lease_expires_at>now()`,
			delivery.OrgID, delivery.ID, delivery.LeaseOwner, delivery.Fence, delivery.Attempt,
			nullableStatus(result.StatusCode), result.Error)
		if err != nil {
			return err
		}
		if err := rowsAffected(tag, ErrLeaseNotOwned); err != nil {
			return err
		}
		outcome.Abandoned = true
		outcome.Reason = "retry_exhausted"
		return tx.QueryRow(ctx, `UPDATE webhooks SET consecutive_failures=consecutive_failures+1,
			disabled_at=CASE WHEN consecutive_failures+1>=20 THEN COALESCE(disabled_at,now()) ELSE disabled_at END,
			updated_at=now() WHERE org_id=$1 AND id=$2 RETURNING disabled_at IS NOT NULL`, delivery.OrgID, delivery.WebhookID).Scan(&outcome.EndpointDisabled)
	})
	return outcome, err
}

func nullableStatus(status int) any {
	if status == 0 {
		return nil
	}
	return status
}

type webhookEventPayload struct {
	Event          string         `json:"event"`
	ID             string         `json:"id"`
	Timestamp      time.Time      `json:"timestamp"`
	OrganizationID string         `json:"organization_id"`
	EnvironmentID  string         `json:"environment_id"`
	ReleaseID      *string        `json:"release_id"`
	Data           map[string]any `json:"data,omitempty"`
}

func enqueueWebhookEvent(ctx context.Context, tx pgx.Tx, orgID, event, environmentID string, releaseID *string, data map[string]any) error {
	var timestamp time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&timestamp); err != nil {
		return err
	}
	payload := webhookEventPayload{Event: event, ID: "", Timestamp: timestamp.UTC(), OrganizationID: orgID, EnvironmentID: environmentID, ReleaseID: releaseID, Data: data}
	rows, err := tx.Query(ctx, `SELECT id::text,target_url,secret_ref FROM webhooks WHERE org_id=$1 AND disabled_at IS NULL
		AND (cardinality(events)=0 OR $2=ANY(events)) ORDER BY id`, orgID, event)
	if err != nil {
		return err
	}
	defer rows.Close()
	type endpoint struct{ id, target, secret string }
	var endpoints []endpoint
	for rows.Next() {
		var e endpoint
		if err := rows.Scan(&e.id, &e.target, &e.secret); err != nil {
			return err
		}
		endpoints = append(endpoints, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		var deliveryID string
		if err := tx.QueryRow(ctx, `SELECT uuidv7()::text`).Scan(&deliveryID); err != nil {
			return err
		}
		payload.ID = deliveryID
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries(id,org_id,webhook_id,event,payload,raw_body,target_url,secret_ref)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, deliveryID, orgID, endpoint.id, event, raw, raw, endpoint.target, endpoint.secret); err != nil {
			return err
		}
	}
	return nil
}
