package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

type GitHubWebhookPayloadCodec interface {
	Encrypt([]byte, GitHubWebhookPayloadContext) (ciphertext, nonce []byte, keyVersion string, err error)
	Decrypt(ciphertext, nonce []byte, keyVersion string, context GitHubWebhookPayloadContext) ([]byte, error)
}

type GitHubWebhookPayloadContext struct {
	DeliveryID, Event, Action, OrgID, PayloadDigest string
	InstallationID                                  int64
	InstallationIDPresent                           bool
	RepositoryID                                    *int64
	RepositoryNodeID                                *string
}

const githubWebhookPayloadAADVersion = "cyclopus/github-webhook-payload/v1"

func githubWebhookPayloadAAD(context GitHubWebhookPayloadContext, keyVersion string) []byte {
	var aad bytes.Buffer
	writeString := func(value string) {
		_ = binary.Write(&aad, binary.BigEndian, uint64(len(value)))
		_, _ = aad.WriteString(value)
	}
	writeString(githubWebhookPayloadAADVersion)
	writeString(context.DeliveryID)
	writeString(context.Event)
	writeString(context.Action)
	writeString(context.OrgID)
	if !context.InstallationIDPresent {
		_ = aad.WriteByte(0)
	} else {
		_ = aad.WriteByte(1)
		_ = binary.Write(&aad, binary.BigEndian, context.InstallationID)
	}
	if context.RepositoryID == nil {
		_ = aad.WriteByte(0)
	} else {
		_ = aad.WriteByte(1)
		_ = binary.Write(&aad, binary.BigEndian, *context.RepositoryID)
	}
	if context.RepositoryNodeID == nil {
		_ = aad.WriteByte(0)
	} else {
		_ = aad.WriteByte(1)
		writeString(*context.RepositoryNodeID)
	}
	writeString(context.PayloadDigest)
	writeString(keyVersion)
	return aad.Bytes()
}

type AESGCMGitHubWebhookPayloadCodec struct {
	activeVersion string
	keys          map[string]cipher.AEAD
}

func NewAESGCMGitHubWebhookPayloadCodec(keyVersion string, key []byte) (*AESGCMGitHubWebhookPayloadCodec, error) {
	return NewAESGCMGitHubWebhookPayloadKeyring(keyVersion, map[string][]byte{keyVersion: key})
}

