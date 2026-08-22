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

// Push-подписка принадлежит устройству (ADR-023). Сервер хранит её как
// непрозрачный JSON: разбирает его только отправитель пушей.

// SetPush ставит подписку устройства и сбрасывает неотработанный пуш:
// устройство снова готово его принять (ADR-023). Первое значение — было
// ли такое устройство у этого пользователя.
func (s *Store) SetPush(ctx context.Context, id, nick, subscription string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE devices SET push_subscription = ?, push_pending = 0
		WHERE id = ? AND nick = ?`, subscription, id, nick)
	if err != nil {
		return false, fmt.Errorf("store: push-подписка: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: push-подписка: %w", err)
	}
	return n > 0, nil
}

// ClearPush снимает подписку устройства. Подписки не было — это не
// ошибка: снимать нечего.
func (s *Store) ClearPush(ctx context.Context, id, nick string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE devices SET push_subscription = NULL WHERE id = ? AND nick = ?`, id, nick)
	if err != nil {
		return false, fmt.Errorf("store: снятие push-подписки: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: снятие push-подписки: %w", err)
	}
	return n > 0, nil
}

// ClaimPush забирает право на пуш: устройству с подпиской и без
// неотработанного пуша ставит push_pending = 1 и отдаёт подписку.
// Второе значение — досталось ли право.
//
// Захват и проверка — один запрос: два сообщения подряд приходят
// в разных горутинах, а молчащее устройство получает один пуш, не ленту
// (ADR-023). Проигравший запрос уходит ни с чем.
func (s *Store) ClaimPush(ctx context.Context, id string) (string, bool, error) {
	var subscription string
	err := s.db.QueryRowContext(ctx, `
		UPDATE devices SET push_pending = 1
		WHERE id = ? AND push_pending = 0 AND push_subscription IS NOT NULL
		RETURNING push_subscription`, id).Scan(&subscription)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: захват пуша: %w", err)
	}
	return subscription, true, nil
}

// ReleasePush возвращает право на пуш: отправка не состоялась, значит
// и неотработанного пуша у устройства нет. Иначе одна ошибка push-сервиса
// затыкала бы уведомления устройства до следующего подключения по SSE.
func (s *Store) ReleasePush(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE devices SET push_pending = 0 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: возврат пуша: %w", err)
	}
	return nil
}

// DropPush снимает мёртвую подписку: push-сервис ответил 404 или 410
// (ADR-011). Неотработанного пуша заодно не остаётся — он никуда не ушёл.
func (s *Store) DropPush(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE devices SET push_subscription = NULL, push_pending = 0 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: снятие мёртвой push-подписки: %w", err)
	}
	return nil
}
