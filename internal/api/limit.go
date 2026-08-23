package api

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xmatic-squad/bare/internal/auth"
)

// Лимиты ADR-021, все четыре правила. Token bucket в памяти сервера:
// рестарт их обнуляет — для маленького сервера это принято.
//
// Пакет отдельным числом задан только у сообщений. У остальных правил он
// равен самому лимиту: «5 в час» означает, что за час набегает пять
// попыток и потратить их можно разом (ADR-055).
var (
	// registerRule — регистрация: 5 в час на IP.
	registerRule = rule{count: 5, window: time.Hour, burst: 5}
	// loginRule — вход: 10 за 10 минут на пару IP+ник.
	loginRule = rule{count: 10, window: 10 * time.Minute, burst: 10}
	// messagesRule — сообщения: 30 в минуту на пользователя, пакет 10.
	messagesRule = rule{count: 30, window: time.Minute, burst: 10}
	// writesRule — остальные изменяющие запросы: 60 в минуту
	// на пользователя.
	writesRule = rule{count: 60, window: time.Minute, burst: 60}
)

// rule — правило лимита: count запросов за window, пакетом не больше burst.
type rule struct {
	count  int
	window time.Duration
	burst  int
}

// generation — сколько ключей карта лимита держит до смены поколения.
//
// Ведро заводится на каждый новый ключ, а ключ — это чужой адрес или чужой
// ник: их бывает сколько угодно. Выбрасывать полные вёдра мало: под потоком
// новых ключей полных не бывает вовсе — каждое только что потратило токен.
// Поэтому карты две, нынешняя и прежняя. Как только нынешняя дорастает до
// generation, она становится прежней, а прежняя выбрасывается целиком.
// Ключ, по которому продолжают ходить, переезжает в нынешнюю и смену
// переживает; забывается только то, к чему не обращались целое поколение,
// а забытое ведро — то же самое, что новое.
//
// Отсюда предел: обе карты вместе держат не больше 2×generation вёдер,
// то есть около мегабайта на правило. Миллион разных адресов памяти
// не съедает — он протачивает поколения насквозь.
const generation = 4096

// buckets — token bucket в памяти сервера, по ведру на ключ.
type buckets struct {
	mu    sync.Mutex
	rate  float64 // токенов в секунду
	burst float64
	cur   map[string]*bucket // нынешнее поколение
	old   map[string]*bucket // прежнее, пока к его ключам ещё обращаются
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newBuckets(r rule) *buckets {
	return &buckets{
		rate:  float64(r.count) / r.window.Seconds(),
		burst: float64(r.burst),
		cur:   make(map[string]*bucket),
	}
}

// take забирает токен. Второе значение — можно ли; если нет, первое —
// сколько ждать до следующего токена.
func (b *buckets) take(key string, now time.Time) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.bucket(key, now)
	e.tokens = math.Min(b.burst, e.tokens+b.refill(e.at, now))
	e.at = now
	if e.tokens < 1 {
		return time.Duration((1 - e.tokens) / b.rate * float64(time.Second)), false
	}
	e.tokens--
	return 0, true
}

// bucket находит ведро ключа или заводит новое. Смена поколения идёт
// до поиска: так в нынешней карте никогда не больше generation ключей,
// а в обеих вместе — не больше двух таких карт.
func (b *buckets) bucket(key string, now time.Time) *bucket {
	if len(b.cur) >= generation {
		b.old = b.cur
		b.cur = make(map[string]*bucket, generation)
	}
	if e, ok := b.cur[key]; ok {
		return e
	}
	if e, ok := b.old[key]; ok {
		delete(b.old, key)
		b.cur[key] = e
		return e
	}
	e := &bucket{tokens: b.burst, at: now}
	b.cur[key] = e
	return e
}

// refill — сколько токенов набежало. Время назад не идёт: часы могли
// прыгнуть, но долг за это выставлять некому.
func (b *buckets) refill(since, now time.Time) float64 {
	d := now.Sub(since)
	if d <= 0 {
		return 0
	}
	return d.Seconds() * b.rate
}

// retryAfter — значение заголовка в секундах, не меньше одной: нулевое
// ожидание после отказа сбивало бы клиента с толку.
func retryAfter(wait time.Duration) int {
	if wait < time.Second {
		return 1
	}
	return int(math.Ceil(wait.Seconds()))
}

// rateLimited — 429 с Retry-After в целых секундах (ADR-021).
func (s *server) rateLimited(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter(wait)))
	Error(w, http.StatusTooManyRequests, "rate_limited", "слишком часто, попробуйте позже")
}

// limitWrites — общий лимит изменяющих запросов: 60 в минуту
// на пользователя (ADR-021). Стоит на маршруте, а не в обработчике,
// поэтому отвечает раньше разбора тела: смысл лимита в том, чтобы сервер
// не брался за работу, а разбор тела — уже работа. Форму это не обгоняет
// в смысле ADR-043: 429 говорит не о правах и не о существовании
// сущностей, а о частоте.
//
// Сообщения сюда не входят: у них своё правило, своё ведро и своё место
// в порядке проверок (docs/protocol.md, «Сообщения»).
func (s *server) limitWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := auth.From(r)
		if wait, ok := s.writes.take(sess.Nick, time.Now()); !ok {
			s.rateLimited(w, wait)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP — ключ лимитов, привязанных к адресу.
//
// X-Real-IP ставит nginx на той же машине (ADR-022), и верить заголовку
// можно только тогда, когда соединение пришло оттуда же. Иначе его
// подставит кто угодно: новая строка в заголовке — новое ведро, и лимита
// на IP не существует вовсе. Соединение не с loopback — заголовок
// не читается, ключом становится адрес соединения.
func clientIP(r *http.Request) string {
	remote := connIP(r.RemoteAddr)
	if !remote.IsValid() {
		// Адрес соединения не разобрать. Одно общее ведро на всех —
		// лучше, чем ни одного.
		return r.RemoteAddr
	}
	if remote.IsLoopback() {
		if ip, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
			return ip.Unmap().WithZone("").String()
		}
	}
	return remote.String()
}

// connIP — адрес, с которого пришло соединение. Невалидный Addr означает,
// что RemoteAddr не разобрать.
func connIP(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap().WithZone("")
}