func NewAESGCMGitHubWebhookPayloadKeyring(activeVersion string, keys map[string][]byte) (*AESGCMGitHubWebhookPayloadCodec, error) {
	if activeVersion == "" || len(keys) == 0 {
		return nil, ErrInvalidArgument
	}
	aeads := make(map[string]cipher.AEAD, len(keys))
	for version, key := range keys {
		if version == "" {
			return nil, ErrInvalidArgument
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("github webhook payload key version %q: %w", version, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		aeads[version] = aead
	}
	if _, ok := aeads[activeVersion]; !ok {
		return nil, ErrInvalidArgument
	}
	return &AESGCMGitHubWebhookPayloadCodec{activeVersion: activeVersion, keys: aeads}, nil
}
func (c *AESGCMGitHubWebhookPayloadCodec) Encrypt(payload []byte, context GitHubWebhookPayloadContext) ([]byte, []byte, string, error) {
	aead := c.keys[c.activeVersion]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, "", err
	}
	return aead.Seal(nil, nonce, payload, githubWebhookPayloadAAD(context, c.activeVersion)), nonce, c.activeVersion, nil
}
func (c *AESGCMGitHubWebhookPayloadCodec) Decrypt(payload, nonce []byte, version string, context GitHubWebhookPayloadContext) ([]byte, error) {
	aead, ok := c.keys[version]
	if !ok {
		return nil, ErrFailedPrecondition
	}
	plain, err := aead.Open(nil, nonce, payload, githubWebhookPayloadAAD(context, version))
	if err != nil {
		return nil, fmt.Errorf("decrypt github webhook payload: %w", ErrFailedPrecondition)
	}
	return plain, nil
}

func (s *Store) WithGitHubWebhookPayloadCodec(codec GitHubWebhookPayloadCodec) *Store {
	s.webhookPayloadCodec = codec
	return s
}
func (s *Store) githubWebhookPayloadCodec() (GitHubWebhookPayloadCodec, error) {
	if s.webhookPayloadCodec == nil {
		return nil, ErrFailedPrecondition
	}
	return s.webhookPayloadCodec, nil
}

type GitHubWebhookReceiveDisposition string

const (
	GitHubWebhookAccepted GitHubWebhookReceiveDisposition = "accepted"
	GitHubWebhookReplayed GitHubWebhookReceiveDisposition = "replayed"
	GitHubWebhookConflict GitHubWebhookReceiveDisposition = "conflict"
)

type LeaseToken struct {
	Owner     string
	Epoch     int64
	ExpiresAt time.Time
}

type GitHubWebhookDelivery struct {
	ID, DeliveryID, Event, Action, OrgID, PayloadDigest string
	PayloadKeyVersion, SignatureKeyVersion              string
	RepositoryNodeID                                    *string
	Payload                                             []byte
	InstallationID                                      *int64
	LeaseEpoch                                          int64
	RepositoryID                                        *int64
	IngestionSequence                                   int64
	Attempts                                            int
	Status                                              string
	NextAttemptAt, ReceivedAt                           time.Time
	Lease                                               *LeaseToken
}

type ReceiveGitHubWebhookParams struct {
	DeliveryID, Event, Action, OrgID, PayloadDigest, SignatureKeyVersion string
	RepositoryNodeID                                                     *string
	Payload                                                              []byte
	InstallationID                                                       *int64
	RepositoryID                                                         *int64
}

type GitHubWebhookIdentity struct {
	OrgID            string
	InstallationID   *int64
	RepositoryID     *int64
	RepositoryNodeID *string
}

type GitHubWebhookLookupDisposition string

const (
	GitHubWebhookNotFound GitHubWebhookLookupDisposition = "not_found"
	GitHubWebhookReplay   GitHubWebhookLookupDisposition = "replay"
	GitHubWebhookMismatch GitHubWebhookLookupDisposition = "conflict"
)

type LookupGitHubWebhookParams struct {
	DeliveryID, Event, Action, PayloadDigest string
	InstallationID                           *int64
	RepositoryID                             *int64
	RepositoryNodeID                         *string
}

func (s *Store) LookupGitHubWebhook(ctx context.Context, p LookupGitHubWebhookParams) (GitHubWebhookLookupDisposition, GitHubWebhookIdentity, error) {
	var identity GitHubWebhookIdentity
	var event, action, payloadDigest string
	err := s.db.QueryRow(ctx, `SELECT event,action,installation_id,repository_id,repository_node_id,org_id::text,payload_digest
		FROM github_webhook_deliveries WHERE delivery_id=$1`, p.DeliveryID).Scan(
		&event, &action, &identity.InstallationID, &identity.RepositoryID, &identity.RepositoryNodeID, &identity.OrgID, &payloadDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubWebhookNotFound, GitHubWebhookIdentity{}, nil
	}
	if err != nil {
		return "", GitHubWebhookIdentity{}, err
	}
	if event != p.Event || action != p.Action || payloadDigest != p.PayloadDigest ||
		!equalInt64Pointers(identity.InstallationID, p.InstallationID) ||
		!equalInt64Pointers(identity.RepositoryID, p.RepositoryID) ||
		!equalStringPointers(identity.RepositoryNodeID, p.RepositoryNodeID) {
		return GitHubWebhookMismatch, identity, nil
	}
	return GitHubWebhookReplay, identity, nil
}

func (s *Store) ResolveGitHubWebhookIdentity(ctx context.Context, installationID *int64, repositoryID *int64, nodeID *string) (GitHubWebhookIdentity, error) {
	var value GitHubWebhookIdentity
	if installationID == nil {
		rows, err := s.db.Query(ctx, `SELECT DISTINCT org_id::text FROM github_installations WHERE access_state<>'deleted' LIMIT 2`)
		if err != nil {
			return value, err
		}
		defer rows.Close()
		for rows.Next() {
			if value.OrgID != "" {
				return GitHubWebhookIdentity{}, ErrFailedPrecondition
			}
			if err := rows.Scan(&value.OrgID); err != nil {
				return value, err
			}
		}
		if err := rows.Err(); err != nil {
			return value, err
		}
		if value.OrgID == "" {
			return value, ErrNotFound
		}
		return value, nil
	}
	if repositoryID == nil && nodeID == nil {
		err := s.db.QueryRow(ctx, `SELECT org_id::text,github_installation_id FROM github_installations WHERE github_installation_id=$1 AND access_state<>'deleted'`, *installationID).Scan(&value.OrgID, &installationID)
		value.InstallationID = installationID
		return value, resourceQueryError("resolve github webhook installation", err)
	}
	if repositoryID == nil || nodeID == nil {
		return value, ErrFailedPrecondition
	}
	err := s.db.QueryRow(ctx, `SELECT org_id::text,github_installation_id,github_repository_id,node_id FROM github_repositories WHERE github_installation_id=$1 AND github_repository_id=$2 AND node_id=$3 AND access_state<>'removed'`, *installationID, *repositoryID, *nodeID).Scan(&value.OrgID, &installationID, &repositoryID, &nodeID)
	value.InstallationID, value.RepositoryID, value.RepositoryNodeID = installationID, repositoryID, nodeID
	return value, resourceQueryError("resolve github webhook repository", err)
}

func (p ReceiveGitHubWebhookParams) payloadContext() GitHubWebhookPayloadContext {
	context := GitHubWebhookPayloadContext{DeliveryID: p.DeliveryID, Event: p.Event, Action: p.Action, OrgID: p.OrgID, PayloadDigest: p.PayloadDigest, RepositoryID: p.RepositoryID, RepositoryNodeID: p.RepositoryNodeID}
	if p.InstallationID != nil {
		context.InstallationID, context.InstallationIDPresent = *p.InstallationID, true
	}
	return context
}

func (d GitHubWebhookDelivery) payloadContext() GitHubWebhookPayloadContext {
	context := GitHubWebhookPayloadContext{DeliveryID: d.DeliveryID, Event: d.Event, Action: d.Action, OrgID: d.OrgID, PayloadDigest: d.PayloadDigest, RepositoryID: d.RepositoryID, RepositoryNodeID: d.RepositoryNodeID}
	if d.InstallationID != nil {
		context.InstallationID, context.InstallationIDPresent = *d.InstallationID, true
	}
	return context
}

func (s *Store) ReceiveGitHubWebhook(ctx context.Context, p ReceiveGitHubWebhookParams) (GitHubWebhookReceiveDisposition, GitHubWebhookDelivery, error) {
	var disposition GitHubWebhookReceiveDisposition
	var delivery GitHubWebhookDelivery
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		found, err := receiveGitHubWebhookExisting(ctx, tx, p, &disposition, &delivery)
		if err != nil || found {
			return err
		}
		codec, err := s.githubWebhookPayloadCodec()
		if err != nil {
			return err
		}
		ciphertext, nonce, keyVersion, err := codec.Encrypt(p.Payload, p.payloadContext())
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO github_webhook_deliveries
				(delivery_id,event,action,installation_id,repository_id,repository_node_id,org_id,payload_digest,verified_payload,payload_nonce,key_version,signature_key_version)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(delivery_id) DO NOTHING`,
			p.DeliveryID, p.Event, p.Action, p.InstallationID, p.RepositoryID, p.RepositoryNodeID, p.OrgID, p.PayloadDigest, ciphertext, nonce, keyVersion, p.SignatureKeyVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			_, err = receiveGitHubWebhookExisting(ctx, tx, p, &disposition, &delivery)
			return err
		}
		disposition = GitHubWebhookAccepted
		return s.scanGitHubWebhookDelivery(tx.QueryRow(ctx, deliverySelect+` WHERE delivery_id=$1`, p.DeliveryID), &delivery)
	})
	return disposition, delivery, err
}

// Replays and conflicts return immutable delivery metadata only. Payload access
// remains confined to worker claims and does not require active encryption.
func receiveGitHubWebhookExisting(ctx context.Context, tx pgx.Tx, p ReceiveGitHubWebhookParams, disposition *GitHubWebhookReceiveDisposition, delivery *GitHubWebhookDelivery) (bool, error) {
	err := tx.QueryRow(ctx, `SELECT id::text,delivery_id::text,event,action,installation_id,repository_id,repository_node_id,org_id::text,
				payload_digest,key_version,signature_key_version,status,attempts,next_attempt_at,received_at,ingestion_sequence FROM github_webhook_deliveries WHERE delivery_id=$1 FOR SHARE`, p.DeliveryID).Scan(
		&delivery.ID, &delivery.DeliveryID, &delivery.Event, &delivery.Action, &delivery.InstallationID, &delivery.RepositoryID,
		&delivery.RepositoryNodeID, &delivery.OrgID, &delivery.PayloadDigest, &delivery.PayloadKeyVersion, &delivery.SignatureKeyVersion, &delivery.Status,
		&delivery.Attempts, &delivery.NextAttemptAt, &delivery.ReceivedAt, &delivery.IngestionSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if delivery.PayloadDigest == p.PayloadDigest && delivery.Event == p.Event && delivery.Action == p.Action && equalInt64Pointers(delivery.InstallationID, p.InstallationID) && equalInt64Pointers(delivery.RepositoryID, p.RepositoryID) && equalStringPointers(delivery.RepositoryNodeID, p.RepositoryNodeID) && delivery.OrgID == p.OrgID {
		*disposition = GitHubWebhookReplayed
	} else {
		*disposition = GitHubWebhookConflict
	}
	return true, nil
}

func equalInt64Pointers(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func equalStringPointers(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

const deliverySelect = `SELECT id::text,delivery_id::text,event,action,installation_id,repository_id,repository_node_id,org_id::text,
			payload_digest,verified_payload,payload_nonce,key_version,signature_key_version,status,attempts,next_attempt_at,lease_owner,lease_epoch,lease_expires_at,received_at,ingestion_sequence FROM github_webhook_deliveries`

type GitHubEventLeaseStatus struct {
	Deliveries, Intents int64
}

func (s *Store) CountUnexpiredGitHubEventLeases(ctx context.Context) (GitHubEventLeaseStatus, error) {
	var status GitHubEventLeaseStatus
	err := s.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM github_webhook_deliveries WHERE status='leased' AND lease_expires_at>clock_timestamp()),
		(SELECT count(*) FROM github_routing_intents WHERE status='leased' AND lease_expires_at>clock_timestamp())`).Scan(&status.Deliveries, &status.Intents)
	return status, err
}

