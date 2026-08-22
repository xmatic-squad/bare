package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/xmatic-squad/bare/internal/auth"
	"github.com/xmatic-squad/bare/internal/hub"
	"github.com/xmatic-squad/bare/internal/store"
)

// Комнаты (ADR-018): владелец меняет состав, ключи заворачивают клиенты.
// Сервер проверяет форму и права, хранит шифротекст и раздаёт события.

// maxRoomName — имя комнаты, символов (ADR-021). Имя открыто: это
// метаданные, как и состав.
const maxRoomName = 64

// roomOut — тип Room из docs/protocol.md. key присутствует всегда,
// пустой — null; needsRekey — состояние комнаты, а не свойство события,
// поэтому идёт и в списке, и в событии (ADR-041).
type roomOut struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Owner      string   `json:"owner"`
	Members    []string `json:"members"`
	CreatedAt  int64    `json:"createdAt"`
	Key        *keyOut  `json:"key"`
	NeedsRekey bool     `json:"needsRekey"`
}

// keyOut — завёрнутый ключ комнаты для того, кто его получает.
type keyOut struct {
	KeyID string `json:"keyId"`
	From  string `json:"from"`
	IV    string `json:"iv"`
	CT    string `json:"ct"`
}

// keyIn — запись keys[] запроса: WrappedKey из docs/protocol.md.
type keyIn struct {
	To string `json:"to"`
	IV string `json:"iv"`
	CT string `json:"ct"`
}

// GET /api/rooms — комнаты, где пользователь участник, каждая с его
// текущим ключом и признаком needsRekey: владелец, пропустивший событие,
// поднимает долг по ключу отсюда (ADR-041).
func (s *server) rooms(w http.ResponseWriter, r *http.Request) {
	sess, _ := auth.From(r)
	list, err := s.st.Rooms(r.Context(), sess.Nick)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]roomOut, 0, len(list))
	for _, room := range list {
		out = append(out, roomJSON(room, room.Key))
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/rooms — создание комнаты. Идентификатор выдаёт клиент
// (ADR-037), ключ приходит ровно один и заворачивается создателем себе:
// его другие устройства получают комнату вместе с ключом (ADR-018).
func (s *server) createRoom(w http.ResponseWriter, r *http.Request) {
	// X-Device здесь необязателен, но чужой и кривой — 403, как и везде,
	// где устройство важно (docs/protocol.md, «Общие правила»).
	device, ok := s.optionalDevice(w, r)
	if !ok {
		return
	}
	var in struct {
		ID    string  `json:"id"`
		Name  string  `json:"name"`
		KeyID string  `json:"keyId"`
		Keys  []keyIn `json:"keys"`
	}
	if !decode(w, r, &in) {
		return
	}
	// Идентификатор комнаты генерирует клиент: ключ заворачивается до
	// запроса и привязан к roomId в info и AAD (ADR-037).
	if !validID(in.ID) {
		Invalid(w, "id", "id комнаты — не 16 байт base64url")
		return
	}
	if !validRoomName(in.Name) {
		Invalid(w, "name", "имя комнаты: 1–64 символа")
		return
	}
	if !validID(in.KeyID) {
		Invalid(w, "keyId", "keyId — не 16 байт base64url")
		return
	}
	keys, ok := wrappedKeys(w, in.Keys)
	if !ok {
		return
	}
	sess, _ := auth.From(r)
	// Состав новой комнаты — один создатель, поэтому и ключ ровно один.
	// Несовпадение — то же самое, что при rekey: keys не по составу.
	if len(keys) != 1 || keys[0].To != sess.Nick {
		keysMismatch(w)
		return
	}
	change, err := s.st.CreateRoom(r.Context(), store.NewRoom{
		ID:    in.ID,
		Name:  in.Name,
		Owner: sess.Nick,
		KeyID: in.KeyID,
		Key:   keys[0],
		Now:   time.Now().UnixMilli(),
	})
	if errors.Is(err, store.ErrRoomExists) {
		// Занятый идентификатор не присоединяет к чужой комнате и не
		// перезаписывает свою: клиент берёт новый (ADR-037).
		Error(w, http.StatusConflict, "room_conflict", "такая комната уже есть")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	// Комната уже записана: остальным устройствам создателя она уходит
	// событием, отправившему — ответом на запрос.
	s.sendRoom(r, change, device)
	writeJSON(w, http.StatusCreated, roomJSON(change.Room, keyFor(change, sess.Nick)))
}

// POST /api/rooms/{id}/members — смена состава и rekey одним запросом
// (ADR-018). Пустые add и remove — чистый rekey.
func (s *server) updateMembers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
		KeyID  string   `json:"keyId"`
		Keys   []keyIn  `json:"keys"`
	}
	if !decode(w, r, &in) {
		return
	}
	add, ok := uniqueNicks(in.Add)
	if !ok {
		// Форма — это форма: несуществующий ник верной формы отвечает
		// unknown_user, а ник не по форме — invalid, как и в remove
		// (ADR-043).
		Invalid(w, "add", "добавить можно только ник a–z, 0–9, _")
		return
	}
	remove, ok := uniqueNicks(in.Remove)
	if !ok {
		Invalid(w, "remove", "убрать можно только участника комнаты")
		return
	}
	for _, nick := range remove {
		for _, other := range add {
			if nick == other {
				Invalid(w, "remove", "один ник нельзя добавить и убрать одним запросом")
				return
			}
		}
	}
	if !validID(in.KeyID) {
		Invalid(w, "keyId", "keyId — не 16 байт base64url")
		return
	}
	keys, ok := wrappedKeys(w, in.Keys)
	if !ok {
		return
	}
	sess, _ := auth.From(r)
	change, err := s.st.UpdateMembers(r.Context(), store.MembersChange{
		RoomID: r.PathValue("id"),
		Owner:  sess.Nick,
		Add:    add,
		Remove: remove,
		KeyID:  in.KeyID,
		Keys:   keys,
		Now:    time.Now().UnixMilli(),
	})
	if err != nil {
		s.roomError(w, r, err)
		return
	}
	// Событие room уходит и участникам, и — как room_left — убранным;
	// каждому участнику со своим ключом (docs/protocol.md, «Комнаты»).
	s.sendRoom(r, change, "")
	writeJSON(w, http.StatusOK, roomJSON(change.Room, keyFor(change, sess.Nick)))
}

