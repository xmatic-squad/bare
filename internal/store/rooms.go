package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Комнаты (ADR-007, ADR-018): владелец меняет состав, ключи заворачивают
// клиенты. Сервер хранит и раздаёт завёрнутые ключи, но прочитать их
// не может — это шифротекст, как и всё остальное в базе.

// Ошибки комнат, которые обработчику нужно различать. Остальное —
// внутренние сбои.
var (
	// ErrNotOwner — комнату меняет не её владелец. Несуществующая комната
	// отвечает тем же: знать о ней постороннему незачем.
	ErrNotOwner = errors.New("store: не владелец комнаты")
	// ErrUnknownUser — в add ник, которого нет.
	ErrUnknownUser = errors.New("store: нет такого ника")
	// ErrNotMember — в remove ник, который не участник комнаты.
	ErrNotMember = errors.New("store: не участник комнаты")
	// ErrOwnerRemoval — владельца из состава убрать нельзя.
	ErrOwnerRemoval = errors.New("store: владельца убрать нельзя")
	// ErrKeyExists — такой keyId у комнаты уже был.
	ErrKeyExists = errors.New("store: ключ комнаты уже есть")
	// ErrKeysMismatch — множество keys[].to не равно итоговому составу.
	ErrKeysMismatch = errors.New("store: ключи не по составу")
	// ErrRoomExists — идентификатор комнаты занят (ADR-037).
	ErrRoomExists = errors.New("store: такая комната уже есть")
)

// RoomKey — завёрнутый ключ комнаты, каким его видит участник
// (docs/protocol.md, «Типы»). Развернуть его может только он.
type RoomKey struct {
	KeyID string
	From  string // кто завернул
	IV    string
	CT    string
}

// WrappedKey — запись keys[] запроса: кому предназначен ключ и что в нём.
// Заворачивал тот, кто прислал запрос.
type WrappedKey struct {
	To string
	IV string
	CT string
}

// Room — комната и её состав. Keys — завёрнутые ключи того, кто
// спрашивает, от старого к новому: сервер держит два последних keyId
// (ADR-018) и отдаёт участнику все, иначе пропущенный в офлайне ключ
// не добыть ничем (ADR-059). Пусто — ключей у него нет. NeedsRekey —
// состав уменьшился, а нового ключа ещё не было (ADR-041).
type Room struct {
	ID         string
	Name       string
	Owner      string
	Members    []string // по joined_at
	CreatedAt  int64
	Keys       []RoomKey
	NeedsRekey bool
}

// Recipient — участник, его устройства и его текущий ключ: событие room
// уходит каждому со своим ключом (docs/protocol.md, «Комнаты»).
type Recipient struct {
	Nick    string
	Devices []string
	Key     *RoomKey
}

// RoomChange — итог изменения комнаты: кому уходит room, а кому room_left.
// Room.Keys всегда пусты — ключ у каждого получателя свой, он в Recipient.
type RoomChange struct {
	Room    Room
	Members []Recipient // итоговый состав
	Left    []Recipient // выбывшие
}

