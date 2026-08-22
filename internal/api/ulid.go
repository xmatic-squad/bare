package api

import "strings"

// ULID — 48 бит миллисекунд и 80 бит случайности, Crockford base32,
// 26 символов (docs/crypto.md, «Идентификаторы»). Сервер читает из него
// только время: расхождение с серверными часами больше пяти минут —
// clock_skew (ADR-017).
const ulidLen = 26

// crockford — алфавит Crockford base32: без I, L, O и U. Канонический
// ULID записывается заглавными; строчные буквы сервер не принимает —
// идентификатор входит в AAD шифротекста побайтно (docs/crypto.md).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidTime разбирает ULID и отдаёт его метку времени в миллисекундах.
func ulidTime(id string) (int64, bool) {
	if len(id) != ulidLen {
		return 0, false
	}
	var ms int64
	for i := 0; i < ulidLen; i++ {
		v := strings.IndexByte(crockford, id[i])
		if v < 0 {
			return 0, false
		}
		if i < 10 {
			ms = ms<<5 | int64(v)
		}
	}
	// Первые десять символов — 50 бит, времени отведено 48: старшие два
	// обязаны быть нулевыми.
	if ms > 1<<48-1 {
		return 0, false
	}
	return ms, true
}
