package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// crockford — алфавит ULID (docs/crypto.md, «Идентификаторы»).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func nowMillis() int64 { return time.Now().UnixMilli() }

// ulid собирает ULID с заданным временем: первые десять символов —
// 48 бит миллисекунд, остальные шестнадцать — 80 бит «случайности».
func ulid(ms int64, seed byte) string {
	out := make([]byte, 26)
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&31]
		ms >>= 5
	}
	for i := 10; i < 26; i++ {
		out[i] = crockford[(int(seed)+i)%32]
	}
	return string(out)
}

// message — тело POST /api/messages в личный чат. Шифротекст сервер
// не проверяет: ему важна только форма.
func message(id, to string) map[string]any {
	return map[string]any{
		"id":    id,
		"to":    map[string]string{"dm": to},
		"keyId": "dm",
		"iv":    bytesOf(12, 21),
		"ct":    bytesOf(48, 23),
	}
}

// queue — очередь устройства как её видит сервер.
func (e *env) queue(device string) []string {
	e.t.Helper()
	got, err := e.st.Queue(context.Background(), device)
	if err != nil {
		e.t.Fatalf("очередь %s: %v", device, err)
	}
	return got
}

// envelopes разбирает конверты очереди.
func (e *env) envelopes(device string) []envelope {
	e.t.Helper()
	raw := e.queue(device)
	out := make([]envelope, 0, len(raw))
	for _, s := range raw {
		var env envelope
		if err := json.Unmarshal([]byte(s), &env); err != nil {
			e.t.Fatalf("разбор конверта %q: %v", s, err)
		}
		out = append(out, env)
	}
	return out
}

// envelope — конверт в том виде, в каком его видит клиент.
type envelope struct {
	ID string `json:"id"`
	To struct {
		DM   string `json:"dm"`
		Room string `json:"room"`
	} `json:"to"`
	From  string `json:"from"`
	KeyID string `json:"keyId"`
	IV    string `json:"iv"`
	CT    string `json:"ct"`
	TS    int64  `json:"ts"`
}

// Конверт уходит на все устройства обоих собеседников, кроме отправившего
// (ADR-017): мультидевайс без отдельной логики.
func TestFanout(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	m2 := e.addDevice(marta, deviceOf(2))
	petya, p1 := e.join("petya", 3)
	p2 := e.addDevice(petya, deviceOf(4))

	id := ulid(nowMillis(), 5)
	rec := e.do(http.MethodPost, "/api/messages", message(id, "petya"), with(marta), withDevice(m1))
	expect(t, rec, http.StatusAccepted, "")
	var accepted struct {
		ID string `json:"id"`
		TS int64  `json:"ts"`
	}
	decodeBody(t, rec, &accepted)
	if accepted.ID != id || accepted.TS == 0 {
		t.Errorf("ответ: %+v", accepted)
	}

	if got := e.queue(m1); len(got) != 0 {
		t.Errorf("эхо отправившему устройству: %v", got)
	}
	for _, device := range []string{m2, p1, p2} {
		got := e.envelopes(device)
		if len(got) != 1 {
			t.Fatalf("очередь %s: получено %d конвертов, ожидался 1", device, len(got))
		}
		env := got[0]
		if env.ID != id || env.From != "marta" || env.To.DM != "petya" || env.KeyID != "dm" {
			t.Errorf("конверт для %s: %+v", device, env)
		}
		if env.IV != bytesOf(12, 21) || env.CT != bytesOf(48, 23) {
			t.Errorf("шифротекст изменился: %+v", env)
		}
		if env.TS != accepted.TS {
			t.Errorf("ts: получено %d, ожидалось %d", env.TS, accepted.TS)
		}
	}
}

// from ставит сервер из сессии; поле from в теле запроса не читается
// вовсе (ADR-017, модель угроз).
func TestFromComesFromSession(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	_, p1 := e.join("petya", 2)

	body := message(ulid(nowMillis(), 3), "petya")
	body["from"] = "petya"
	body["ts"] = 1
	expect(t, e.do(http.MethodPost, "/api/messages", body, with(marta), withDevice(m1)), http.StatusAccepted, "")

	got := e.envelopes(p1)
	if len(got) != 1 {
		t.Fatalf("очередь: получено %d конвертов, ожидался 1", len(got))
	}
	if got[0].From != "marta" {
		t.Errorf("from: получено %q, ожидалось \"marta\"", got[0].From)
	}
	if got[0].TS == 1 {
		t.Errorf("ts взят из тела запроса: %d", got[0].TS)
	}
}

