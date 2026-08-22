// Package auth — argon2id, сессии, cookie и проверки на входе (ADR-021).
//
// Пароля здесь нет: клиент присылает authKey, выведенный из пароля
// (ADR-015). Сервер хранит argon2id от authKey — чтобы дамп базы не давал
// готового ключа для входа.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"

	"golang.org/x/crypto/argon2"

	"github.com/xmatic-squad/bare/internal/store"
)

// Параметры argon2id из ADR-021. Вход — 32 случайных байта с точки зрения
// сервера, поэтому параметры умеренные.
const (
	SaltLen = 16 // байт
	KeyLen  = 32 // байт
)

// Params — параметры одного хеша. Пишутся рядом с ним в users.auth_params
// и читаются оттуда при проверке: повышение параметров — перехеш при
// очередном входе, а не миграция всех аккаунтов разом.
type Params struct {
	Memory  uint32 // КиБ
	Time    uint32
	Threads uint8
}

// Current — параметры для новых хешей.
var Current = Params{Memory: 19456, Time: 2, Threads: 1}

// String — форма записи в базе: "argon2id,m=19456,t=2,p=1".
func (p Params) String() string {
	return fmt.Sprintf("argon2id,m=%d,t=%d,p=%d", p.Memory, p.Time, p.Threads)
}

// ParseParams разбирает строку из users.auth_params.
func ParseParams(s string) (Params, error) {
	var p Params
	n, err := fmt.Sscanf(s, "argon2id,m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads)
	if err != nil || n != 3 {
		return Params{}, fmt.Errorf("auth: не разобрать параметры %q", s)
	}
	if p.Memory == 0 || p.Time == 0 || p.Threads == 0 {
		return Params{}, fmt.Errorf("auth: нулевой параметр в %q", s)
	}
	// Sscanf не жалуется на хвост после последнего числа; сверка с обратной
	// записью делает разбор точным.
	if p.String() != s {
		return Params{}, fmt.Errorf("auth: не разобрать параметры %q", s)
	}
	return p, nil
}

// Hash считает argon2id от authKey с текущими параметрами и новой солью.
func Hash(authKey []byte) (store.Credential, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return store.Credential{}, fmt.Errorf("auth: соль: %w", err)
	}
	return store.Credential{
		Hash:   derive(authKey, salt, Current),
		Salt:   salt,
		Params: Current.String(),
	}, nil
}

// Verify сверяет authKey с хешем из базы. Второе значение — нужен ли
// перехеш: параметры записи отстали от текущих.
func Verify(authKey []byte, cred store.Credential) (ok, rehash bool) {
	p, err := ParseParams(cred.Params)
	if err != nil || len(cred.Hash) != KeyLen || len(cred.Salt) == 0 {
		return false, false
	}
	got := derive(authKey, cred.Salt, p)
	if subtle.ConstantTimeCompare(got, cred.Hash) != 1 {
		return false, false
	}
	return true, p != Current || len(cred.Salt) != SaltLen
}

// Waste считает столько же, сколько Verify, и выбрасывает результат.
// Вход с несуществующим ником не должен отвечать заметно быстрее входа
// с неверным authKey: одна ошибка на все случаи (docs/protocol.md).
func Waste(authKey []byte) {
	derive(authKey, make([]byte, SaltLen), Current)
}

func derive(authKey, salt []byte, p Params) []byte {
	return argon2.IDKey(authKey, salt, p.Time, p.Memory, p.Threads, KeyLen)
}
