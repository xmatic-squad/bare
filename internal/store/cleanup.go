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
	}
	for _, step := range steps {
		if _, err := s.db.ExecContext(ctx, step.query, step.args...); err != nil {
			return fmt.Errorf("store: чистка (%s): %w", step.what, err)
		}
	}
	// Ключи комнат: у каждой комнаты остаются два последних key_id. Тем же
	// запросом обрезает их rekey (internal/store/rooms.go): порядок один,
	// иначе чистка и rekey держали бы разные ключи.
	return trimRoomKeys(ctx, s.db, "")
}
