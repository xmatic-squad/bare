package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xmatic-squad/bare/internal/api"
	"github.com/xmatic-squad/bare/internal/config"
)

// bytesOf — детерминированные «случайные» байты: содержимое сервер
// не проверяет, ему важна только форма.
func bytesOf(n int, seed byte) string {
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// blobOf — ключевой блоб в форме docs/crypto.md.
func blobOf(iter int) string {
	return fmt.Sprintf(`{"v":1,"iter":%d,"iv":"%s","ct":"%s"}`, iter, bytesOf(12, 7), bytesOf(48, 11))
}

func jwk() map[string]string {
	return map[string]string{"kty": "EC", "crv": "P-256", "x": bytesOf(32, 3), "y": bytesOf(32, 5)}
}

func account(nick string) map[string]any {
	return map[string]any{
		"nick":      nick,
		"authKey":   bytesOf(32, 1),
		"publicKey": jwk(),
		"blob":      blobOf(config.KDFIterations),
	}
}

// signUp регистрирует аккаунт и отдаёт cookie сессии. Каждый ник приходит
// со своего адреса: регистрация ограничена пятью в час на IP (ADR-021),
// и общий адрес упирался бы в лимит на шестом аккаунте теста.
func (e *env) signUp(nick string) *http.Cookie {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/register", account(nick), fromNick(nick))
	expect(e.t, rec, http.StatusCreated, "")
	return e.cookie(rec)
}

// fromNick — свой адрес соединения на каждый ник, лишь бы разный
// и не loopback.
func fromNick(nick string) func(*http.Request) {
	sum := sha256.Sum256([]byte(nick))
	return withRemote(fmt.Sprintf("198.51.%d.%d:41000", sum[0], sum[1]))
}

func (e *env) cookie(rec *httptest.ResponseRecorder) *http.Cookie {
	e.t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "bare_session" {
			return c
		}
	}
	e.t.Fatal("в ответе нет cookie bare_session")
	return nil
}

func TestConfig(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/api/config", nil)
	expect(t, rec, http.StatusOK, "")

	var got struct {
		InviteRequired  bool   `json:"inviteRequired"`
		VAPIDPublicKey  string `json:"vapidPublicKey"`
		KDFIterations   int    `json:"kdfIterations"`
		MaxMessageChars int    `json:"maxMessageChars"`
	}
	decodeBody(t, rec, &got)
	if got.InviteRequired {
		t.Error("inviteRequired: получено true, ожидалось false")
	}
	if got.VAPIDPublicKey != "vapid" {
		t.Errorf("vapidPublicKey: получено %q", got.VAPIDPublicKey)
	}
	if got.KDFIterations != 1_000_000 {
		t.Errorf("kdfIterations: получено %d, ожидалось 1000000", got.KDFIterations)
	}
	if got.MaxMessageChars != 4000 {
		t.Errorf("maxMessageChars: получено %d, ожидалось 4000", got.MaxMessageChars)
	}
}

