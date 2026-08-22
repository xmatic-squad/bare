package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xmatic-squad/bare/internal/api"
	"github.com/xmatic-squad/bare/internal/web"
)

func handler(t *testing.T) http.Handler {
	t.Helper()
	static, err := web.New()
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return api.New(static, nil)
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	handler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("статус: получено %d, ожидалось 200", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("чтение тела: %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("тело: получено %q, ожидалось \"ok\"", body)
	}

	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
	}
	for header, value := range want {
		if got := res.Header.Get(header); got != value {
			t.Errorf("%s: получено %q, ожидалось %q", header, got, value)
		}
	}
}

func TestStaticNotModified(t *testing.T) {
	h := handler(t)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/app.css", nil))
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

	req := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	h.ServeHTTP(second, req)

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
	body := strings.NewReader(strings.Repeat("a", api.MaxBody+1))
	rec := httptest.NewRecorder()
	handler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/nope", body))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("статус: получено %d, ожидалось 413", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор тела: %v", err)
	}
	if got["error"] != "too_large" {
		t.Errorf("код ошибки: получено %q, ожидалось \"too_large\"", got["error"])
	}
}

// Неподдерживаемый метод на известном пути — 404 not_found (ADR-026).
func TestStaticRejectsWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	handler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/app.css", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("статус: получено %d, ожидалось 404", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("разбор тела: %v", err)
	}
	if got["error"] != "not_found" {
		t.Errorf("код ошибки: получено %q, ожидалось \"not_found\"", got["error"])
	}
}

// Путь из запроса не должен уметь дописать строку в журнал.
func TestLogPathEscaped(t *testing.T) {
	static, err := web.New()
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	var log bytes.Buffer
	h := api.New(static, &log)

	target := "/x%0a2026-01-01T00:00:00Z%20GET%20/fake%20200%201ms"
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))

	line := log.String()
	if n := strings.Count(line, "\n"); n != 1 {
		t.Errorf("строк в логе: получено %d, ожидалась 1: %q", n, line)
	}
	if !strings.Contains(line, "%0a") {
		t.Errorf("путь не в percent-форме: %q", line)
	}

	log.Reset()
	long := "/" + strings.Repeat("z", 4096)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, long, nil))
	if len(log.String()) > 512 {
		t.Errorf("длина строки лога: получено %d байт, ожидалось не больше 512", len(log.String()))
	}
}

// SSE (docs/protocol.md, «События») флашит каждое событие: обёртка логгера
// не должна прятать Flush от http.ResponseController.
func TestFlushThroughMiddleware(t *testing.T) {
	var flushErr error
	h := api.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	}), io.Discard)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if flushErr != nil {
		t.Errorf("Flush: %v", flushErr)
	}
}
