package api

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xmatic-squad/bare/internal/auth"
)

// pingEvery — период комментария-пинга: он держит соединение живым
// через прокси и показывает клиенту, что поток цел (docs/protocol.md).
const pingEvery = 20 * time.Second

// GET /api/events?device= — поток событий устройства (ADR-004).
// Устройство передаётся в query: EventSource не умеет заголовки.
//
// Last-Event-ID игнорируется: механизм восстановления — не докрутка
// по идентификатору, а повторная выдача очереди при каждом подключении.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	sess, _ := auth.From(r)
	device := r.URL.Query().Get("device")
	if !validID(device) {
		unknownDevice(w)
		return
	}
	owned, err := s.st.DeviceOwned(r.Context(), device, sess.Nick)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !owned {
		unknownDevice(w)
		return
	}

	// Порядок — docs/protocol.md, «События»: сначала push_pending и
	// last_seen, потом поток, потом очередь. Сорвавшаяся запись last_seen
	// не должна рвать исправный поток устройства, а он был бы уже закрыт
	// открытием нового.
	now := time.Now().UnixMilli()
	if err := s.st.TouchDevice(r.Context(), device, now); err != nil {
		s.internal(w, r, err)
		return
	}

	// Поток открывается до чтения очереди: конверт, попавший в очередь
	// между выборкой и подпиской, иначе пролежал бы там до следующего
	// подключения. Обратная крайность — дубль, а его клиент сливает по id
	// (ADR-017). Открытие закрывает прежний поток этого устройства.
	stream := s.hub.Open(device)
	defer stream.Close()

	queued, err := s.st.Queue(r.Context(), device)
	if err != nil {
		s.internal(w, r, err)
		return
	}

	head := w.Header()
	head.Set("Content-Type", "text/event-stream")
	head.Set("Cache-Control", "no-cache")
	// nginx буферизует ответы проксируемых приложений; для потока это
	// означало бы, что события копятся и не уходят (docs/deploy.md).
	head.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := sender(w)
	for _, envelope := range queued {
		if !send("msg", envelope) {
			return
		}
	}
	if !send("ready", "{}") {
		return
	}

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			// Клиент ушёл.
			return
		case <-stream.Done():
			// Поток закрыли: новое соединение того же устройства,
			// удаление устройства или остановка сервера.
			return
		case ev := <-stream.Events():
			if !send(ev.Name, ev.Data) {
				return
			}
		case <-ping.C:
			if !write(w, ": ping\n\n") {
				return
			}
		}
	}
}

// sender собирает функцию записи события. Данные — компактный JSON
// без переводов строки, поэтому кадр SSE собирается одной строкой data.
// Ответ false означает, что писать больше некуда: соединение оборвалось.
func sender(w http.ResponseWriter) func(name, data string) bool {
	return func(name, data string) bool {
		return write(w, fmt.Sprintf("event: %s\ndata: %s\n\n", name, data))
	}
}

func write(w http.ResponseWriter, frame string) bool {
	if _, err := io.WriteString(w, frame); err != nil {
		return false
	}
	// Без Flush кадр остался бы в буфере net/http до конца ответа,
	// а конца у потока нет.
	return http.NewResponseController(w).Flush() == nil
}