// Rooms — комнаты, где пользователь участник, каждая со всеми его
// завёрнутыми ключами: сервер держит два последних keyId, и участник,
// пропустивший rekey в офлайне, добирает пропущенный отсюда
// (ADR-059, docs/protocol.md, «Комнаты»).
func (s *Store) Rooms(ctx context.Context, nick string) ([]Room, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.name, r.owner, r.created_at, r.needs_rekey
		FROM rooms r JOIN room_members m ON m.room_id = r.id
		WHERE m.nick = ? ORDER BY r.created_at, r.id`, nick)
	if err != nil {
		return nil, fmt.Errorf("store: список комнат: %w", err)
	}
	defer rows.Close()

	var out []Room
	at := make(map[string]int)
	for rows.Next() {
		var r Room
		if err := rows.Scan(&r.ID, &r.Name, &r.Owner, &r.CreatedAt, &r.NeedsRekey); err != nil {
			return nil, fmt.Errorf("store: список комнат: %w", err)
		}
		r.Members = []string{}
		at[r.ID] = len(out)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: список комнат: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}

	members, err := s.db.QueryContext(ctx, `
		SELECT room_id, nick FROM room_members
		WHERE room_id IN (SELECT room_id FROM room_members WHERE nick = ?)
		ORDER BY room_id, joined_at, nick`, nick)
	if err != nil {
		return nil, fmt.Errorf("store: состав комнат: %w", err)
	}
	defer members.Close()

	for members.Next() {
		var room, member string
		if err := members.Scan(&room, &member); err != nil {
			return nil, fmt.Errorf("store: состав комнат: %w", err)
		}
		if i, ok := at[room]; ok {
			out[i].Members = append(out[i].Members, member)
		}
	}
	if err := members.Err(); err != nil {
		return nil, fmt.Errorf("store: состав комнат: %w", err)
	}

	// Порядок — от старого ключа к новому, тот же, что у обрезки
	// и у «текущего» (ADR-042): последний в списке и есть текущий.
	keys, err := s.db.QueryContext(ctx, `
		SELECT room_id, key_id, sender, iv, ct FROM room_keys
		WHERE nick = ? AND room_id IN (SELECT room_id FROM room_members WHERE nick = ?)
		ORDER BY room_id, created_at, key_id`, nick, nick)
	if err != nil {
		return nil, fmt.Errorf("store: ключи комнат: %w", err)
	}
	defer keys.Close()

	for keys.Next() {
		var room string
		var k RoomKey
		if err := keys.Scan(&room, &k.KeyID, &k.From, &k.IV, &k.CT); err != nil {
			return nil, fmt.Errorf("store: ключи комнат: %w", err)
		}
		if i, ok := at[room]; ok {
			out[i].Keys = append(out[i].Keys, k)
		}
	}
	if err := keys.Err(); err != nil {
		return nil, fmt.Errorf("store: ключи комнат: %w", err)
	}
	return out, nil
}

// NewRoom — что нужно, чтобы завести комнату. Идентификатор выдаёт
// клиент (ADR-037), ключ ровно один — себе (docs/protocol.md, «Комнаты»).
type NewRoom struct {
	ID    string
	Name  string
	Owner string
	KeyID string
	Key   WrappedKey
	Now   int64
}

// CreateRoom заводит комнату, её единственного участника-владельца и его
// завёрнутый ключ — в одной транзакции. Идентификатор приходит от клиента
// (ADR-037); занятый — ErrRoomExists, без слияния с существующей комнатой.
func (s *Store) CreateRoom(ctx context.Context, n NewRoom) (RoomChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoomChange{}, fmt.Errorf("store: создание комнаты: %w", err)
	}
	defer tx.Rollback()

	// Проверка до вставки, а не разбор ошибки драйвера: она читаема
	// и не зависит от текста, который вернёт SQLite.
	if _, err := roomRow(ctx, tx, n.ID); err == nil {
		return RoomChange{}, ErrRoomExists
	} else if !errors.Is(err, ErrNotFound) {
		return RoomChange{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rooms (id, name, owner, created_at) VALUES (?, ?, ?, ?)`,
		n.ID, n.Name, n.Owner, n.Now); err != nil {
		return RoomChange{}, fmt.Errorf("store: создание комнаты: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO room_members (room_id, nick, joined_at) VALUES (?, ?, ?)`,
		n.ID, n.Owner, n.Now); err != nil {
		return RoomChange{}, fmt.Errorf("store: создание комнаты (участник): %w", err)
	}
	if err := insertKeys(ctx, tx, n.ID, n.Owner, n.KeyID, []WrappedKey{n.Key}, n.Now); err != nil {
		return RoomChange{}, err
	}
	devices, err := devicesOf(ctx, tx, []string{n.Owner})
	if err != nil {
		return RoomChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return RoomChange{}, fmt.Errorf("store: создание комнаты: %w", err)
	}

	key := RoomKey{KeyID: n.KeyID, From: n.Owner, IV: n.Key.IV, CT: n.Key.CT}
	return RoomChange{
		Room: Room{
			ID:        n.ID,
			Name:      n.Name,
			Owner:     n.Owner,
			Members:   []string{n.Owner},
			CreatedAt: n.Now,
		},
		Members: []Recipient{{Nick: n.Owner, Devices: devices[n.Owner], Key: &key}},
	}, nil
}

