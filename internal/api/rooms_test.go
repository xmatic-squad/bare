package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// roomBody — тип Room из docs/protocol.md, как его видит клиент.
type roomBody struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Owner      string   `json:"owner"`
	Members    []string `json:"members"`
	CreatedAt  int64    `json:"createdAt"`
	Key        *keyBody `json:"key"`
	NeedsRekey bool     `json:"needsRekey"`
}

type keyBody struct {
	KeyID string `json:"keyId"`
	From  string `json:"from"`
	IV    string `json:"iv"`
	CT    string `json:"ct"`
}

// keyID — идентификатор ключа комнаты: 16 байт base64url (docs/crypto.md).
func keyID(seed byte) string { return bytesOf(16, seed) }

// roomIDOf — идентификатор комнаты: его генерирует клиент (ADR-037).
func roomIDOf(seed byte) string { return bytesOf(16, seed+100) }

// wrapped — завёрнутый ключ участнику. Порядковый номер входит в «шифротекст»:
// по нему видно, что каждому ушёл его собственный ключ. Содержимое сервер
// не проверяет и проверить не может.
func wrapped(to string, seed byte, nth int) map[string]any {
	return map[string]any{"to": to, "iv": ivOf(seed, nth), "ct": ctOf(seed, nth)}
}

func ivOf(seed byte, nth int) string { return bytesOf(12, seed+byte(nth)) }
func ctOf(seed byte, nth int) string { return bytesOf(48, seed+byte(nth)) }
func keysFor(to []string, seed byte) []any {
	out := make([]any, 0, len(to))
	for i, nick := range to {
		out = append(out, wrapped(nick, seed, i))
	}
	return out
}

// makeRoom заводит комнату: состав — один создатель, ключ ровно один.
func (e *env) makeRoom(c *http.Cookie, owner, name string, seed byte, opts ...func(*http.Request)) roomBody {
	e.t.Helper()
	body := map[string]any{
		"id":    roomIDOf(seed),
		"name":  name,
		"keyId": keyID(seed),
		"keys":  keysFor([]string{owner}, seed),
	}
	rec := e.do(http.MethodPost, "/api/rooms", body, append([]func(*http.Request){with(c)}, opts...)...)
	expect(e.t, rec, http.StatusCreated, "")
	var room roomBody
	decodeBody(e.t, rec, &room)
	return room
}

// changeMembers — смена состава и rekey одним запросом: to — итоговый
// состав, которому заворачивается новый ключ.
func (e *env) changeMembers(c *http.Cookie, id string, add, remove, to []string, seed byte) *httptest.ResponseRecorder {
	e.t.Helper()
	body := map[string]any{
		"add":    add,
		"remove": remove,
		"keyId":  keyID(seed),
		"keys":   keysFor(to, seed),
	}
	return e.do(http.MethodPost, "/api/rooms/"+id+"/members", body, with(c))
}

func (e *env) rooms(c *http.Cookie) []roomBody {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/rooms", nil, with(c))
	expect(e.t, rec, http.StatusOK, "")
	var out []roomBody
	decodeBody(e.t, rec, &out)
	return out
}

// room — комната из списка; nil, если её там нет.
func (e *env) room(c *http.Cookie, id string) *roomBody {
	e.t.Helper()
	for _, got := range e.rooms(c) {
		if got.ID == id {
			room := got
			return &room
		}
	}
	return nil
}

// roomMessage — тело POST /api/messages в комнату.
func roomMessage(id, room, key string) map[string]any {
	return map[string]any{
		"id":    id,
		"to":    map[string]string{"room": room},
		"keyId": key,
		"iv":    bytesOf(12, 21),
		"ct":    bytesOf(48, 23),
	}
}

// nicks — состав как множество: joined_at у сервера в миллисекундах,
// и две операции подряд попадают в одну и ту же. Порядок по joined_at
// проверяет TestMembersJoinOrder, где операции разведены во времени.
func nicks(members []string) string {
	sorted := append([]string(nil), members...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// tick разводит операции по разным миллисекундам.
func tick() { time.Sleep(2 * time.Millisecond) }

// field достаёт поле, на котором остановилась валидация.
func field(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Field string `json:"field"`
	}
	decodeBody(t, rec, &body)
	return body.Field
}

func TestCreateRoom(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)

	room := e.makeRoom(marta, "marta", "общая", 40)
	if room.ID != roomIDOf(40) {
		t.Errorf("id комнаты: получено %q, ожидалось %q", room.ID, roomIDOf(40))
	}
	if room.Name != "общая" || room.Owner != "marta" || room.CreatedAt == 0 {
		t.Errorf("комната: %+v", room)
	}
	if len(room.Members) != 1 || room.Members[0] != "marta" {
		t.Errorf("состав: %v", room.Members)
	}
	if room.NeedsRekey {
		t.Error("needsRekey в ответе на создание")
	}
	if room.Key == nil || room.Key.KeyID != keyID(40) || room.Key.From != "marta" ||
		room.Key.IV != ivOf(40, 0) || room.Key.CT != ctOf(40, 0) {
		t.Errorf("ключ: %+v", room.Key)
	}

	// Та же комната приходит списком, с тем же ключом.
	list := e.rooms(marta)
	if len(list) != 1 {
		t.Fatalf("комнат: получено %d, ожидалась 1", len(list))
	}
	if list[0].ID != room.ID || list[0].Key == nil || list[0].Key.CT != ctOf(40, 0) {
		t.Errorf("список комнат: %+v", list[0])
	}

	// Вторая комната — другой идентификатор.
	other := e.makeRoom(marta, "marta", "вторая", 60)
	if other.ID == room.ID {
		t.Error("идентификаторы комнат совпали")
	}
	if got := e.rooms(marta); len(got) != 2 {
		t.Errorf("комнат: получено %d, ожидалось 2", len(got))
	}
	// Чужому комната не видна.
	petya, _ := e.join("petya", 2)
	if got := e.rooms(petya); len(got) != 0 {
		t.Errorf("чужие комнаты: %+v", got)
	}
}

