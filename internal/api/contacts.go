package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/xmatic-squad/bare/internal/auth"
	"github.com/xmatic-squad/bare/internal/store"
)

// Контакт — строка в списке чатов, не разрешение на переписку: писать
// можно любому нику, согласия не требуется (ADR-019).

// GET /api/contacts — список чатов 1:1 с публичными ключами собеседников.
func (s *server) contacts(w http.ResponseWriter, r *http.Request) {
	sess, _ := auth.From(r)
	list, err := s.st.Contacts(r.Context(), sess.Nick)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	type contactOut struct {
		Nick      string          `json:"nick"`
		PublicKey json.RawMessage `json:"publicKey"`
		CreatedAt int64           `json:"createdAt"`
	}
	out := make([]contactOut, 0, len(list))
	for _, c := range list {
		out = append(out, contactOut{c.Nick, json.RawMessage(c.PublicKey), c.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/contacts — завести чат с ником вручную, до первого сообщения.
func (s *server) addContact(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Nick string `json:"nick"`
	}
	if !decode(w, r, &in) {
		return
	}
	sess, _ := auth.From(r)
	peer, ok := s.peer(w, r, in.Nick, sess.Nick)
	if !ok {
		return
	}
	created, err := s.st.AddContact(r.Context(), sess.Nick, peer.Nick, time.Now().UnixMilli())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, struct {
		Nick      string          `json:"nick"`
		PublicKey json.RawMessage `json:"publicKey"`
	}{peer.Nick, json.RawMessage(peer.PublicKey)})
}

// DELETE /api/contacts/{nick} — убрать чат из списка. Зеркальная строка
// у собеседника остаётся: это не блокировка (ADR-019). Строки не было —
// тот же 204, удалять нечего.
func (s *server) deleteContact(w http.ResponseWriter, r *http.Request) {
	sess, _ := auth.From(r)
	if err := s.st.DeleteContact(r.Context(), sess.Nick, r.PathValue("nick")); err != nil {
		s.internal(w, r, err)
		return
	}
	noContent(w)
}

// peer читает собеседника по нику. Порядок отказов — docs/protocol.md:
// сначала существование ника, потом запрет писать себе.
func (s *server) peer(w http.ResponseWriter, r *http.Request, nick, me string) (store.User, bool) {
	if !validNick(nick) {
		unknownUser(w)
		return store.User{}, false
	}
	u, err := s.st.User(r.Context(), nick)
	if errors.Is(err, store.ErrNotFound) {
		unknownUser(w)
		return store.User{}, false
	}
	if err != nil {
		s.internal(w, r, err)
		return store.User{}, false
	}
	if u.Nick == me {
		Error(w, http.StatusBadRequest, "self", "нельзя писать себе")
		return store.User{}, false
	}
	return u, true
}