// ACK удаляет строки очереди только своего устройства.
func TestAck(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	p2 := e.addDevice(petya, deviceOf(3))

	first := ulid(nowMillis(), 4)
	second := ulid(nowMillis()+1, 5)
	for _, id := range []string{first, second} {
		expect(t, e.do(http.MethodPost, "/api/messages", message(id, "petya"), with(marta), withDevice(m1)),
			http.StatusAccepted, "")
	}

	ack := map[string]any{"ids": []string{first}}
	expect(t, e.do(http.MethodPost, "/api/ack", ack, with(petya), withDevice(p1)), http.StatusNoContent, "")

	left := e.envelopes(p1)
	if len(left) != 1 || left[0].ID != second {
		t.Errorf("очередь p1 после ack: %+v", left)
	}
	if got := e.queue(p2); len(got) != 2 {
		t.Errorf("очередь p2: получено %d конвертов, ожидалось 2", len(got))
	}
	// Чужие идентификаторы и повторный ack ничего не ломают.
	expect(t, e.do(http.MethodPost, "/api/ack", map[string]any{"ids": []string{first, second}},
		with(petya), withDevice(p1)), http.StatusNoContent, "")
	if got := e.queue(p1); len(got) != 0 {
		t.Errorf("очередь p1: получено %d конвертов, ожидалось 0", len(got))
	}
	if got := e.queue(p2); len(got) != 2 {
		t.Errorf("очередь p2 после ack чужого устройства: получено %d", len(got))
	}
}

func TestAckLimit(t *testing.T) {
	e := newEnv(t)
	c, device := e.join("marta", 1)

	ids := make([]string, 500)
	for i := range ids {
		ids[i] = ulid(nowMillis(), byte(i))
	}
	expect(t, e.do(http.MethodPost, "/api/ack", map[string]any{"ids": ids}, with(c), withDevice(device)),
		http.StatusNoContent, "")

	rec := e.do(http.MethodPost, "/api/ack", map[string]any{"ids": append(ids, ulid(nowMillis(), 9))},
		with(c), withDevice(device))
	expect(t, rec, http.StatusBadRequest, "invalid")
	var field struct {
		Field string `json:"field"`
	}
	decodeBody(t, rec, &field)
	if field.Field != "ids" {
		t.Errorf("field: получено %q, ожидалось \"ids\"", field.Field)
	}
}

// Часы клиента врут в обе стороны одинаково плохо (ADR-017).
func TestClockSkew(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	e.join("petya", 2)

	minute := int64(60 * 1000)
	for _, shift := range []int64{-6 * minute, 6 * minute, -24 * 60 * minute, 24 * 60 * minute} {
		body := message(ulid(nowMillis()+shift, 3), "petya")
		expect(t, e.do(http.MethodPost, "/api/messages", body, with(marta), withDevice(m1)),
			http.StatusBadRequest, "clock_skew")
	}
	// В пределах пяти минут — принимается.
	for _, shift := range []int64{-4 * minute, 4 * minute} {
		body := message(ulid(nowMillis()+shift, 4), "petya")
		expect(t, e.do(http.MethodPost, "/api/messages", body, with(marta), withDevice(m1)),
			http.StatusAccepted, "")
	}
}

// Строки contacts заводятся в обе стороны при первом сообщении (ADR-019):
// новое устройство видит список чатов без истории.
func TestContactsFromFirstMessage(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, _ := e.join("petya", 2)

	if got := e.contacts(marta); len(got) != 0 {
		t.Fatalf("контакты до первого сообщения: %+v", got)
	}
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 3), "petya"),
		with(marta), withDevice(m1)), http.StatusAccepted, "")

	mine := e.contacts(marta)
	if len(mine) != 1 || mine[0].Nick != "petya" || mine[0].CreatedAt == 0 {
		t.Errorf("контакты marta: %+v", mine)
	}
	theirs := e.contacts(petya)
	if len(theirs) != 1 || theirs[0].Nick != "marta" {
		t.Errorf("контакты petya: %+v", theirs)
	}
	if len(theirs[0].PublicKey) == 0 {
		t.Errorf("в контакте нет публичного ключа: %+v", theirs[0])
	}

	// Второе сообщение ничего не удваивает.
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 4), "petya"),
		with(marta), withDevice(m1)), http.StatusAccepted, "")
	if got := e.contacts(marta); len(got) != 1 {
		t.Errorf("контакты marta после второго сообщения: %+v", got)
	}
}

// Повтор POST с тем же id не ломает запрос: сервер историю идентификаторов
// не хранит, склеивает клиент (ADR-017).
func TestRepeatedMessageID(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	_, p1 := e.join("petya", 2)

	id := ulid(nowMillis(), 3)
	for i := 0; i < 3; i++ {
		expect(t, e.do(http.MethodPost, "/api/messages", message(id, "petya"), with(marta), withDevice(m1)),
			http.StatusAccepted, "")
	}
	if got := e.queue(p1); len(got) != 1 {
		t.Errorf("очередь: получено %d конвертов, ожидался 1", len(got))
	}
}