// Занятый идентификатор комнаты не присоединяет к чужой и не перезаписывает
// свою: клиент берёт новый (ADR-037).
func TestCreateRoomConflict(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	petya, _ := e.join("petya", 2)

	room := e.makeRoom(marta, "marta", "общая", 40)

	taken := func(c *http.Cookie, owner string) {
		t.Helper()
		body := map[string]any{
			"id":    room.ID,
			"name":  "чужая",
			"keyId": keyID(60),
			"keys":  keysFor([]string{owner}, 60),
		}
		expect(t, e.do(http.MethodPost, "/api/rooms", body, with(c)), http.StatusConflict, "room_conflict")
	}
	taken(petya, "petya")
	taken(marta, "marta")

	if got := e.rooms(petya); len(got) != 0 {
		t.Errorf("занятый id присоединил к чужой комнате: %+v", got)
	}
	list := e.rooms(marta)
	if len(list) != 1 || list[0].Name != "общая" || list[0].Key == nil || list[0].Key.KeyID != keyID(40) {
		t.Errorf("занятый id тронул существующую комнату: %+v", list)
	}
}

func TestCreateRoomRejects(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
		status int
		code   string
		field  string
	}{
		{"нет id", func(m map[string]any) { delete(m, "id") }, http.StatusBadRequest, "invalid", "id"},
		{"кривой id", func(m map[string]any) { m["id"] = "room-1" }, http.StatusBadRequest, "invalid", "id"},
		{"пустое имя", func(m map[string]any) { m["name"] = "" }, http.StatusBadRequest, "invalid", "name"},
		{"имя длиннее 64", func(m map[string]any) {
			m["name"] = strings.Repeat("я", 65)
		}, http.StatusBadRequest, "invalid", "name"},
		// Имя уходит в заголовок системного уведомления (ADR-045):
		// ни перевода строки, ни разворота текста в нём быть не должно.
		{"имя с переводом строки", func(m map[string]any) {
			m["name"] = "общая\nсрочно: перезагрузите телефон"
		}, http.StatusBadRequest, "invalid", "name"},
		{"имя с bidi", func(m map[string]any) {
			m["name"] = "общая\u202eяандекс"
		}, http.StatusBadRequest, "invalid", "name"},
		{"имя из пробелов", func(m map[string]any) {
			m["name"] = "   "
		}, http.StatusBadRequest, "invalid", "name"},
		{"кривой keyId", func(m map[string]any) { m["keyId"] = "dm" }, http.StatusBadRequest, "invalid", "keyId"},
		{"нет ключа", func(m map[string]any) { m["keys"] = []any{} }, http.StatusBadRequest, "keys_mismatch", ""},
		{"ключ чужому", func(m map[string]any) {
			m["keys"] = keysFor([]string{"petya"}, 40)
		}, http.StatusBadRequest, "keys_mismatch", ""},
		{"два ключа", func(m map[string]any) {
			m["keys"] = keysFor([]string{"marta", "petya"}, 40)
		}, http.StatusBadRequest, "keys_mismatch", ""},
		{"кривой iv", func(m map[string]any) {
			m["keys"] = []any{map[string]any{"to": "marta", "iv": bytesOf(16, 1), "ct": ctOf(40, 0)}}
		}, http.StatusBadRequest, "invalid", "keys"},
		{"короткий ct", func(m map[string]any) {
			m["keys"] = []any{map[string]any{"to": "marta", "iv": ivOf(40, 0), "ct": bytesOf(8, 1)}}
		}, http.StatusBadRequest, "invalid", "keys"},
		{"кривой ник в ключе", func(m map[string]any) {
			m["keys"] = []any{map[string]any{"to": "МАРТА", "iv": ivOf(40, 0), "ct": ctOf(40, 0)}}
		}, http.StatusBadRequest, "invalid", "keys"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			marta, _ := e.join("marta", 1)
			e.join("petya", 2)

			body := map[string]any{
				"id":    roomIDOf(40),
				"name":  "общая",
				"keyId": keyID(40),
				"keys":  keysFor([]string{"marta"}, 40),
			}
			c.change(body)
			rec := e.do(http.MethodPost, "/api/rooms", body, with(marta))
			expect(t, rec, c.status, c.code)
			if c.field != "" {
				if got := field(t, rec); got != c.field {
					t.Errorf("field: получено %q, ожидалось %q", got, c.field)
				}
			}
			if got := e.rooms(marta); len(got) != 0 {
				t.Errorf("отвергнутое создание завело комнату: %+v", got)
			}
		})
	}
}