func TestRegisterAndLogin(t *testing.T) {
	e := newEnv(t)

	rec := e.do(http.MethodPost, "/api/register", account("marta"))
	expect(t, rec, http.StatusCreated, "")
	var created struct {
		Nick string `json:"nick"`
	}
	decodeBody(t, rec, &created)
	if created.Nick != "marta" {
		t.Errorf("ник в ответе: получено %q", created.Nick)
	}

	c := e.cookie(rec)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("флаги cookie: %+v", c)
	}
	if c.MaxAge < 89*24*3600 || c.MaxAge > 90*24*3600 {
		t.Errorf("срок cookie: получено %d секунд, ожидалось около 90 суток", c.MaxAge)
	}

	// Занятый ник.
	expect(t, e.do(http.MethodPost, "/api/register", account("marta")), http.StatusConflict, "nick_taken")

	// Сессия из регистрации работает.
	me := e.do(http.MethodGet, "/api/me", nil, with(c))
	expect(t, me, http.StatusOK, "")
	var self struct {
		Nick      string            `json:"nick"`
		PublicKey map[string]string `json:"publicKey"`
		CreatedAt int64             `json:"createdAt"`
	}
	decodeBody(t, me, &self)
	if self.Nick != "marta" || self.PublicKey["crv"] != "P-256" || self.CreatedAt == 0 {
		t.Errorf("GET /api/me: %+v", self)
	}

	// Вход тем же authKey отдаёт публичный ключ и блоб.
	login := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)})
	expect(t, login, http.StatusOK, "")
	var in struct {
		Nick      string            `json:"nick"`
		PublicKey map[string]string `json:"publicKey"`
		Blob      string            `json:"blob"`
	}
	decodeBody(t, login, &in)
	if in.Nick != "marta" || in.Blob != blobOf(config.KDFIterations) || in.PublicKey["x"] != bytesOf(32, 3) {
		t.Errorf("вход: %+v", in)
	}
	if _, ok := in.PublicKey["d"]; ok {
		t.Error("в публичном ключе есть d")
	}
	e.cookie(login)

	// Неверный authKey и несуществующий ник неразличимы.
	bad := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 9)})
	expect(t, bad, http.StatusUnauthorized, "invalid_credentials")
	none := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "никого", "authKey": bytesOf(32, 1)})
	expect(t, none, http.StatusUnauthorized, "invalid_credentials")
	unknown := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "petya", "authKey": bytesOf(32, 1)})
	expect(t, unknown, http.StatusUnauthorized, "invalid_credentials")
}

func TestRegisterRejects(t *testing.T) {
	private := jwk()
	private["d"] = bytesOf(32, 13)

	cases := []struct {
		name   string
		change func(map[string]any)
		status int
		code   string
		field  string
	}{
		{"кривой ник", func(m map[string]any) { m["nick"] = "Марта" }, http.StatusBadRequest, "invalid_nick", ""},
		{"короткий ник", func(m map[string]any) { m["nick"] = "m" }, http.StatusBadRequest, "invalid_nick", ""},
		{"ник с заглавной", func(m map[string]any) { m["nick"] = "Marta" }, http.StatusBadRequest, "invalid_nick", ""},
		{"короткий authKey", func(m map[string]any) { m["authKey"] = bytesOf(16, 1) }, http.StatusBadRequest, "invalid", "authKey"},
		{"authKey не base64url", func(m map[string]any) { m["authKey"] = strings.Repeat("=", 44) }, http.StatusBadRequest, "invalid", "authKey"},
		{"приватный ключ в jwk", func(m map[string]any) { m["publicKey"] = private }, http.StatusBadRequest, "invalid", "publicKey"},
		{"чужая кривая", func(m map[string]any) {
			k := jwk()
			k["crv"] = "P-384"
			m["publicKey"] = k
		}, http.StatusBadRequest, "invalid", "publicKey"},
		{"нет публичного ключа", func(m map[string]any) { delete(m, "publicKey") }, http.StatusBadRequest, "invalid", "publicKey"},
		{"слабый iter", func(m map[string]any) { m["blob"] = blobOf(599_999) }, http.StatusBadRequest, "invalid", "blob"},
		// Неподъёмный iter сервер отдал бы клиентам из GET /api/kdf (ADR-030).
		{"неподъёмный iter", func(m map[string]any) {
			m["blob"] = blobOf(config.KDFMaxIterations + 1)
		}, http.StatusBadRequest, "invalid", "blob"},
		{"iter в триллион", func(m map[string]any) { m["blob"] = blobOf(1_000_000_000_000) }, http.StatusBadRequest, "invalid", "blob"},
		{"дробный iter", func(m map[string]any) {
			m["blob"] = `{"v":1,"iter":1e6,"iv":"` + bytesOf(12, 7) + `","ct":"` + bytesOf(48, 11) + `"}`
		}, http.StatusBadRequest, "invalid", "blob"},
		{"версия блоба", func(m map[string]any) {
			m["blob"] = strings.Replace(blobOf(config.KDFIterations), `"v":1`, `"v":2`, 1)
		}, http.StatusBadRequest, "invalid", "blob"},
		{"блоб больше 8 КиБ", func(m map[string]any) {
			m["blob"] = fmt.Sprintf(`{"v":1,"iter":%d,"iv":"%s","ct":"%s"}`,
				config.KDFIterations, bytesOf(12, 7), strings.Repeat("a", 8<<10))
		}, http.StatusBadRequest, "invalid", "blob"},
		{"блоб не json", func(m map[string]any) { m["blob"] = "не json" }, http.StatusBadRequest, "invalid", "blob"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			body := account("marta")
			c.change(body)
			rec := e.do(http.MethodPost, "/api/register", body)
			expect(t, rec, c.status, c.code)
			if c.field != "" {
				var got struct {
					Field string `json:"field"`
				}
				decodeBody(t, rec, &got)
				if got.Field != c.field {
					t.Errorf("field: получено %q, ожидалось %q", got.Field, c.field)
				}
			}
			// Ни одна из этих регистраций не должна была создать аккаунт.
			expect(t, e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)}),
				http.StatusUnauthorized, "invalid_credentials")
		})
	}
}