func (s *Store) FailExpiredExhaustedGitHubEventLeases(ctx context.Context, retention time.Duration, limit int) (GitHubEventLeaseStatus, error) {
	if retention < 0 || retention > 30*24*time.Hour || limit < 1 || limit > 1000 {
		return GitHubEventLeaseStatus{}, ErrInvalidArgument
	}
	var status GitHubEventLeaseStatus
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		deliveries, err := tx.Exec(ctx, `WITH expired AS MATERIALIZED (SELECT id FROM github_webhook_deliveries WHERE status='leased' AND attempts>=12 AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at,received_at,id FOR UPDATE SKIP LOCKED LIMIT $1)
			UPDATE github_webhook_deliveries d SET status='failed',result_code=NULL,error_code='attempts_exhausted',error_detail='attempts_exhausted',terminal_at=clock_timestamp(),retain_until=clock_timestamp()+($2*interval '1 microsecond'),lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() FROM expired WHERE d.id=expired.id`, limit, retention.Microseconds())
		if err != nil {
			return err
		}
		intents, err := tx.Exec(ctx, `WITH expired AS MATERIALIZED (SELECT id FROM github_routing_intents WHERE status='leased' AND attempts>=12 AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at,created_at,id FOR UPDATE SKIP LOCKED LIMIT $1)
			UPDATE github_routing_intents i SET status='failed',result_code=NULL,error_code='attempts_exhausted',error_detail='attempts_exhausted',terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() FROM expired WHERE i.id=expired.id`, limit)
		if err != nil {
			return err
		}
		status.Deliveries, status.Intents = deliveries.RowsAffected(), intents.RowsAffected()
		return nil
	})
	return status, err
}

