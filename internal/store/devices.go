package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrDeviceTaken — идентификатор устройства занят другим пользователем
// (ADR-017): клиент генерирует новый.
var ErrDeviceTaken = errors.New("store: устройство занято")

// Device — строка devices без самой push-подписки: клиенту отдаётся
// только факт её наличия.
type Device struct {
	ID        string
	CreatedAt int64
	LastSeen  int64
	HasPush   bool
}

// RegisterDevice заводит устройство или подтверждает уже заведённое,
// обновляет last_seen и привязывает к устройству текущую сессию (ADR-021).
// Первое значение — было ли устройство создано; занятый чужим id —
// ErrDeviceTaken.
func (s *Store) RegisterDevice(ctx context.Context, id, nick string, tokenHash []byte, now int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: регистрация устройства: %w", err)
	}
	defer tx.Rollback()

	// ON CONFLICT DO NOTHING вместо разбора кода ошибки драйвера: занятый
	// id виден по нулю затронутых строк, а чей он — по следующему запросу.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO devices (id, nick, created_at, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`, id, nick, now, now)
	if err != nil {
		return false, fmt.Errorf("store: регистрация устройства: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: регистрация устройства: %w", err)
	}
	created := n == 1
	if !created {
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT nick FROM devices WHERE id = ?`, id).Scan(&owner); err != nil {
			return false, fmt.Errorf("store: регистрация устройства: %w", err)
		}
		if owner != nick {
			return false, ErrDeviceTaken
		}
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET last_seen = ? WHERE id = ?`, now, id); err != nil {
			return false, fmt.Errorf("store: регистрация устройства: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET device_id = ? WHERE token_hash = ?`, id, tokenHash); err != nil {
		return false, fmt.Errorf("store: привязка сессии к устройству: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: регистрация устройства: %w", err)
	}
	return created, nil
}

// DeviceOwned — принадлежит ли устройство этому пользователю. Чужое
// и несуществующее неразличимы: снаружи и то и другое unknown_device.
func (s *Store) DeviceOwned(ctx context.Context, id, nick string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE id = ? AND nick = ?`, id, nick).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: проверка устройства: %w", err)
	}
	return true, nil
}

// Devices — устройства пользователя в порядке появления.
func (s *Store) Devices(ctx context.Context, nick string) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, last_seen, push_subscription IS NOT NULL
		FROM devices WHERE nick = ? ORDER BY created_at, id`, nick)
	if err != nil {
		return nil, fmt.Errorf("store: список устройств: %w", err)
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.CreatedAt, &d.LastSeen, &d.HasPush); err != nil {
			return nil, fmt.Errorf("store: список устройств: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: список устройств: %w", err)
	}
	return out, nil
}

// DeleteDevice удаляет устройство пользователя; очередь, push-подписку
// и сессии устройства уносит каскад (docs/storage.md). Первое значение —
// была ли строка: чужое устройство удалить нельзя.
func (s *Store) DeleteDevice(ctx context.Context, id, nick string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ? AND nick = ?`, id, nick)
	if err != nil {
		return false, fmt.Errorf("store: удаление устройства: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: удаление устройства: %w", err)
	}
	return n > 0, nil
}

// TouchDevice — устройство подключилось по SSE: неотработанного пуша
// больше нет (ADR-023), время последнего появления — сейчас.
func (s *Store) TouchDevice(ctx context.Context, id string, now int64) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE devices SET push_pending = 0, last_seen = ? WHERE id = ?`, now, id); err != nil {
		return fmt.Errorf("store: подключение устройства: %w", err)
	}
	return nil
}