func TestRegisterBadJSON(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do(http.MethodPost, "/api/register", "{"), http.StatusBadRequest, "bad_json")
}

func TestInvite(t *testing.T) {
	e := invited(t, "секрет")

	rec := e.do(http.MethodGet, "/api/config", nil)
	var cfg struct {
		InviteRequired bool `json:"inviteRequired"`
	}
	decodeBody(t, rec, &cfg)
	if !cfg.InviteRequired {
		t.Error("inviteRequired: получено false, ожидалось true")
	}

	expect(t, e.do(http.MethodPost, "/api/register", account("marta")), http.StatusForbidden, "invite_required")

	wrong := account("marta")
	wrong["invite"] = "не секрет"
	expect(t, e.do(http.MethodPost, "/api/register", wrong), http.StatusForbidden, "invalid_invite")

	right := account("marta")
	right["invite"] = "секрет"
	expect(t, e.do(http.MethodPost, "/api/register", right), http.StatusCreated, "")
}

func TestKDF(t *testing.T) {
	e := newEnv(t)

	// Неизвестный ник — целевое значение, тем же статусом.
	for _, nick := range []string{"marta", "", "МАРТА", strings.Repeat("x", 40)} {
		rec := e.do(http.MethodGet, "/api/kdf?nick="+nick, nil)
		expect(t, rec, http.StatusOK, "")
		var got struct {
			Iterations int `json:"iterations"`
		}
		decodeBody(t, rec, &got)
		if got.Iterations != config.KDFIterations {
			t.Errorf("iterations для %q: получено %d, ожидалось %d", nick, got.Iterations, config.KDFIterations)
		}
	}

	// Известный ник — iter из его блоба.
	body := account("marta")
	body["blob"] = blobOf(700_000)
	expect(t, e.do(http.MethodPost, "/api/register", body), http.StatusCreated, "")

	rec := e.do(http.MethodGet, "/api/kdf?nick=marta", nil)
	expect(t, rec, http.StatusOK, "")
	var got struct {
		Iterations int `json:"iterations"`
	}
	decodeBody(t, rec, &got)
	if got.Iterations != 700_000 {
		t.Errorf("iterations: получено %d, ожидалось 700000", got.Iterations)
	}
}

