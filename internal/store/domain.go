package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Domain is one row of the domains table: a custom hostname bound to an app,
// with the DNS verification method and (for dns_txt) challenge token that prove
// the customer controls it (§7.5). VerifiedAt is set once verification succeeds.
type Domain struct {
	ID, OrgID, AppID, Hostname, VerificationMethod string
	VerificationToken                              *string
	VerifiedAt                                     *time.Time
	CreatedAt                                      time.Time
}

type SaveDomainParams struct {
	Domain      Domain
	Idempotency CompleteIdempotencyParams
	Audit       *AuditEvent
}

const domainSelect = `SELECT id::text,org_id::text,app_id::text,hostname,verification_method,verification_token,verified_at,created_at FROM domains`

func scanDomain(row rowScanner, value *Domain) error {
	return row.Scan(&value.ID, &value.OrgID, &value.AppID, &value.Hostname, &value.VerificationMethod,
		&value.VerificationToken, &value.VerifiedAt, &value.CreatedAt)
}

// CreateDomain inserts a domain, guarding that the target app exists and is not
// deleted in the actor's org so a missing app yields ErrNotFound rather than a
// bare foreign-key violation. The domains CHECK constraint enforces the
// method/token invariant (dns_txt <=> token present); a violation maps to
// ErrInvalidArgument, and a duplicate hostname maps to ErrAlreadyExists.
func (s *Store) CreateDomain(ctx context.Context, p SaveDomainParams) (Domain, error) {
	var value Domain
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanDomain(tx.QueryRow(ctx, `INSERT INTO domains(id,org_id,app_id,hostname,verification_method,verification_token)
			SELECT $1,$2,$3,$4,$5,$6 WHERE EXISTS(SELECT 1 FROM apps WHERE org_id=$2 AND id=$3 AND deleted_at IS NULL)
			RETURNING id::text,org_id::text,app_id::text,hostname,verification_method,verification_token,verified_at,created_at`,
			p.Domain.ID, p.Domain.OrgID, p.Domain.AppID, p.Domain.Hostname, p.Domain.VerificationMethod, p.Domain.VerificationToken), &value); err != nil {
			return mapMutationError("create domain", err)
		}
		if err := finishAcceptedMutation(ctx, tx, value.OrgID, "domain", value.ID, &p.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, value.OrgID, p.Audit, "domain.created", "domain", value.ID, nil, after)
	})
	return value, err
}

// VerifyDomain marks a domain verified (verified_at=now()) org-scoped. It is
// idempotent: verified_at is only set on the first success and re-verifying a
// verified domain returns the existing row without changing the timestamp.
func (s *Store) VerifyDomain(ctx context.Context, orgID, id string, idem CompleteIdempotencyParams, audit *AuditEvent) (Domain, error) {
	var value Domain
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(domains) FROM domains WHERE org_id=$1 AND id=$2 FOR UPDATE`, orgID, id).Scan(&before); err != nil {
			return mapMutationError("lock domain", err)
		}
		if err := scanDomain(tx.QueryRow(ctx, `UPDATE domains SET verified_at=COALESCE(verified_at,now())
			WHERE org_id=$1 AND id=$2
			RETURNING id::text,org_id::text,app_id::text,hostname,verification_method,verification_token,verified_at,created_at`,
			orgID, id), &value); err != nil {
			return mapMutationError("verify domain", err)
		}
		if err := finishAcceptedMutation(ctx, tx, value.OrgID, "domain", value.ID, &idem); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, value.OrgID, audit, "domain.verified", "domain", value.ID, before, after)
	})
	return value, err
}

func (s *Store) DeleteDomain(ctx context.Context, orgID, id string, idem CompleteIdempotencyParams, audit *AuditEvent) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(domains) FROM domains WHERE org_id=$1 AND id=$2 FOR UPDATE`, orgID, id).Scan(&before); err != nil {
			return mapMutationError("lock domain", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM domains WHERE org_id=$1 AND id=$2`, orgID, id); err != nil {
			return fmt.Errorf("delete domain: %w", err)
		}
		if err := completeIdempotency(ctx, tx, idem); err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, orgID, audit, "domain.deleted", "domain", id, before, nil)
	})
}

func (s *Store) GetDomain(ctx context.Context, orgID, id string) (Domain, error) {
	var value Domain
	err := scanDomain(s.db.QueryRow(ctx, domainSelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &value)
	return value, resourceQueryError("get domain", err)
}

// ListDomains returns every domain for an app in the org, ordered stably for
// pagination.
func (s *Store) ListDomains(ctx context.Context, orgID, appID string) ([]Domain, error) {
	rows, err := s.db.Query(ctx, domainSelect+` WHERE org_id=$1 AND app_id=$2 ORDER BY created_at,id`, orgID, appID)
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	defer rows.Close()
	var values []Domain
	for rows.Next() {
		var value Domain
		if err := scanDomain(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
