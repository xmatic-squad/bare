package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bare.db")

	first := open(t, path)
	want := []string{"001_init.sql", "002_room_needs_rekey.sql"}
	if got := first.Applied(); !equal(got, want) {
		t.Fatalf("применённые миграции: получено %v, ожидалось %v", got, want)
	}
	if got := version(t, first); got != len(want) {
		t.Errorf("user_version: получено %d, ожидалась %d", got, len(want))
	}
	// Все восемь таблиц из docs/storage.md на месте.
	for _, table := range []string{"users", "devices", "sessions", "contacts", "rooms", "room_members", "room_keys", "queue"} {
		var name string
		err := first.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("таблица %s: %v", table, err)
		}
	}
	// Внешние ключи включены — иначе каскадные удаления молча не работают.
	var fk int
	if err := first.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys: получено %d (%v), ожидалась 1", fk, err)
	}
	var mode string
	if err := first.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode: получено %q (%v), ожидался wal", mode, err)
	}
	first.Close()

	// Повторный старт на той же базе ничего не применяет.
	second := open(t, path)
	if got := second.Applied(); len(got) != 0 {
		t.Errorf("повторный старт применил %v, ожидалось ничего", got)
	}
	if got := version(t, second); got != len(want) {
		t.Errorf("user_version после перезапуска: получено %d, ожидалась %d", got, len(want))
	}
}

func version(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func TestUsersAndSessions(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "bare.db"))

	u := User{
		Nick:      "marta",
		Cred:      Credential{Hash: []byte("hash"), Salt: []byte("salt"), Params: "argon2id,m=19456,t=2,p=1"},
		PublicKey: `{"kty":"EC"}`,
		KeyBlob:   `{"v":1}`,
		CreatedAt: 1,
	}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateUser(ctx, u); !errors.Is(err, ErrNickTaken) {
		t.Errorf("повторный ник: получено %v, ожидалось ErrNickTaken", err)
	}
	if _, err := s.User(ctx, "нет-такого"); !errors.Is(err, ErrNotFound) {
		t.Errorf("чужой ник: получено %v, ожидалось ErrNotFound", err)
	}

	now := time.Now().UnixMilli()
	live := []byte("token-hash-1")
	other := []byte("token-hash-2")
	if err := s.CreateSession(ctx, live, "marta", now, now+1000); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSession(ctx, other, "marta", now, now+1000); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := s.Session(ctx, live, now)
	if err != nil || sess.Nick != "marta" || sess.DeviceID != "" {
		t.Fatalf("Session: %+v, %v", sess, err)
	}
	if _, err := s.Session(ctx, live, now+2000); !errors.Is(err, ErrNotFound) {
		t.Errorf("истёкшая сессия: получено %v, ожидалось ErrNotFound", err)
	}

	// Смена пароля с logoutOthers: остаётся только текущая сессия.
	cred := Credential{Hash: []byte("new"), Salt: []byte("salt2"), Params: "argon2id,m=19456,t=2,p=1"}
	if err := s.SetPassword(ctx, "marta", cred, `{"v":1,"new":true}`, true, live); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, err := s.Session(ctx, other, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("чужая сессия после logoutOthers: получено %v, ожидалось ErrNotFound", err)
	}
	if _, err := s.Session(ctx, live, now); err != nil {
		t.Errorf("текущая сессия после logoutOthers: %v", err)
	}
	got, err := s.User(ctx, "marta")
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if string(got.Cred.Hash) != "new" || got.KeyBlob != `{"v":1,"new":true}` {
		t.Errorf("хеш и блоб: получено %q / %q", got.Cred.Hash, got.KeyBlob)
	}

	// Удаление пользователя уносит сессии каскадом.
	if _, err := s.DeleteUser(ctx, "marta"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.Session(ctx, live, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("сессия после удаления аккаунта: получено %v, ожидалось ErrNotFound", err)
	}
}

func TestCleanup(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "bare.db"))
	now := time.Now()
	ms := now.UnixMilli()
	day := int64(24 * 60 * 60 * 1000)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO users (nick, auth_hash, auth_salt, auth_params, public_key, key_blob, created_at)
	      VALUES ('marta', x'00', x'00', 'argon2id,m=19456,t=2,p=1', '{}', '{}', ?)`, ms)
	exec(`INSERT INTO devices (id, nick, created_at, last_seen) VALUES ('old', 'marta', ?, ?)`, ms, ms-100*day)
	exec(`INSERT INTO devices (id, nick, created_at, last_seen) VALUES ('new', 'marta', ?, ?)`, ms, ms)
	exec(`INSERT INTO queue (device_id, msg_id, envelope, created_at) VALUES ('old', 'cascade', '{}', ?)`, ms)
	exec(`INSERT INTO queue (device_id, msg_id, envelope, created_at) VALUES ('new', 'stale', '{}', ?)`, ms-40*day)
	exec(`INSERT INTO queue (device_id, msg_id, envelope, created_at) VALUES ('new', 'fresh', '{}', ?)`, ms)
	exec(`INSERT INTO sessions (token_hash, nick, created_at, expires_at) VALUES (x'01', 'marta', ?, ?)`, ms, ms-day)
	exec(`INSERT INTO sessions (token_hash, nick, created_at, expires_at) VALUES (x'02', 'marta', ?, ?)`, ms, ms+day)
	exec(`INSERT INTO rooms (id, name, owner, created_at) VALUES ('r', 'общая', 'marta', ?)`, ms)
	for i, key := range []string{"k1", "k2", "k3"} {
		exec(`INSERT INTO room_keys (room_id, nick, key_id, sender, iv, ct, created_at)
		      VALUES ('r', 'marta', ?, 'marta', 'iv', 'ct', ?)`, key, ms+int64(i))
	}

	if err := s.Cleanup(ctx, now); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if got := ids(t, s, `SELECT id FROM devices ORDER BY id`); !equal(got, []string{"new"}) {
		t.Errorf("устройства: получено %v, ожидалось [new]", got)
	}
	// Очередь устройства 'old' ушла каскадом вместе с ним, 'stale' — по сроку.
	if got := ids(t, s, `SELECT msg_id FROM queue ORDER BY msg_id`); !equal(got, []string{"fresh"}) {
		t.Errorf("очередь: получено %v, ожидалось [fresh]", got)
	}
	if got := ids(t, s, `SELECT hex(token_hash) FROM sessions ORDER BY token_hash`); !equal(got, []string{"02"}) {
		t.Errorf("сессии: получено %v, ожидалось [02]", got)
	}
	if got := ids(t, s, `SELECT DISTINCT key_id FROM room_keys ORDER BY key_id`); !equal(got, []string{"k2", "k3"}) {
		t.Errorf("ключи комнат: получено %v, ожидалось [k2 k3]", got)
	}
}

func ids(t *testing.T, s *Store, query string) []string {
	t.Helper()
	rows, err := s.db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
