package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/xmatic-squad/bare/internal/auth"
	"github.com/xmatic-squad/bare/internal/config"
	"github.com/xmatic-squad/bare/internal/store"
)

// GET /api/config — то, что клиенту нужно знать до входа.
func (s *server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		InviteRequired  bool   `json:"inviteRequired"`
		VAPIDPublicKey  string `json:"vapidPublicKey"`
		KDFIterations   int    `json:"kdfIterations"`
		MaxMessageChars int    `json:"maxMessageChars"`
	}{
		InviteRequired:  s.cfg.InviteCode != "",
		VAPIDPublicKey:  s.cfg.VAPIDPublic,
		KDFIterations:   config.KDFIterations,
		MaxMessageChars: config.MaxMessageChars,
	})
}

// GET /api/kdf?nick= — сколько итераций PBKDF2 брать для этого ника.
//
// Значение лежит открытым полем iter в ключевом блобе: другого места
// у него нет (docs/crypto.md). Неизвестный ник получает целевое значение
// тем же статусом 200. Скрытием существования ника этот ответ
// не занимается: после повышения цели у аккаунта, который с тех пор
// не входил, iter свой, и по числу его видно (ADR-062). Существование
// ника публично и так (ADR-019).
func (s *server) kdf(w http.ResponseWriter, r *http.Request) {
	iterations := config.KDFIterations
	if nick := r.URL.Query().Get("nick"); validNick(nick) {
		u, err := s.st.User(r.Context(), nick)
		switch {
		case err == nil:
			if iter, err := blobIterations(u.KeyBlob); err == nil {
				iterations = iter
			}
		case errors.Is(err, store.ErrNotFound):
			// молча: целевое значение
		default:
			s.internal(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Iterations int `json:"iterations"`
	}{iterations})
}

// POST /api/register — регистрация. Сервер проверяет только форму:
// содержимое блоба и стойкость пароля ему недоступны by design.
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Nick      string          `json:"nick"`
		AuthKey   string          `json:"authKey"`
		PublicKey json.RawMessage `json:"publicKey"`
		Blob      string          `json:"blob"`
		Invite    string          `json:"invite"`
	}
	if !decode(w, r, &in) {
		return
	}
	// Форма — раньше инвайт-кода: он даёт право регистрироваться, а права
	// идут после формы (ADR-043). Занятость ника этим не выдаётся: nick_taken
	// живёт дальше по тексту, за инвайтом.
	if !validNick(in.Nick) {
		Error(w, http.StatusBadRequest, "invalid_nick", "ник: 2–32 символа, a–z, 0–9, _")
		return
	}
	key, ok := authKey(in.AuthKey)
	if !ok {
		Invalid(w, "authKey", "authKey — не 32 байта base64url")
		return
	}
	public, err := publicKeyJSON(in.PublicKey)
	if err != nil {
		Invalid(w, "publicKey", err.Error())
		return
	}
	if _, err := blobIterations(in.Blob); err != nil {
		Invalid(w, "blob", err.Error())
		return
	}
	// Лимит — 5 в час на IP (ADR-021) — стоит раньше проверки инвайт-кода:
	// иначе код подбирался бы запросами без счёта.
	if wait, ok := s.regs.take(clientIP(r), time.Now()); !ok {
		s.rateLimited(w, wait)
		return
	}
	if code := s.cfg.InviteCode; code != "" {
		if in.Invite == "" {
			Error(w, http.StatusForbidden, "invite_required", "нужен инвайт-код")
			return
		}
		if subtle.ConstantTimeCompare([]byte(code), []byte(in.Invite)) != 1 {
			Error(w, http.StatusForbidden, "invalid_invite", "инвайт-код не подходит")
			return
		}
	}

	cred, err := auth.Hash(key)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	err = s.st.CreateUser(r.Context(), store.User{
		Nick:      in.Nick,
		Cred:      cred,
		PublicKey: public,
		KeyBlob:   in.Blob,
		CreatedAt: time.Now().UnixMilli(),
	})
	if errors.Is(err, store.ErrNickTaken) {
		Error(w, http.StatusConflict, "nick_taken", "ник занят")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if err := s.startSession(w, r, in.Nick); err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Nick string `json:"nick"`
	}{in.Nick})
}

// POST /api/login — вход. Ошибка одна на все случаи: неверный ник,
// неверный authKey и кривая форма неразличимы снаружи.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Nick    string `json:"nick"`
		AuthKey string `json:"authKey"`
	}
	if !decode(w, r, &in) {
		return
	}
	key, ok := authKey(in.AuthKey)
	if !ok || !validNick(in.Nick) {
		invalidCredentials(w)
		return
	}
	// Лимит — 10 за 10 минут на пару IP+ник (ADR-021) — стоит раньше
	// хранилища и argon2: перебор не должен заказывать серверу работу.
	// Ключ ведра собирается из адреса и ника через байт, которого нет
	// ни в том ни в другом.
	if wait, ok := s.logins.take(clientIP(r)+"\x00"+in.Nick, time.Now()); !ok {
		s.rateLimited(w, wait)
		return
	}
	u, err := s.st.User(r.Context(), in.Nick)
	if errors.Is(err, store.ErrNotFound) {
		// Считаем впустую: вход с несуществующим ником не должен
		// отвечать заметно быстрее входа с неверным authKey.
		auth.Waste(key)
		invalidCredentials(w)
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	valid, rehash := auth.Verify(key, u.Cred)
	if !valid {
		invalidCredentials(w)
		return
	}
	if rehash {
		// Параметры отстали от текущих (ADR-021). Не удалось перехешировать —
		// не повод отказывать во входе: старый хеш остаётся рабочим.
		if cred, err := auth.Hash(key); err != nil {
			s.report(r, err)
		} else if err := s.st.SetAuth(r.Context(), u.Nick, cred); err != nil {
			s.report(r, err)
		}
	}
	if err := s.startSession(w, r, u.Nick); err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Nick      string          `json:"nick"`
		PublicKey json.RawMessage `json:"publicKey"`
		Blob      string          `json:"blob"`
	}{u.Nick, json.RawMessage(u.PublicKey), u.KeyBlob})
}

