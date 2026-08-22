package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xmatic-squad/bare/internal/api"
	"github.com/xmatic-squad/bare/internal/config"
	"github.com/xmatic-squad/bare/internal/store"
	"github.com/xmatic-squad/bare/internal/web"
)

const origin = "https://bare.test"

// env — сервер на временной базе плюс журнал, в который он пишет.
type env struct {
	t   *testing.T
	h   http.Handler
	st  *store.Store
	log *bytes.Buffer
}

func newEnv(t *testing.T) *env { return invited(t, "") }

// invited — сервер на временной базе; непустой code включает инвайты.
func invited(t *testing.T, code string) *env {
	t.Helper()
	static, err := web.New()
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Addr:        "127.0.0.1:0",
		DB:          "bare.db",
		Origin:      origin,
		VAPIDPublic: "vapid",
		InviteCode:  code,
	}
	e := &env{t: t, st: st, log: &bytes.Buffer{}}
	e.h = api.New(cfg, st, static, e.log)
	return e
}

// do отправляет запрос. Origin для методов кроме GET и HEAD ставится сам —
// без него любой такой запрос получил бы 403 (ADR-021).
func (e *env) do(method, target string, body any, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			e.t.Fatalf("сборка тела: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, target, reader)
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Origin", origin)
	}
	for _, opt := range opts {
		opt(r)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func with(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
	}
}

func withOrigin(value string) func(*http.Request) {
	return func(r *http.Request) {
		if value == "" {
			r.Header.Del("Origin")
			return
		}
		r.Header.Set("Origin", value)
	}
}

// code достаёт код ошибки из тела ответа.
func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("разбор тела %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

// expect проверяет статус и код ошибки; код "" — ответ без ошибки.
func expect(t *testing.T, rec *httptest.ResponseRecorder, status int, errCode string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("статус: получено %d (%s), ожидалось %d", rec.Code, rec.Body.String(), status)
	}
	if errCode != "" {
		if got := code(t, rec); got != errCode {
			t.Errorf("код ошибки: получено %q, ожидалось %q", got, errCode)
		}
	}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("разбор тела %q: %v", rec.Body.String(), err)
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/healthz", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("статус: получено %d, ожидалось 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("тело: получено %q, ожидалось \"ok\"", rec.Body.String())
	}

	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s: получено %q, ожидалось %q", header, got, value)
		}
	}
}

func TestStaticNotModified(t *testing.T) {
	e := newEnv(t)

	first := e.do(http.MethodGet, "/app.css", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("статус: получено %d, ожидалось 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("нет ETag")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control: получено %q, ожидалось \"no-cache\"", got)
	}

	second := e.do(http.MethodGet, "/app.css", nil, func(r *http.Request) {
		r.Header.Set("If-None-Match", etag)
	})
	if second.Code != http.StatusNotModified {
		t.Errorf("статус: получено %d, ожидалось 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("тело 304 не пустое: %q", second.Body.String())
	}
	if got := second.Header().Get("ETag"); got != etag {
		t.Errorf("ETag на 304: получено %q, ожидалось %q", got, etag)
	}
}

func TestBodyTooLarge(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodPost, "/api/login", strings.Repeat("a", api.MaxBody+1))
	expect(t, rec, http.StatusRequestEntityTooLarge, "too_large")
}

// Тело без заявленной длины обрывается при чтении — тем же кодом.
func TestBodyTooLargeUnannounced(t *testing.T) {
	e := newEnv(t)
	// Тело — валидный json, чтобы разбор дошёл до предела чтения, а не
	// споткнулся о первый же байт.
	body := `{"nick":"` + strings.Repeat("a", api.MaxBody) + `"}`
	rec := e.do(http.MethodPost, "/api/login", nil, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = -1
	})
	expect(t, rec, http.StatusRequestEntityTooLarge, "too_large")
}

// Неподдерживаемый метод на известном пути — 404 not_found (ADR-026).
func TestStaticRejectsWrite(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do(http.MethodPost, "/app.css", nil), http.StatusNotFound, "not_found")
}

func TestMethodOnKnownAPIPathIs404(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do(http.MethodPost, "/api/me", nil), http.StatusNotFound, "not_found")
	expect(t, e.do(http.MethodGet, "/api/nope", nil), http.StatusNotFound, "not_found")
}

// Путь из запроса не должен уметь дописать строку в журнал.
func TestLogPathEscaped(t *testing.T) {
	e := newEnv(t)

	target := "/x%0a2026-01-01T00:00:00Z%20GET%20/fake%20200%201ms"
	e.do(http.MethodGet, target, nil)

	line := e.log.String()
	if n := strings.Count(line, "\n"); n != 1 {
		t.Errorf("строк в логе: получено %d, ожидалась 1: %q", n, line)
	}
	if !strings.Contains(line, "%0a") {
		t.Errorf("путь не в percent-форме: %q", line)
	}

	e.log.Reset()
	e.do(http.MethodGet, "/"+strings.Repeat("z", 4096), nil)
	if len(e.log.String()) > 512 {
		t.Errorf("длина строки лога: получено %d байт, ожидалось не больше 512", len(e.log.String()))
	}
}

// SSE (docs/protocol.md, «События») флашит каждое событие: обёртка логгера
// не должна прятать Flush от http.ResponseController.
func TestFlushThroughMiddleware(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	var flushErr error
	cfg := &config.Config{Addr: "127.0.0.1:0", DB: "bare.db", Origin: origin}
	h := api.New(cfg, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	}), io.Discard)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if flushErr != nil {
		t.Errorf("Flush: %v", flushErr)
	}
}

func TestInternalErrorHasCode(t *testing.T) {
	e := newEnv(t)
	// Закрытая база — единственный простой способ получить сбой хранилища.
	e.st.Close()
	rec := e.do(http.MethodGet, "/api/kdf?nick=marta", nil)
	expect(t, rec, http.StatusInternalServerError, "internal")
	if !strings.Contains(e.log.String(), "ошибка:") {
		t.Errorf("причина не попала в журнал: %q", e.log.String())
	}
	if strings.Contains(rec.Body.String(), "sql") {
		t.Errorf("причина уехала клиенту: %q", rec.Body.String())
	}
}
