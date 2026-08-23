package api_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/xmatic-squad/bare/internal/api"
)

// nickOf — ник для очередного аккаунта теста.
func nickOf(i int) string { return fmt.Sprintf("marta%d", i) }

// fromIP — соединение с этого адреса, без заголовков.
func fromIP(ip string) func(*http.Request) { return withRemote(ip + ":41000") }

// Регистрация — 5 в час на IP (ADR-021).
func TestRegisterRateLimit(t *testing.T) {
	e := newEnv(t)
	one := fromIP("203.0.113.7")

	for i := 0; i < 5; i++ {
		expect(t, e.do(http.MethodPost, "/api/register", account(nickOf(i)), one), http.StatusCreated, "")
	}
	rec := e.do(http.MethodPost, "/api/register", account("kolya"), one)
	expect(t, rec, http.StatusTooManyRequests, "rate_limited")
	// Токен набегает раз в двенадцать минут; ведро пусто, значит ждать
	// почти все 720 секунд.
	if got := retryAfterOf(t, rec); got < 700 || got > 720 {
		t.Errorf("Retry-After: получено %d, ожидалось около 720", got)
	}
	// Отказ ничего не завёл.
	expect(t, e.do(http.MethodPost, "/api/login", map[string]any{"nick": "kolya", "authKey": bytesOf(32, 1)}),
		http.StatusUnauthorized, "invalid_credentials")

	// Другой адрес — своё ведро.
	expect(t, e.do(http.MethodPost, "/api/register", account("kolya"), fromIP("203.0.113.8")),
		http.StatusCreated, "")
}

// Неудачная регистрация тратит попытку так же, как удачная: иначе занятые
// ники перебирались бы без счёта.
func TestRegisterRateLimitCountsFailures(t *testing.T) {
	e := invited(t, "секрет")
	one := fromIP("203.0.113.7")

	for i := 0; i < 5; i++ {
		body := account(nickOf(i))
		body["invite"] = "не секрет"
		expect(t, e.do(http.MethodPost, "/api/register", body, one), http.StatusForbidden, "invalid_invite")
	}
	right := account("marta")
	right["invite"] = "секрет"
	expect(t, e.do(http.MethodPost, "/api/register", right, one), http.StatusTooManyRequests, "rate_limited")

	// Форма разбирается раньше лимита и попытки не тратит (ADR-043).
	fresh := fromIP("203.0.113.9")
	for i := 0; i < 20; i++ {
		body := account("МАРТА")
		body["invite"] = "секрет"
		expect(t, e.do(http.MethodPost, "/api/register", body, fresh), http.StatusBadRequest, "invalid_nick")
	}
	expect(t, e.do(http.MethodPost, "/api/register", right, fresh), http.StatusCreated, "")
}

// Вход — 10 за 10 минут на пару IP+ник (ADR-021).
func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	e.signUp("marta")
	e.signUp("petya")

	one := fromIP("203.0.113.7")
	wrong := map[string]any{"nick": "marta", "authKey": bytesOf(32, 9)}
	for i := 0; i < 10; i++ {
		expect(t, e.do(http.MethodPost, "/api/login", wrong, one), http.StatusUnauthorized, "invalid_credentials")
	}
	rec := e.do(http.MethodPost, "/api/login", wrong, one)
	expect(t, rec, http.StatusTooManyRequests, "rate_limited")
	if got := retryAfterOf(t, rec); got < 55 || got > 60 {
		t.Errorf("Retry-After: получено %d, ожидалось около 60", got)
	}

	// Верный пароль с того же адреса ждёт вместе с неверными: ведро
	// на паре, а не на исходе попытки.
	right := map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)}
	expect(t, e.do(http.MethodPost, "/api/login", right, one), http.StatusTooManyRequests, "rate_limited")

	// Другой ник с того же адреса — своё ведро.
	expect(t, e.do(http.MethodPost, "/api/login", map[string]any{"nick": "petya", "authKey": bytesOf(32, 9)}, one),
		http.StatusUnauthorized, "invalid_credentials")
	// Тот же ник с другого адреса — тоже своё.
	expect(t, e.do(http.MethodPost, "/api/login", right, fromIP("203.0.113.8")), http.StatusOK, "")
}

