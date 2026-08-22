package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/xmatic-squad/bare/internal/store"
)

// Сессия по ADR-021: токен 32 случайных байта, в базе SHA-256 от него,
// в cookie — base64url. Срок 90 дней без продления.
const (
	CookieName = "bare_session"
	TokenLen   = 32
	TTL        = 90 * 24 * time.Hour
)

// NewToken выдаёт токен для cookie и его SHA-256 для базы.
func NewToken() (token string, hash []byte, err error) {
	raw := make([]byte, TokenLen)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: токен: %w", err)
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}

// TokenHash разбирает токен из cookie в его SHA-256. Мусор — false.
func TokenHash(token string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != TokenLen {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// SetCookie ставит cookie сессии. Secure стоит всегда: браузеры считают
// localhost и 127.0.0.1 доверенным происхождением, поэтому локальная
// разработка по http этим не ломается.
func SetCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie стирает cookie сессии.
func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// Fail — как auth отвечает на отказ. Тело ошибки в форме протокола
// собирает internal/api (ADR-026), а импортировать его отсюда нельзя:
// api импортирует auth. Поэтому хелперы передаются значениями.
type Fail struct {
	// Error пишет ошибку протокола: статус, код, текст для человека.
	Error func(w http.ResponseWriter, status int, code, message string)
	// Internal пишет 500 и кладёт причину в журнал сервера.
	Internal func(w http.ResponseWriter, r *http.Request, err error)
}

// Origin — второй барьер CSRF рядом с SameSite=Strict (ADR-021).
// На всех методах кроме GET и HEAD заголовок Origin обязан равняться
// origin сервера; отсутствующий Origin — тоже отказ.
func Origin(origin string, fail Fail) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if r.Header.Get("Origin") != origin {
					fail.Error(w, http.StatusForbidden, "bad_origin", "запрос не с этого сайта")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Require пропускает дальше только запросы с живой сессией и кладёт её
// в контекст. Без сессии — 401 unauthenticated.
func Require(st *store.Store, fail Fail) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, err := session(r, st)
			if errors.Is(err, store.ErrNotFound) {
				fail.Error(w, http.StatusUnauthorized, "unauthenticated", "нужен вход")
				return
			}
			if err != nil {
				fail.Internal(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
		})
	}
}

// session читает cookie и находит сессию. Нет cookie, мусор в ней
// и истёкшая сессия неразличимы: store.ErrNotFound.
func session(r *http.Request, st *store.Store) (store.Session, error) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return store.Session{}, store.ErrNotFound
	}
	hash, ok := TokenHash(c.Value)
	if !ok {
		return store.Session{}, store.ErrNotFound
	}
	return st.Session(r.Context(), hash, time.Now().UnixMilli())
}

type sessionKey struct{}

// From отдаёт сессию из контекста. Её кладёт Require.
func From(r *http.Request) (store.Session, bool) {
	sess, ok := r.Context().Value(sessionKey{}).(store.Session)
	return sess, ok
}