// Смена состава и rekey — один запрос: у каждого участника свой ключ
// (ADR-018).
func TestMembersAdd(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	petya, _ := e.join("petya", 2)
	kolya, _ := e.join("kolya", 3)

	room := e.makeRoom(marta, "marta", "общая", 40)
	rec := e.changeMembers(marta, room.ID, []string{"petya", "kolya"}, nil,
		[]string{"marta", "petya", "kolya"}, 60)
	expect(t, rec, http.StatusOK, "")

	var got roomBody
	decodeBody(t, rec, &got)
	want := []string{"marta", "petya", "kolya"}
	if nicks(got.Members) != nicks(want) {
		t.Errorf("состав: получено %v, ожидалось %v", got.Members, want)
	}
	if got.Key == nil || got.Key.KeyID != keyID(60) || got.Key.CT != ctOf(60, 0) {
		t.Errorf("ключ владельца в ответе: %+v", got.Key)
	}

	// Каждый видит комнату со своим ключом.
	for i, c := range []*http.Cookie{marta, petya, kolya} {
		list := e.rooms(c)
		if len(list) != 1 {
			t.Fatalf("комнат у %d: получено %d, ожидалась 1", i, len(list))
		}
		if list[0].Owner != "marta" || nicks(list[0].Members) != nicks(want) {
			t.Errorf("комната у %d: %+v", i, list[0])
		}
		if list[0].Key == nil || list[0].Key.KeyID != keyID(60) || list[0].Key.From != "marta" ||
			list[0].Key.CT != ctOf(60, i) {
			t.Errorf("ключ у %d: %+v", i, list[0].Key)
		}
	}

	// Повторное добавление участника ничего не меняет.
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, want, 80), http.StatusOK, "")
	if got := e.room(marta, room.ID); nicks(got.Members) != nicks(want) {
		t.Errorf("состав после повторного добавления: %v", got.Members)
	}
}

// Состав идёт по joined_at: кто вступил раньше, тот и раньше в списке
// (docs/protocol.md, «Типы»). Повторное добавление участника его не двигает.
func TestMembersJoinOrder(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	e.join("petya", 2)
	e.join("kolya", 3)

	room := e.makeRoom(marta, "marta", "общая", 40)
	tick()
	expect(t, e.changeMembers(marta, room.ID, []string{"kolya"}, nil, []string{"marta", "kolya"}, 60),
		http.StatusOK, "")
	tick()
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "kolya", "petya"}, 80),
		http.StatusOK, "")
	tick()
	expect(t, e.changeMembers(marta, room.ID, []string{"kolya"}, nil, []string{"marta", "kolya", "petya"}, 100),
		http.StatusOK, "")

	want := "marta,kolya,petya"
	if got := e.room(marta, room.ID); strings.Join(got.Members, ",") != want {
		t.Errorf("состав: получено %v, ожидалось %q", got.Members, want)
	}
}

// Множество keys[].to обязано равняться итоговому составу; отказ не меняет
// ни состава, ни ключей (docs/protocol.md, «Комнаты»).
func TestMembersKeysMismatch(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	e.join("petya", 2)
	kolya, _ := e.join("kolya", 3)
	room := e.makeRoom(marta, "marta", "общая", 40)

	cases := []struct {
		name string
		add  []string
		to   []string
	}{
		{"ключ не всем", []string{"petya"}, []string{"marta"}},
		{"ключ лишнему", []string{"petya"}, []string{"marta", "petya", "kolya"}},
		{"ключ вместо участника", []string{"petya"}, []string{"marta", "kolya"}},
		{"ключей нет вовсе", []string{"petya"}, nil},
		{"чистый rekey без себя", nil, []string{"petya"}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.changeMembers(marta, room.ID, c.add, nil, c.to, byte(60+i*10))
			expect(t, rec, http.StatusBadRequest, "keys_mismatch")
		})
	}

	// Ни один отказ не изменил ни состава, ни ключа.
	got := e.room(marta, room.ID)
	if len(got.Members) != 1 || got.Members[0] != "marta" {
		t.Errorf("состав после отказов: %v", got.Members)
	}
	if got.Key == nil || got.Key.KeyID != keyID(40) {
		t.Errorf("ключ после отказов: %+v", got.Key)
	}
	if list := e.rooms(kolya); len(list) != 0 {
		t.Errorf("комната у постороннего: %+v", list)
	}
	// Комната по-прежнему работает на старом ключе.
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 5), room.ID, keyID(40)),
		with(marta), withDevice(m1)), http.StatusAccepted, "")
}

