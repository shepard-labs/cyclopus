package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type AuditEvent struct {
	ID          string
	ActorUserID *string
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
