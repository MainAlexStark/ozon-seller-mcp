package pgstore

import (
	"context"
	"time"
)

// APIToken — персональный токен для автоматизации.
type APIToken struct {
	ID         int64
	UserID     int64
	ShopID     int64
	ShopName   string
	Name       string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// CreateAPIToken сохраняет токен (хеш).
func (db *DB) CreateAPIToken(ctx context.Context, userID, shopID int64, name, tokenHash string, scopes []string) (int64, error) {
	var id int64
	// Магазин проверяется в том же запросе: токен к чужому магазину
	// не создастся, даже если номер подставлен руками.
	err := db.pool.QueryRow(ctx, `
		INSERT INTO api_tokens (token_hash, user_id, shop_id, name, scopes)
		SELECT $1::text, $2::bigint, s.id, $4::text, $5::text[]
		FROM shops s WHERE s.id = $3::bigint AND s.user_id = $2::bigint
		RETURNING id`, tokenHash, userID, shopID, name, scopes).Scan(&id)
	return id, notFound(err)
}

// APITokens — токены пользователя.
func (db *DB) APITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT t.id, t.user_id, t.shop_id, s.name, t.name, t.scopes, t.created_at, t.last_used_at
		FROM api_tokens t JOIN shops s ON s.id = t.shop_id
		WHERE t.user_id = $1 ORDER BY t.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.ShopID, &t.ShopName, &t.Name, &t.Scopes,
			&t.CreatedAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken отзывает токен.
func (db *DB) DeleteAPIToken(ctx context.Context, userID, id int64) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM api_tokens WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// APITokenByHash проверяет предъявленный токен.
//
// Время последнего использования обновляется не чаще раза в минуту:
// писать в базу на каждый вызов инструмента ради этой отметки дорого.
func (db *DB) APITokenByHash(ctx context.Context, tokenHash string) (APIToken, error) {
	var t APIToken
	err := db.pool.QueryRow(ctx, `
		SELECT t.id, t.user_id, t.shop_id, t.name, t.scopes, t.created_at, t.last_used_at
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1 AND u.disabled_at IS NULL`, tokenHash).
		Scan(&t.ID, &t.UserID, &t.ShopID, &t.Name, &t.Scopes, &t.CreatedAt, &t.LastUsedAt)
	if err != nil {
		return APIToken{}, notFound(err)
	}
	if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
		_, _ = db.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, t.ID)
	}
	return t, nil
}

// --- Подключения OAuth, как их видит пользователь ---

// Grant — действующее подключение приложения к магазину.
type Grant struct {
	GrantID    string
	ClientName string
	ShopName   string
	Scopes     []string
	ExpiresAt  time.Time
}

// UserGrants — действующие подключения пользователя. Выдача живёт,
// пока жив её refresh (или access, если refresh не выдавался).
func (db *DB) UserGrants(ctx context.Context, userID int64) ([]Grant, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT g.grant_id, COALESCE(NULLIF(c.name, ''), g.client_id), s.name, g.scopes, g.expires_at
		FROM (
			SELECT DISTINCT ON (grant_id) grant_id, client_id, shop_id, scopes, expires_at
			FROM (
				SELECT grant_id, client_id, shop_id, scopes, expires_at
				FROM oauth_access_tokens WHERE user_id = $1 AND expires_at > now()
				UNION ALL
				SELECT grant_id, client_id, shop_id, scopes, expires_at
				FROM oauth_refresh_tokens WHERE user_id = $1 AND expires_at > now()
			) t
			ORDER BY grant_id, expires_at DESC
		) g
		JOIN shops s ON s.id = g.shop_id
		LEFT JOIN oauth_clients c ON c.id = g.client_id
		ORDER BY g.expires_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.GrantID, &g.ClientName, &g.ShopName, &g.Scopes, &g.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeUserGrant отзывает подключение, только если оно пользователя.
func (db *DB) RevokeUserGrant(ctx context.Context, userID int64, grantID string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	a, err := tx.Exec(ctx, `DELETE FROM oauth_access_tokens WHERE grant_id = $1 AND user_id = $2`, grantID, userID)
	if err != nil {
		return err
	}
	r, err := tx.Exec(ctx, `DELETE FROM oauth_refresh_tokens WHERE grant_id = $1 AND user_id = $2`, grantID, userID)
	if err != nil {
		return err
	}
	if a.RowsAffected()+r.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// --- Учёт вызовов ---

// RecordUsage увеличивает счётчик вызовов инструмента за сегодня.
func (db *DB) RecordUsage(ctx context.Context, userID, shopID int64, tool string, failed bool) error {
	errs := 0
	if failed {
		errs = 1
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO usage_daily (user_id, shop_id, day, tool, calls, errors)
		VALUES ($1, $2, current_date, $3, 1, $4)
		ON CONFLICT (user_id, shop_id, day, tool)
		DO UPDATE SET calls = usage_daily.calls + 1, errors = usage_daily.errors + EXCLUDED.errors`,
		userID, shopID, tool, errs)
	return err
}

// UsageSince — сколько вызовов пользователь сделал за последние days дней.
func (db *DB) UsageSince(ctx context.Context, userID int64, days int) (int64, error) {
	var n int64
	err := db.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(calls), 0)::bigint FROM usage_daily
		WHERE user_id = $1 AND day > current_date - $2::int`, userID, days).Scan(&n)
	return n, err
}