// POST /api/rooms/{id}/leave — выход из комнаты. Владение переходит
// участнику с наименьшим joined_at, опустевшая комната удаляется;
// оставшимся уходит room с needsRekey (ADR-018), другим устройствам
// вышедшего — room_left (ADR-041).
//
// Не участник и несуществующая комната отвечают тем же 204: выходить
// неоткуда, а отдельного кода на этот случай в протоколе нет.
func (s *server) leaveRoom(w http.ResponseWriter, r *http.Request) {
	// Заголовок необязателен, но чужой и кривой — 403, как и везде,
	// где устройство важно (docs/protocol.md, «Общие правила»).
	device, ok := s.optionalDevice(w, r)
	if !ok {
		return
	}
	sess, _ := auth.From(r)
	change, err := s.st.LeaveRoom(r.Context(), r.PathValue("id"), sess.Nick)
	if errors.Is(err, store.ErrNotFound) {
		noContent(w)
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.sendRoom(r, change, device)
	noContent(w)
}

// DELETE /api/rooms/{id} — удаление комнаты владельцем. Всем участникам,
// включая его самого, уходит room_left.
func (s *server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	sess, _ := auth.From(r)
	change, err := s.st.DeleteRoom(r.Context(), r.PathValue("id"), sess.Nick)
	if err != nil {
		s.roomError(w, r, err)
		return
	}
	s.sendRoom(r, change, "")
	noContent(w)
}

// roomError переводит отказы хранилища в коды протокола
// (docs/protocol.md, «Комнаты»).
func (s *server) roomError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotOwner):
		Error(w, http.StatusForbidden, "not_owner", "комнату меняет её владелец")
	case errors.Is(err, store.ErrUnknownUser):
		unknownUser(w)
	case errors.Is(err, store.ErrNotMember):
		Invalid(w, "remove", "убрать можно только участника комнаты")
	case errors.Is(err, store.ErrOwnerRemoval):
		Error(w, http.StatusBadRequest, "owner", "владельца убрать нельзя")
	case errors.Is(err, store.ErrKeyExists):
		Error(w, http.StatusConflict, "key_exists", "такой ключ у комнаты уже был")
	case errors.Is(err, store.ErrKeysMismatch):
		keysMismatch(w)
	default:
		s.internal(w, r, err)
	}
}

