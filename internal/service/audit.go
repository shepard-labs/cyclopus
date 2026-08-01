package service

import (
	"context"

	"github.com/shepard-labs/cyclopus/internal/store"
)

// maxAuditEventScan bounds how many audit rows a single list may scan before
// in-handler pagination slices the page. It is a safety ceiling on the
// org-scoped read, not the page size.
const maxAuditEventScan = 1000

// AuditEventFilter narrows a ListAuditEvents read. TargetID is only honored
// together with TargetType (a target_id is only meaningful within a type).
type AuditEventFilter struct {
	TargetType string
	TargetID   string
}

// ListAuditEvents returns the actor's org's audit events, most-recent first. It
// is a pure read: validate the actor, then delegate to the org-scoped store
// query. Strict org-scoping (WHERE org_id=actor.OrgID in the store) guarantees
// one org can never read another's audit trail, even when a target_id from
// another org is supplied.
func (s *Service) ListAuditEvents(ctx context.Context, actor ActorContext, filter AuditEventFilter) ([]store.AuditEventRecord, error) {
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	storeFilter := store.AuditEventFilter{TargetType: filter.TargetType, TargetID: filter.TargetID}
	return s.store.ListAuditEvents(ctx, actor.OrgID, storeFilter, maxAuditEventScan)
}
