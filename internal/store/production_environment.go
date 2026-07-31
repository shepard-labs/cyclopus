package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// CreateProductionEnvironmentParams holds the validated, service-assembled
// values for inserting a production environment row. The planned_hostname and
// lifecycle_status are set here — production environments skip the async
// lifecycle provisioner used for preview environments and are inserted
// ready-to-deploy.
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
// lifecycle_status='ready' and planned_hostname pre-set. It fails with
// ErrAlreadyExists when a live production environment already exists for the
// app+branch pair, or ErrNotFound when the app does not exist.
func (s *Store) CreateProductionEnvironment(ctx context.Context, params CreateProductionEnvironmentParams) (Environment, error) {
	var value Environment
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO environments (id, org_id, app_id, branch, kind, namespace, planned_hostname, lifecycle_status)
			SELECT $1, $2, $3, $4, 'production', $5, $6, 'ready'
			WHERE EXISTS (SELECT 1 FROM apps WHERE org_id=$2 AND id=$3 AND deleted_at IS NULL)
			RETURNING id::text`,
			params.ID, params.OrgID, params.AppID, params.Branch, params.Namespace, params.Hostname,
		).Scan(&id)
		if err != nil {
			return mapMutationError("create production environment", err)
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
