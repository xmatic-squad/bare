package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Все четыре правила ADR-021: пакет расходуется целиком, следующий токен
// набегает ровно через window/count, ведро не переполняется.
func TestRules(t *testing.T) {
	cases := []struct {
		name string
		rule rule
		// token — сколько ждать одного токена на пустом ведре.
		token time.Duration
	}{
		{"регистрация", registerRule, 12 * time.Minute},
		{"вход", loginRule, time.Minute},
		{"сообщения", messagesRule, 2 * time.Second},
		{"изменяющие", writesRule, time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBuckets(c.rule)
			now := time.Now()

			for i := 0; i < c.rule.burst; i++ {
				if _, ok := b.take("ключ", now); !ok {
					t.Fatalf("запрос %d из пакета %d отклонён", i+1, c.rule.burst)
				}
			}
			wait, ok := b.take("ключ", now)
			if ok {
				t.Fatal("пакет не кончился")
			}
			if wait != c.token {
				t.Errorf("ожидание: получено %v, ожидалось %v", wait, c.token)
			}
			if got, want := retryAfter(wait), int(c.token.Seconds()); got != want {
				t.Errorf("Retry-After: получено %d, ожидалось %d", got, want)
			}

			// Восстановление: ровно через это время набегает ровно один токен.
			if _, ok := b.take("ключ", now.Add(c.token)); !ok {
				t.Error("токен не набежал")
			}
			if _, ok := b.take("ключ", now.Add(c.token)); ok {
				t.Error("набежало больше одного токена")
			}

			// За долгую паузу копится пакет, а не весь пропущенный поток.
			for i := 0; i < c.rule.burst; i++ {
				if _, ok := b.take("ключ", now.Add(24*time.Hour)); !ok {
					t.Fatalf("запрос %d после долгой паузы отклонён", i+1)
				}
			}
			if _, ok := b.take("ключ", now.Add(24*time.Hour)); ok {
				t.Error("ведро больше пакета")
			}

			// Ведро на ключ: чужое полное.
			if _, ok := b.take("другой ключ", now); !ok {
				t.Error("лимит одного ключа задел другой")
			}
		})
	}
}

// Часы могут прыгнуть назад; долг за это никому не выставляется.
func TestBucketsClockBack(t *testing.T) {
	b := newBuckets(messagesRule)
	now := time.Now()

	for i := 0; i < messagesRule.burst; i++ {
		b.take("marta", now)
	}
	if _, ok := b.take("marta", now.Add(-time.Hour)); ok {
		t.Error("время назад добавило токенов")
	}
}

// Карта лимита не растёт бесконечно: миллион разных ключей проходит
// сквозь поколения, а вёдер остаётся не больше двух карт.
func TestBucketsBounded(t *testing.T) {
	b := newBuckets(registerRule)
	now := time.Now()

	for i := 0; i < 1_000_000; i++ {
		b.take(strconv.Itoa(i), now)
	}
	if got := b.size(); got > 2*generation {
		t.Errorf("вёдер: получено %d, ожидалось не больше %d", got, 2*generation)
	}

	// Ключ, по которому ходят, смену поколения переживает: его ведро
	// переезжает в нынешнюю карту, а не заводится заново.
	b = newBuckets(registerRule)
	for i := 0; i < registerRule.burst; i++ {
		b.take("свой", now)
	}
	for i := 0; i < 3*generation; i++ {
		b.take(strconv.Itoa(i), now)
		if _, ok := b.take("свой", now); ok {
			t.Fatalf("ведро забыто на %d-м чужом ключе", i+1)
		}
	}
}

// Ждать меньше секунды бессмысленно: Retry-After в секундах.
func TestRetryAfter(t *testing.T) {
	cases := map[time.Duration]int{
		-time.Second:            1,
		0:                       1,
		100 * time.Millisecond:  1,
		time.Second:             1,
		1500 * time.Millisecond: 2,
		2 * time.Second:         2,
		12 * time.Minute:        720,
	}
	for wait, want := range cases {
		if got := retryAfter(wait); got != want {
			t.Errorf("retryAfter(%v): получено %d, ожидалось %d", wait, got, want)
		}
	}
}

// X-Real-IP ставит nginx с той же машины (ADR-022). Заголовку из сети
// веры нет: иначе лимит на IP снимался бы новой строкой в заголовке.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		real   string
		want   string
	}{
		{"без заголовка", "203.0.113.7:41000", "", "203.0.113.7"},
		{"заголовок из сети", "203.0.113.7:41000", "198.51.100.1", "203.0.113.7"},
		{"заголовок от nginx", "127.0.0.1:41000", "198.51.100.1", "198.51.100.1"},
		{"nginx по ipv6", "[::1]:41000", "198.51.100.1", "198.51.100.1"},
		{"loopback без заголовка", "127.0.0.1:41000", "", "127.0.0.1"},
		{"мусор в заголовке", "127.0.0.1:41000", "не адрес", "127.0.0.1"},
		{"пробелы в заголовке", "127.0.0.1:41000", " 198.51.100.1 ", "198.51.100.1"},
		{"адрес с портом в заголовке", "127.0.0.1:41000", "198.51.100.1:80", "127.0.0.1"},
		{"ipv6 клиента", "[2001:db8::1]:41000", "", "2001:db8::1"},
		{"ipv4 в ipv6-форме", "[::ffff:203.0.113.7]:41000", "", "203.0.113.7"},
		{"ipv4 в ipv6-форме в заголовке", "127.0.0.1:41000", "::ffff:198.51.100.1", "198.51.100.1"},
		{"не разобрать соединение", "сокет", "198.51.100.1", "сокет"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/register", nil)
			r.RemoteAddr = c.remote
			if c.real != "" {
				r.Header.Set("X-Real-IP", c.real)
			}
			if got := clientIP(r); got != c.want {
				t.Errorf("clientIP: получено %q, ожидалось %q", got, c.want)
			}
		})
	}
}

// ULID: 26 символов Crockford base32, время в первых десяти.
func TestULIDTime(t *testing.T) {
	// 01ARZ3NDEK — 2016-07-30T23:54:10.259Z.
	ms, ok := ulidTime("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if !ok || ms != 1469922850259 {
		t.Errorf("ulidTime: получено %d, %v; ожидалось 1469922850259", ms, ok)
	}
	for _, id := range []string{
		"",
		"01ARZ3NDEKTSV4RRFFQ69G5FA",   // 25 символов
		"01ARZ3NDEKTSV4RRFFQ69G5FAVX", // 27 символов
		"01arz3ndektsv4rrffq69g5fav",  // строчные
		"01ARZ3NDEKTSV4RRFFQ69G5FAU",  // U вне алфавита Crockford
		"81ARZ3NDEKTSV4RRFFQ69G5FAV",  // время больше 48 бит
		"01ARZ3NDEKTSV4RRFFQ69G5F☺",
	} {
		if _, ok := ulidTime(id); ok {
			t.Errorf("принят кривой ulid %q", id)
		}
	}
}

// size — сколько вёдер помнят обе карты. Только для тестов: предел размера
// проверяется, а не подразумевается.
func (b *buckets) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.cur) + len(b.old)
}