func TestMessageRejects(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
		status int
		code   string
		field  string
	}{
		{"id не ulid", func(m map[string]any) { m["id"] = "не ulid" }, http.StatusBadRequest, "invalid", "id"},
		{"строчный ulid", func(m map[string]any) {
			m["id"] = "01hqzz0000zzzzzzzzzzzzzzzz"
		}, http.StatusBadRequest, "invalid", "id"},
		{"буква вне алфавита", func(m map[string]any) {
			m["id"] = "0" + "I" + ulid(nowMillis(), 1)[2:]
		}, http.StatusBadRequest, "invalid", "id"},
		{"нет адресата", func(m map[string]any) { delete(m, "to") }, http.StatusBadRequest, "invalid", "to"},
		{"оба адресата", func(m map[string]any) {
			m["to"] = map[string]string{"dm": "petya", "room": bytesOf(16, 1)}
		}, http.StatusBadRequest, "invalid", "to"},
		{"кривой ник", func(m map[string]any) {
			m["to"] = map[string]string{"dm": "МАРТА"}
		}, http.StatusBadRequest, "invalid", "to"},
		{"чужой keyId в личном чате", func(m map[string]any) {
			m["keyId"] = bytesOf(16, 1)
		}, http.StatusBadRequest, "invalid", "keyId"},
		{"iv не 12 байт", func(m map[string]any) { m["iv"] = bytesOf(16, 21) }, http.StatusBadRequest, "invalid", "iv"},
		{"короткий ct", func(m map[string]any) { m["ct"] = bytesOf(8, 23) }, http.StatusBadRequest, "invalid", "ct"},
		{"ct не base64url", func(m map[string]any) { m["ct"] = "!!!" }, http.StatusBadRequest, "invalid", "ct"},
		{"неизвестный ник", func(m map[string]any) {
			m["to"] = map[string]string{"dm": "kolya"}
		}, http.StatusNotFound, "unknown_user", ""},
		{"себе", func(m map[string]any) {
			m["to"] = map[string]string{"dm": "marta"}
		}, http.StatusBadRequest, "self", ""},
		// Комнаты — этап 3; членства нет ни у кого.
		{"в комнату", func(m map[string]any) {
			m["to"] = map[string]string{"room": bytesOf(16, 1)}
			m["keyId"] = bytesOf(16, 2)
		}, http.StatusForbidden, "not_member", ""},
		{"кривой roomId", func(m map[string]any) {
			m["to"] = map[string]string{"room": "нет"}
		}, http.StatusBadRequest, "invalid", "to"},
		{"кривой keyId комнаты", func(m map[string]any) {
			m["to"] = map[string]string{"room": bytesOf(16, 1)}
			m["keyId"] = "dm"
		}, http.StatusBadRequest, "invalid", "keyId"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			marta, m1 := e.join("marta", 1)
			_, p1 := e.join("petya", 2)

			body := message(ulid(nowMillis(), 3), "petya")
			c.change(body)
			rec := e.do(http.MethodPost, "/api/messages", body, with(marta), withDevice(m1))
			expect(t, rec, c.status, c.code)
			if c.field != "" {
				var got struct {
					Field string `json:"field"`
				}
				decodeBody(t, rec, &got)
				if got.Field != c.field {
					t.Errorf("field: получено %q, ожидалось %q", got.Field, c.field)
				}
			}
			if got := e.queue(p1); len(got) != 0 {
				t.Errorf("отвергнутое сообщение попало в очередь: %v", got)
			}
		})
	}
}

// Лимит сообщений — 30 в минуту на пользователя, пакет 10 (ADR-021).
func TestMessageRateLimit(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	e.join("petya", 2)

	for i := 0; i < 10; i++ {
		expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), byte(i)), "petya"),
			with(marta), withDevice(m1)), http.StatusAccepted, "")
	}
	rec := e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 11), "petya"), with(marta), withDevice(m1))
	expect(t, rec, http.StatusTooManyRequests, "rate_limited")
	// Токен набегает раз в две секунды: пакет кончился, ждать до двух.
	after, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || after < 1 || after > 2 {
		t.Errorf("Retry-After: получено %q", rec.Header().Get("Retry-After"))
	}

	// Лимит на пользователе, а не на устройстве.
	m2 := e.addDevice(marta, deviceOf(3))
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 12), "petya"),
		with(marta), withDevice(m2)), http.StatusTooManyRequests, "rate_limited")

	// Другому пользователю чужой лимит не мешает.
	petya, p1 := e.join("kolya", 4)
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 13), "marta"),
		with(petya), withDevice(p1)), http.StatusAccepted, "")
}
