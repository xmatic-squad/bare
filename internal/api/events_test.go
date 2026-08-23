package api_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xmatic-squad/bare/internal/config"
)

// wait — сколько тест ждёт события. Всё локально, задержек быть не должно.
const wait = 2 * time.Second

// sseEvent — одно событие потока.
type sseEvent struct {
	name string
	data string
}

// stream — открытый GET /api/events. Идёт через настоящий сервер:
// httptest.ResponseRecorder не отдаёт тело, пока обработчик не вернулся.
type stream struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc
	events chan sseEvent
	head   http.Header
}

// open подключается к потоку событий устройства.
func (e *env) open(device string, c *http.Cookie) *stream {
	e.t.Helper()
	srv := e.live()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/events?device="+url.QueryEscape(device), nil)
	if err != nil {
		cancel()
		e.t.Fatalf("запрос: %v", err)
	}
	req.AddCookie(c)
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		e.t.Fatalf("подключение: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		e.t.Fatalf("статус потока: получено %d, ожидалось 200", resp.StatusCode)
	}
	s := &stream{t: e.t, ctx: ctx, cancel: cancel, events: make(chan sseEvent, 64), head: resp.Header}
	go s.read(resp.Body)
	e.t.Cleanup(s.close)
	return s
}

// read разбирает кадры SSE: строки event и data, пустая строка — конец
// события, строка с двоеточия — комментарий-пинг.
func (s *stream) read(body io.ReadCloser) {
	defer body.Close()
	defer close(s.events)

	sc := bufio.NewScanner(body)
	var ev sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.name == "" {
				continue
			}
			select {
			case s.events <- ev:
			case <-s.ctx.Done():
				return
			}
			ev = sseEvent{}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// next ждёт следующее событие.
func (s *stream) next() sseEvent {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			s.t.Fatal("поток закрылся, события нет")
		}
		return ev
	case <-time.After(wait):
		s.t.Fatal("событие не пришло")
	}
	return sseEvent{}
}

// untilReady собирает события до ready — то, что лежало в очереди.
func (s *stream) untilReady() []sseEvent {
	s.t.Helper()
	var out []sseEvent
	for {
		ev := s.next()
		if ev.name == "ready" {
			if ev.data != "{}" {
				s.t.Errorf("данные ready: получено %q, ожидалось \"{}\"", ev.data)
			}
			return out
		}
		out = append(out, ev)
	}
}

// ended ждёт, что поток закроет сервер.
func (s *stream) ended() {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if ok {
			s.t.Fatalf("вместо закрытия пришло событие %q", ev.name)
		}
	case <-time.After(wait):
		s.t.Fatal("поток не закрылся")
	}
}

func (s *stream) close() { s.cancel() }

// Порядок после подключения: очередь, ready, живые события
// (docs/protocol.md, «События»).
func TestEventsQueueThenReady(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)

	first := ulid(nowMillis(), 3)
	second := ulid(nowMillis()+1, 4)
	for _, id := range []string{first, second} {
		expect(t, e.do(http.MethodPost, "/api/messages", message(id, "petya"), with(marta), withDevice(m1)),
			http.StatusAccepted, "")
	}

	s := e.open(p1, petya)
	for header, value := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := s.head.Get(header); got != value {
			t.Errorf("%s: получено %q, ожидалось %q", header, got, value)
		}
	}

	queued := s.untilReady()
	if len(queued) != 2 {
		t.Fatalf("событий из очереди: получено %d, ожидалось 2", len(queued))
	}
	for i, ev := range queued {
		if ev.name != "msg" {
			t.Errorf("событие %d: получено %q, ожидалось \"msg\"", i, ev.name)
		}
		if !strings.Contains(ev.data, `"from":"marta"`) {
			t.Errorf("конверт %d: %s", i, ev.data)
		}
	}
	if !strings.Contains(queued[0].data, first) || !strings.Contains(queued[1].data, second) {
		t.Errorf("порядок очереди: %q, %q", queued[0].data, queued[1].data)
	}

	// Реконнект без ACK повторяет очередь целиком: Last-Event-ID сервер
	// не смотрит (docs/protocol.md, «События»).
	s.close()
	again := e.open(p1, petya)
	if got := again.untilReady(); len(got) != 2 {
		t.Fatalf("после реконнекта: получено %d событий, ожидалось 2", len(got))
	}

	// После ACK очередь пуста, остаётся только ready.
	expect(t, e.do(http.MethodPost, "/api/ack", map[string]any{"ids": []string{first, second}},
		with(petya), withDevice(p1)), http.StatusNoContent, "")
	again.close()
	third := e.open(p1, petya)
	if got := third.untilReady(); len(got) != 0 {
		t.Errorf("после ack: получено %d событий, ожидалось 0", len(got))
	}
}