// keyId обязан быть новым для комнаты: повтор — 409, и запрос не проходит
// целиком (docs/protocol.md, «Комнаты»).
func TestMembersKeyExists(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	e.join("petya", 2)
	room := e.makeRoom(marta, "marta", "общая", 40)

	// Тот же keyId, что у ключа при создании.
	rec := e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 40)
	expect(t, rec, http.StatusConflict, "key_exists")

	// Состав не изменился: проверки идут до записи.
	got := e.room(marta, room.ID)
	if len(got.Members) != 1 || got.Members[0] != "marta" {
		t.Errorf("состав после key_exists: %v", got.Members)
	}

	// Новый keyId проходит, а повтор уже его — снова 409.
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")
	expect(t, e.changeMembers(marta, room.ID, nil, nil, []string{"marta", "petya"}, 60),
		http.StatusConflict, "key_exists")
}

func TestMembersRejects(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	petya, _ := e.join("petya", 2)
	e.join("kolya", 3)
	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")

	// Не владелец — 403 not_owner, хоть участник, хоть посторонний.
	expect(t, e.changeMembers(petya, room.ID, nil, nil, []string{"marta", "petya"}, 80),
		http.StatusForbidden, "not_owner")
	kolya, _ := e.join("kolya2", 4)
	expect(t, e.changeMembers(kolya, room.ID, nil, nil, []string{"marta", "petya"}, 80),
		http.StatusForbidden, "not_owner")
	// Несуществующая комната неотличима от чужой.
	expect(t, e.changeMembers(marta, bytesOf(16, 9), nil, nil, []string{"marta"}, 80),
		http.StatusForbidden, "not_owner")
	expect(t, e.changeMembers(marta, "мусор", nil, nil, []string{"marta"}, 80),
		http.StatusForbidden, "not_owner")

	// Владельца убрать нельзя.
	expect(t, e.changeMembers(marta, room.ID, nil, []string{"marta"}, []string{"petya"}, 80),
		http.StatusBadRequest, "owner")

	// Несуществующий ник в add — unknown_user; ник не по форме — invalid
	// с полем add, как и в remove (ADR-043).
	expect(t, e.changeMembers(marta, room.ID, []string{"nikogo"}, nil, []string{"marta", "petya", "nikogo"}, 80),
		http.StatusNotFound, "unknown_user")
	rec := e.changeMembers(marta, room.ID, []string{"МАРТА"}, nil, []string{"marta", "petya"}, 80)
	expect(t, rec, http.StatusBadRequest, "invalid")
	if got := field(t, rec); got != "add" {
		t.Errorf("field: получено %q, ожидалось \"add\"", got)
	}

	// Убрать можно только участника.
	rec = e.changeMembers(marta, room.ID, nil, []string{"kolya"}, []string{"marta", "petya"}, 80)
	expect(t, rec, http.StatusBadRequest, "invalid")
	if got := field(t, rec); got != "remove" {
		t.Errorf("field: получено %q, ожидалось \"remove\"", got)
	}

	// Один ник в add и remove сразу — противоречие.
	rec = e.changeMembers(marta, room.ID, []string{"kolya"}, []string{"kolya"}, []string{"marta", "petya"}, 80)
	expect(t, rec, http.StatusBadRequest, "invalid")

	// Ни один отказ не тронул состав.
	got := e.room(marta, room.ID)
	if nicks(got.Members) != "marta,petya" {
		t.Errorf("состав после отказов: %v", got.Members)
	}
}

// Убранный участник теряет комнату, ключи и доставку.
func TestMembersRemove(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")

	expect(t, e.changeMembers(marta, room.ID, nil, []string{"petya"}, []string{"marta"}, 80),
		http.StatusOK, "")

	if got := e.rooms(petya); len(got) != 0 {
		t.Errorf("комната у убранного: %+v", got)
	}
	if got := e.room(marta, room.ID); len(got.Members) != 1 || got.Members[0] != "marta" {
		t.Errorf("состав: %+v", got)
	}
	// Убранный не пишет и не получает.
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 5), room.ID, keyID(80)),
		with(petya), withDevice(p1)), http.StatusForbidden, "not_member")
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 6), room.ID, keyID(80)),
		with(marta), withDevice(m1)), http.StatusAccepted, "")
	if got := e.queue(p1); len(got) != 0 {
		t.Errorf("убранному пришло сообщение: %v", got)
	}
}

// Выход владельца передаёт владение участнику с наименьшим joined_at
// (ADR-018).
func TestLeaveTransfersOwnership(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	petya, _ := e.join("petya", 2)
	kolya, _ := e.join("kolya", 3)
	room := e.makeRoom(marta, "marta", "общая", 40)
	// Владение получает участник с наименьшим joined_at, поэтому petya
	// и kolya вступают в разные миллисекунды.
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")
	tick()
	expect(t, e.changeMembers(marta, room.ID, []string{"kolya"}, nil, []string{"marta", "petya", "kolya"}, 80),
		http.StatusOK, "")

	expect(t, e.do(http.MethodPost, "/api/rooms/"+room.ID+"/leave", nil, with(marta)), http.StatusNoContent, "")

	if got := e.rooms(marta); len(got) != 0 {
		t.Errorf("комната у вышедшего: %+v", got)
	}
	got := e.room(petya, room.ID)
	if got == nil {
		t.Fatal("комната пропала у оставшихся")
	}
	if got.Owner != "petya" {
		t.Errorf("владелец: получено %q, ожидалось \"petya\"", got.Owner)
	}
	if nicks(got.Members) != "kolya,petya" {
		t.Errorf("состав: %v", got.Members)
	}
	// Ключ оставшихся никуда не делся: rekey делает клиент нового владельца.
	if got.Key == nil || got.Key.KeyID != keyID(80) {
		t.Errorf("ключ после выхода владельца: %+v", got.Key)
	}
	// Новый владелец меняет состав, прежний — уже нет.
	expect(t, e.changeMembers(kolya, room.ID, nil, nil, []string{"petya", "kolya"}, 100),
		http.StatusForbidden, "not_owner")
	expect(t, e.changeMembers(petya, room.ID, nil, nil, []string{"petya", "kolya"}, 100),
		http.StatusOK, "")
}

