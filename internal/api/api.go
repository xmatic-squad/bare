// Package api собирает маршруты и общие для всех ответов правила:
// заголовки безопасности (ADR-021), лимит тела запроса, лог в stdout.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// MaxBody — предел тела запроса, 32 КиБ (ADR-021).
const MaxBody = 32 << 10

// maxLogPath — сколько байт пути попадает в строку лога.
const maxLogPath = 256

// csp — политика из ADR-021. HSTS ставит nginx, здесь его нет.
const csp = "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// New собирает обработчик: /healthz, всё остальное — статика.
// log — куда писать строки запросов; nil отключает лог.
func New(static http.Handler, logw io.Writer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("/", static)
	return logging(logw, headers(limitBody(mux)))
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok")
}

// Error пишет ошибку в форме протокола: {"error": код, "message": текст}.
// Единственное место, где эта форма собирается, — коды берутся из
// перечня в docs/protocol.md.
func Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": message,
	})
}

// NotFound — ответ на неизвестный путь и на неподдерживаемый метод
// известного пути (ADR-026).
func NotFound(w http.ResponseWriter) {
	Error(w, http.StatusNotFound, "not_found", "такого пути нет")
}

// headers ставит заголовки безопасности на каждый ответ, включая ошибки.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// limitBody отрезает тело на 32 КиБ. Заявленный размер сверх лимита
// отклоняется сразу, незаявленный — обрывается при чтении.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBody {
			Error(w, http.StatusRequestEntityTooLarge, "too_large", "тело запроса больше 32 КиБ")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// logging пишет время, метод, путь, статус и длительность.
// Ни IP, ни ник, ни query в лог не попадают.
func logging(out io.Writer, next http.Handler) http.Handler {
	if out == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		fmt.Fprintf(out, "%s %s %s %d %s\n",
			start.Format(time.RFC3339),
			r.Method,
			logPath(r.URL),
			rec.status,
			time.Since(start).Round(time.Microsecond))
	})
}

// logPath даёт путь в percent-форме: перевод строки, escape-последовательности
// и прочие управляющие байты в журнал не попадают — иначе любой запрос
// подделывал бы строки в journald. Длинный путь обрезается.
func logPath(u *url.URL) string {
	p := u.EscapedPath()
	if len(p) > maxLogPath {
		return p[:maxLogPath] + "…"
	}
	return p
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap отдаёт исходный ResponseWriter: через него http.ResponseController
// добирается до Flush и Hijack. Без этого SSE (docs/protocol.md, «События»)
// буферизовался бы — лог стоит самым внешним слоем и виден всем маршрутам.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
