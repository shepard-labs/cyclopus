package store

import (
	"context"
	"fmt"
	"time"
)

// Alias is one row of the aliases table: a bookkeeping record binding a
// hostname to the release currently serving at it for an environment (§3.7).
// There is at most one alias per environment (UNIQUE(environment_id)).
type Alias struct {
	ID, OrgID, EnvironmentID, Hostname, ReleaseID string
	UpdatedAt                                     time.Time
}

const aliasSelect = `SELECT id::text,org_id::text,environment_id::text,hostname,release_id::text,updated_at FROM aliases`

func scanAlias(row rowScanner, value *Alias) error {
	return row.Scan(&value.ID, &value.OrgID, &value.EnvironmentID, &value.Hostname, &value.ReleaseID, &value.UpdatedAt)
}

// GetAlias returns a single alias org-scoped by id. A cross-org id yields
// ErrNotFound (the WHERE org_id filter makes another org's alias indistinguishable
// from a non-existent one).
func (s *Store) GetAlias(ctx context.Context, orgID, id string) (Alias, error) {
	var value Alias
	err := scanAlias(s.db.QueryRow(ctx, aliasSelect+` WHERE org_id=$1 AND id=$2`, orgID, id), &value)
	return value, resourceQueryError("get alias", err)
}

// ListAliases returns the aliases for an environment in the org, ordered stably
// for pagination. The environment must belong to the org: the org_id filter is
// applied to the aliases rows directly, so an environment in another org returns
// an empty list rather than leaking rows.
func (s *Store) ListAliases(ctx context.Context, orgID, environmentID string) ([]Alias, error) {
	rows, err := s.db.Query(ctx, aliasSelect+` WHERE org_id=$1 AND environment_id=$2 ORDER BY updated_at,id`, orgID, environmentID)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()
	var values []Alias
	for rows.Next() {
		var value Alias
		if err := scanAlias(rows, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
