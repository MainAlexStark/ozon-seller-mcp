package pgstore

import (
	"context"
	"errors"
	"time"
)

// ErrShopExists — этот Client-Id у пользователя уже подключён.
var ErrShopExists = errors.New("pgstore: магазин с таким Client-Id уже подключён")

// Shop — подключённый магазин Ozon.
type Shop struct {
	ID           int64
	UserID       int64
	Name         string
	OzonClientID string
	APIKeyEnc    []byte
	APIKeyHint   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CheckedAt    *time.Time
	CheckError   string
}

const shopColumns = `id, user_id, name, ozon_client_id, api_key_enc, api_key_hint,
	created_at, updated_at, checked_at, check_error`

func scanShop(row interface{ Scan(...any) error }) (Shop, error) {
	var s Shop
	err := row.Scan(&s.ID, &s.UserID, &s.Name, &s.OzonClientID, &s.APIKeyEnc, &s.APIKeyHint,
		&s.CreatedAt, &s.UpdatedAt, &s.CheckedAt, &s.CheckError)
	return s, notFound(err)
}

// AddShop сохраняет магазин. checked — ключ только что проверен.
func (db *DB) AddShop(ctx context.Context, s Shop) (Shop, error) {
	out, err := scanShop(db.pool.QueryRow(ctx, `
		INSERT INTO shops (user_id, name, ozon_client_id, api_key_enc, api_key_hint, checked_at, check_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+shopColumns,
		s.UserID, s.Name, s.OzonClientID, s.APIKeyEnc, s.APIKeyHint, s.CheckedAt, s.CheckError))
	if isUniqueViolation(err) {
		return Shop{}, ErrShopExists
	}
	return out, err
}

// Shops — магазины пользователя в порядке подключения.
func (db *DB) Shops(ctx context.Context, userID int64) ([]Shop, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT `+shopColumns+` FROM shops WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Shop
	for rows.Next() {
		s, err := scanShop(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Shop возвращает магазин, только если он принадлежит пользователю.
func (db *DB) Shop(ctx context.Context, userID, shopID int64) (Shop, error) {
	return scanShop(db.pool.QueryRow(ctx,
		`SELECT `+shopColumns+` FROM shops WHERE id = $1 AND user_id = $2`, shopID, userID))
}

// UpdateShopKey меняет ключ магазина (ключ перевыпустили в кабинете Ozon).
func (db *DB) UpdateShopKey(ctx context.Context, userID, shopID int64, keyEnc []byte, hint string) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE shops SET api_key_enc = $3, api_key_hint = $4, updated_at = now(),
		                 checked_at = now(), check_error = ''
		WHERE id = $1 AND user_id = $2`, shopID, userID, keyEnc, hint)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RenameShop меняет отображаемое имя.
func (db *DB) RenameShop(ctx context.Context, userID, shopID int64, name string) error {
	tag, err := db.pool.Exec(ctx,
		`UPDATE shops SET name = $3, updated_at = now() WHERE id = $1 AND user_id = $2`, shopID, userID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetShopCheck записывает итог проверки ключа ("" — ключ рабочий).
func (db *DB) SetShopCheck(ctx context.Context, shopID int64, checkErr string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE shops SET checked_at = now(), check_error = $2 WHERE id = $1`, shopID, checkErr)
	return err
}

// DeleteShop удаляет магазин. Все подключения к нему умирают каскадом.
func (db *DB) DeleteShop(ctx context.Context, userID, shopID int64) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM shops WHERE id = $1 AND user_id = $2`, shopID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
