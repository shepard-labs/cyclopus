package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type User struct {
	ID           string
	OrgID        string
	Email        string
	FirstName    string
	LastName     string
	DisplayName  string
	AvatarKey    string
	PasswordHash *string
	GitHubUserID *int64
	Operator     bool
	CreatedAt    time.Time
}

type ClaimFirstUserParams struct {
	Email        string
	AvatarKey    string
	PasswordHash *string
	GitHubUserID *int64
	Audit        *AuditEvent
}

func (s *Store) InstallationClaimed(ctx context.Context) (bool, error) {
	var claimed bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&claimed)
	})
	return claimed, err
}

func (s *Store) ClaimFirstUser(ctx context.Context, params ClaimFirstUserParams) (User, error) {
	var user User
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id::text, org_id::text, email, first_name, last_name, display_name, avatar_key, password_hash,
			github_user_id, operator, created_at FROM claim_first_user($1, $2, $3, $4)`,
			params.Email, params.PasswordHash, params.GitHubUserID, params.AvatarKey).Scan(
			&user.ID, &user.OrgID, &user.Email, &user.FirstName, &user.LastName, &user.DisplayName, &user.AvatarKey,
			&user.PasswordHash, &user.GitHubUserID, &user.Operator, &user.CreatedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrConflict
			}
			return fmt.Errorf("claim first user: %w", err)
		}
		after, _ := json.Marshal(map[string]any{"id": user.ID, "email": user.Email, "operator": user.Operator})
		return insertAuditEvent(ctx, tx, user.OrgID, params.Audit, "auth.bootstrap", "user", user.ID, nil, after)
	})
	return user, err
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	var user User
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id::text, org_id::text, email, first_name, last_name, display_name, avatar_key, password_hash,
			github_user_id, operator, created_at FROM users WHERE email = $1`, email).Scan(
			&user.ID, &user.OrgID, &user.Email, &user.FirstName, &user.LastName, &user.DisplayName, &user.AvatarKey,
			&user.PasswordHash, &user.GitHubUserID, &user.Operator, &user.CreatedAt)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("find user by email: %w", err)
		}
		return nil
	})
	return user, err
}

func (s *Store) UserByID(ctx context.Context, orgID, userID string) (User, error) {
	var user User
	err := s.db.QueryRow(ctx, `SELECT id::text, org_id::text, email, first_name, last_name, display_name, avatar_key, password_hash,
		github_user_id, operator, created_at FROM users WHERE org_id = $1 AND id = $2`, orgID, userID).Scan(
		&user.ID, &user.OrgID, &user.Email, &user.FirstName, &user.LastName, &user.DisplayName, &user.AvatarKey,
		&user.PasswordHash, &user.GitHubUserID, &user.Operator, &user.CreatedAt)
	if err == pgx.ErrNoRows {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by ID: %w", err)
	}
	return user, nil
}

type UpdateUserProfileParams struct {
	OrgID, UserID, Email, FirstName, LastName, DisplayName, AvatarKey string
	Audit                                                             *AuditEvent
}

