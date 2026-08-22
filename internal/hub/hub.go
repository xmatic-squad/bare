// Package hub держит открытые SSE-потоки устройств (ADR-004, ADR-017).
//
// На устройство приходится один поток: новое соединение закрывает
// предыдущее. Потерянное живое событие не теряет сообщения — оно лежит
// в очереди до ACK и выдаётся заново при следующем подключении
// (docs/protocol.md, «События»).
package hub

import "sync"

// buffer — сколько событий поток держит, пока обработчик их не разобрал.
const buffer = 32

// Event — одно событие SSE: имя и готовый JSON. Данные — строка: её
// нельзя изменить после того, как она ушла в несколько потоков сразу.
type Event struct {
	Name string
	Data string
}

// Hub — карта «устройство → открытый поток». Пуст, пока никто не подключён.
type Hub struct {
	mu      sync.Mutex
	streams map[string]*Stream
}

// New заводит пустой hub.
func New() *Hub { return &Hub{streams: make(map[string]*Stream)} }

// Stream — поток одного устройства. Обработчик читает Events до тех пор,
// пока не закроется Done или не уйдёт клиент.
type Stream struct {
	hub    *Hub
	device string
	events chan Event
	done   chan struct{}
	once   sync.Once
}

// Open открывает поток устройства и закрывает предыдущий, если он был:
// одно соединение на устройство (docs/protocol.md, «События»).
func (h *Hub) Open(device string) *Stream {
	s := &Stream{
		hub:    h,
		device: device,
		events: make(chan Event, buffer),
		done:   make(chan struct{}),
	}
	h.mu.Lock()
	prev := h.streams[device]
	h.streams[device] = s
	h.mu.Unlock()
	if prev != nil {
		prev.stop()
	}
	return s
}

// Send отдаёт событие подключённому устройству. Устройство не подключено —
// молча ничего: конверт уже лежит в его очереди.
func (h *Hub) Send(device string, ev Event) {
	h.mu.Lock()
	s := h.streams[device]
	h.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.events <- ev:
	default:
		// Клиент не успевает читать. Закрываем поток: переподключение
		// выдаст очередь целиком, а копить события в памяти сервера —
		// не его дело (ADR-008).
		h.drop(s)
	}
}

// Close закрывает поток устройства: устройство удалили (docs/protocol.md,
// «Устройства»).
func (h *Hub) Close(device string) {
	h.mu.Lock()
	s := h.streams[device]
	delete(h.streams, device)
	h.mu.Unlock()
	if s != nil {
		s.stop()
	}
}

// CloseAll закрывает все потоки: сервер останавливается. Без этого
// остановка ждала бы, пока клиенты уйдут сами.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	streams := h.streams
	h.streams = make(map[string]*Stream)
	h.mu.Unlock()
	for _, s := range streams {
		s.stop()
	}
}

// drop снимает регистрацию именно этого потока и закрывает его. Если
// устройство успело подключиться заново, новый поток остаётся на месте.
func (h *Hub) drop(s *Stream) {
	h.mu.Lock()
	if h.streams[s.device] == s {
		delete(h.streams, s.device)
	}
	h.mu.Unlock()
	s.stop()
}

// Events — события, пришедшие потоку.
func (s *Stream) Events() <-chan Event { return s.events }

// Done закрывается, когда поток закрыт: новым соединением того же
// устройства, удалением устройства или остановкой сервера.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Close закрывает поток — его зовёт обработчик, когда клиент ушёл.
func (s *Stream) Close() { s.hub.drop(s) }

func (s *Stream) stop() { s.once.Do(func() { close(s.done) }) }
