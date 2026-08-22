package api

import (
	"testing"
	"time"
)

// Token bucket из ADR-021: 30 в минуту, пакет 10.
func TestBuckets(t *testing.T) {
	b := newBuckets(messagesPerMinute, messagesBurst)
	now := time.Now()

	for i := 0; i < messagesBurst; i++ {
		if _, ok := b.take("marta", now); !ok {
			t.Fatalf("запрос %d из пакета отклонён", i+1)
		}
	}
	wait, ok := b.take("marta", now)
	if ok {
		t.Fatal("пакет не кончился")
	}
	// Тридцать в минуту — токен раз в две секунды.
	if wait != 2*time.Second {
		t.Errorf("ожидание: получено %v, ожидалось 2s", wait)
	}
	if got := retryAfter(wait); got != 2 {
		t.Errorf("Retry-After: получено %d, ожидалось 2", got)
	}

	// Через две секунды набегает ровно один токен.
	if _, ok := b.take("marta", now.Add(2*time.Second)); !ok {
		t.Error("токен не набежал")
	}
	if _, ok := b.take("marta", now.Add(2*time.Second)); ok {
		t.Error("набежало больше одного токена")
	}

	// Ведро не переполняется: за час копится пакет, не тридцать в минуту.
	for i := 0; i < messagesBurst; i++ {
		if _, ok := b.take("marta", now.Add(time.Hour)); !ok {
			t.Fatalf("запрос %d после долгой паузы отклонён", i+1)
		}
	}
	if _, ok := b.take("marta", now.Add(time.Hour)); ok {
		t.Error("ведро больше пакета")
	}

	// Лимит на ключ: чужое ведро полное.
	if _, ok := b.take("petya", now); !ok {
		t.Error("лимит одного пользователя задел другого")
	}
}

// Часы могут прыгнуть назад; долг за это никому не выставляется.
func TestBucketsClockBack(t *testing.T) {
	b := newBuckets(messagesPerMinute, messagesBurst)
	now := time.Now()

	for i := 0; i < messagesBurst; i++ {
		b.take("marta", now)
	}
	if _, ok := b.take("marta", now.Add(-time.Hour)); ok {
		t.Error("время назад добавило токенов")
	}
}

// Полные вёдра выкидываются: карта не растёт на каждый ник навсегда.
func TestBucketsSweep(t *testing.T) {
	b := newBuckets(messagesPerMinute, messagesBurst)
	now := time.Now()

	for i := 0; i < sweepAt; i++ {
		b.take(string(rune(i)), now)
	}
	if len(b.seen) != sweepAt {
		t.Fatalf("вёдер: получено %d, ожидалось %d", len(b.seen), sweepAt)
	}
	// Все вёдра успели наполниться заново — чистка их и уносит.
	b.take("marta", now.Add(time.Hour))
	if len(b.seen) != 1 {
		t.Errorf("вёдер после чистки: получено %d, ожидалось 1", len(b.seen))
	}
}

// Ждать меньше секунды бессмысленно: Retry-After в секундах.
func TestRetryAfter(t *testing.T) {
	cases := map[time.Duration]int{
		0:                       1,
		100 * time.Millisecond:  1,
		time.Second:             1,
		1500 * time.Millisecond: 2,
		2 * time.Second:         2,
	}
	for wait, want := range cases {
		if got := retryAfter(wait); got != want {
			t.Errorf("retryAfter(%v): получено %d, ожидалось %d", wait, got, want)
		}
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