func (s *Store) scanGitHubWebhookDelivery(row pgx.Row, d *GitHubWebhookDelivery) error {
	var owner *string
	var expires *time.Time
	var ciphertext, nonce []byte
	if err := row.Scan(&d.ID, &d.DeliveryID, &d.Event, &d.Action, &d.InstallationID, &d.RepositoryID, &d.RepositoryNodeID, &d.OrgID,
		&d.PayloadDigest, &ciphertext, &nonce, &d.PayloadKeyVersion, &d.SignatureKeyVersion, &d.Status, &d.Attempts, &d.NextAttemptAt, &owner, &d.LeaseEpoch, &expires, &d.ReceivedAt, &d.IngestionSequence); err != nil {
		return err
	}
	if ciphertext != nil {
		codec, err := s.githubWebhookPayloadCodec()
		if err != nil {
			return err
		}
		d.Payload, err = codec.Decrypt(ciphertext, nonce, d.PayloadKeyVersion, d.payloadContext())
		if err != nil {
			return err
		}
	}
	if owner != nil && expires != nil {
		d.Lease = &LeaseToken{Owner: *owner, Epoch: d.LeaseEpoch, ExpiresAt: *expires}
	}
	return nil
}

func (s *Store) ClaimGitHubWebhookDeliveries(ctx context.Context, owner string, leaseDuration time.Duration, limit int) ([]GitHubWebhookDelivery, error) {
	if owner == "" || limit < 1 || limit > 100 || leaseDuration <= 0 {
		return nil, ErrInvalidArgument
	}
	var claimed []GitHubWebhookDelivery
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH pending AS MATERIALIZED (SELECT id,next_attempt_at AS due_at,received_at FROM github_webhook_deliveries WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,received_at,id FOR UPDATE SKIP LOCKED LIMIT $1), expired AS MATERIALIZED (SELECT id,lease_expires_at AS due_at,received_at FROM github_webhook_deliveries WHERE status='leased' AND attempts<12 AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at,received_at,id FOR UPDATE SKIP LOCKED LIMIT $1), due AS (SELECT * FROM pending UNION ALL SELECT * FROM expired), selected AS (SELECT id FROM due ORDER BY due_at,received_at,id LIMIT $1)
			UPDATE github_webhook_deliveries d SET status='leased',attempts=d.attempts+1,lease_owner=$2,lease_epoch=d.lease_epoch+1,
			lease_expires_at=clock_timestamp()+($3 * interval '1 microsecond'),updated_at=clock_timestamp() FROM selected WHERE d.id=selected.id
			RETURNING d.id::text,d.delivery_id::text,d.event,d.action,d.installation_id,d.repository_id,d.repository_node_id,d.org_id::text,
				d.payload_digest,d.verified_payload,d.payload_nonce,d.key_version,d.signature_key_version,d.status,d.attempts,d.next_attempt_at,d.lease_owner,d.lease_epoch,d.lease_expires_at,d.received_at,d.ingestion_sequence`, limit, owner, leaseDuration.Microseconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d GitHubWebhookDelivery
			if err := s.scanGitHubWebhookDelivery(rows, &d); err != nil {
				return err
			}
			claimed = append(claimed, d)
		}
		return rows.Err()
	})
	return claimed, err
}

func (s *Store) RenewGitHubWebhookDelivery(ctx context.Context, id string, token LeaseToken, duration time.Duration) (LeaseToken, error) {
	if duration <= 0 {
		return LeaseToken{}, ErrInvalidArgument
	}
	var renewed LeaseToken
	err := s.db.QueryRow(ctx, `UPDATE github_webhook_deliveries SET lease_expires_at=GREATEST(lease_expires_at,clock_timestamp()+($4 * interval '1 microsecond')),updated_at=clock_timestamp()
		WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()
		RETURNING lease_owner,lease_epoch,lease_expires_at`, id, token.Owner, token.Epoch, duration.Microseconds()).Scan(&renewed.Owner, &renewed.Epoch, &renewed.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseToken{}, ErrLeaseNotOwned
	}
	return renewed, err
}

func (s *Store) PinGitHubEventExternalCall(ctx context.Context, id string, token LeaseToken, orgID string, duration time.Duration) (LeaseToken, error) {
	if id == "" || token.Owner == "" || token.Epoch < 1 || orgID == "" || duration <= 0 {
		return LeaseToken{}, ErrInvalidArgument
	}
	var pinned LeaseToken
	err := s.db.QueryRow(ctx, `UPDATE github_webhook_deliveries
		SET lease_expires_at=GREATEST(lease_expires_at,clock_timestamp()+($5 * interval '1 microsecond')),updated_at=clock_timestamp()
		WHERE id=$1 AND org_id=$2 AND status='leased' AND lease_owner=$3 AND lease_epoch=$4 AND lease_expires_at>clock_timestamp()
		RETURNING lease_owner,lease_epoch,lease_expires_at`, id, orgID, token.Owner, token.Epoch, duration.Microseconds()).Scan(&pinned.Owner, &pinned.Epoch, &pinned.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseToken{}, ErrLeaseNotOwned
	}
	return pinned, err
}

func (s *Store) RotateGitHubWebhookDeliveryLease(ctx context.Context, id string, token LeaseToken, duration time.Duration) (LeaseToken, error) {
	if duration <= 0 {
		return LeaseToken{}, ErrInvalidArgument
	}
	var rotated LeaseToken
	err := s.db.QueryRow(ctx, `UPDATE github_webhook_deliveries SET lease_epoch=lease_epoch+1,lease_expires_at=clock_timestamp()+($4 * interval '1 microsecond'),updated_at=clock_timestamp()
		WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()
		RETURNING lease_owner,lease_epoch,lease_expires_at`, id, token.Owner, token.Epoch, duration.Microseconds()).Scan(&rotated.Owner, &rotated.Epoch, &rotated.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseToken{}, ErrLeaseNotOwned
	}
	return rotated, err
}

func (s *Store) RetryGitHubWebhookDelivery(ctx context.Context, id string, token LeaseToken, delay, retention time.Duration, code, detail string) error {
	if !allowedWebhookError(code) {
		return ErrInvalidArgument
	}
	if delay < 0 || delay > 24*time.Hour {
		return ErrInvalidArgument
	}
	if retention < 0 || retention > 30*24*time.Hour {
		return ErrInvalidArgument
	}
	detail = sanitizeWebhookDetail(detail)
	return fencedWebhookUpdate(ctx, s.db, `UPDATE github_webhook_deliveries SET status=CASE WHEN attempts=12 THEN 'failed' ELSE 'pending' END,
		next_attempt_at=clock_timestamp()+($4*interval '1 microsecond'),error_code=CASE WHEN attempts=12 THEN 'attempts_exhausted' ELSE $5 END,error_detail=$6,result_code=NULL,terminal_at=CASE WHEN attempts=12 THEN clock_timestamp() END,
		retain_until=CASE WHEN attempts=12 THEN clock_timestamp()+($7*interval '1 microsecond') END,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()`, id, token, delay.Microseconds(), code, detail, retention.Microseconds())
}

func (s *Store) CompleteGitHubWebhookDelivery(ctx context.Context, id string, token LeaseToken, result string, retention time.Duration) error {
	if !allowedWebhookResult(result) {
		return ErrInvalidArgument
	}
	if retention < 0 || retention > 30*24*time.Hour {
		return ErrInvalidArgument
	}
	return fencedWebhookUpdate(ctx, s.db, `UPDATE github_webhook_deliveries SET status='completed',result_code=$4,error_code=NULL,error_detail=NULL,
		terminal_at=clock_timestamp(),retain_until=clock_timestamp()+($5*interval '1 microsecond'),lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
		WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()`, id, token, result, retention.Microseconds())
}

func (s *Store) FailGitHubWebhookDelivery(ctx context.Context, id string, token LeaseToken, code, detail string, retention time.Duration) error {
	if !allowedWebhookError(code) {
		return ErrInvalidArgument
	}
	detail = sanitizeWebhookDetail(detail)
	if retention < 0 || retention > 30*24*time.Hour {
		return ErrInvalidArgument
	}
	return fencedWebhookUpdate(ctx, s.db, `UPDATE github_webhook_deliveries SET status='failed',error_code=$4,error_detail=$5,terminal_at=clock_timestamp(),retain_until=clock_timestamp()+($6*interval '1 microsecond'),
		lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()`, id, token, code, detail, retention.Microseconds())
}

func fencedWebhookUpdate(ctx context.Context, db DB, query, id string, token LeaseToken, args ...any) error {
	params := []any{id, token.Owner, token.Epoch}
	params = append(params, args...)
	tag, err := db.Exec(ctx, query, params...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseNotOwned
	}
	return nil
}

func (s *Store) RedactTerminalGitHubWebhookPayloads(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalidArgument
	}
	tag, err := s.db.Exec(ctx, `WITH expired AS (SELECT id FROM github_webhook_deliveries WHERE status IN ('completed','failed') AND verified_payload IS NOT NULL AND retain_until<=clock_timestamp() ORDER BY retain_until,id FOR UPDATE SKIP LOCKED LIMIT $1)
		UPDATE github_webhook_deliveries d SET verified_payload=NULL,payload_nonce=NULL,payload_redacted_at=clock_timestamp(),updated_at=clock_timestamp() FROM expired WHERE d.id=expired.id`, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func allowedWebhookError(v string) bool {
	switch v {
	case "database_unavailable", "github_unavailable", "github_rate_limited", "permission_revoked", "malformed_response", "invalid_event", "github_conflict", "fanout_limit", "internal":
		return true
	}
	return false
}
func allowedWebhookResult(v string) bool {
	switch v {
	case "processed", "ignored", "stale_push", "production_branch_deleted", "exhausted":
		return true
	}
	return false
}
func sanitizeWebhookDetail(v string) string {
	if len(v) > 512 {
		v = v[:512]
	}
	for i := range v {
		if v[i] < ' ' || v[i] > 126 {
			return "sanitized"
		}
	}
	return v
}

type GitHubRoutingIntentParams struct {
	DeliveryID, OrgID, RepositoryNodeID, AppID, EnvironmentID, Ref, CommitSHA, EventKind, WorkflowPath string
	RepositoryID, RoutingGeneration, RepositoryGeneration, RefLifecycleGeneration, ConfigGeneration    int64
	ProductionRef                                                                                      bool
}

func (s *Store) PersistGitHubRoutingIntent(ctx context.Context, p GitHubRoutingIntentParams) (string, bool, error) {
	var id string
	err := s.db.QueryRow(ctx, `INSERT INTO github_routing_intents(delivery_id,org_id,repository_id,repository_node_id,installation_id,app_id,environment_id,ref,commit_sha,event_kind,routing_generation,repository_generation,ref_lifecycle_generation,config_generation,workflow_path,production_ref)
		SELECT $1,$2,$3,$4,state.installation_id,$5,NULLIF($6,'')::uuid,$7,NULLIF($8,''),$9,$10,$11,$12,$13,$14,$15 FROM github_repository_routing_state state WHERE state.repository_id=$3 AND state.repository_node_id=$4
		ON CONFLICT(delivery_id,app_id,ref,event_kind,routing_generation) DO NOTHING RETURNING id::text`, p.DeliveryID, p.OrgID, p.RepositoryID, p.RepositoryNodeID, p.AppID, p.EnvironmentID, p.Ref, p.CommitSHA, p.EventKind, p.RoutingGeneration, p.RepositoryGeneration, p.RefLifecycleGeneration, p.ConfigGeneration, p.WorkflowPath, p.ProductionRef).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var existing GitHubRoutingIntentParams
		err = s.db.QueryRow(ctx, `SELECT id::text,delivery_id::text,org_id::text,repository_id,repository_node_id,app_id::text,COALESCE(environment_id::text,''),ref,COALESCE(commit_sha,''),event_kind,routing_generation,repository_generation,ref_lifecycle_generation,config_generation,workflow_path,production_ref FROM github_routing_intents WHERE delivery_id=$1 AND app_id=$2 AND ref=$3 AND event_kind=$4 AND routing_generation=$5`, p.DeliveryID, p.AppID, p.Ref, p.EventKind, p.RoutingGeneration).Scan(&id, &existing.DeliveryID, &existing.OrgID, &existing.RepositoryID, &existing.RepositoryNodeID, &existing.AppID, &existing.EnvironmentID, &existing.Ref, &existing.CommitSHA, &existing.EventKind, &existing.RoutingGeneration, &existing.RepositoryGeneration, &existing.RefLifecycleGeneration, &existing.ConfigGeneration, &existing.WorkflowPath, &existing.ProductionRef)
		if err != nil {
			return "", false, err
		}
		if existing != p {
			return "", false, ErrConflict
		}
		return id, false, nil
	}
	return id, true, err
}

func (s *Store) AdvanceGitHubRoutingGeneration(ctx context.Context, orgID string, repositoryID int64, nodeID, ref string, repositoryGeneration, lifecycleGeneration int64, sha *string, exists bool) (int64, error) {
	var generation int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var authoritativeNode string
		var installationID int64
		if err := tx.QueryRow(ctx, `SELECT node_id,github_installation_id FROM github_repositories WHERE org_id=$1 AND github_repository_id=$2 FOR UPDATE`, orgID, repositoryID).Scan(&authoritativeNode, &installationID); err != nil {
			return err
		}
		if authoritativeNode != nodeID {
			return ErrConflict
		}
		tag, err := tx.Exec(ctx, `INSERT INTO github_repository_routing_state(repository_id,org_id,repository_node_id,installation_id,repository_generation,lifecycle_generation) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, repositoryID, orgID, nodeID, installationID, repositoryGeneration, lifecycleGeneration)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var storedNode string
			var storedInstallation int64
			var storedRepository, storedLifecycle int64
			if err := tx.QueryRow(ctx, `SELECT repository_node_id,installation_id,repository_generation,lifecycle_generation FROM github_repository_routing_state WHERE repository_id=$1 FOR UPDATE`, repositoryID).Scan(&storedNode, &storedInstallation, &storedRepository, &storedLifecycle); err != nil {
				return err
			}
			if storedNode != nodeID || repositoryGeneration < storedRepository || storedInstallation == installationID && lifecycleGeneration < storedLifecycle {
				return ErrConflict
			}
			if _, err = tx.Exec(ctx, `UPDATE github_repository_routing_state SET installation_id=$2,repository_generation=$3,lifecycle_generation=$4,updated_at=clock_timestamp() WHERE repository_id=$1`, repositoryID, installationID, repositoryGeneration, lifecycleGeneration); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO github_ref_routing_state(repository_id,ref) VALUES($1,$2) ON CONFLICT DO NOTHING`, repositoryID, ref)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE github_ref_routing_state SET routing_generation=routing_generation+1,lifecycle_generation=GREATEST(lifecycle_generation,$3),validated_sha=$4,ref_exists=$5,updated_at=clock_timestamp() WHERE repository_id=$1 AND ref=$2 RETURNING routing_generation`, repositoryID, ref, lifecycleGeneration, sha, exists).Scan(&generation)
	})
	return generation, err
}