// Выход последнего участника удаляет комнату (ADR-018).
func TestLeaveDeletesEmptyRoom(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	room := e.makeRoom(marta, "marta", "общая", 40)

	expect(t, e.do(http.MethodPost, "/api/rooms/"+room.ID+"/leave", nil, with(marta)), http.StatusNoContent, "")
	if got := e.rooms(marta); len(got) != 0 {
		t.Errorf("комнаты после выхода: %+v", got)
	}
	// Комнаты больше нет: писать в неё некому.
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 5), room.ID, keyID(40)),
		with(marta), withDevice(m1)), http.StatusForbidden, "not_member")

	// Повторный выход и выход не участника — тот же 204.
	expect(t, e.do(http.MethodPost, "/api/rooms/"+room.ID+"/leave", nil, with(marta)), http.StatusNoContent, "")
	expect(t, e.do(http.MethodPost, "/api/rooms/"+bytesOf(16, 9)+"/leave", nil, with(marta)), http.StatusNoContent, "")
	petya, _ := e.join("petya", 2)
	other := e.makeRoom(petya, "petya", "вторая", 60)
	expect(t, e.do(http.MethodPost, "/api/rooms/"+other.ID+"/leave", nil, with(marta)), http.StatusNoContent, "")
	if got := e.rooms(petya); len(got) != 1 {
		t.Errorf("чужой выход тронул комнату: %+v", got)
	}
}

// DELETE /api/rooms/{id} — только владелец; комната исчезает у всех.
func TestDeleteRoom(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, _ := e.join("petya", 2)
	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")

	expect(t, e.do(http.MethodDelete, "/api/rooms/"+room.ID, nil, with(petya)), http.StatusForbidden, "not_owner")
	expect(t, e.do(http.MethodDelete, "/api/rooms/"+bytesOf(16, 9), nil, with(marta)), http.StatusForbidden, "not_owner")
	if got := e.rooms(petya); len(got) != 1 {
		t.Fatalf("комната пропала до удаления: %+v", got)
	}

	expect(t, e.do(http.MethodDelete, "/api/rooms/"+room.ID, nil, with(marta)), http.StatusNoContent, "")
	if got := e.rooms(marta); len(got) != 0 {
		t.Errorf("комнаты у владельца: %+v", got)
	}
	if got := e.rooms(petya); len(got) != 0 {
		t.Errorf("комнаты у участника: %+v", got)
	}
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 5), room.ID, keyID(60)),
		with(marta), withDevice(m1)), http.StatusForbidden, "not_member")
	// Удалять больше нечего — и это уже чужая комната.
	expect(t, e.do(http.MethodDelete, "/api/rooms/"+room.ID, nil, with(marta)), http.StatusForbidden, "not_owner")
}

// Конверт комнаты уходит на все устройства всех участников, кроме
// отправившего (ADR-017).
func TestRoomMessageFanout(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	m2 := e.addDevice(marta, deviceOf(2))
	petya, p1 := e.join("petya", 3)
	p2 := e.addDevice(petya, deviceOf(4))
	kolya, k1 := e.join("kolya", 5)

	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")

	id := ulid(nowMillis(), 6)
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(id, room.ID, keyID(60)),
		with(marta), withDevice(m1)), http.StatusAccepted, "")

	if got := e.queue(m1); len(got) != 0 {
		t.Errorf("эхо отправившему устройству: %v", got)
	}
	for _, device := range []string{m2, p1, p2} {
		got := e.envelopes(device)
		if len(got) != 1 {
			t.Fatalf("очередь %s: получено %d конвертов, ожидался 1", device, len(got))
		}
		if got[0].ID != id || got[0].From != "marta" || got[0].To.Room != room.ID || got[0].To.DM != "" {
			t.Errorf("конверт для %s: %+v", device, got[0])
		}
		if got[0].KeyID != keyID(60) || got[0].TS == 0 {
			t.Errorf("конверт для %s: %+v", device, got[0])
		}
	}
	// Посторонний не получает и не пишет.
	if got := e.queue(k1); len(got) != 0 {
		t.Errorf("конверт постороннему: %v", got)
	}
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 7), room.ID, keyID(60)),
		with(kolya), withDevice(k1)), http.StatusForbidden, "not_member")

	// Контактов комната не заводит: список комнат приходит из GET /api/rooms.
	if got := e.contacts(marta); len(got) != 0 {
		t.Errorf("сообщение в комнату завело контакт: %+v", got)
	}
}

