package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type AuditEvent struct {
	ID          string
	ActorUserID *string
}

// AuditEventRecord is a read projection of one audit_events row. before/after
// carry the raw jsonb (a JSON object or null). These are always org-scoped by
// the query and never contain plaintext secrets — only secret references.
type AuditEventRecord struct {
	ID          string
	OrgID       string
	ActorUserID *string
	Action      string
	TargetType  string
	TargetID    string
	Before      json.RawMessage
	After       json.RawMessage
	CreatedAt   time.Time
}

// AuditEventFilter narrows a ListAuditEvents query. Empty fields are ignored.
// TargetID is only applied together with TargetType.
type AuditEventFilter struct {
	TargetType string
	TargetID   string
}

const auditEventSelect = `SELECT id::text,org_id::text,actor_user_id::text,action,target_type,target_id::text,before,after,created_at FROM audit_events`

func scanAuditEventRecord(row rowScanner, value *AuditEventRecord) error {
	return row.Scan(&value.ID, &value.OrgID, &value.ActorUserID, &value.Action, &value.TargetType,
		&value.TargetID, &value.Before, &value.After, &value.CreatedAt)
}

// ListAuditEvents returns the org's audit events, most-recent first (created_at
// DESC, id DESC — matching the audit_events_org_created index for keyset
// pagination). The mandatory org_id filter enforces strict tenant isolation:
// one org can never read another's audit trail. Optional target_type (and
// target_id) filters narrow the result. limit bounds the rows scanned.
func (s *Store) ListAuditEvents(ctx context.Context, orgID string, filter AuditEventFilter, limit int) ([]AuditEventRecord, error) {
	query := auditEventSelect + ` WHERE org_id=$1`
	args := []any{orgID}
	if filter.TargetType != "" {
		args = append(args, filter.TargetType)
		query += fmt.Sprintf(` AND target_type=$%d`, len(args))
		if filter.TargetID != "" {
			args = append(args, filter.TargetID)
			query += fmt.Sprintf(` AND target_id=$%d`, len(args))
		}
	}
	query += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	var values []AuditEventRecord
	for rows.Next() {
		var value AuditEventRecord
		if err := scanAuditEventRecord(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func insertAuditEvent(ctx context.Context, tx pgx.Tx, orgID string, event *AuditEvent,
	action, targetType, targetID string, before, after json.RawMessage) error {
	if event == nil {
		return fmt.Errorf("%w: audit event is required", ErrInvalidArgument)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_events (
			id, org_id, actor_user_id, action, target_type, target_id, before, after
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		event.ID, orgID, event.ActorUserID, action, targetType,
		targetID, nullableJSON(before), nullableJSON(after))
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
