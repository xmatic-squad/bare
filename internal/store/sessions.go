package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Session — строка sessions. Токена здесь нет: в базе лежит только
// SHA-256 от него (ADR-021).
type Session struct {
	TokenHash []byte
	Nick      string
	DeviceID  string // пусто, пока сессия не привязана к устройству
	CreatedAt int64
	ExpiresAt int64
}

// CreateSession записывает сессию. tokenHash — SHA-256 токена из cookie.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, nick string, createdAt, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, nick, device_id, created_at, expires_at)
		VALUES (?, ?, NULL, ?, ?)`, tokenHash, nick, createdAt, expiresAt)
	if err != nil {
		return fmt.Errorf("store: создание сессии: %w", err)
	}
	return nil
}

// Session читает живую сессию по хешу токена. Истёкшая считается
// отсутствующей: чистит её фоновая задача, а не запрос.
func (s *Store) Session(ctx context.Context, tokenHash []byte, now int64) (Session, error) {
	var (
		sess   Session
		device sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT token_hash, nick, device_id, created_at, expires_at
		FROM sessions WHERE token_hash = ? AND expires_at > ?`, tokenHash, now).
		Scan(&sess.TokenHash, &sess.Nick, &device, &sess.CreatedAt, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: чтение сессии: %w", err)
	}
	sess.DeviceID = device.String
	return sess, nil
}

// DeleteSession удаляет одну сессию — выход на этом устройстве.
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("store: удаление сессии: %w", err)
	}
	return nil
}