// GET /api/me — кто вошёл.
func (s *server) me(w http.ResponseWriter, r *http.Request) {
	u, ok := s.self(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Nick      string          `json:"nick"`
		PublicKey json.RawMessage `json:"publicKey"`
		CreatedAt int64           `json:"createdAt"`
	}{u.Nick, json.RawMessage(u.PublicKey), u.CreatedAt})
}

// POST /api/logout — выход на этом устройстве.
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	sess, _ := auth.From(r)
	if err := s.st.DeleteSession(r.Context(), sess.TokenHash); err != nil {
		s.internal(w, r, err)
		return
	}
	auth.ClearCookie(w)
	noContent(w)
}

// POST /api/password — смена пароля и повышение итераций: одна операция
// (ADR-015). Хеш и блоб меняются в одной транзакции.
func (s *server) password(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AuthKey      string `json:"authKey"`
		NewAuthKey   string `json:"newAuthKey"`
		Blob         string `json:"blob"`
		LogoutOthers bool   `json:"logoutOthers"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, ok := s.self(w, r)
	if !ok {
		return
	}
	if !s.confirm(w, in.AuthKey, u) {
		return
	}
	newKey, ok := authKey(in.NewAuthKey)
	if !ok {
		Invalid(w, "newAuthKey", "newAuthKey — не 32 байта base64url")
		return
	}
	if _, err := blobIterations(in.Blob); err != nil {
		Invalid(w, "blob", err.Error())
		return
	}
	cred, err := auth.Hash(newKey)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	sess, _ := auth.From(r)
	revoked, err := s.st.SetPassword(r.Context(), u.Nick, cred, in.Blob, in.LogoutOthers, sess.TokenHash)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	// Сессия проверяется при подключении к потоку, а не в его цикле,
	// поэтому отозванная продолжала бы получать конверты до обрыва
	// соединения. Отзыв доступа закрывает поток сам — тем же способом,
	// что и удаление устройства (ADR-058).
	for _, device := range revoked {
		s.hub.Close(device)
	}
	noContent(w)
}

// DELETE /api/me — удаление аккаунта, подтверждённое authKey.
func (s *server) deleteMe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AuthKey string `json:"authKey"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, ok := s.self(w, r)
	if !ok {
		return
	}
	if !s.confirm(w, in.AuthKey, u) {
		return
	}
	// Устройства, сессии, контакты, членство, ключи комнат и очереди уносит
	// каскад; комнаты, где пользователь владелец, меняют владельца или
	// удаляются пустыми (ADR-018) — всё в одной транзакции хранилища.
	changes, err := s.st.DeleteUser(r.Context(), u.Nick)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	// Удаление аккаунта — выход из всех его комнат: оставшимся уходит room
	// с needsRekey, каждому со своим ключом (ADR-041).
	for _, change := range changes {
		s.sendRoom(r, change, "")
	}
	auth.ClearCookie(w)
	noContent(w)
}

// GET /api/users/{nick} — публичный ключ собеседника. Доверие к нему —
// TOFU на клиенте (ADR-016).
func (s *server) user(w http.ResponseWriter, r *http.Request) {
	nick := r.PathValue("nick")
	if !validNick(nick) {
		unknownUser(w)
		return
	}
	u, err := s.st.User(r.Context(), nick)
	if errors.Is(err, store.ErrNotFound) {
		unknownUser(w)
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Nick      string          `json:"nick"`
		PublicKey json.RawMessage `json:"publicKey"`
	}{u.Nick, json.RawMessage(u.PublicKey)})
}

// self читает пользователя сессии. Строки нет — сессия недействительна:
// аккаунт удалён на другом устройстве.
func (s *server) self(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	sess, _ := auth.From(r)
	u, err := s.st.User(r.Context(), sess.Nick)
	if errors.Is(err, store.ErrNotFound) {
		Error(w, http.StatusUnauthorized, "unauthenticated", "нужен вход")
		return store.User{}, false
	}
	if err != nil {
		s.internal(w, r, err)
		return store.User{}, false
	}
	return u, true
}

// confirm проверяет authKey — подтверждение опасной операции.
func (s *server) confirm(w http.ResponseWriter, given string, u store.User) bool {
	key, ok := authKey(given)
	if !ok {
		invalidCredentials(w)
		return false
	}
	if valid, _ := auth.Verify(key, u.Cred); !valid {
		invalidCredentials(w)
		return false
	}
	return true
}

// startSession выдаёт сессию и ставит cookie.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, nick string) error {
	token, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	now := time.Now()
	expires := now.Add(auth.TTL)
	if err := s.st.CreateSession(r.Context(), hash, nick, now.UnixMilli(), expires.UnixMilli()); err != nil {
		return err
	}
	auth.SetCookie(w, token, expires)
	return nil
}

func invalidCredentials(w http.ResponseWriter) {
	Error(w, http.StatusUnauthorized, "invalid_credentials", "неверный ник или пароль")
}

func unknownUser(w http.ResponseWriter) {
	Error(w, http.StatusNotFound, "unknown_user", "такого ника нет")
}