// keyId сообщения обязан быть ключом этой комнаты (docs/protocol.md).
func TestRoomMessageUnknownKey(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	room := e.makeRoom(marta, "marta", "общая", 40)
	other := e.makeRoom(petya, "petya", "чужая", 60)

	// Ключ другой комнаты — не ключ этой.
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 3), room.ID, keyID(60)),
		with(marta), withDevice(m1)), http.StatusBadRequest, "unknown_key")
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 4), room.ID, keyID(99)),
		with(marta), withDevice(m1)), http.StatusBadRequest, "unknown_key")
	// Свой — принимается.
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 5), room.ID, keyID(40)),
		with(marta), withDevice(m1)), http.StatusAccepted, "")

	// Членство проверяется раньше ключа.
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 6), other.ID, keyID(99)),
		with(marta), withDevice(m1)), http.StatusForbidden, "not_member")
	if got := e.queue(p1); len(got) != 0 {
		t.Errorf("отвергнутое сообщение попало в очередь: %v", got)
	}
}

// Прежний ключ комнаты остаётся рабочим, пока его не вытеснила обрезка:
// у комнаты живут два последних keyId (ADR-018).
func TestRoomKeysKeepTwo(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	room := e.makeRoom(marta, "marta", "общая", 40)

	expect(t, e.changeMembers(marta, room.ID, nil, nil, []string{"marta"}, 60), http.StatusOK, "")
	// Два последних — первый ещё жив.
	for _, key := range []string{keyID(40), keyID(60)} {
		expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 3), room.ID, key),
			with(marta), withDevice(m1)), http.StatusAccepted, "")
	}

	expect(t, e.changeMembers(marta, room.ID, nil, nil, []string{"marta"}, 80), http.StatusOK, "")
	// Третий rekey вытеснил самый старый ключ.
	expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 4), room.ID, keyID(40)),
		with(marta), withDevice(m1)), http.StatusBadRequest, "unknown_key")
	for _, key := range []string{keyID(60), keyID(80)} {
		expect(t, e.do(http.MethodPost, "/api/messages", roomMessage(ulid(nowMillis(), 5), room.ID, key),
			with(marta), withDevice(m1)), http.StatusAccepted, "")
	}
	// Текущий ключ участника — последний.
	if got := e.room(marta, room.ID); got.Key == nil || got.Key.KeyID != keyID(80) {
		t.Errorf("текущий ключ: %+v", got.Key)
	}
}

// Удаление аккаунта передаёт владение по ADR-018, а комнату без участников
// удаляет.
func TestDeleteAccountWithRooms(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	petya, _ := e.join("petya", 2)

	shared := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, shared.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")
	alone := e.makeRoom(marta, "marta", "своя", 80)

	expect(t, e.do(http.MethodDelete, "/api/me", map[string]any{"authKey": bytesOf(32, 1)}, with(marta)),
		http.StatusNoContent, "")

	// Комната с оставшимся участником живёт, комната без участников —
	// исчезла вместе с владельцем.
	list := e.rooms(petya)
	if len(list) != 1 || list[0].ID != shared.ID || list[0].ID == alone.ID {
		t.Fatalf("комнаты petya: %+v", list)
	}
	if list[0].Owner != "petya" {
		t.Errorf("владелец после удаления аккаунта: получено %q, ожидалось \"petya\"", list[0].Owner)
	}
	if len(list[0].Members) != 1 || list[0].Members[0] != "petya" {
		t.Errorf("состав: %v", list[0].Members)
	}
	if list[0].Key == nil || list[0].Key.KeyID != keyID(60) {
		t.Errorf("ключ оставшегося: %+v", list[0].Key)
	}
	// Ник свободен, а комната, где не осталось никого, исчезла вместе с ним.
	expect(t, e.do(http.MethodPost, "/api/register", account("marta")), http.StatusCreated, "")
	fresh := e.do(http.MethodPost, "/api/login", map[string]any{"nick": "marta", "authKey": bytesOf(32, 1)})
	expect(t, fresh, http.StatusOK, "")
	if got := e.rooms(e.cookie(fresh)); len(got) != 0 {
		t.Errorf("комнаты нового аккаунта: %+v", got)
	}
}

