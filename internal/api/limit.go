package api

import (
	"math"
	"sync"
	"time"
)

// Лимит сообщений (ADR-021): 30 в минуту на пользователя, пакет 10.
// Остальные лимиты — этап 6.
const (
	messagesPerMinute = 30
	messagesBurst     = 10
)

// sweepAt — с какого размера карты имеет смысл выкидывать полные вёдра.
const sweepAt = 1024

// buckets — token bucket в памяти сервера, по ведру на ключ (ник).
// Рестарт обнуляет лимиты: для маленького сервера это принято (ADR-021).
type buckets struct {
	mu    sync.Mutex
	rate  float64 // токенов в секунду
	burst float64
	seen  map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newBuckets(perMinute, burst int) *buckets {
	return &buckets{
		rate:  float64(perMinute) / 60,
		burst: float64(burst),
		seen:  make(map[string]*bucket),
	}
}

// take забирает токен. Второе значение — можно ли; если нет, первое —
// сколько ждать до следующего токена.
func (b *buckets) take(key string, now time.Time) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	e, ok := b.seen[key]
	if !ok {
		if len(b.seen) >= sweepAt {
			b.sweep(now)
		}
		e = &bucket{tokens: b.burst, at: now}
		b.seen[key] = e
	}
	e.tokens = math.Min(b.burst, e.tokens+b.refill(e.at, now))
	e.at = now
	if e.tokens < 1 {
		return time.Duration((1 - e.tokens) / b.rate * float64(time.Second)), false
	}
	e.tokens--
	return 0, true
}

// refill — сколько токенов набежало. Время назад не идёт: часы могли
// прыгнуть, но долг за это выставлять некому.
func (b *buckets) refill(since, now time.Time) float64 {
	d := now.Sub(since)
	if d <= 0 {
		return 0
	}
	return d.Seconds() * b.rate
}

// sweep выкидывает полные вёдра: они уже ничего не помнят. Иначе карта
// росла бы на каждый новый ник и не уменьшалась никогда.
func (b *buckets) sweep(now time.Time) {
	for key, e := range b.seen {
		if e.tokens+b.refill(e.at, now) >= b.burst {
			delete(b.seen, key)
		}
	}
}

// retryAfter — значение заголовка в секундах, не меньше одной: нулевое
// ожидание после отказа сбивало бы клиента с толку.
func retryAfter(wait time.Duration) int {
	if wait < time.Second {
		return 1
	}
	return int(math.Ceil(wait.Seconds()))
}
