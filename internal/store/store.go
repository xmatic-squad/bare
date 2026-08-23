// Package store — SQLite: открытие базы, миграции, запросы (ADR-020).
//
// В базе только шифротексты и метаданные: истории сообщений, плейнтекста
// и паролей здесь нет и не будет (docs/storage.md).
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations
var migrations embed.FS

// Ошибки, которые обработчикам нужно различать. Остальное — внутренние сбои.
var (
	// ErrNotFound — строки нет.
	ErrNotFound = errors.New("store: не найдено")
	// ErrNickTaken — ник уже занят.
	ErrNickTaken = errors.New("store: ник занят")
)

// Store — база и её единственное соединение на запись.
type Store struct {
	db      *sql.DB
	applied []string
}

// Applied — миграции, применённые при этом открытии базы. Пусто, если
// схема уже была свежей.
func (s *Store) Applied() []string { return s.applied }

// Open открывает базу, ставит режим из docs/storage.md и применяет миграции.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: открытие %s: %w", path, err)
	}

	// Одно соединение на всю базу. modernc.org/sqlite, как и любой SQLite,
	// допускает ровно одного писателя; при нескольких соединениях запись
	// упирается в SQLITE_BUSY, а busy_timeout лечит это ожиданием, а не
	// корректностью — «database is locked» всё равно возможен на upgrade
	// транзакции из read в write. Пул из одной штуки убирает класс ошибок
	// целиком: очередь выстраивает database/sql. Цена — чтения ждут запись;
	// для чата на десятки человек это незаметно. SSE держит соединение
	// с клиентом, а не с базой, поэтому поток событий пул не занимает.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: %s недоступна: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close закрывает базу.
func (s *Store) Close() error { return s.db.Close() }

// dsn собирает строку соединения с режимом из docs/storage.md.
// Прагмы применяются к каждому новому соединению; journal_mode=WAL
// хранится в самом файле, остальные — свойство соединения.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// migrate применяет недостающие миграции по порядку, каждую в своей
// транзакции. Версия схемы — PRAGMA user_version, она же номер последней
// применённой миграции. Откатов нет: ошибку правит следующая миграция.
func (s *Store) migrate() error {
	files, err := migrationFiles()
	if err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: чтение user_version: %w", err)
	}
	if version > len(files) {
		return fmt.Errorf("store: база версии %d новее бинаря (%d миграций)", version, len(files))
	}
	for i := version; i < len(files); i++ {
		name := files[i]
		body, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("store: чтение миграции %s: %w", name, err)
		}
		if err := s.applyMigration(i+1, name, string(body)); err != nil {
			return err
		}
		s.applied = append(s.applied, name)
	}
	return nil
}

func (s *Store) applyMigration(version int, name, body string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: миграция %s: %w", name, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(body); err != nil {
		return fmt.Errorf("store: миграция %s: %w", name, err)
	}
	// user_version не принимает подстановку, поэтому число подставляется
	// форматированием; version — счётчик миграций, не пользовательские данные.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("store: миграция %s: %w", name, err)
	}
	return tx.Commit()
}

// migrationFiles отдаёт имена миграций в порядке номеров.
func migrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: каталог миграций: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	// Имена вида NNN_*.sql: лексикографический порядок совпадает с числовым.
	sort.Strings(names)
	return names, nil
}
