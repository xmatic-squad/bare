// Package api собирает маршруты и общие для всех ответов правила:
// заголовки безопасности (ADR-021), проверку Origin, лимит тела запроса,
// лог в stdout.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xmatic-squad/bare/internal/auth"
	"github.com/xmatic-squad/bare/internal/config"
	"github.com/xmatic-squad/bare/internal/hub"
	"github.com/xmatic-squad/bare/internal/push"
	"github.com/xmatic-squad/bare/internal/store"
)

// MaxBody — предел тела запроса, 32 КиБ (ADR-021).
const MaxBody = 32 << 10

// maxLogPath — сколько байт пути попадает в строку лога.
const maxLogPath = 256

// csp — политика из ADR-021. HSTS ставит nginx, здесь его нет.
const csp = "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// server — общее для обработчиков: настройки, база, открытые потоки
// событий, лимиты, куда писать журнал.
type server struct {
	cfg  *config.Config
	st   *store.Store
	hub  *hub.Hub
	push *push.Sender
	msgs *buckets
	logw io.Writer
}

// Handler — обработчик всех маршрутов, живые SSE-потоки и очередь пушей
// за ним.
type Handler struct {
	http.Handler
	hub  *hub.Hub
	push *push.Sender
}

// Close закрывает открытые потоки событий и останавливает отправку
// пушей. Без него остановка сервера ждала бы, пока клиенты уйдут сами:
// у потока нет конца (ADR-004).
func (h *Handler) Close() {
	h.hub.CloseAll()
	h.push.Close()
}

// New собирает обработчик: /api/, /healthz, всё остальное — статика.
// logw — куда писать строки запросов и причины отказов; nil отключает лог.
func New(cfg *config.Config, st *store.Store, static http.Handler, logw io.Writer) *Handler {
	// Отправитель пушей спрашивает у hub, подключено ли устройство:
	// решение «пуш только молчащему» принимается в момент захвата права
	// на него, а не при постановке в очередь (ADR-023).
	live := hub.New()
	s := &server{
		cfg:  cfg,
		st:   st,
		hub:  live,
		push: push.New(cfg, st, live.Connected, logw),
		msgs: newBuckets(messagesPerMinute, messagesBurst),
		logw: logw,
	}
	fail := auth.Fail{Error: Error, Internal: s.internal}
	// Сессия проверяется на всех непубличных маршрутах (docs/protocol.md).
	private := auth.Require(st, fail)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)

	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/kdf", s.kdf)
	mux.HandleFunc("POST /api/register", s.register)
	mux.HandleFunc("POST /api/login", s.login)

	mux.Handle("GET /api/me", private(http.HandlerFunc(s.me)))
	mux.Handle("DELETE /api/me", private(http.HandlerFunc(s.deleteMe)))
	mux.Handle("POST /api/logout", private(http.HandlerFunc(s.logout)))
	mux.Handle("POST /api/password", private(http.HandlerFunc(s.password)))
	mux.Handle("GET /api/users/{nick}", private(http.HandlerFunc(s.user)))

	mux.Handle("POST /api/devices", private(http.HandlerFunc(s.createDevice)))
	mux.Handle("GET /api/devices", private(http.HandlerFunc(s.devices)))
	mux.Handle("DELETE /api/devices/{id}", private(http.HandlerFunc(s.deleteDevice)))
	mux.Handle("PUT /api/devices/{id}/push", private(http.HandlerFunc(s.setPush)))
	mux.Handle("DELETE /api/devices/{id}/push", private(http.HandlerFunc(s.deletePush)))

	mux.Handle("GET /api/contacts", private(http.HandlerFunc(s.contacts)))
	mux.Handle("POST /api/contacts", private(http.HandlerFunc(s.addContact)))
	mux.Handle("DELETE /api/contacts/{nick}", private(http.HandlerFunc(s.deleteContact)))

	mux.Handle("GET /api/rooms", private(http.HandlerFunc(s.rooms)))
	mux.Handle("POST /api/rooms", private(http.HandlerFunc(s.createRoom)))
	mux.Handle("POST /api/rooms/{id}/members", private(http.HandlerFunc(s.updateMembers)))
	mux.Handle("POST /api/rooms/{id}/leave", private(http.HandlerFunc(s.leaveRoom)))
	mux.Handle("DELETE /api/rooms/{id}", private(http.HandlerFunc(s.deleteRoom)))

	mux.Handle("GET /api/events", private(http.HandlerFunc(s.events)))
	mux.Handle("POST /api/messages", private(http.HandlerFunc(s.sendMessage)))
	mux.Handle("POST /api/ack", private(http.HandlerFunc(s.ack)))

	// Всё прочее под /api/ — 404, включая неподдерживаемый метод известного
	// пути: кода 405 в протоколе нет (ADR-026). Этот маршрут заодно не даёт
	// запросам к /api/ уходить в обработчик статики.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { NotFound(w) })
	mux.Handle("/", static)

	return &Handler{
		Handler: logging(logw, headers(auth.Origin(cfg.Origin, fail)(limitBody(mux)))),
		hub:     s.hub,
		push:    s.push,
	}
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok")
}