// MembersChange — смена состава и rekey одним запросом (ADR-018).
// Add и Remove — ники без повторов и без пересечения; пустые — чистый rekey.
type MembersChange struct {
	RoomID string
	Owner  string // от чьего имени идёт запрос: он обязан быть владельцем
	Add    []string
	Remove []string
	KeyID  string
	Keys   []WrappedKey
	Now    int64
}

// UpdateMembers меняет состав и раздаёт новый ключ — всё в одной
// транзакции: состав без ключа или ключ без состава невозможны (ADR-018).
// Порядок проверок — docs/protocol.md, «Комнаты»; отказ на любой из них
// не меняет ни строки.
func (s *Store) UpdateMembers(ctx context.Context, c MembersChange) (RoomChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoomChange{}, fmt.Errorf("store: смена состава: %w", err)
	}
	defer tx.Rollback()

	room, err := roomRow(ctx, tx, c.RoomID)
	if errors.Is(err, ErrNotFound) {
		return RoomChange{}, ErrNotOwner
	}
	if err != nil {
		return RoomChange{}, err
	}
	if room.Owner != c.Owner {
		return RoomChange{}, ErrNotOwner
	}

	current, err := roomMembers(ctx, tx, c.RoomID)
	if err != nil {
		return RoomChange{}, err
	}
	for _, nick := range c.Add {
		ok, err := userExists(ctx, tx, nick)
		if err != nil {
			return RoomChange{}, err
		}
		if !ok {
			return RoomChange{}, ErrUnknownUser
		}
	}
	member := make(map[string]bool, len(current))
	for _, nick := range current {
		member[nick] = true
	}
	for _, nick := range c.Remove {
		if !member[nick] {
			return RoomChange{}, ErrNotMember
		}
	}
	for _, nick := range c.Remove {
		if nick == room.Owner {
			return RoomChange{}, ErrOwnerRemoval
		}
	}
	used, err := keyUsed(ctx, tx, c.RoomID, c.KeyID)
	if err != nil {
		return RoomChange{}, err
	}
	if used {
		return RoomChange{}, ErrKeyExists
	}
	if !sameNicks(afterChange(current, c.Add, c.Remove), keyTargets(c.Keys)) {
		return RoomChange{}, ErrKeysMismatch
	}

	for _, nick := range c.Remove {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM room_members WHERE room_id = ? AND nick = ?`, c.RoomID, nick); err != nil {
			return RoomChange{}, fmt.Errorf("store: смена состава (убрать): %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM room_keys WHERE room_id = ? AND nick = ?`, c.RoomID, nick); err != nil {
			return RoomChange{}, fmt.Errorf("store: смена состава (ключи убранного): %w", err)
		}
	}
	for _, nick := range c.Add {
		// Уже состоящего участника запрос не двигает: joined_at остаётся
		// прежним, порядок состава не прыгает.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO room_members (room_id, nick, joined_at) VALUES (?, ?, ?)
			ON CONFLICT(room_id, nick) DO NOTHING`, c.RoomID, nick, c.Now); err != nil {
			return RoomChange{}, fmt.Errorf("store: смена состава (добавить): %w", err)
		}
	}
	if err := insertKeys(ctx, tx, c.RoomID, c.Owner, c.KeyID, c.Keys, c.Now); err != nil {
		return RoomChange{}, err
	}
	if err := trimRoomKeys(ctx, tx, c.RoomID); err != nil {
		return RoomChange{}, err
	}
	// Ключ роздан всему итоговому составу — долг закрыт (ADR-041).
	if _, err := tx.ExecContext(ctx, `
		UPDATE rooms SET needs_rekey = 0 WHERE id = ?`, c.RoomID); err != nil {
		return RoomChange{}, fmt.Errorf("store: снятие долга по ключу: %w", err)
	}
	room.NeedsRekey = false

	final, err := roomMembers(ctx, tx, c.RoomID)
	if err != nil {
		return RoomChange{}, err
	}
	devices, err := devicesOf(ctx, tx, final)
	if err != nil {
		return RoomChange{}, err
	}
	left, err := devicesOf(ctx, tx, c.Remove)
	if err != nil {
		return RoomChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return RoomChange{}, fmt.Errorf("store: смена состава: %w", err)
	}

	room.Members = final
	change := RoomChange{Room: room}
	wrapped := make(map[string]WrappedKey, len(c.Keys))
	for _, k := range c.Keys {
		wrapped[k.To] = k
	}
	for _, nick := range final {
		k := wrapped[nick]
		key := RoomKey{KeyID: c.KeyID, From: c.Owner, IV: k.IV, CT: k.CT}
		change.Members = append(change.Members, Recipient{Nick: nick, Devices: devices[nick], Key: &key})
	}
	for _, nick := range c.Remove {
		change.Left = append(change.Left, Recipient{Nick: nick, Devices: left[nick]})
	}
	return change, nil
}

// LeaveRoom убирает участника и его ключи. Вышел владелец — владение
// получает участник с наименьшим joined_at; не осталось никого — комната
// удаляется (ADR-018). В RoomChange.Members — оставшиеся с их текущими
// ключами: им уходит room с needsRekey. В RoomChange.Left — сам вышедший:
// его другим устройствам уходит room_left, иначе комната висела бы у них
// до следующего ready (ADR-041). Не участник — ErrNotFound.
func (s *Store) LeaveRoom(ctx context.Context, roomID, nick string) (RoomChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoomChange{}, fmt.Errorf("store: выход из комнаты: %w", err)
	}
	defer tx.Rollback()

	room, err := roomRow(ctx, tx, roomID)
	if err != nil {
		return RoomChange{}, err
	}
	var one int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM room_members WHERE room_id = ? AND nick = ?`, roomID, nick).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return RoomChange{}, ErrNotFound
	}
	if err != nil {
		return RoomChange{}, fmt.Errorf("store: выход из комнаты: %w", err)
	}
	gone, err := devicesOf(ctx, tx, []string{nick})
	if err != nil {
		return RoomChange{}, err
	}
	change, err := leaveRoom(ctx, tx, room, nick)
	if err != nil {
		return RoomChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return RoomChange{}, fmt.Errorf("store: выход из комнаты: %w", err)
	}
	change.Left = []Recipient{{Nick: nick, Devices: gone[nick]}}
	return change, nil
}

