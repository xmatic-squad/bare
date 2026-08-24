package config

import (
	"strings"
	"testing"
)

// Форма VAPID-субъекта проверяется на старте: с мусором в нём сервер
// поднимается, подписки ставятся, а push-сервис отвергает каждый токен —
// поломка без единой строки в журнале (docs/deploy.md).
func TestVAPIDSubject(t *testing.T) {
	cases := []struct {
		subject string
		keys    bool
		ok      bool
	}{
		{"mailto:admin@xmatic.team", true, true},
		{"https://bare.xmatic.team", true, true},
		{"https://bare.xmatic.team/", true, true},
		// Голый адрес — не URI: RFC 8292 требует схему, и APNs отвергает
		// токен без неё.
		{"admin@xmatic.team", true, false},
		{"mailto:", true, false},
		{"mailto:admin", true, false},
		{"mailto:@xmatic.team", true, false},
		{"mailto:admin@xmatic.team, second@xmatic.team", true, false},
		{"https://", true, false},
		{"http://bare.xmatic.team", true, false},
		{"bare.xmatic.team", true, false},
		// Ключи заданы, субъекта нет: пуши настроить хотели и не настроили.
		{"", true, false},
		// Ни ключей, ни субъекта — локальный запуск без пушей.
		{"", false, true},
	}
	for _, c := range cases {
		t.Setenv("BARE_VAPID_SUBJECT", c.subject)
		if c.keys {
			t.Setenv("BARE_VAPID_PUBLIC", "public")
			t.Setenv("BARE_VAPID_PRIVATE", "private")
		} else {
			t.Setenv("BARE_VAPID_PUBLIC", "")
			t.Setenv("BARE_VAPID_PRIVATE", "")
		}
		_, err := Load()
		if c.ok && err != nil {
			t.Errorf("BARE_VAPID_SUBJECT=%q (ключи: %v): %v", c.subject, c.keys, err)
		}
		if !c.ok {
			if err == nil {
				t.Errorf("BARE_VAPID_SUBJECT=%q (ключи: %v): принят", c.subject, c.keys)
				continue
			}
			if !strings.Contains(err.Error(), "BARE_VAPID_SUBJECT") {
				t.Errorf("ошибка не называет переменную: %v", err)
			}
		}
	}
}
