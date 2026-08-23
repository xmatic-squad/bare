package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Credential — argon2id-хеш authKey, его соль и параметры (ADR-021).
// Параметры лежат рядом с хешем, чтобы их можно было повышать перехешем
// при очередном входе, а не миграцией всех аккаунтов разом.
type Credential struct {
	Hash   []byte
	Salt   []byte
	Params string
}

// User — строка users. PublicKey и KeyBlob — JSON клиента; сервер их
// не расшифровывает и не интерпретирует сверх проверки формы.
type User struct {
	Nick      string
	Cred      Credential
	PublicKey string
	KeyBlob   string
	CreatedAt int64
}

// CreateUser заводит пользователя. Занятый ник — ErrNickTaken.
func (s *Store) CreateUser(ctx context.Context, u User) error {
	// ON CONFLICT DO NOTHING вместо разбора кода ошибки драйвера:
	// занятый ник виден по нулю затронутых строк.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (nick, auth_hash, auth_salt, auth_params, public_key, key_blob, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(nick) DO NOTHING`,
		u.Nick, u.Cred.Hash, u.Cred.Salt, u.Cred.Params, u.PublicKey, u.KeyBlob, u.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: создание пользователя: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: создание пользователя: %w", err)
	}
	if n == 0 {
		return ErrNickTaken
	}
	return nil
}

// User читает пользователя по нику. Нет такого — ErrNotFound.
func (s *Store) User(ctx context.Context, nick string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `
		SELECT nick, auth_hash, auth_salt, auth_params, public_key, key_blob, created_at
		FROM users WHERE nick = ?`, nick).
		Scan(&u.Nick, &u.Cred.Hash, &u.Cred.Salt, &u.Cred.Params, &u.PublicKey, &u.KeyBlob, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: чтение пользователя: %w", err)
	}
	return u, nil
}

// SetAuth заменяет только хеш authKey — перехеш при входе, когда параметры
// в базе отстали от текущих (ADR-021).
func (s *Store) SetAuth(ctx context.Context, nick string, cred Credential) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET auth_hash = ?, auth_salt = ?, auth_params = ? WHERE nick = ?`,
		cred.Hash, cred.Salt, cred.Params, nick)
	if err != nil {
		return fmt.Errorf("store: перехеш: %w", err)
	}
	return nil
}

// SetPassword заменяет хеш authKey и ключевой блоб в одной транзакции:
// разъехавшиеся хеш и блоб означали бы аккаунт, в который нельзя войти
// или ключ которого не расшифровать. При logoutOthers в той же транзакции
// удаляются все сессии пользователя, кроме keep — текущей.
//
// Первое значение — устройства, к которым были привязаны удалённые сессии:
// их потоки событий закрывает обработчик. Поток проверяет сессию только
// при подключении, поэтому отозванная иначе продолжала бы получать
// конверты до обрыва соединения (ADR-058).
func (s *Store) SetPassword(ctx context.Context, nick string, cred Credential, blob string, logoutOthers bool, keep []byte) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: смена пароля: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET auth_hash = ?, auth_salt = ?, auth_params = ?, key_blob = ? WHERE nick = ?`,
		cred.Hash, cred.Salt, cred.Params, blob, nick); err != nil {
		return nil, fmt.Errorf("store: смена пароля: %w", err)
	}
	var revoked []string
	if logoutOthers {
		revoked, err = revokedDevices(ctx, tx, nick, keep)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM sessions WHERE nick = ? AND token_hash <> ?`, nick, keep); err != nil {
			return nil, fmt.Errorf("store: смена пароля: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: смена пароля: %w", err)
	}
	return revoked, nil
}

// revokedDevices — устройства завершаемых сессий, кроме устройства текущей:
// её оставляют, и закрывать её поток незачем.
func revokedDevices(ctx context.Context, tx *sql.Tx, nick string, keep []byte) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT device_id FROM sessions
		WHERE nick = ? AND token_hash <> ? AND device_id IS NOT NULL
		  AND device_id NOT IN (SELECT device_id FROM sessions WHERE token_hash = ? AND device_id IS NOT NULL)
		ORDER BY device_id`, nick, keep, keep)
	if err != nil {
		return nil, fmt.Errorf("store: устройства завершённых сессий: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: устройства завершённых сессий: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: устройства завершённых сессий: %w", err)
	}
	return out, nil
}

// DeleteUser удаляет пользователя; устройства, сессии, контакты, членство,
// ключи комнат и очереди уносит каскад.
//
// Удаление аккаунта — выход из всех его комнат (ADR-041): по составу это
// тот же уход участника, что и POST /api/rooms/{id}/leave. Членство и ключи
// убираются до каскада, чтобы собрать оставшихся с их устройствами и
// текущими ключами; владение переходит участнику с наименьшим joined_at,
// опустевшая комната удаляется, у остальных выставляется needs_rekey.
// Всё вместе — одна транзакция; события рассылает обработчик после неё.
func (s *Store) DeleteUser(ctx context.Context, nick string) ([]RoomChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: удаление пользователя: %w", err)
	}
	defer tx.Rollback()

	rooms, err := memberRooms(ctx, tx, nick)
	if err != nil {
		return nil, err
	}
	var changes []RoomChange
	for _, id := range rooms {
		room, err := roomRow(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		change, err := leaveRoom(ctx, tx, room, nick)
		if err != nil {
			return nil, err
		}
		// Комната опустела и удалена — рассылать некому.
		if len(change.Members) > 0 {
			changes = append(changes, change)
		}
	}
	// Владелец всегда состоит в своей комнате: из состава его не убрать,
	// а выход передаёт владение (ADR-018), — так что здесь уже пусто.
	// Проверка остаётся ради внешнего ключа: rooms.owner ссылается на
	// users(nick) без ON DELETE, и забытая строка заперла бы удаление.
	owned, err := ownedRooms(ctx, tx, nick)
	if err != nil {
		return nil, err
	}
	for _, room := range owned {
		var heir string
		err := tx.QueryRowContext(ctx, `
			SELECT nick FROM room_members
			WHERE room_id = ? AND nick <> ? ORDER BY joined_at, nick LIMIT 1`, room, nick).Scan(&heir)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, room); err != nil {
				return nil, fmt.Errorf("store: удаление пустой комнаты: %w", err)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: передача владения: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET owner = ? WHERE id = ?`, heir, room); err != nil {
			return nil, fmt.Errorf("store: передача владения: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE nick = ?`, nick); err != nil {
		return nil, fmt.Errorf("store: удаление пользователя: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: удаление пользователя: %w", err)
	}
	return changes, nil
}

// ownedRooms — комнаты, где пользователь владелец.
func ownedRooms(ctx context.Context, tx *sql.Tx, nick string) ([]string, error) {
	return roomIDs(ctx, tx, `SELECT id FROM rooms WHERE owner = ? ORDER BY created_at, id`, nick)
}

// memberRooms — комнаты, где пользователь участник.
func memberRooms(ctx context.Context, tx *sql.Tx, nick string) ([]string, error) {
	return roomIDs(ctx, tx, `SELECT room_id FROM room_members WHERE nick = ? ORDER BY joined_at, room_id`, nick)
}

// roomIDs — идентификаторы комнат по запросу с одним параметром.
func roomIDs(ctx context.Context, tx *sql.Tx, query, nick string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, nick)
	if err != nil {
		return nil, fmt.Errorf("store: комнаты пользователя: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: комнаты пользователя: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: комнаты пользователя: %w", err)
	}
	return out, nil
}