// leaveRoom убирает участника и его ключи внутри чужой транзакции: это
// общее у POST /api/rooms/{id}/leave и удаления аккаунта — по составу
// комнаты это один и тот же выход участника (ADR-018, ADR-041).
//
// В RoomChange.Members — оставшиеся с их текущими ключами и признаком
// needsRekey; пусто, если комната опустела и удалена. Проверку членства
// и рассылку берут на себя вызывающие.
func leaveRoom(ctx context.Context, tx *sql.Tx, room Room, nick string) (RoomChange, error) {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM room_members WHERE room_id = ? AND nick = ?`, room.ID, nick); err != nil {
		return RoomChange{}, fmt.Errorf("store: выход из комнаты: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM room_keys WHERE room_id = ? AND nick = ?`, room.ID, nick); err != nil {
		return RoomChange{}, fmt.Errorf("store: выход из комнаты (ключи): %w", err)
	}
	rest, err := roomMembers(ctx, tx, room.ID)
	if err != nil {
		return RoomChange{}, err
	}
	if len(rest) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, room.ID); err != nil {
			return RoomChange{}, fmt.Errorf("store: удаление пустой комнаты: %w", err)
		}
		// Комнаты больше нет: ключ ей не нужен, и долга за ней не остаётся.
		room.Members = nil
		room.NeedsRekey = false
		return RoomChange{Room: room}, nil
	}
	if room.Owner == nick {
		room.Owner = rest[0]
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET owner = ? WHERE id = ?`, room.Owner, room.ID); err != nil {
			return RoomChange{}, fmt.Errorf("store: передача владения: %w", err)
		}
	}
	// Состав уменьшился: комнате нужен новый ключ. Признак ждёт владельца
	// в базе, а не только в событии, — офлайн его больше не теряет (ADR-041).
	if _, err := tx.ExecContext(ctx, `
		UPDATE rooms SET needs_rekey = 1 WHERE id = ?`, room.ID); err != nil {
		return RoomChange{}, fmt.Errorf("store: долг по ключу комнаты: %w", err)
	}
	room.NeedsRekey = true
	keys, err := currentKeys(ctx, tx, room.ID)
	if err != nil {
		return RoomChange{}, err
	}
	devices, err := devicesOf(ctx, tx, rest)
	if err != nil {
		return RoomChange{}, err
	}

	room.Members = rest
	change := RoomChange{Room: room}
	for _, member := range rest {
		r := Recipient{Nick: member, Devices: devices[member]}
		if k, ok := keys[member]; ok {
			key := k
			r.Key = &key
		}
		change.Members = append(change.Members, r)
	}
	return change, nil
}

// DeleteRoom удаляет комнату целиком; членство и ключи уносит каскад.
// В RoomChange.Left — все участники: им уходит room_left. Не владелец
// и несуществующая комната — ErrNotOwner.
func (s *Store) DeleteRoom(ctx context.Context, roomID, owner string) (RoomChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoomChange{}, fmt.Errorf("store: удаление комнаты: %w", err)
	}
	defer tx.Rollback()

	room, err := roomRow(ctx, tx, roomID)
	if errors.Is(err, ErrNotFound) {
		return RoomChange{}, ErrNotOwner
	}
	if err != nil {
		return RoomChange{}, err
	}
	if room.Owner != owner {
		return RoomChange{}, ErrNotOwner
	}
	members, err := roomMembers(ctx, tx, roomID)
	if err != nil {
		return RoomChange{}, err
	}
	devices, err := devicesOf(ctx, tx, members)
	if err != nil {
		return RoomChange{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, roomID); err != nil {
		return RoomChange{}, fmt.Errorf("store: удаление комнаты: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RoomChange{}, fmt.Errorf("store: удаление комнаты: %w", err)
	}

	room.Members = members
	change := RoomChange{Room: room}
	for _, member := range members {
		change.Left = append(change.Left, Recipient{Nick: member, Devices: devices[member]})
	}
	return change, nil
}

// Access — что сервер знает о комнате перед отправкой в неё
// (docs/protocol.md, «Сообщения»). Имя нужно заголовку пуша: «#имя
// комнаты» (ADR-023).
type Access struct {
	Member   bool
	KnownKey bool
	Name     string
}

// RoomAccess — что сервер проверяет перед отправкой в комнату. keyId
// считается ключом комнаты, если есть хоть одна строка room_keys с таким
// key_id (docs/storage.md). Несуществующая комната отвечает пустым
// Access: снаружи она неотличима от чужой.
func (s *Store) RoomAccess(ctx context.Context, roomID, nick, keyID string) (Access, error) {
	var a Access
	err := s.db.QueryRowContext(ctx, `
		SELECT r.name,
		       EXISTS(SELECT 1 FROM room_members WHERE room_id = r.id AND nick = ?),
		       EXISTS(SELECT 1 FROM room_keys WHERE room_id = r.id AND key_id = ?)
		FROM rooms r WHERE r.id = ?`, nick, keyID, roomID).Scan(&a.Name, &a.Member, &a.KnownKey)
	if errors.Is(err, sql.ErrNoRows) {
		return Access{}, nil
	}
	if err != nil {
		return Access{}, fmt.Errorf("store: доступ к комнате: %w", err)
	}
	return a, nil
}

// currentKeysQuery — текущий ключ участника: строка room_keys с максимальным
// created_at (docs/storage.md). Порядок при совпадении времени тот же, что
// у обрезки, — иначе «текущий» и «оставленный» могли бы разойтись.
const currentKeysQuery = `
	SELECT room_id, nick, key_id, sender, iv, ct FROM (
		SELECT room_id, nick, key_id, sender, iv, ct,
		       ROW_NUMBER() OVER (
		           PARTITION BY room_id, nick
		           ORDER BY created_at DESC, key_id DESC
		       ) AS rn
		FROM room_keys
	) WHERE rn = 1`

// currentKeys — текущие ключи всех участников комнаты.
func currentKeys(ctx context.Context, tx *sql.Tx, roomID string) (map[string]RoomKey, error) {
	rows, err := tx.QueryContext(ctx, currentKeysQuery+` AND room_id = ?`, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: ключи комнаты: %w", err)
	}
	defer rows.Close()

	out := make(map[string]RoomKey)
	for rows.Next() {
		var room, nick string
		var k RoomKey
		if err := rows.Scan(&room, &nick, &k.KeyID, &k.From, &k.IV, &k.CT); err != nil {
			return nil, fmt.Errorf("store: ключи комнаты: %w", err)
		}
		out[nick] = k
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: ключи комнаты: %w", err)
	}
	return out, nil
}

// execer — то общее у *sql.DB и *sql.Tx, что нужно обрезке ключей: её
// зовут и транзакция rekey, и фоновая чистка.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// trimRoomKeys оставляет у комнаты два последних keyId (ADR-018). Пустой
// room — все комнаты: это фоновая чистка (docs/storage.md). Запрос один
// на оба случая, иначе rekey и чистка держали бы разные ключи.
//
// Текущий ключ участника обрезка не трогает: новый keyId раздаётся всем
// участникам сразу (иначе keys_mismatch), поэтому самый свежий key_id
// комнаты есть у каждого, а он остаётся всегда. Какой из ключей свежий —
// однозначно: время ключа строго растёт (insertKeys).
//
// Возраст key_id — время его самой поздней записи: ключ раздаётся не одной
// строкой, а по строке на участника.
func trimRoomKeys(ctx context.Context, x execer, room string) error {
	_, err := x.ExecContext(ctx, `
		DELETE FROM room_keys
		WHERE (? = '' OR room_id = ?)
		  AND (room_id, key_id) NOT IN (
			SELECT room_id, key_id FROM (
				SELECT room_id, key_id,
				       ROW_NUMBER() OVER (
				           PARTITION BY room_id
				           ORDER BY MAX(created_at) DESC, key_id DESC
				       ) AS rn
				FROM room_keys
				GROUP BY room_id, key_id
			) WHERE rn <= ?
		)`, room, room, roomKeysKept)
	if err != nil {
		return fmt.Errorf("store: обрезка ключей комнаты: %w", err)
	}
	return nil
}

// roomRow читает комнату без состава и ключей. Нет такой — ErrNotFound.
func roomRow(ctx context.Context, tx *sql.Tx, roomID string) (Room, error) {
	var r Room
	err := tx.QueryRowContext(ctx, `
		SELECT id, name, owner, created_at, needs_rekey FROM rooms WHERE id = ?`, roomID).
		Scan(&r.ID, &r.Name, &r.Owner, &r.CreatedAt, &r.NeedsRekey)
	if errors.Is(err, sql.ErrNoRows) {
		return Room{}, ErrNotFound
	}
	if err != nil {
		return Room{}, fmt.Errorf("store: чтение комнаты: %w", err)
	}
	return r, nil
}

// roomMembers — состав комнаты по joined_at (docs/protocol.md, «Типы»).
func roomMembers(ctx context.Context, tx *sql.Tx, roomID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT nick FROM room_members WHERE room_id = ? ORDER BY joined_at, nick`, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: состав комнаты: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var nick string
		if err := rows.Scan(&nick); err != nil {
			return nil, fmt.Errorf("store: состав комнаты: %w", err)
		}
		out = append(out, nick)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: состав комнаты: %w", err)
	}
	return out, nil
}