func (s *Store) UpdateUserProfile(ctx context.Context, params UpdateUserProfileParams) (User, error) {
	var user User
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT jsonb_build_object('email', email, 'first_name', first_name, 'last_name', last_name,
			'display_name', display_name, 'avatar_key', avatar_key) FROM users WHERE org_id=$1 AND id=$2`,
			params.OrgID, params.UserID).Scan(&before); err != nil {
			return mapMutationError("load user profile", err)
		}
		err := tx.QueryRow(ctx, `UPDATE users SET email=$3, first_name=$4, last_name=$5, display_name=$6, avatar_key=$7
			WHERE org_id=$1 AND id=$2 RETURNING
			id::text, org_id::text, email, first_name, last_name, display_name, avatar_key, password_hash,
			github_user_id, operator, created_at`, params.OrgID, params.UserID, params.Email,
			params.FirstName, params.LastName, params.DisplayName, params.AvatarKey).Scan(
			&user.ID, &user.OrgID, &user.Email, &user.FirstName, &user.LastName, &user.DisplayName, &user.AvatarKey,
			&user.PasswordHash, &user.GitHubUserID, &user.Operator, &user.CreatedAt)
		if err != nil {
			return mapMutationError("update user profile", err)
		}
		after, _ := json.Marshal(map[string]any{"email": user.Email, "first_name": user.FirstName,
			"last_name": user.LastName, "display_name": user.DisplayName, "avatar_key": user.AvatarKey})
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, "user.profile_updated", "user", params.UserID, before, after)
	})
	return user, err
}

type ChangeUserPasswordParams struct {
	OrgID, UserID, CurrentTokenID, ExpectedPasswordHash, PasswordHash string
	ChangedAt                                                         time.Time
	Audit                                                             *AuditEvent
}

func (s *Store) ChangeUserPassword(ctx context.Context, params ChangeUserPasswordParams) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET password_hash=$4 WHERE org_id=$1 AND id=$2 AND password_hash=$3`,
			params.OrgID, params.UserID, params.ExpectedPasswordHash, params.PasswordHash)
		if err != nil {
			return fmt.Errorf("change user password: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at=$4
			WHERE user_id=$2 AND kind='session' AND id<>$3 AND revoked_at IS NULL`,
			params.OrgID, params.UserID, params.CurrentTokenID, params.ChangedAt); err != nil {
			return fmt.Errorf("revoke other sessions: %w", err)
		}
		after, _ := json.Marshal(map[string]any{"other_sessions_revoked": true})
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, "user.password_changed", "user", params.UserID, nil, after)
	})
}

type APIToken struct {
	ID         string
	UserID     string
	OrgID      string
	Email      string
	Operator   bool
	Kind       string
	Name       string
	Prefix     string
	Hash       string
	Scopes     []string
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

type CreateAPITokenParams struct {
	ID        string
	UserID    string
	OrgID     string
	Kind      string
	Name      string
	Prefix    string
	Hash      string
	Scopes    []string
	ExpiresAt *time.Time
	Audit     *AuditEvent
	Action    string
}

type AcceptAPITokenParams struct {
	Create      CreateAPITokenParams
	Idempotency CompleteIdempotencyParams
	Audit       *AuditEvent
}

func (s *Store) AcceptAPIToken(ctx context.Context, params AcceptAPITokenParams) (APIToken, error) {
	scopes := params.Create.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	var token APIToken
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO api_tokens
			(id, user_id, kind, name, token_prefix, token_hash, scopes, expires_at)
			SELECT $1, user_record.id, $4, $5, $6, $7, $8, $9
			FROM users user_record JOIN orgs org ON org.id = user_record.org_id AND org.deleted_at IS NULL
			WHERE user_record.id = $2 AND user_record.org_id = $3
			RETURNING id::text, user_id::text, $3::text, '', kind, name, token_prefix,
				token_hash, scopes, expires_at, created_at, last_used_at, revoked_at`,
			params.Create.ID, params.Create.UserID, params.Create.OrgID, params.Create.Kind, params.Create.Name,
			params.Create.Prefix, params.Create.Hash, scopes, params.Create.ExpiresAt).
			Scan(&token.ID, &token.UserID, &token.OrgID, &token.Email, &token.Kind, &token.Name,
				&token.Prefix, &token.Hash, &token.Scopes, &token.ExpiresAt, &token.CreatedAt,
				&token.LastUsedAt, &token.RevokedAt)
		if err != nil {
			return mapMutationError("accept API token", err)
		}
		replayToken := token
		replayToken.Hash = ""
		params.Idempotency.Body, _ = json.Marshal(replayToken)
		marker := "ALREADY_CREATED_SECRET_NOT_REPLAYABLE"
		params.Idempotency.ErrorCode = &marker
		if err := finishAcceptedMutation(ctx, tx, params.Create.OrgID, "api_token", token.ID, &params.Idempotency); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"id": token.ID, "name": token.Name, "scopes": token.Scopes, "expires_at": token.ExpiresAt})
		return insertAuditEvent(ctx, tx, params.Create.OrgID, params.Audit, "api_token.created", "api_token", token.ID, nil, after)
	})
	return token, err
}