type GitHubRoutingIntent struct {
	ID, DeliveryID, OrgID, RepositoryNodeID, AppID, EnvironmentID, Ref, CommitSHA, EventKind, WorkflowPath string
	RepositoryID, RoutingGeneration, RepositoryGeneration, RefLifecycleGeneration, ConfigGeneration        int64
	ProductionRef                                                                                          bool
	Attempts                                                                                               int
	Status                                                                                                 string
	Lease                                                                                                  *LeaseToken
}

func (s *Store) ClaimGitHubRoutingIntents(ctx context.Context, owner string, duration time.Duration, limit int) ([]GitHubRoutingIntent, error) {
	if owner == "" || duration <= 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidArgument
	}
	var out []GitHubRoutingIntent
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH pending AS MATERIALIZED (SELECT id,next_attempt_at AS due_at,created_at FROM github_routing_intents WHERE status='pending' AND attempts<12 AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,created_at,id FOR UPDATE SKIP LOCKED LIMIT $1), expired AS MATERIALIZED (SELECT id,lease_expires_at AS due_at,created_at FROM github_routing_intents WHERE status='leased' AND attempts<12 AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at,created_at,id FOR UPDATE SKIP LOCKED LIMIT $1), due AS (SELECT * FROM pending UNION ALL SELECT * FROM expired), selected AS (SELECT id FROM due ORDER BY due_at,created_at,id LIMIT $1) UPDATE github_routing_intents i SET status='leased',attempts=attempts+1,lease_owner=$2,lease_epoch=lease_epoch+1,lease_expires_at=clock_timestamp()+($3*interval '1 microsecond'),updated_at=clock_timestamp() FROM selected WHERE i.id=selected.id RETURNING i.id::text,i.delivery_id::text,i.org_id::text,i.repository_id,i.repository_node_id,i.app_id::text,COALESCE(i.environment_id::text,''),i.ref,COALESCE(i.commit_sha,''),i.event_kind,i.routing_generation,i.repository_generation,i.ref_lifecycle_generation,i.config_generation,i.workflow_path,i.production_ref,i.attempts,i.status,i.lease_owner,i.lease_epoch,i.lease_expires_at`, limit, owner, duration.Microseconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v GitHubRoutingIntent
			var token LeaseToken
			if err := rows.Scan(&v.ID, &v.DeliveryID, &v.OrgID, &v.RepositoryID, &v.RepositoryNodeID, &v.AppID, &v.EnvironmentID, &v.Ref, &v.CommitSHA, &v.EventKind, &v.RoutingGeneration, &v.RepositoryGeneration, &v.RefLifecycleGeneration, &v.ConfigGeneration, &v.WorkflowPath, &v.ProductionRef, &v.Attempts, &v.Status, &token.Owner, &token.Epoch, &token.ExpiresAt); err != nil {
				return err
			}
			v.Lease = &token
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}
func (s *Store) RenewGitHubRoutingIntent(ctx context.Context, id string, token LeaseToken, duration time.Duration) (LeaseToken, error) {
	if duration <= 0 {
		return LeaseToken{}, ErrInvalidArgument
	}
	var v LeaseToken
	err := s.db.QueryRow(ctx, `UPDATE github_routing_intents SET lease_expires_at=clock_timestamp()+($4*interval '1 microsecond'),updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp() RETURNING lease_owner,lease_epoch,lease_expires_at`, id, token.Owner, token.Epoch, duration.Microseconds()).Scan(&v.Owner, &v.Epoch, &v.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrLeaseNotOwned
	}
	return v, err
}
func (s *Store) RotateGitHubRoutingIntentLease(ctx context.Context, id string, token LeaseToken, duration time.Duration) (LeaseToken, error) {
	if duration <= 0 {
		return LeaseToken{}, ErrInvalidArgument
	}
	var v LeaseToken
	err := s.db.QueryRow(ctx, `UPDATE github_routing_intents SET lease_epoch=lease_epoch+1,lease_expires_at=clock_timestamp()+($4*interval '1 microsecond'),updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp() RETURNING lease_owner,lease_epoch,lease_expires_at`, id, token.Owner, token.Epoch, duration.Microseconds()).Scan(&v.Owner, &v.Epoch, &v.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrLeaseNotOwned
	}
	return v, err
}
func (s *Store) RetryGitHubRoutingIntent(ctx context.Context, id string, token LeaseToken, delay time.Duration, code, detail string) error {
	if delay < 0 || delay > 24*time.Hour || !allowedRoutingError(code) {
		return ErrInvalidArgument
	}
	detail = sanitizeWebhookDetail(detail)
	return fencedWebhookUpdate(ctx, s.db, `UPDATE github_routing_intents SET status=CASE WHEN attempts=12 THEN 'failed' ELSE 'pending' END,next_attempt_at=clock_timestamp()+($4*interval '1 microsecond'),error_code=CASE WHEN attempts=12 THEN 'attempts_exhausted' ELSE $5 END,error_detail=$6,terminal_at=CASE WHEN attempts=12 THEN clock_timestamp() END,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()`, id, token, delay.Microseconds(), code, detail)
}
func (s *Store) CompleteGitHubRoutingIntent(ctx context.Context, id string, token LeaseToken, result string) error {
	if !allowedRoutingResult(result) {
		return ErrInvalidArgument
	}
	return fencedWebhookUpdate(ctx, s.db, `UPDATE github_routing_intents SET status='completed',result_code=$4,error_code=NULL,error_detail=NULL,terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()`, id, token, result)
}
func (s *Store) FailGitHubRoutingIntent(ctx context.Context, id string, token LeaseToken, code, detail string) error {
	if !allowedRoutingError(code) {
		return ErrInvalidArgument
	}
	return fencedWebhookUpdate(ctx, s.db, `UPDATE github_routing_intents SET status='failed',error_code=$4,error_detail=$5,terminal_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='leased' AND lease_owner=$2 AND lease_epoch=$3 AND lease_expires_at>clock_timestamp()`, id, token, code, sanitizeWebhookDetail(detail))
}
func allowedRoutingError(v string) bool {
	switch v {
	case "database_unavailable", "github_unavailable", "github_rate_limited", "permission_revoked", "invalid_snapshot", "internal":
		return true
	}
	return false
}
func allowedRoutingResult(v string) bool {
	switch v {
	case "processed", "ignored", "stale", "superseded", "exhausted":
		return true
	}
	return false
}