// userExists — есть ли такой ник.
func userExists(ctx context.Context, tx *sql.Tx, nick string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE nick = ?`, nick).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: проверка ника: %w", err)
	}
	return true, nil
}

// keyUsed — был ли уже такой keyId у комнаты.
func keyUsed(ctx context.Context, tx *sql.Tx, roomID, keyID string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM room_keys WHERE room_id = ? AND key_id = ? LIMIT 1`, roomID, keyID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: проверка ключа комнаты: %w", err)
	}
	return true, nil
}

// insertKeys раскладывает завёрнутые ключи по участникам.
//
// Время ключа — строго позже всех прежних ключей комнаты. Текущий ключ
// участника — строка с максимальным created_at (docs/storage.md), а два
// rekey подряд укладываются в одну миллисекунду. Без этого «последним»
// оказался бы прежний ключ, обрезка до двух последних keyId выбросила бы
// свежий, и комната откатилась бы на ключ, которого у новых участников нет.
func insertKeys(ctx context.Context, tx *sql.Tx, roomID, sender, keyID string, keys []WrappedKey, now int64) error {
	var last int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(created_at), 0) FROM room_keys WHERE room_id = ?`, roomID).Scan(&last); err != nil {
		return fmt.Errorf("store: время ключа комнаты: %w", err)
	}
	if now <= last {
		now = last + 1
	}
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO room_keys (room_id, nick, key_id, sender, iv, ct, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			roomID, k.To, keyID, sender, k.IV, k.CT, now); err != nil {
			return fmt.Errorf("store: раздача ключа комнаты: %w", err)
		}
	}
	return nil
}

// devicesOf — устройства перечисленных пользователей.
func devicesOf(ctx context.Context, tx *sql.Tx, nicks []string) (map[string][]string, error) {
	out := make(map[string][]string, len(nicks))
	if len(nicks) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(nicks))
	for _, nick := range nicks {
		args = append(args, nick)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT nick, id FROM devices WHERE nick IN (?`+
		strings.Repeat(", ?", len(nicks)-1)+`) ORDER BY nick, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: устройства участников: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var nick, id string
		if err := rows.Scan(&nick, &id); err != nil {
			return nil, fmt.Errorf("store: устройства участников: %w", err)
		}
		out[nick] = append(out[nick], id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: устройства участников: %w", err)
	}
	return out, nil
}

// afterChange — итоговый состав: текущий без remove плюс add.
func afterChange(current, add, remove []string) []string {
	gone := make(map[string]bool, len(remove))
	for _, nick := range remove {
		gone[nick] = true
	}
	out := make([]string, 0, len(current)+len(add))
	seen := make(map[string]bool, len(current)+len(add))
	for _, nick := range current {
		if gone[nick] || seen[nick] {
			continue
		}
		seen[nick] = true
		out = append(out, nick)
	}
	for _, nick := range add {
		if seen[nick] {
			continue
		}
		seen[nick] = true
		out = append(out, nick)
	}
	return out
}

// keyTargets — кому предназначены завёрнутые ключи.
func keyTargets(keys []WrappedKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.To)
	}
	return out
}

// sameNicks — совпадают ли множества ников. Порядок не важен: ключи
// приходят в порядке клиента, состав — по joined_at.
//
// Найденный ник из множества вычёркивается: два ключа одному участнику
// вместо ключа другому — это keys_mismatch, а не совпадение по длине.
// Иначе такой запрос дошёл бы до вставки и упал на ключе room_keys уже
// внутри транзакции, отдав клиенту 500 вместо разбираемого кода.
func sameNicks(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, nick := range a {
		set[nick] = true
	}
	for _, nick := range b {
		if !set[nick] {
			return false
		}
		delete(set, nick)
	}
	return len(set) == 0
}
