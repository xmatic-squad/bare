package hub

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

const wait = 2 * time.Second

// next ждёт событие потока.
func next(t *testing.T, s *Stream) Event {
	t.Helper()
	select {
	case ev := <-s.Events():
		return ev
	case <-time.After(wait):
		t.Fatal("событие не пришло")
	}
	return Event{}
}

// closed ждёт закрытия потока.
func closed(t *testing.T, s *Stream) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(wait):
		t.Fatal("поток не закрылся")
	}
}

func open(t *testing.T, s *Stream) {
	t.Helper()
	select {
	case <-s.Done():
		t.Fatal("поток закрыт")
	default:
	}
}

func TestSend(t *testing.T) {
	h := New()
	s := h.Open("device")

	h.Send("device", Event{Name: "msg", Data: `{"id":"1"}`})
	if ev := next(t, s); ev.Name != "msg" || ev.Data != `{"id":"1"}` {
		t.Errorf("событие: %+v", ev)
	}

	// Неподключённое устройство — молча ничего: конверт лежит в очереди.
	h.Send("другое", Event{Name: "msg"})
	open(t, s)
}

// Одно соединение на устройство: новое закрывает предыдущее.
func TestOpenClosesPrevious(t *testing.T) {
	h := New()
	first := h.Open("device")
	second := h.Open("device")

	closed(t, first)
	open(t, second)

	h.Send("device", Event{Name: "msg"})
	if ev := next(t, second); ev.Name != "msg" {
		t.Errorf("событие ушло не в тот поток: %+v", ev)
	}
}

// Close закрывает поток устройства: устройство удалили.
func TestCloseDevice(t *testing.T) {
	h := New()
	s := h.Open("device")

	h.Close("device")
	closed(t, s)

	// Второе закрытие и закрытие неизвестного устройства — не беда.
	h.Close("device")
	h.Close("другое")
}

// CloseAll — остановка сервера.
func TestCloseAll(t *testing.T) {
	h := New()
	first := h.Open("first")
	second := h.Open("second")

	h.CloseAll()
	closed(t, first)
	closed(t, second)
}

// Клиент, который не читает, теряет поток, а не память сервера:
// переподключение выдаст очередь целиком.
func TestOverflowDropsStream(t *testing.T) {
	h := New()
	s := h.Open("device")

	for i := 0; i < buffer+1; i++ {
		h.Send("device", Event{Name: "msg", Data: strconv.Itoa(i)})
	}
	closed(t, s)

	// Место в карте освободилось: следующее подключение начинает с нуля.
	fresh := h.Open("device")
	h.Send("device", Event{Name: "msg", Data: "снова"})
	if ev := next(t, fresh); ev.Data != "снова" {
		t.Errorf("событие: %+v", ev)
	}
}

// Закрытие потока обработчиком не трогает уже открытый новый.
func TestStreamCloseKeepsNewer(t *testing.T) {
	h := New()
	first := h.Open("device")
	second := h.Open("device")
	first.Close()

	h.Send("device", Event{Name: "msg"})
	if ev := next(t, second); ev.Name != "msg" {
		t.Errorf("событие: %+v", ev)
	}
}

// Доставки идут из разных горутин: hub обязан это переживать.
func TestConcurrent(t *testing.T) {
	h := New()
	done := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			device := "device" + strconv.Itoa(n%2)
			for {
				select {
				case <-done:
					return
				default:
				}
				h.Send(device, Event{Name: "msg"})
				h.Open(device)
			}
		}(i)
	}
	// Читатель, чтобы буфер не переполнялся мгновенно.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s := h.Open("device0")
		for {
			select {
			case <-done:
				return
			case <-s.Events():
			case <-s.Done():
				s = h.Open("device0")
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
	h.CloseAll()
}