func TestOrigin(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)}

	expect(t, e.do(http.MethodPost, "/api/login", body, withOrigin("")), http.StatusForbidden, "bad_origin")
	expect(t, e.do(http.MethodPost, "/api/login", body, withOrigin("https://зло.example")), http.StatusForbidden, "bad_origin")
	expect(t, e.do(http.MethodPost, "/api/login", body, withOrigin("null")), http.StatusForbidden, "bad_origin")

	// GET без Origin работает.
	expect(t, e.do(http.MethodGet, "/api/config", nil), http.StatusOK, "")
	expect(t, e.do(http.MethodGet, "/", nil), http.StatusOK, "")

	// Свой Origin проходит: дальше — обычная ошибка входа, не 403.
	expect(t, e.do(http.MethodPost, "/api/login", body), http.StatusUnauthorized, "invalid_credentials")
}

func TestPasswordChange(t *testing.T) {
	e := newEnv(t)
	first := e.signUp("marta")

	// Второе устройство: свой вход, своя сессия.
	login := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)})
	expect(t, login, http.StatusOK, "")
	second := e.cookie(login)

	newBlob := blobOf(config.KDFIterations)
	change := map[string]any{
		"authKey":      bytesOf(32, 1),
		"newAuthKey":   bytesOf(32, 9),
		"blob":         newBlob,
		"logoutOthers": true,
	}

	// Без сессии — 401 unauthenticated, а не invalid_credentials.
	expect(t, e.do(http.MethodPost, "/api/password", change), http.StatusUnauthorized, "unauthenticated")

	// Неверный старый authKey.
	wrong := map[string]any{"authKey": bytesOf(32, 42), "newAuthKey": bytesOf(32, 9), "blob": newBlob}
	expect(t, e.do(http.MethodPost, "/api/password", wrong, with(second)), http.StatusUnauthorized, "invalid_credentials")

	// Слабый новый блоб не принимается.
	weak := map[string]any{"authKey": bytesOf(32, 1), "newAuthKey": bytesOf(32, 9), "blob": blobOf(599_999)}
	expect(t, e.do(http.MethodPost, "/api/password", weak, with(second)), http.StatusBadRequest, "invalid")

	expect(t, e.do(http.MethodPost, "/api/password", change, with(second)), http.StatusNoContent, "")

	// Текущая сессия жива, остальные — нет.
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(second)), http.StatusOK, "")
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(first)), http.StatusUnauthorized, "unauthenticated")

	// Старый authKey больше не подходит, новый отдаёт новый блоб.
	expect(t, e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)}),
		http.StatusUnauthorized, "invalid_credentials")
	fresh := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 9)})
	expect(t, fresh, http.StatusOK, "")
	var got struct {
		Blob string `json:"blob"`
	}
	decodeBody(t, fresh, &got)
	if got.Blob != newBlob {
		t.Errorf("блоб после смены пароля: получено %q", got.Blob)
	}
}

// Повышение итераций — та же операция без выхода на других устройствах.
func TestPasswordKeepsOtherSessions(t *testing.T) {
	e := newEnv(t)
	first := e.signUp("marta")
	login := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)})
	second := e.cookie(login)

	change := map[string]any{
		"authKey":      bytesOf(32, 1),
		"newAuthKey":   bytesOf(32, 9),
		"blob":         blobOf(config.KDFIterations),
		"logoutOthers": false,
	}
	expect(t, e.do(http.MethodPost, "/api/password", change, with(second)), http.StatusNoContent, "")
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(first)), http.StatusOK, "")
}

func TestSessionRequired(t *testing.T) {
	e := newEnv(t)
	e.signUp("marta")

	for _, target := range []string{"/api/me", "/api/users/marta"} {
		expect(t, e.do(http.MethodGet, target, nil), http.StatusUnauthorized, "unauthenticated")
	}
	expect(t, e.do(http.MethodPost, "/api/logout", nil), http.StatusUnauthorized, "unauthenticated")

	garbage := &http.Cookie{Name: "bare_session", Value: "not-a-token"}
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(garbage)), http.StatusUnauthorized, "unauthenticated")

	stranger := &http.Cookie{Name: "bare_session", Value: bytesOf(32, 77)}
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(stranger)), http.StatusUnauthorized, "unauthenticated")
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	c := e.signUp("marta")

	rec := e.do(http.MethodPost, "/api/logout", nil, with(c))
	expect(t, rec, http.StatusNoContent, "")
	if cleared := e.cookie(rec); cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("cookie не стёрта: %+v", cleared)
	}
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(c)), http.StatusUnauthorized, "unauthenticated")
}