func keysMismatch(w http.ResponseWriter) {
	Error(w, http.StatusBadRequest, "keys_mismatch", "ключи не совпадают с составом комнаты")
}

// sendRoom раздаёт события изменившейся комнаты: room участникам, каждому
// с его собственным ключом, и room_left выбывшим. exclude — устройство,
// которому событие не нужно; пусто — нужно всем.
//
// Событие в очередь не кладётся: клиент после каждого ready перечитывает
// GET /api/rooms, а всё, что несёт room, включая needsRekey, есть и там,
// поэтому пропуск во время офлайна ничего не ломает (docs/protocol.md,
// «События», ADR-041).
func (s *server) sendRoom(r *http.Request, change store.RoomChange, exclude string) {
	for _, member := range change.Members {
		raw, err := json.Marshal(roomJSON(change.Room, member.Key))
		if err != nil {
			s.report(r, err)
			continue
		}
		s.send(member.Devices, exclude, hub.Event{Name: "room", Data: string(raw)})
	}
	if len(change.Left) == 0 {
		return
	}
	raw, err := json.Marshal(struct {
		ID string `json:"id"`
	}{change.Room.ID})
	if err != nil {
		s.report(r, err)
		return
	}
	for _, gone := range change.Left {
		s.send(gone.Devices, exclude, hub.Event{Name: "room_left", Data: string(raw)})
	}
}

// send отдаёт событие подключённым устройствам, кроме exclude.
func (s *server) send(devices []string, exclude string, ev hub.Event) {
	for _, device := range devices {
		if device == exclude {
			continue
		}
		s.hub.Send(device, ev)
	}
}

// roomJSON собирает Room протокола: состав всегда список, ключ — null,
// если его нет.
func roomJSON(room store.Room, key *store.RoomKey) roomOut {
	out := roomOut{
		ID:         room.ID,
		Name:       room.Name,
		Owner:      room.Owner,
		Members:    room.Members,
		CreatedAt:  room.CreatedAt,
		NeedsRekey: room.NeedsRekey,
	}
	if out.Members == nil {
		out.Members = []string{}
	}
	if key != nil {
		out.Key = &keyOut{KeyID: key.KeyID, From: key.From, IV: key.IV, CT: key.CT}
	}
	return out
}

// keyFor — ключ участника в итоге изменения: у каждого он свой.
func keyFor(change store.RoomChange, nick string) *store.RoomKey {
	for _, member := range change.Members {
		if member.Nick == nick {
			return member.Key
		}
	}
	return nil
}

// wrappedKeys проверяет форму завёрнутых ключей. Содержимое сервер
// не проверяет и проверить не может: это шифротекст (ADR-018).
func wrappedKeys(w http.ResponseWriter, in []keyIn) ([]store.WrappedKey, bool) {
	out := make([]store.WrappedKey, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, k := range in {
		if !validNick(k.To) {
			Invalid(w, "keys", "keys[].to — не ник")
			return nil, false
		}
		if seen[k.To] {
			// Два ключа одному участнику — это множество keys[].to,
			// не равное составу, а не отдельный отказ (ADR-043).
			keysMismatch(w)
			return nil, false
		}
		seen[k.To] = true
		if _, ok := decodeExactly(k.IV, ivLen); !ok {
			Invalid(w, "keys", "iv — не 12 байт base64url")
			return nil, false
		}
		if ct, err := b64.DecodeString(k.CT); err != nil || len(ct) < minCTLen {
			Invalid(w, "keys", "ct — не base64url или слишком короткий")
			return nil, false
		}
		out = append(out, store.WrappedKey{To: k.To, IV: k.IV, CT: k.CT})
	}
	return out, true
}

// uniqueNicks разбирает список ников запроса: повторы схлопываются,
// порядок сохраняется. Второе значение — прошёл ли список проверку формы.
func uniqueNicks(list []string) ([]string, bool) {
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, nick := range list {
		if !validNick(nick) {
			return nil, false
		}
		if seen[nick] {
			continue
		}
		seen[nick] = true
		out = append(out, nick)
	}
	return out, true
}

// validRoomName — имя комнаты: непустое, до 64 символов (ADR-021).
func validRoomName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= maxRoomName
}
