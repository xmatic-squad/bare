package store

import (
	"context"
	"fmt"
)

// Contact — строка списка чатов: собеседник и его публичный ключ.
// Доверие к ключу — TOFU на клиенте (ADR-016).
type Contact struct {
	Nick      string
	PublicKey string
	CreatedAt int64
}

// Contacts — контакты пользователя в порядке появления.
func (s *Store) Contacts(ctx context.Context, nick string) ([]Contact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.peer, u.public_key, c.created_at
		FROM contacts c JOIN users u ON u.nick = c.peer
		WHERE c.nick = ? ORDER BY c.created_at, c.peer`, nick)
	if err != nil {
		return nil, fmt.Errorf("store: список контактов: %w", err)
	}
	defer rows.Close()

	var out []Contact
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.Nick, &c.PublicKey, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: список контактов: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: список контактов: %w", err)
	}
	return out, nil
}

// AddContact заводит одну строку списка чатов. Первое значение — была ли
// она создана. Зеркальную строку собеседнику эта операция не заводит:
// обе стороны появляются только при первом сообщении (ADR-019).
func (s *Store) AddContact(ctx context.Context, nick, peer string, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO contacts (nick, peer, created_at) VALUES (?, ?, ?)
		ON CONFLICT(nick, peer) DO NOTHING`, nick, peer, now)
	if err != nil {
		return false, fmt.Errorf("store: добавление контакта: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: добавление контакта: %w", err)
	}
	return n > 0, nil
}

// DeleteContact убирает чат из списка. Только своя строка: зеркальная
// у собеседника остаётся, блокировок в v1 нет (ADR-019).
func (s *Store) DeleteContact(ctx context.Context, nick, peer string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM contacts WHERE nick = ? AND peer = ?`, nick, peer); err != nil {
		return fmt.Errorf("store: удаление контакта: %w", err)
	}
	return nil
}
