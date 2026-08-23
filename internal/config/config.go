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

	// PushLocal разрешает отправку пушей на непубличные адреса. Из
	// окружения не читается и в работе всегда false: сервер ходит
	// только по публичным адресам (ADR-047). Поле существует ради
	// тестов, где push-сервис вендора подменён сервером на 127.0.0.1.
	PushLocal bool
}

// Значения по умолчанию — локальный запуск без окружения.
const (
	defaultAddr   = "127.0.0.1:8411"
	defaultDB     = "bare.db"
	defaultOrigin = "http://127.0.0.1:8411"
)

// Параметры, которые сервер сообщает клиенту в GET /api/config. Это
// константы, а не переменные окружения: их значения — часть криптосистемы
// (ADR-013) и протокола (ADR-021), а не настройка машины.
const (
	// KDFIterations — целевое число итераций PBKDF2 на клиенте.
	KDFIterations = 1_000_000
	// KDFMinIterations — нижняя граница: блоб с меньшим iter сервер не примет.
	KDFMinIterations = 600_000
	// KDFMaxIterations — верхняя граница (ADR-030). Сервер сам раздаёт iter
	// из блоба в GET /api/kdf, и с неподъёмным значением аккаунт нельзя
	// ни открыть, ни удалить: обе операции начинаются с PBKDF2.
	KDFMaxIterations = 10_000_000
	// MaxMessageChars — предел текста сообщения.
	MaxMessageChars = 4000
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
	if err := checkVAPIDSubject(c); err != nil {
		return nil, err
	}
	return c, nil
}

// checkVAPIDSubject проверяет форму VAPID-субъекта при старте. Ошибка
// в нём иначе не видна вовсе: сервер поднимается, подписки ставятся,
// а push-сервис отвергает каждый токен — «пуши не приходят» без единой
// строки о причине. Отказ на старте дешевле.
//
// Годится ровно то, что допускает RFC 8292: «mailto:<адрес>» или
// «https://<хост>» (docs/deploy.md).
func checkVAPIDSubject(c *Config) error {
	if c.VAPIDSubject == "" {
		// Без ключей пуши и так выключены — это рабочий локальный запуск.
		// А вот ключи без субъекта означают, что настроить пуши хотели
		// и не настроили.
		if c.VAPIDPublic == "" && c.VAPIDPrivate == "" {
			return nil
		}
		return fmt.Errorf("BARE_VAPID_SUBJECT пуст при заданных VAPID-ключах: нужен mailto:<адрес> или https://<хост>")
	}
	bad := fmt.Errorf("BARE_VAPID_SUBJECT=%q не годится: нужен mailto:<адрес> или https://<хост>", c.VAPIDSubject)
	if addr, ok := strings.CutPrefix(c.VAPIDSubject, "mailto:"); ok {
		local, domain, at := strings.Cut(addr, "@")
		if !at || local == "" || domain == "" || strings.ContainsAny(addr, " \t") {
			return bad
		}
		return nil
	}
	if rest, ok := strings.CutPrefix(c.VAPIDSubject, "https://"); ok {
		host, _, _ := strings.Cut(rest, "/")
		if host == "" || strings.ContainsAny(rest, " \t") {
			return bad
		}
		return nil
	}
	return bad
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return fallback
}
