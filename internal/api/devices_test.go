package api_test

import (
	"net/http"
	"testing"
)

// deviceOf — идентификатор устройства: 16 байт base64url (docs/crypto.md).
func deviceOf(seed byte) string { return bytesOf(16, seed) }

// join регистрирует аккаунт и его устройство, отдаёт cookie и id.
func (e *env) join(nick string, seed byte) (*http.Cookie, string) {
	e.t.Helper()
	c := e.signUp(nick)
	return c, e.addDevice(c, deviceOf(seed))
}

// addDevice регистрирует устройство под уже открытой сессией.
func (e *env) addDevice(c *http.Cookie, id string) string {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/devices", map[string]any{"id": id}, with(c))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		e.t.Fatalf("регистрация устройства: %d (%s)", rec.Code, rec.Body.String())
	}
	return id
}

func TestDevices(t *testing.T) {
	e := newEnv(t)
	c := e.signUp("marta")
	id := deviceOf(1)

	first := e.do(http.MethodPost, "/api/devices", map[string]any{"id": id}, with(c))
	expect(t, first, http.StatusCreated, "")
	var created struct {
		ID string `json:"id"`
	}
	decodeBody(t, first, &created)
	if created.ID != id {
		t.Errorf("id в ответе: получено %q, ожидалось %q", created.ID, id)
	}

	// Повтор — то же устройство того же пользователя.
	expect(t, e.do(http.MethodPost, "/api/devices", map[string]any{"id": id}, with(c)), http.StatusOK, "")

	rec := e.do(http.MethodGet, "/api/devices", nil, with(c))
	expect(t, rec, http.StatusOK, "")
	var list []struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"createdAt"`
		LastSeen  int64  `json:"lastSeen"`
		HasPush   bool   `json:"hasPush"`
		Current   bool   `json:"current"`
	}
	decodeBody(t, rec, &list)
	if len(list) != 1 {
		t.Fatalf("устройств: получено %d, ожидалось 1", len(list))
	}
	if list[0].ID != id || list[0].CreatedAt == 0 || list[0].LastSeen == 0 || list[0].HasPush || !list[0].Current {
		t.Errorf("устройство: %+v", list[0])
	}

	// Второе устройство — своя сессия, свой вход. current у каждой сессии
	// своё: сессия привязана к устройству (ADR-021).
	login := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)})
	expect(t, login, http.StatusOK, "")
	second := e.cookie(login)
	e.addDevice(second, deviceOf(2))

	rec = e.do(http.MethodGet, "/api/devices", nil, with(c))
	decodeBody(t, rec, &list)
	if len(list) != 2 {
		t.Fatalf("устройств: получено %d, ожидалось 2", len(list))
	}
	if !list[0].Current || list[1].Current {
		t.Errorf("текущее устройство первой сессии: %+v", list)
	}
	rec = e.do(http.MethodGet, "/api/devices", nil, with(second))
	decodeBody(t, rec, &list)
	if list[0].Current || !list[1].Current {
		t.Errorf("текущее устройство второй сессии: %+v", list)
	}

	// Push-подписка — этап 4: пути ещё нет, а неизвестный путь отвечает
	// 404 not_found (ADR-026).
	expect(t, e.do(http.MethodPut, "/api/devices/"+id+"/push", map[string]any{}, with(c)),
		http.StatusNotFound, "not_found")
}

// Занятый чужим идентификатор — 409: клиент берёт новый (ADR-017).
func TestDeviceConflict(t *testing.T) {
	e := newEnv(t)
	marta, id := e.join("marta", 1)
	petya := e.signUp("petya")

	expect(t, e.do(http.MethodPost, "/api/devices", map[string]any{"id": id}, with(petya)),
		http.StatusConflict, "device_conflict")

	// Устройство осталось за прежним владельцем.
	rec := e.do(http.MethodGet, "/api/devices", nil, with(marta))
	var mine []struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &mine)
	if len(mine) != 1 || mine[0].ID != id {
		t.Errorf("устройства marta: %+v", mine)
	}
	rec = e.do(http.MethodGet, "/api/devices", nil, with(petya))
	var theirs []struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &theirs)
	if len(theirs) != 0 {
		t.Errorf("устройства petya: %+v", theirs)
	}
}

func TestDeviceIDForm(t *testing.T) {
	e := newEnv(t)
	c := e.signUp("marta")

	for _, id := range []any{"", "короткий", bytesOf(8, 1), bytesOf(32, 1), 42} {
		rec := e.do(http.MethodPost, "/api/devices", map[string]any{"id": id}, with(c))
		if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
			t.Errorf("id %v принят: %d", id, rec.Code)
		}
	}
}

// X-Device чужого пользователя — 403 unknown_device на всех маршрутах,
// где устройство важно (docs/protocol.md, «Общие правила»).
func TestForeignDevice(t *testing.T) {
	e := newEnv(t)
	_, martaDevice := e.join("marta", 1)
	petya, petyaDevice := e.join("petya", 2)

	body := message(ulid(nowMillis(), 3), "marta")
	expect(t, e.do(http.MethodPost, "/api/messages", body, with(petya), withDevice(martaDevice)),
		http.StatusForbidden, "unknown_device")
	expect(t, e.do(http.MethodPost, "/api/ack", map[string]any{"ids": []string{}}, with(petya), withDevice(martaDevice)),
		http.StatusForbidden, "unknown_device")

	// Заголовка нет вовсе или в нём мусор — тот же ответ.
	expect(t, e.do(http.MethodPost, "/api/messages", body, with(petya)),
		http.StatusForbidden, "unknown_device")
	expect(t, e.do(http.MethodPost, "/api/messages", body, with(petya), withDevice("мусор")),
		http.StatusForbidden, "unknown_device")
	expect(t, e.do(http.MethodPost, "/api/messages", body, with(petya), withDevice(deviceOf(9))),
		http.StatusForbidden, "unknown_device")

	// Со своим устройством — обычная отправка.
	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 4), "marta"),
		with(petya), withDevice(petyaDevice)), http.StatusAccepted, "")

	// Комнаты: заголовок здесь необязателен — он всего лишь просит не слать
	// событие отправившему устройству, — но принадлежность проверяется
	// та же (docs/protocol.md, «Общие правила», «Комнаты»).
	room := map[string]any{
		"id":    roomIDOf(40),
		"name":  "общая",
		"keyId": keyID(40),
		"keys":  keysFor([]string{"petya"}, 40),
	}
	expect(t, e.do(http.MethodPost, "/api/rooms", room, with(petya), withDevice(martaDevice)),
		http.StatusForbidden, "unknown_device")
	expect(t, e.do(http.MethodPost, "/api/rooms", room, with(petya), withDevice("мусор")),
		http.StatusForbidden, "unknown_device")
	expect(t, e.do(http.MethodPost, "/api/rooms", room, with(petya), withDevice(deviceOf(9))),
		http.StatusForbidden, "unknown_device")
	// Отказ ничего не создал: идентификатор комнаты свободен.
	expect(t, e.do(http.MethodPost, "/api/rooms", room, with(petya)), http.StatusCreated, "")

	leave := "/api/rooms/" + roomIDOf(40) + "/leave"
	expect(t, e.do(http.MethodPost, leave, nil, with(petya), withDevice(martaDevice)),
		http.StatusForbidden, "unknown_device")
	expect(t, e.do(http.MethodPost, leave, nil, with(petya), withDevice("мусор")),
		http.StatusForbidden, "unknown_device")
	// Отказ ничего не изменил: из комнаты никто не вышел.
	if got := e.room(petya, roomIDOf(40)); got == nil {
		t.Fatal("комната пропала после отказа по устройству")
	}
	expect(t, e.do(http.MethodPost, leave, nil, with(petya), withDevice(petyaDevice)),
		http.StatusNoContent, "")
}

// Удаление устройства уносит очередь и сессии устройства.
func TestDeleteDevice(t *testing.T) {
	e := newEnv(t)
	marta, martaDevice := e.join("marta", 1)
	petya, petyaDevice := e.join("petya", 2)

	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 3), "petya"),
		with(marta), withDevice(martaDevice)), http.StatusAccepted, "")
	if got := e.queue(petyaDevice); len(got) != 1 {
		t.Fatalf("очередь petya: получено %d конвертов, ожидался 1", len(got))
	}

	// Чужое устройство удалить нельзя — и это не ошибка.
	expect(t, e.do(http.MethodDelete, "/api/devices/"+petyaDevice, nil, with(marta)), http.StatusNoContent, "")
	if got := e.queue(petyaDevice); len(got) != 1 {
		t.Errorf("очередь petya после чужого удаления: получено %d конвертов", len(got))
	}

	expect(t, e.do(http.MethodDelete, "/api/devices/"+petyaDevice, nil, with(petya)), http.StatusNoContent, "")
	if got := e.queue(petyaDevice); len(got) != 0 {
		t.Errorf("очередь после удаления устройства: получено %d конвертов, ожидалось 0", len(got))
	}
	// Сессия, привязанная к устройству, ушла каскадом.
	expect(t, e.do(http.MethodGet, "/api/me", nil, with(petya)), http.StatusUnauthorized, "unauthenticated")
}