// errorBody — единственная форма ошибки в протоколе.
type errorBody struct {
	Error   string `json:"error"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// Error пишет ошибку в форме протокола: {"error": код, "message": текст}.
// Единственное место, где эта форма собирается, — коды берутся из
// перечня в docs/protocol.md.
func Error(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}

// Invalid — 400 invalid с полем, на котором остановилась валидация.
func Invalid(w http.ResponseWriter, field, message string) {
	writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid", Field: field, Message: message})
}

// NotFound — ответ на неизвестный путь и на неподдерживаемый метод
// известного пути (ADR-026).
func NotFound(w http.ResponseWriter) {
	Error(w, http.StatusNotFound, "not_found", "такого пути нет")
}

// internal — 500: сбой на нашей стороне. Клиенту уходит только код,
// причина — в журнал сервера (ADR-027).
func (s *server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.report(r, err)
	Error(w, http.StatusInternalServerError, "internal", "сервер не справился, попробуйте позже")
}

// report кладёт причину в журнал. Ник в строку не попадает: пишется
// шаблон маршрута (docs/deploy.md, «Логи»).
func (s *server) report(r *http.Request, err error) {
	if s.logw == nil {
		return
	}
	fmt.Fprintf(s.logw, "%s %s %s ошибка: %v\n",
		time.Now().Format(time.RFC3339), r.Method, logTarget(r), err)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func noContent(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// decode разбирает тело запроса в v. Ответ об ошибке уже написан,
// если вернулось false.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			Error(w, http.StatusRequestEntityTooLarge, "too_large", "тело запроса больше 32 КиБ")
			return false
		}
		Error(w, http.StatusBadRequest, "bad_json", "тело запроса — не json")
		return false
	}
	return true
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
			logTarget(r),
			rec.status,
			time.Since(start).Round(time.Microsecond))
	})
}

// logTarget — что пишется в журнал вместо пути. Для маршрутов /api/ —
// шаблон, а не путь: ник из GET /api/users/{nick} в журнал попадать
// не должен (docs/deploy.md, «Логи»). Для статики — сам путь: там
// пользовательских данных нет, а знать, какой файл не нашёлся, полезно.
// Шаблон известен после маршрутизации, поэтому вызывается после ответа.
//
// Шаблона может не быть вовсе: проверка Origin и предел тела отвечают
// раньше маршрутизации. Тогда для /api/ пишется голое "/api/" — путь
// с ником в журнал не уходит и в этом случае.
func logTarget(r *http.Request) string {
	if p := patternPath(r.Pattern); strings.HasPrefix(p, "/api/") {
		return p
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return "/api/"
	}
	return logPath(r.URL)
}

// patternPath отрезает от шаблона метод: "GET /api/users/{nick}" → путь.
func patternPath(pattern string) string {
	if i := strings.LastIndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
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