func (s *Store) CreateAPIToken(ctx context.Context, params CreateAPITokenParams) (APIToken, error) {
	scopes := params.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	var token APIToken
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO api_tokens
			(user_id, kind, name, token_prefix, token_hash, scopes, expires_at)
			SELECT user_record.id, $3, $4, $5, $6, $7, $8
			FROM users user_record
			JOIN orgs org ON org.id = user_record.org_id AND org.deleted_at IS NULL
			WHERE user_record.id = $1 AND user_record.org_id = $2
			RETURNING id::text, user_id::text, $2::text, '', kind, name, token_prefix,
				token_hash, scopes, expires_at, created_at, last_used_at, revoked_at`,
			params.UserID, params.OrgID, params.Kind, params.Name, params.Prefix, params.Hash,
			scopes, params.ExpiresAt).Scan(&token.ID, &token.UserID, &token.OrgID,
			&token.Email, &token.Kind, &token.Name, &token.Prefix, &token.Hash, &token.Scopes,
			&token.ExpiresAt, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrConflict
			}
			return fmt.Errorf("create API token: %w", err)
		}
		action := params.Action
		if action == "" {
			action = "api_token.created"
		}
		after, _ := json.Marshal(map[string]any{"id": token.ID, "kind": token.Kind, "name": token.Name,
			"prefix": token.Prefix, "scopes": token.Scopes, "expires_at": token.ExpiresAt})
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, action, "api_token", token.ID, nil, after)
	})
	return token, err
}

func (s *Store) APITokenByPrefix(ctx context.Context, prefix string) (APIToken, error) {
	var token APIToken
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT token.id::text, token.user_id::text,
			user_record.org_id::text, user_record.email, user_record.operator, token.kind, token.name, token.token_prefix,
			token.token_hash, token.scopes, token.expires_at, token.created_at,
			token.last_used_at, token.revoked_at
			FROM api_tokens token
			JOIN users user_record ON user_record.id = token.user_id
			JOIN orgs org ON org.id = user_record.org_id AND org.deleted_at IS NULL
			WHERE token.token_prefix = $1`, prefix).Scan(&token.ID, &token.UserID,
			&token.OrgID, &token.Email, &token.Operator, &token.Kind, &token.Name, &token.Prefix, &token.Hash,
			&token.Scopes, &token.ExpiresAt, &token.CreatedAt, &token.LastUsedAt,
			&token.RevokedAt)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("find API token: %w", err)
		}
		return nil
	})
	return token, err
}

func (s *Store) TouchAPIToken(ctx context.Context, tokenID string, usedAt, before time.Time) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE api_tokens SET last_used_at = GREATEST($2, created_at, last_used_at)
			WHERE id = $1 AND revoked_at IS NULL
			AND (last_used_at IS NULL OR last_used_at < $3)`, tokenID, usedAt, before)
		if err != nil {
			return fmt.Errorf("touch API token: %w", err)
		}
		return nil
	})
}

func (s *Store) RevokeAPIToken(ctx context.Context, orgID, userID, tokenID string, revokedAt time.Time, audit *AuditEvent) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		err := tx.QueryRow(ctx, `UPDATE api_tokens token SET revoked_at = $4
			FROM users user_record
			WHERE token.id = $3 AND token.user_id = user_record.id
			AND user_record.id = $2 AND user_record.org_id = $1
			AND token.revoked_at IS NULL
			RETURNING jsonb_build_object('id', token.id, 'kind', token.kind, 'name', token.name,
				'prefix', token.token_prefix, 'scopes', token.scopes, 'expires_at', token.expires_at,
				'created_at', token.created_at, 'last_used_at', token.last_used_at)`, orgID, userID, tokenID, revokedAt).Scan(&before)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("revoke API token: %w", err)
		}
		return insertAuditEvent(ctx, tx, orgID, audit, "api_token.revoked", "api_token", tokenID, before, nil)
	})
}

type AcceptRevokeAPITokenParams struct {
	OrgID, UserID, TokenID string
	RevokedAt              time.Time
	Idempotency            CompleteIdempotencyParams
	Audit                  *AuditEvent
}

func (s *Store) AcceptRevokeAPIToken(ctx context.Context, params AcceptRevokeAPITokenParams) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var before json.RawMessage
		err := tx.QueryRow(ctx, `UPDATE api_tokens token SET revoked_at = $4
			FROM users user_record WHERE token.id = $3 AND token.user_id = user_record.id
			AND user_record.id = $2 AND user_record.org_id = $1 AND token.revoked_at IS NULL
			RETURNING jsonb_build_object('id', token.id, 'kind', token.kind, 'name', token.name,
				'prefix', token.token_prefix, 'scopes', token.scopes, 'expires_at', token.expires_at,
				'created_at', token.created_at, 'last_used_at', token.last_used_at)`,
			params.OrgID, params.UserID, params.TokenID, params.RevokedAt).Scan(&before)
		if err != nil {
			return mapMutationError("accept API token revocation", err)
		}
		if err := completeIdempotency(ctx, tx, params.Idempotency); err != nil {
			return err
		}
		return insertAuditEvent(ctx, tx, params.OrgID, params.Audit, "api_token.revoked", "api_token", params.TokenID, before, nil)
	})
}

func (s *Store) AuthorizeApp(ctx context.Context, orgID, appID string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		err := tx.QueryRow(ctx, `SELECT true FROM apps
			WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`, orgID, appID).Scan(&exists)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("authorize app: %w", err)
		}
		return nil
	})
}