func TestUsers(t *testing.T) {
	e := newEnv(t)
	c := e.signUp("marta")

	rec := e.do(http.MethodGet, "/api/users/marta", nil, with(c))
	expect(t, rec, http.StatusOK, "")
	var got struct {
		Nick      string          `json:"nick"`
		PublicKey json.RawMessage `json:"publicKey"`
	}
	decodeBody(t, rec, &got)
	if got.Nick != "marta" || !strings.Contains(string(got.PublicKey), `"P-256"`) {
		t.Errorf("ответ: %s", rec.Body.String())
	}

	expect(t, e.do(http.MethodGet, "/api/users/petya", nil, with(c)), http.StatusNotFound, "unknown_user")
	expect(t, e.do(http.MethodGet, "/api/users/МАРТА", nil, with(c)), http.StatusNotFound, "unknown_user")
}

func TestDeleteMe(t *testing.T) {
	e := newEnv(t)
	c := e.signUp("marta")

	expect(t, e.do(http.MethodDelete, "/api/me", map[string]any{"authKey": bytesOf(32, 42)}, with(c)),
		http.StatusUnauthorized, "invalid_credentials")

	rec := e.do(http.MethodDelete, "/api/me", map[string]any{"authKey": bytesOf(32, 1)}, with(c))
	expect(t, rec, http.StatusNoContent, "")
	if cleared := e.cookie(rec); cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("cookie не стёрта: %+v", cleared)
	}

	// Сессия ушла каскадом, ник свободен.
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(c)), http.StatusUnauthorized, "unauthenticated")
	expect(t, e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)}),
		http.StatusUnauthorized, "invalid_credentials")
	expect(t, e.do(http.MethodPost, "/api/register", account("marta")), http.StatusCreated, "")
}

// Ник не должен попадать в журнал (docs/deploy.md, «Логи»).
func TestNickStaysOutOfLog(t *testing.T) {
	e := newEnv(t)
	c := e.signUp("marta")
	e.log.Reset()

	e.do(http.MethodGet, "/api/users/marta", nil, with(c))
	e.do(http.MethodGet, "/api/kdf?nick=marta", nil)
	e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)})

	if strings.Contains(e.log.String(), "marta") {
		t.Errorf("ник в журнале: %q", e.log.String())
	}
	if !strings.Contains(e.log.String(), "/api/users/{nick}") {
		t.Errorf("шаблон маршрута не в журнале: %q", e.log.String())
	}
}

// Отказ до маршрутизации — шаблона ещё нет, а путь с ником в журнал
// попадать не должен всё равно (docs/deploy.md, «Логи»).
func TestNickStaysOutOfLogBeforeRouting(t *testing.T) {
	e := newEnv(t)

	// 403 bad_origin: любой не-GET со стороннего сайта.
	e.do(http.MethodPost, "/api/users/marta", nil, withOrigin("https://зло.example"))
	e.do(http.MethodDelete, "/api/contacts/marta", nil, withOrigin(""))
	// 413 too_large: тело больше предела, ответ до маршрутизации.
	e.do(http.MethodGet, "/api/users/marta", strings.Repeat("a", api.MaxBody+1))

	line := e.log.String()
	if strings.Count(line, "\n") != 3 {
		t.Fatalf("строк в журнале: %q", line)
	}
	if strings.Contains(line, "marta") {
		t.Errorf("ник в журнале: %q", line)
	}
	if strings.Count(line, "/api/ ") != 3 {
		t.Errorf("вместо пути ожидалось \"/api/\": %q", line)
	}
}
