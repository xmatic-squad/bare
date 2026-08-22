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
func (s *Store) SetPassword(ctx context.Context, nick string, cred Credential, blob string, logoutOthers bool, keep []byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: смена пароля: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET auth_hash = ?, auth_salt = ?, auth_params = ?, key_blob = ? WHERE nick = ?`,
		cred.Hash, cred.Salt, cred.Params, blob, nick); err != nil {
		return fmt.Errorf("store: смена пароля: %w", err)
	}
	if logoutOthers {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM sessions WHERE nick = ? AND token_hash <> ?`, nick, keep); err != nil {
			return fmt.Errorf("store: смена пароля: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: смена пароля: %w", err)
	}
	return nil
}

// DeleteUser удаляет пользователя; устройства, сессии, контакты, членство
// и очереди уносит каскад.
//
// Комнаты, где пользователь владелец, каскадом не удаляются: rooms.owner
// ссылается на users(nick) без ON DELETE, и удаление такого пользователя
// упрётся в внешний ключ. Передача владения и удаление пустых комнат —
// ADR-018, этап 3; до появления комнат случай не наступает.
func (s *Store) DeleteUser(ctx context.Context, nick string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE nick = ?`, nick); err != nil {
		return fmt.Errorf("store: удаление пользователя: %w", err)
	}
	return nil
}
