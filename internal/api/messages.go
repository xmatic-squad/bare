package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/xmatic-squad/bare/internal/auth"
	"github.com/xmatic-squad/bare/internal/hub"
	"github.com/xmatic-squad/bare/internal/store"
)

// clockSkew — на сколько метка времени ULID вправе разойтись с часами
// сервера (ADR-017).
const clockSkew = 5 * time.Minute

// dmKeyID — keyId личного чата: ключ выводится из ECDH, идентификатора
// у него нет (docs/crypto.md, «Сообщение»).
const dmKeyID = "dm"

// maxAck — сколько идентификаторов принимает один ACK.
const maxAck = 500

// target — адресат конверта: ровно одно из двух.
type target struct {
	DM   string `json:"dm,omitempty"`
	Room string `json:"room,omitempty"`
}

// envelope — конверт из docs/protocol.md. Порядок полей — как в нём.
// from и ts ставит сервер: клиентские значения не читаются вовсе (ADR-017).
type envelope struct {
	ID    string `json:"id"`
	To    target `json:"to"`
	From  string `json:"from"`
	KeyID string `json:"keyId"`
	IV    string `json:"iv"`
	CT    string `json:"ct"`
	TS    int64  `json:"ts"`
}

// messageIn — тело POST /api/messages. Полей from и ts здесь нет
// намеренно: что бы клиент ни прислал, сервер ставит своё (ADR-017).
type messageIn struct {
	ID    string `json:"id"`
	To    target `json:"to"`
	KeyID string `json:"keyId"`
	IV    string `json:"iv"`
	CT    string `json:"ct"`
}

// POST /api/messages — отправка. Сервер не умеет проверять шифротекст,
// он проверяет форму и раскладывает конверт по очередям (ADR-008).
// Порядок проверок — docs/protocol.md, «Сообщения».
func (s *server) sendMessage(w http.ResponseWriter, r *http.Request) {
	device, ok := s.device(w, r)
	if !ok {
		return
	}
	var in messageIn
	if !decode(w, r, &in) {
		return
	}
	ms, ok := checkForm(w, in)
	if !ok {
		return
	}

	now := time.Now()
	if d := now.Sub(time.UnixMilli(ms)); d > clockSkew || d < -clockSkew {
		Error(w, http.StatusBadRequest, "clock_skew",
			"проверьте часы на устройстве: расхождение больше 5 минут")
		return
	}

	sess, _ := auth.From(r)
	room := in.To.Room != ""
	if room {
		member, knownKey, err := s.st.RoomAccess(r.Context(), in.To.Room, sess.Nick, in.KeyID)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if !member {
			Error(w, http.StatusForbidden, "not_member", "вы не участник комнаты")
			return
		}
		if !knownKey {
			Error(w, http.StatusBadRequest, "unknown_key", "у комнаты нет такого ключа")
			return
		}
	} else if _, ok := s.peer(w, r, in.To.DM, sess.Nick); !ok {
		return
	}

	if wait, ok := s.msgs.take(sess.Nick, now); !ok {
		s.rateLimited(w, wait)
		return
	}

	env := envelope{
		ID:    in.ID,
		To:    target{DM: in.To.DM, Room: in.To.Room},
		From:  sess.Nick,
		KeyID: in.KeyID,
		IV:    in.IV,
		CT:    in.CT,
		TS:    now.UnixMilli(),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	delivery := store.Delivery{
		From:     env.From,
		To:       env.To.DM,
		Room:     env.To.Room,
		Exclude:  device,
		MsgID:    env.ID,
		Envelope: string(raw),
		Now:      env.TS,
	}
	var devices []string
	if room {
		devices, err = s.st.DeliverRoom(r.Context(), delivery)
	} else {
		devices, err = s.st.DeliverDM(r.Context(), delivery)
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	// Очередь уже записана: подключённое устройство получает конверт
	// сразу, остальные — при подключении. Пуши — этап 4.
	for _, id := range devices {
		s.hub.Send(id, hub.Event{Name: "msg", Data: string(raw)})
	}
	writeJSON(w, http.StatusAccepted, struct {
		ID string `json:"id"`
		TS int64  `json:"ts"`
	}{env.ID, env.TS})
}

// checkForm проверяет форму полей конверта (docs/crypto.md, «Что сервер
// проверяет») и отдаёт метку времени из ULID. Ответ об ошибке уже написан,
// если вернулось false.
func checkForm(w http.ResponseWriter, in messageIn) (int64, bool) {
	ms, ok := ulidTime(in.ID)
	if !ok {
		Invalid(w, "id", "id — не ulid из 26 символов")
		return 0, false
	}
	if (in.To.DM == "") == (in.To.Room == "") {
		Invalid(w, "to", "to — ровно одно из dm и room")
		return 0, false
	}
	if in.To.DM != "" {
		if !validNick(in.To.DM) {
			Invalid(w, "to", "ник: 2–32 символа, a–z, 0–9, _")
			return 0, false
		}
		if in.KeyID != dmKeyID {
			Invalid(w, "keyId", `keyId личного чата — "dm"`)
			return 0, false
		}
	} else {
		if !validID(in.To.Room) {
			Invalid(w, "to", "room — не 16 байт base64url")
			return 0, false
		}
		if !validID(in.KeyID) {
			Invalid(w, "keyId", "keyId — не 16 байт base64url")
			return 0, false
		}
	}
	if _, ok := decodeExactly(in.IV, ivLen); !ok {
		Invalid(w, "iv", "iv — не 12 байт base64url")
		return 0, false
	}
	if ct, err := b64.DecodeString(in.CT); err != nil || len(ct) < minCTLen {
		Invalid(w, "ct", "ct — не base64url или слишком короткий")
		return 0, false
	}
	return ms, true
}

// POST /api/ack — клиент записал сообщения в IndexedDB: из очереди
// устройства их можно убрать (ADR-008).
func (s *server) ack(w http.ResponseWriter, r *http.Request) {
	device, ok := s.device(w, r)
	if !ok {
		return
	}
	var in struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.IDs) > maxAck {
		Invalid(w, "ids", "не больше 500 идентификаторов")
		return
	}
	if err := s.st.Ack(r.Context(), device, in.IDs); err != nil {
		s.internal(w, r, err)
		return
	}
	noContent(w)
}

// rateLimited — 429 с Retry-After в секундах (ADR-021).
func (s *server) rateLimited(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter(wait)))
	Error(w, http.StatusTooManyRequests, "rate_limited", "слишком часто, попробуйте позже")
}