// Остальные изменяющие запросы — 60 в минуту на пользователя (ADR-021).
func TestWritesRateLimit(t *testing.T) {
	e := newEnv(t)
	marta := e.signUp("marta")
	id := deviceOf(1)

	for i := 0; i < 60; i++ {
		rec := e.do(http.MethodPost, "/api/devices", map[string]any{"id": id}, with(marta))
		if rec.Code >= http.StatusMultipleChoices {
			t.Fatalf("запрос %d из шестидесяти отклонён: %d (%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(http.MethodPost, "/api/devices", map[string]any{"id": id}, with(marta))
	expect(t, rec, http.StatusTooManyRequests, "rate_limited")
	// Шестьдесят в минуту — токен раз в секунду.
	if got := retryAfterOf(t, rec); got != 1 {
		t.Errorf("Retry-After: получено %d, ожидалось 1", got)
	}

	// Ведро общее на все изменяющие маршруты пользователя.
	expect(t, e.do(http.MethodPost, "/api/contacts", map[string]any{"nick": "petya"}, with(marta)),
		http.StatusTooManyRequests, "rate_limited")
	expect(t, e.do(http.MethodPost, "/api/logout", nil, with(marta)),
		http.StatusTooManyRequests, "rate_limited")

	// Чтения лимитом не считаются: ADR-021 ограничивает изменяющие.
	expect(t, e.do(http.MethodGet, "/api/devices", nil, with(marta)), http.StatusOK, "")
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(marta)), http.StatusOK, "")
	expect(t, e.do(http.MethodGet, "/api/rooms", nil, with(marta)), http.StatusOK, "")

	// Сообщения считаются своим правилом и своим ведром.
	petya := e.signUp("petya")
	e.addDevice(petya, deviceOf(2))
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 3), "petya"),
		with(marta), withDevice(id)), http.StatusAccepted, "")

	// Другой пользователь чужим лимитом не задет. Строка контакта уже есть:
	// её завело сообщение, поэтому 200, а не 201 (ADR-019).
	expect(t, e.do(http.MethodPost, "/api/contacts", map[string]any{"nick": "marta"}, with(petya)),
		http.StatusOK, "")
}

// Лимит сообщений считается отдельно от общего: тридцать в минуту
// не отнимают шестьдесят у остальных запросов (ADR-021).
func TestMessagesOutsideWritesLimit(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	e.join("petya", 2)

	for i := 0; i < 10; i++ {
		expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), byte(i)), "petya"),
			with(marta), withDevice(m1)), http.StatusAccepted, "")
	}
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 11), "petya"),
		with(marta), withDevice(m1)), http.StatusTooManyRequests, "rate_limited")
	// Пакет сообщений кончился, изменяющие запросы работают.
	expect(t, e.do(http.MethodPost, "/api/ack", map[string]any{"ids": []string{}}, with(marta), withDevice(m1)),
		http.StatusNoContent, "")
	expect(t, e.do(http.MethodPost, "/api/contacts", map[string]any{"nick": "petya"}, with(marta)),
		http.StatusOK, "")
}

// X-Real-IP ставит nginx с той же машины: за ним у каждого адреса своё
// ведро (ADR-055).
func TestRealIPFromLoopback(t *testing.T) {
	e := newEnv(t)
	nginx := fromIP("127.0.0.1")

	for i := 0; i < 5; i++ {
		expect(t, e.do(http.MethodPost, "/api/register", account(nickOf(i)), nginx, withRealIP("198.51.100.7")),
			http.StatusCreated, "")
	}
	expect(t, e.do(http.MethodPost, "/api/register", account("kolya"), nginx, withRealIP("198.51.100.7")),
		http.StatusTooManyRequests, "rate_limited")
	// Соседний адрес за тем же nginx ждать не должен.
	expect(t, e.do(http.MethodPost, "/api/register", account("kolya"), nginx, withRealIP("198.51.100.8")),
		http.StatusCreated, "")
}

// Заголовок из сети не читается: иначе лимит на IP снимался бы новой
// строкой в заголовке, то есть не существовал бы вовсе (ADR-055).
func TestRealIPFromNetworkIgnored(t *testing.T) {
	e := newEnv(t)
	one := fromIP("203.0.113.7")

	for i := 0; i < 5; i++ {
		expect(t, e.do(http.MethodPost, "/api/register", account(nickOf(i)), one,
			withRealIP(fmt.Sprintf("198.51.100.%d", i))), http.StatusCreated, "")
	}
	rec := e.do(http.MethodPost, "/api/register", account("kolya"), one, withRealIP("198.51.100.200"))
	expect(t, rec, http.StatusTooManyRequests, "rate_limited")

	// И на входе тоже: ведро на паре адрес соединения + ник.
	e2 := newEnv(t)
	e2.signUp("marta")
	wrong := map[string]any{"nick": "marta", "authKey": bytesOf(32, 9)}
	for i := 0; i < 10; i++ {
		expect(t, e2.do(http.MethodPost, "/api/login", wrong, one, withRealIP(fmt.Sprintf("198.51.100.%d", i))),
			http.StatusUnauthorized, "invalid_credentials")
	}
	expect(t, e2.do(http.MethodPost, "/api/login", wrong, one, withRealIP("198.51.100.200")),
		http.StatusTooManyRequests, "rate_limited")
}

