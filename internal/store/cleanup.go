package store

import (
	"context"
	"fmt"
	"time"
)

// Сроки хранения из docs/storage.md.
const (
	queueTTL     = 30 * 24 * time.Hour // недоставленное сообщение
	deviceTTL    = 90 * 24 * time.Hour // молчащее устройство
	roomKeysKept = 2                   // ключей комнаты на комнату

	cleanupEvery = time.Hour
)

// RunCleanup чистит базу раз в час, пока не отменён ctx. Первый проход —
// сразу при старте: сервер, который перезапускают чаще раза в час, иначе
// не чистился бы никогда. Ошибку отдаёт report; nil — молчать.
func (s *Store) RunCleanup(ctx context.Context, report func(error)) {
	tick := time.NewTicker(cleanupEvery)
	defer tick.Stop()
	for {
		if err := s.Cleanup(ctx, time.Now()); err != nil && report != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Cleanup выполняет один проход чистки (docs/storage.md, «Фоновая чистка»).
func (s *Store) Cleanup(ctx context.Context, now time.Time) error {
	ms := now.UnixMilli()
	steps := []struct {
		what  string
		query string
		args  []any
	}{
		{"очередь", `DELETE FROM queue WHERE created_at < ?`, []any{ms - queueTTL.Milliseconds()}},
		{"устройства", `DELETE FROM devices WHERE last_seen < ?`, []any{ms - deviceTTL.Milliseconds()}},
		{"сессии", `DELETE FROM sessions WHERE expires_at < ?`, []any{ms}},
		// Ключи комнат: у каждой комнаты остаются два последних key_id.
		// Возраст key_id — время его самой поздней записи: ключ раздаётся
		// участникам не одной строкой, а по строке на участника.
		{"ключи комнат", `
			DELETE FROM room_keys WHERE (room_id, key_id) NOT IN (
				SELECT room_id, key_id FROM (
					SELECT room_id, key_id,
					       ROW_NUMBER() OVER (
					           PARTITION BY room_id
					           ORDER BY MAX(created_at) DESC, key_id DESC
					       ) AS rn
					FROM room_keys
					GROUP BY room_id, key_id
				) WHERE rn <= ?
			)`, []any{roomKeysKept}},
	}
	for _, step := range steps {
		if _, err := s.db.ExecContext(ctx, step.query, step.args...); err != nil {
			return fmt.Errorf("store: чистка (%s): %w", step.what, err)
		}
	}
	return nil
}
