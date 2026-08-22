package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Транзитная очередь недоставленных конвертов, по строке на устройство
// (ADR-008). Доставлено и подтверждено ACK — удалено; не забрано
// за 30 дней — удалено фоновой чисткой.

// Queue — очередь устройства в порядке выдачи при подключении:
// created_at, msg_id (docs/protocol.md, «События»). Строки — готовые
// конверты, сервер их не разбирает.
func (s *Store) Queue(ctx context.Context, device string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT envelope FROM queue WHERE device_id = ? ORDER BY created_at, msg_id`, device)
	if err != nil {
		return nil, fmt.Errorf("store: очередь устройства: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var envelope string
		if err := rows.Scan(&envelope); err != nil {
			return nil, fmt.Errorf("store: очередь устройства: %w", err)
		}
		out = append(out, envelope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: очередь устройства: %w", err)
	}
	return out, nil
}

// Ack удаляет из очереди устройства перечисленные сообщения: клиент
// записал их в IndexedDB (docs/storage.md).
func (s *Store) Ack(ctx context.Context, device string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, device)
	for _, id := range ids {
		args = append(args, id)
	}
	query := `DELETE FROM queue WHERE device_id = ? AND msg_id IN (?` +
		strings.Repeat(", ?", len(ids)-1) + `)`
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: подтверждение доставки: %w", err)
	}
	return nil
}

// Delivery — одна доставка: готовый конверт и всё, что нужно, чтобы
// разложить его по очередям. Envelope сервер не разбирает, поэтому id
// приходит отдельным полем. Заполнено ровно одно из To и Room — адресат
// у конверта один (docs/protocol.md, «Типы»).
type Delivery struct {
	From     string // отправитель, он же один из получателей
	To       string // собеседник личного чата
	Room     string // комната
	Exclude  string // устройство отправителя: эхо ему не нужно (ADR-017)
	MsgID    string // id конверта, вторая половина ключа очереди
	Envelope string // готовый JSON конверта
	Now      int64  // серверное время, оно же ts конверта
}

// DeliverDM кладёт конверт личного чата в очередь всех устройств обоих
// собеседников, кроме отправившего, и заводит недостающие строки contacts
// в обе стороны — всё в одной транзакции (docs/protocol.md, «Сообщения»).
// Возвращает устройства, которым конверт надо отдать живьём.
//
// Повторный POST с тем же id — не ошибка: сервер историю идентификаторов
// не хранит, повтор порождает повторную доставку, а склеивает её клиент
// (ADR-017). Поэтому вставка молча пропускает уже лежащую в очереди
// строку, а список устройств от этого не зависит.
func (s *Store) DeliverDM(ctx context.Context, d Delivery) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: доставка: %w", err)
	}
	defer tx.Rollback()

	for _, pair := range [2][2]string{{d.From, d.To}, {d.To, d.From}} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contacts (nick, peer, created_at) VALUES (?, ?, ?)
			ON CONFLICT(nick, peer) DO NOTHING`, pair[0], pair[1], d.Now); err != nil {
			return nil, fmt.Errorf("store: доставка (контакты): %w", err)
		}
	}

	devices, err := deviceIDs(ctx, tx, d.From, d.To, d.Exclude)
	if err != nil {
		return nil, err
	}
	for _, id := range devices {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO queue (device_id, msg_id, envelope, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(device_id, msg_id) DO NOTHING`,
			id, d.MsgID, d.Envelope, d.Now); err != nil {
			return nil, fmt.Errorf("store: доставка (очередь): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: доставка: %w", err)
	}
	return devices, nil
}

// DeliverRoom кладёт конверт комнаты в очередь всех устройств всех
// участников, кроме отправившего (ADR-018), и возвращает эти устройства.
// Контактов у комнаты нет: список комнат клиент берёт из GET /api/rooms.
//
// Членство и keyId проверены раньше, отдельным запросом: между проверкой
// и этой транзакцией состав мог измениться, поэтому получателей она берёт
// из состава на момент доставки.
func (s *Store) DeliverRoom(ctx context.Context, d Delivery) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: доставка в комнату: %w", err)
	}
	defer tx.Rollback()

	devices, err := roomDeviceIDs(ctx, tx, d.Room, d.Exclude)
	if err != nil {
		return nil, err
	}
	for _, id := range devices {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO queue (device_id, msg_id, envelope, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(device_id, msg_id) DO NOTHING`,
			id, d.MsgID, d.Envelope, d.Now); err != nil {
			return nil, fmt.Errorf("store: доставка в комнату (очередь): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: доставка в комнату: %w", err)
	}
	return devices, nil
}

// deviceIDs — устройства обоих собеседников, кроме отправившего.
func deviceIDs(ctx context.Context, tx *sql.Tx, from, to, exclude string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM devices WHERE nick IN (?, ?) AND id <> ? ORDER BY id`, from, to, exclude)
	if err != nil {
		return nil, fmt.Errorf("store: доставка (устройства): %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: доставка (устройства): %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: доставка (устройства): %w", err)
	}
	return out, nil
}

// roomDeviceIDs — устройства всех участников комнаты, кроме отправившего.
func roomDeviceIDs(ctx context.Context, tx *sql.Tx, room, exclude string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT d.id FROM devices d JOIN room_members m ON m.nick = d.nick
		WHERE m.room_id = ? AND d.id <> ? ORDER BY d.id`, room, exclude)
	if err != nil {
		return nil, fmt.Errorf("store: доставка в комнату (устройства): %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: доставка в комнату (устройства): %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: доставка в комнату (устройства): %w", err)
	}
	return out, nil
}
