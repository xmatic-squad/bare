package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/xmatic-squad/bare/internal/auth"
	"github.com/xmatic-squad/bare/internal/store"
)

// deviceID — тело и ответ POST /api/devices: идентификатор выдаёт клиент
// (ADR-017), сервер только проверяет форму и принадлежность.
type deviceID struct {
	ID string `json:"id"`
}

// POST /api/devices — регистрация устройства. Уже заведённое своё —
// 200 и обновлённый last_seen; занятое чужим — 409, клиент берёт новый id.
func (s *server) createDevice(w http.ResponseWriter, r *http.Request) {
	var in deviceID
	if !decode(w, r, &in) {
		return
	}
	if !validID(in.ID) {
		Invalid(w, "id", "id — не 16 байт base64url")
		return
	}
	sess, _ := auth.From(r)
	created, err := s.st.RegisterDevice(r.Context(), in.ID, sess.Nick, sess.TokenHash, time.Now().UnixMilli())
	if errors.Is(err, store.ErrDeviceTaken) {
		Error(w, http.StatusConflict, "device_conflict", "такое устройство уже есть")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, deviceID{in.ID})
}

// deviceOut — строка ответа GET /api/devices.
type deviceOut struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"`
	LastSeen  int64  `json:"lastSeen"`
	HasPush   bool   `json:"hasPush"`
	Current   bool   `json:"current"`
}

// GET /api/devices — устройства аккаунта. Самой push-подписки в ответе
// нет, только факт её наличия.
func (s *server) devices(w http.ResponseWriter, r *http.Request) {
	sess, _ := auth.From(r)
	list, err := s.st.Devices(r.Context(), sess.Nick)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]deviceOut, 0, len(list))
	for _, d := range list {
		out = append(out, deviceOut{
			ID:        d.ID,
			CreatedAt: d.CreatedAt,
			LastSeen:  d.LastSeen,
			HasPush:   d.HasPush,
			Current:   d.ID == sess.DeviceID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /api/devices/{id} — удаление устройства: очередь, подписка
// и сессии уходят каскадом, открытый поток событий закрывается.
//
// Чужое и несуществующее устройство отвечают тем же 204: удалять нечего,
// а отдельного кода на этот случай в протоколе нет (docs/protocol.md).
func (s *server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	sess, _ := auth.From(r)
	id := r.PathValue("id")
	deleted, err := s.st.DeleteDevice(r.Context(), id, sess.Nick)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if deleted {
		s.hub.Close(id)
	}
	noContent(w)
}

// device читает X-Device и проверяет, что устройство принадлежит
// пользователю сессии. Заголовка нет, форма кривая, устройство чужое —
// всё это 403 unknown_device (docs/protocol.md, «Общие правила»).
func (s *server) device(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, _ := auth.From(r)
	id := r.Header.Get("X-Device")
	if !validID(id) {
		unknownDevice(w)
		return "", false
	}
	owned, err := s.st.DeviceOwned(r.Context(), id, sess.Nick)
	if err != nil {
		s.internal(w, r, err)
		return "", false
	}
	if !owned {
		unknownDevice(w)
		return "", false
	}
	return id, true
}

func unknownDevice(w http.ResponseWriter) {
	Error(w, http.StatusForbidden, "unknown_device", "это устройство не ваше")
}