// Подключённое устройство получает конверт сразу после коммита.
func TestEventsLive(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)

	s := e.open(p1, petya)
	if got := s.untilReady(); len(got) != 0 {
		t.Fatalf("очередь нового устройства: %+v", got)
	}

	id := ulid(nowMillis(), 3)
	expect(t, e.do(http.MethodPost, "/api/messages", message(id, "petya"), with(marta), withDevice(m1)),
		http.StatusAccepted, "")

	ev := s.next()
	if ev.name != "msg" || !strings.Contains(ev.data, id) {
		t.Errorf("живое событие: %+v", ev)
	}
	// Живая доставка не отменяет ACK: конверт лежит в очереди до него.
	if got := e.queue(p1); len(got) != 1 {
		t.Errorf("очередь: получено %d конвертов, ожидался 1", len(got))
	}
}

// Отправитель эха не получает даже живьём, другие его устройства — да.
func TestEventsNoEchoToSender(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	m2 := e.addDevice(marta, deviceOf(2))
	e.join("petya", 3)

	sender := e.open(m1, marta)
	sender.untilReady()
	other := e.open(m2, marta)
	other.untilReady()

	id := ulid(nowMillis(), 4)
	expect(t, e.do(http.MethodPost, "/api/messages", message(id, "petya"), with(marta), withDevice(m1)),
		http.StatusAccepted, "")

	if ev := other.next(); ev.name != "msg" || !strings.Contains(ev.data, id) {
		t.Errorf("второе устройство отправителя: %+v", ev)
	}
	select {
	case ev := <-sender.events:
		t.Errorf("эхо отправившему устройству: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// Одно соединение на устройство: новое закрывает предыдущее.
func TestEventsSingleConnection(t *testing.T) {
	e := newEnv(t)
	petya, p1 := e.join("petya", 1)

	first := e.open(p1, petya)
	first.untilReady()
	second := e.open(p1, petya)
	second.untilReady()

	first.ended()
}

// Удаление устройства закрывает его поток (docs/protocol.md, «Устройства»).
func TestEventsClosedOnDeviceDelete(t *testing.T) {
	e := newEnv(t)
	petya, p1 := e.join("petya", 1)

	s := e.open(p1, petya)
	s.untilReady()

	expect(t, e.do(http.MethodDelete, "/api/devices/"+p1, nil, with(petya)), http.StatusNoContent, "")
	s.ended()
}

// Смена пароля с logoutOthers закрывает потоки отозванных сессий:
// поток проверяет сессию только при подключении, и без этого отозванное
// устройство продолжало бы получать конверты (ADR-058).
func TestEventsClosedOnLogoutOthers(t *testing.T) {
	e := newEnv(t)
	first, d1 := e.join("marta", 1)

	login := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)})
	expect(t, login, http.StatusOK, "")
	second := e.cookie(login)
	d2 := e.addDevice(second, deviceOf(2))

	revoked := e.open(d1, first)
	revoked.untilReady()
	kept := e.open(d2, second)
	kept.untilReady()

	expect(t, e.do(http.MethodPost, "/api/password", map[string]any{
		"authKey":      bytesOf(32, 1),
		"newAuthKey":   bytesOf(32, 9),
		"blob":         blobOf(config.KDFIterations),
		"logoutOthers": true,
	}, with(second)), http.StatusNoContent, "")

	revoked.ended()

	// Поток той сессии, ради которой всё затевалось, остаётся живым.
	select {
	case ev, ok := <-kept.events:
		if !ok {
			t.Fatal("закрылся поток текущей сессии")
		}
		t.Fatalf("лишнее событие текущей сессии: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// Чужое устройство в query — 403 unknown_device, поток не открывается.
func TestEventsUnknownDevice(t *testing.T) {
	e := newEnv(t)
	_, martaDevice := e.join("marta", 1)
	petya, _ := e.join("petya", 2)

	for _, device := range []string{martaDevice, deviceOf(9), "мусор", ""} {
		rec := e.do(http.MethodGet, "/api/events?device="+url.QueryEscape(device), nil, with(petya))
		expect(t, rec, http.StatusForbidden, "unknown_device")
	}
	// Без сессии — обычный 401.
	expect(t, e.do(http.MethodGet, "/api/events?device="+martaDevice, nil), http.StatusUnauthorized, "unauthenticated")
}