// Предел тела — 32 КиБ на всех маршрутах, ответ один: 413 too_large
// (ADR-026). Отказ приходит раньше сессии, устройства и разбора тела.
func TestTooLargeEverywhere(t *testing.T) {
	e := newEnv(t)
	marta, device := e.join("marta", 1)
	big := strings.Repeat("a", api.MaxBody+1)

	routes := []struct{ method, target string }{
		{http.MethodPost, "/api/register"},
		{http.MethodPost, "/api/login"},
		{http.MethodPost, "/api/logout"},
		{http.MethodPost, "/api/password"},
		{http.MethodDelete, "/api/me"},
		{http.MethodPost, "/api/devices"},
		{http.MethodDelete, "/api/devices/" + device},
		{http.MethodPut, "/api/devices/" + device + "/push"},
		{http.MethodDelete, "/api/devices/" + device + "/push"},
		{http.MethodPost, "/api/contacts"},
		{http.MethodDelete, "/api/contacts/petya"},
		{http.MethodPost, "/api/rooms"},
		{http.MethodPost, "/api/rooms/" + roomIDOf(1) + "/members"},
		{http.MethodPost, "/api/rooms/" + roomIDOf(1) + "/leave"},
		{http.MethodDelete, "/api/rooms/" + roomIDOf(1)},
		{http.MethodPost, "/api/messages"},
		{http.MethodPost, "/api/ack"},
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/events?device=" + device},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.target, func(t *testing.T) {
			// С сессией и своим устройством — отказ всё равно по телу.
			expect(t, e.do(route.method, route.target, big, with(marta), withDevice(device)),
				http.StatusRequestEntityTooLarge, "too_large")
			// И без сессии тоже: тело проверяется раньше прав (ADR-043).
			expect(t, e.do(route.method, route.target, big),
				http.StatusRequestEntityTooLarge, "too_large")
		})
	}
}

// Тело без заявленной длины обрывается при чтении — тем же кодом и там,
// где раньше отвечало устройство (ADR-043).
func TestTooLargeUnannounced(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	_, foreign := e.join("petya", 2)

	// Валидный json, чтобы разбор дошёл до предела чтения, а не споткнулся
	// о первый же байт.
	body := `{"id":"` + strings.Repeat("a", api.MaxBody) + `"}`
	for _, target := range []string{"/api/messages", "/api/ack", "/api/rooms", "/api/devices", "/api/contacts"} {
		rec := e.do(http.MethodPost, target, nil, with(marta), withDevice(foreign), func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(body))
			r.ContentLength = -1
		})
		expect(t, rec, http.StatusRequestEntityTooLarge, "too_large")
	}
}

// Форма запроса разбирается раньше прав: кривое тело с чужим устройством
// отвечает про тело, а не про устройство (ADR-043).
func TestFormBeforeDevice(t *testing.T) {
	e := newEnv(t)
	_, martaDevice := e.join("marta", 1)
	petya, _ := e.join("petya", 2)

	for _, target := range []string{"/api/messages", "/api/ack", "/api/rooms"} {
		expect(t, e.do(http.MethodPost, target, "{", with(petya), withDevice(martaDevice)),
			http.StatusBadRequest, "bad_json")
		// Заголовка нет вовсе — то же самое.
		expect(t, e.do(http.MethodPost, target, "{", with(petya)),
			http.StatusBadRequest, "bad_json")
	}

	// Кривое поле тела — invalid с этим полем, хотя устройство чужое.
	rec := e.do(http.MethodPost, "/api/messages", message("не ulid", "marta"), with(petya), withDevice(martaDevice))
	expect(t, rec, http.StatusBadRequest, "invalid")
	if got := field(t, rec); got != "id" {
		t.Errorf("field: получено %q, ожидалось \"id\"", got)
	}
	// Часы — свойство самого запроса, а не право: clock_skew тоже раньше
	// (docs/protocol.md, «Сообщения»).
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis()-6*60*1000, 3), "marta"),
		with(petya), withDevice(martaDevice)), http.StatusBadRequest, "clock_skew")

	ids := make([]string, 501)
	for i := range ids {
		ids[i] = ulid(nowMillis(), byte(i))
	}
	expect(t, e.do(http.MethodPost, "/api/ack", map[string]any{"ids": ids}, with(petya), withDevice(martaDevice)),
		http.StatusBadRequest, "invalid")

	room := map[string]any{
		"id":    "не комната",
		"name":  "общая",
		"keyId": keyID(40),
		"keys":  keysFor([]string{"petya"}, 40),
	}
	expect(t, e.do(http.MethodPost, "/api/rooms", room, with(petya), withDevice(martaDevice)),
		http.StatusBadRequest, "invalid")

	// Тело по форме — тогда отказ по устройству.
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 4), "marta"),
		with(petya), withDevice(martaDevice)), http.StatusForbidden, "unknown_device")
}
