// Package config читает конфигурацию из переменных окружения BARE_* (ADR-022).
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config — всё, что сервер знает о своём окружении.
type Config struct {
	Addr         string // BARE_ADDR — адрес прослушивания
	DB           string // BARE_DB — путь к файлу SQLite
	Origin       string // BARE_ORIGIN — единственный допустимый Origin (ADR-021)
	VAPIDPublic  string // BARE_VAPID_PUBLIC
	VAPIDPrivate string // BARE_VAPID_PRIVATE
	VAPIDSubject string // BARE_VAPID_SUBJECT
	InviteCode   string // BARE_INVITE_CODE — пусто означает открытую регистрацию
}

// Значения по умолчанию — локальный запуск без окружения.
const (
	defaultAddr   = "127.0.0.1:8411"
	defaultDB     = "bare.db"
	defaultOrigin = "http://127.0.0.1:8411"
)

// Load читает окружение. Незаданная переменная берёт значение по умолчанию;
// заданная пустой — ошибка: пустой адрес, путь к базе или origin неработоспособны.
func Load() (*Config, error) {
	c := &Config{
		Addr:         env("BARE_ADDR", defaultAddr),
		DB:           env("BARE_DB", defaultDB),
		Origin:       env("BARE_ORIGIN", defaultOrigin),
		VAPIDPublic:  env("BARE_VAPID_PUBLIC", ""),
		VAPIDPrivate: env("BARE_VAPID_PRIVATE", ""),
		VAPIDSubject: env("BARE_VAPID_SUBJECT", ""),
		InviteCode:   env("BARE_INVITE_CODE", ""),
	}
	for _, v := range []struct{ key, value string }{
		{"BARE_ADDR", c.Addr},
		{"BARE_DB", c.DB},
		{"BARE_ORIGIN", c.Origin},
	} {
		if v.value == "" {
			return nil, fmt.Errorf("%s пуст: уберите переменную, чтобы взять значение по умолчанию, или задайте непустое", v.key)
		}
	}
	return c, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return fallback
}