// Событие room уходит остальным устройствам создателя, но не отправившему
// (docs/protocol.md, «Комнаты»).
func TestRoomEventOnCreate(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	m2 := e.addDevice(marta, deviceOf(2))

	sender := e.open(m1, marta)
	sender.untilReady()
	other := e.open(m2, marta)
	other.untilReady()

	room := e.makeRoom(marta, "marta", "общая", 40, withDevice(m1))

	ev := other.next()
	if ev.name != "room" {
		t.Fatalf("событие: получено %q, ожидалось \"room\"", ev.name)
	}
	var got roomBody
	if err := json.Unmarshal([]byte(ev.data), &got); err != nil {
		t.Fatalf("разбор события %q: %v", ev.data, err)
	}
	if got.ID != room.ID || got.Name != "общая" || got.Owner != "marta" {
		t.Errorf("комната в событии: %+v", got)
	}
	if got.Key == nil || got.Key.CT != ctOf(40, 0) {
		t.Errorf("ключ в событии: %+v", got.Key)
	}
	if got.NeedsRekey {
		t.Error("needsRekey при создании")
	}
	select {
	case ev := <-sender.events:
		t.Errorf("эхо отправившему устройству: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// Смена состава: room всем участникам — каждому со своим ключом,
// room_left убранным.
func TestRoomEventsOnMembers(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	kolya, k1 := e.join("kolya", 3)

	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya", "kolya"}, nil,
		[]string{"marta", "petya", "kolya"}, 60), http.StatusOK, "")

	streams := map[string]*stream{
		"marta": e.open(m1, marta),
		"petya": e.open(p1, petya),
		"kolya": e.open(k1, kolya),
	}
	for _, s := range streams {
		s.untilReady()
	}

	expect(t, e.changeMembers(marta, room.ID, nil, []string{"kolya"}, []string{"marta", "petya"}, 80),
		http.StatusOK, "")

	for i, nick := range []string{"marta", "petya"} {
		ev := streams[nick].next()
		if ev.name != "room" {
			t.Fatalf("событие у %s: получено %q, ожидалось \"room\"", nick, ev.name)
		}
		var got roomBody
		if err := json.Unmarshal([]byte(ev.data), &got); err != nil {
			t.Fatalf("разбор события %q: %v", ev.data, err)
		}
		if nicks(got.Members) != "marta,petya" {
			t.Errorf("состав в событии у %s: %v", nick, got.Members)
		}
		if got.Key == nil || got.Key.KeyID != keyID(80) || got.Key.CT != ctOf(80, i) {
			t.Errorf("ключ в событии у %s: %+v", nick, got.Key)
		}
		if got.NeedsRekey {
			t.Errorf("needsRekey при смене состава у %s", nick)
		}
	}

	ev := streams["kolya"].next()
	if ev.name != "room_left" || ev.data != `{"id":"`+room.ID+`"}` {
		t.Errorf("событие у убранного: %+v", ev)
	}
}

// Выход участника: остальным — room с needsRekey (ADR-018).
func TestRoomEventOnLeave(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)

	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")

	owner := e.open(m1, marta)
	owner.untilReady()
	leaving := e.open(p1, petya)
	leaving.untilReady()

	expect(t, e.do(http.MethodPost, "/api/rooms/"+room.ID+"/leave", nil, with(petya)), http.StatusNoContent, "")

	ev := owner.next()
	if ev.name != "room" {
		t.Fatalf("событие: получено %q, ожидалось \"room\"", ev.name)
	}
	var got roomBody
	if err := json.Unmarshal([]byte(ev.data), &got); err != nil {
		t.Fatalf("разбор события %q: %v", ev.data, err)
	}
	if !got.NeedsRekey {
		t.Errorf("needsRekey: получено false, ожидалось true: %s", ev.data)
	}
	if len(got.Members) != 1 || got.Members[0] != "marta" {
		t.Errorf("состав в событии: %v", got.Members)
	}
	if got.Key == nil || got.Key.KeyID != keyID(60) {
		t.Errorf("ключ в событии: %+v", got.Key)
	}
	// Другим устройствам вышедшего — room_left: комната ушла из списка,
	// и ждать следующего ready им незачем (ADR-041). Запрос шёл без
	// X-Device, поэтому событие получает и это устройство.
	gone := leaving.next()
	if gone.name != "room_left" || gone.data != `{"id":"`+room.ID+`"}` {
		t.Errorf("событие вышедшему: %+v", gone)
	}
}

