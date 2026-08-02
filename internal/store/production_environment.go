package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// CreateProductionEnvironmentParams holds the validated, service-assembled
// values for inserting a production environment row. The planned_hostname is
// set here; the environment is inserted as lifecycle_status='reserved' with a
// queued provision job so the async lifecycle provisioner creates the
// namespace and network policies (same as previews, minus the preview-only
// resource bounds of §4.3) before the worker flips the environment to
// 'ready'.
type CreateProductionEnvironmentParams struct {
	ID        string
	OrgID     string
	AppID     string
	Branch    string
	Namespace string
	Hostname  string
	Audit     *AuditEvent
}

// CreateProductionEnvironment inserts a production environment with
// lifecycle_status='reserved' and planned_hostname pre-set, and enqueues an
// environment_lifecycle_jobs row (kind='provision', reason='production_create')
// in the same transaction. It fails with ErrAlreadyExists when a live
// production environment already exists for the app+branch pair, or
// ErrNotFound when the app does not exist.
func (s *Store) CreateProductionEnvironment(ctx context.Context, params CreateProductionEnvironmentParams) (Environment, error) {
	var value Environment
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO environments (id, org_id, app_id, branch, kind, namespace, planned_hostname, lifecycle_status)
			SELECT $1, $2, $3, $4, 'production', $5, $6, 'reserved'
			WHERE EXISTS (SELECT 1 FROM apps WHERE org_id=$2 AND id=$3 AND deleted_at IS NULL)
			RETURNING id::text`,
			params.ID, params.OrgID, params.AppID, params.Branch, params.Namespace, params.Hostname,
		).Scan(&id)
		if err != nil {
			return mapMutationError("create production environment", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_lifecycle_jobs
			(org_id,environment_id,kind,status,checkpoint,reason)
			VALUES($1,$2,'provision','queued','reserved','production_create')`, params.OrgID, id); err != nil {
			return err
		}
		var v Environment
		if err := scanEnvironment(tx.QueryRow(ctx, environmentSelect+` WHERE environment.org_id=$1 AND environment.id=$2`, params.OrgID, id), &v); err != nil {
			return err
		}
		value = v
		after, _ := json.Marshal(value)
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, "environment.created", "environment", value.ID, nil, after)
	})
	return value, err
}