// Выход с X-Device: room_left уходит другим устройствам вышедшего,
// но не отправившему запрос (ADR-041).
func TestRoomEventOnLeaveExcludesSender(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	p2 := e.addDevice(petya, deviceOf(3))

	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")

	sender := e.open(p1, petya)
	sender.untilReady()
	other := e.open(p2, petya)
	other.untilReady()

	expect(t, e.do(http.MethodPost, "/api/rooms/"+room.ID+"/leave", nil, with(petya), withDevice(p1)),
		http.StatusNoContent, "")

	ev := other.next()
	if ev.name != "room_left" || ev.data != `{"id":"`+room.ID+`"}` {
		t.Errorf("событие другому устройству: %+v", ev)
	}
	select {
	case ev := <-sender.events:
		t.Errorf("эхо отправившему устройству: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// Удаление комнаты: room_left всем участникам, включая владельца.
func TestRoomEventOnDelete(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)

	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")

	owner := e.open(m1, marta)
	owner.untilReady()
	member := e.open(p1, petya)
	member.untilReady()

	expect(t, e.do(http.MethodDelete, "/api/rooms/"+room.ID, nil, with(marta)), http.StatusNoContent, "")

	want := `{"id":"` + room.ID + `"}`
	for _, s := range []*stream{owner, member} {
		ev := s.next()
		if ev.name != "room_left" || ev.data != want {
			t.Errorf("событие: %+v", ev)
		}
	}
}

// Комнаты требуют сессии, как и всё непубличное.
func TestRoomsNeedSession(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do(http.MethodGet, "/api/rooms", nil), http.StatusUnauthorized, "unauthenticated")
	expect(t, e.do(http.MethodPost, "/api/rooms", map[string]any{"name": "общая"}),
		http.StatusUnauthorized, "unauthenticated")
	expect(t, e.do(http.MethodPost, "/api/rooms/"+bytesOf(16, 1)+"/leave", nil),
		http.StatusUnauthorized, "unauthenticated")
	expect(t, e.do(http.MethodDelete, "/api/rooms/"+bytesOf(16, 1), nil),
		http.StatusUnauthorized, "unauthenticated")
	// Идентификатор комнаты в журнал не уходит: пишется шаблон маршрута.
	if strings.Contains(e.log.String(), bytesOf(16, 1)) {
		t.Errorf("идентификатор комнаты в журнале: %q", e.log.String())
	}
}

// Долг по ключу — состояние комнаты: владелец, пропустивший событие,
// поднимает его из GET /api/rooms, а смена состава долг снимает (ADR-041).
func TestNeedsRekeyOutlivesEvent(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	petya, _ := e.join("petya", 2)
	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")
	if got := e.room(marta, room.ID); got.NeedsRekey {
		t.Error("needsRekey до выхода участника")
	}

	// Владелец не подключён: событие room ему уходить некуда.
	expect(t, e.do(http.MethodPost, "/api/rooms/"+room.ID+"/leave", nil, with(petya)),
		http.StatusNoContent, "")

	got := e.room(marta, room.ID)
	if got == nil || !got.NeedsRekey {
		t.Fatalf("needsRekey в списке комнат: %+v", got)
	}
	if got.Key == nil || got.Key.KeyID != keyID(60) {
		t.Errorf("ключ в списке: %+v", got.Key)
	}

	// Rekey закрывает долг.
	expect(t, e.changeMembers(marta, room.ID, nil, nil, []string{"marta"}, 80), http.StatusOK, "")
	if got := e.room(marta, room.ID); got.NeedsRekey {
		t.Error("needsRekey после rekey")
	}
}

// Удаление аккаунта — выход из всех его комнат: оставшимся уходит room
// с needsRekey и их собственным ключом, владение переходит (ADR-041).
func TestDeleteAccountLeavesRooms(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	kolya, k1 := e.join("kolya", 3)

	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya", "kolya"}, nil,
		[]string{"marta", "petya", "kolya"}, 60), http.StatusOK, "")

	first := e.open(p1, petya)
	first.untilReady()
	second := e.open(k1, kolya)
	second.untilReady()

	expect(t, e.do(http.MethodDelete, "/api/me", map[string]any{"authKey": bytesOf(32, 1)}, with(marta)),
		http.StatusNoContent, "")

	for i, s := range []*stream{first, second} {
		ev := s.next()
		if ev.name != "room" {
			t.Fatalf("событие: получено %q, ожидалось \"room\"", ev.name)
		}
		var got roomBody
		if err := json.Unmarshal([]byte(ev.data), &got); err != nil {
			t.Fatalf("разбор события %q: %v", ev.data, err)
		}
		if !got.NeedsRekey {
			t.Errorf("needsRekey в событии: %s", ev.data)
		}
		// Владение — участнику с наименьшим joined_at; petya и kolya
		// вступили одной операцией, поэтому порядок решает ник.
		if got.Owner != "kolya" {
			t.Errorf("владелец в событии: %q, ожидался kolya", got.Owner)
		}
		if nicks(got.Members) != nicks([]string{"petya", "kolya"}) {
			t.Errorf("состав в событии: %v", got.Members)
		}
		// Каждому — его собственный ключ: он различается порядковым
		// номером внутри «шифротекста».
		if got.Key == nil || got.Key.CT != ctOf(60, i+1) {
			t.Errorf("ключ в событии: %+v", got.Key)
		}
	}

	// Признак пережил и рассылку: новый владелец увидит его после ready.
	if got := e.room(kolya, room.ID); got == nil || !got.NeedsRekey || got.Owner != "kolya" {
		t.Errorf("комната у нового владельца: %+v", got)
	}
	// Ключи удалённого аккаунта ушли каскадом, комната жива.
	expect(t, e.changeMembers(kolya, room.ID, nil, nil, []string{"petya", "kolya"}, 80),
		http.StatusOK, "")
	if got := e.room(petya, room.ID); got.NeedsRekey {
		t.Error("needsRekey после rekey нового владельца")
	}
}

// Два ключа одному участнику — это множество keys[].to, не равное
// составу, и код у него тот же (ADR-043).
func TestMembersDuplicateKeyTarget(t *testing.T) {
	e := newEnv(t)
	marta, _ := e.join("marta", 1)
	e.join("petya", 2)
	room := e.makeRoom(marta, "marta", "общая", 40)
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 60),
		http.StatusOK, "")

	rec := e.changeMembers(marta, room.ID, nil, nil, []string{"marta", "marta"}, 80)
	expect(t, rec, http.StatusBadRequest, "keys_mismatch")
	// Отказ ничего не изменил: ключ комнаты прежний.
	if got := e.room(marta, room.ID); got.Key == nil || got.Key.KeyID != keyID(60) {
		t.Errorf("ключ после keys_mismatch: %+v", got.Key)
	}
}
