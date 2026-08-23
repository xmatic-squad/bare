package api

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/xmatic-squad/bare/internal/auth"
	"github.com/xmatic-squad/bare/internal/push"
)

// Push-подписка принадлежит устройству (ADR-023): её ставит и снимает
// само устройство. Сервер хранит подписку как непрозрачный JSON и лезет
// в неё только при отправке.

// Длины ключей подписки (RFC 8291): p256dh — несжатая точка P-256,
// auth — общий секрет.
const (
	p256dhLen = 65
	authLen   = 16
	// maxEndpoint — предел длины адреса подписки. Адреса вендоров —
	// две-три сотни символов; всё остальное push-сервисом не является,
	// а прочие поля протокола ограничены явно (docs/protocol.md).
	maxEndpoint = 2 << 10
)

// subscriptionIn — объект PushSubscription.toJSON(). Поле expirationTime
// браузеры кладут рядом; сервер его не читает и не хранит — хранится
// ровно то, что нужно для отправки.
type subscriptionIn struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// PUT /api/devices/{id}/push — подписка устройства на пуши. Сбрасывает
// неотработанный пуш: устройство снова готово его принять (ADR-023).
//
// X-Device на этом маршруте обязателен, и устройство в пути тоже обязано
// быть своим: чужому устройству подписку не поставить (docs/protocol.md,
// «Общие правила»).
func (s *server) setPush(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Subscription subscriptionIn `json:"subscription"`
	}
	if !decode(w, r, &in) {
		return
	}
	// Форма проверяется раньше прав (ADR-043).
	subscription, ok := checkSubscription(w, in.Subscription)
	if !ok {
		return
	}
	if _, ok := s.device(w, r); !ok {
		return
	}
	sess, _ := auth.From(r)
	set, err := s.st.SetPush(r.Context(), r.PathValue("id"), sess.Nick, subscription)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !set {
		unknownDevice(w)
		return
	}
	noContent(w)
}

// DELETE /api/devices/{id}/push — снять подписку. Подписки не было —
// тот же 204: снимать нечего. Чужое устройство — 403, как и на PUT.
func (s *server) deletePush(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.device(w, r); !ok {
		return
	}
	sess, _ := auth.From(r)
	cleared, err := s.st.ClearPush(r.Context(), r.PathValue("id"), sess.Nick)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !cleared {
		unknownDevice(w)
		return
	}
	noContent(w)
}

// checkSubscription проверяет форму подписки и отдаёт её канонический
// JSON: три поля и ничего больше. Ответ об ошибке уже написан, если
// вернулось false.
func checkSubscription(w http.ResponseWriter, in subscriptionIn) (string, bool) {
	if !validEndpoint(in.Endpoint) {
		Invalid(w, "subscription", "endpoint — не публичный https-url до 2 КиБ")
		return "", false
	}
	if !pushPoint(in.Keys.P256dh) {
		Invalid(w, "subscription", "keys.p256dh — не точка p-256 в 65 байтах base64url")
		return "", false
	}
	if _, ok := pushKey(in.Keys.Auth, authLen); !ok {
		Invalid(w, "subscription", "keys.auth — не 16 байт base64url")
		return "", false
	}
	out, err := json.Marshal(in)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// validEndpoint — адрес push-сервиса. Выбирает его браузер, сервер знает
// о нём только то, что это абсолютный https-url разумной длины: без TLS
// пуш ушёл бы открытым текстом мимо всех обещаний.
//
// Литеральный непубличный адрес отвергается сразу: push-сервиса по нему
// не бывает, а внутренняя служба бывает (ADR-047). Имя здесь не
// разрешается — за именем всё равно может стоять внутренний адрес,
// поэтому решающая проверка идёт при соединении, в отправщике.
func validEndpoint(raw string) bool {
	if raw == "" || len(raw) > maxEndpoint {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		// Не литерал, а имя: его разберёт отправщик.
		return true
	}
	return push.Public(ip)
}

// pushPoint — p256dh: несжатая точка кривой P-256. Одной длины мало:
// случайные 65 байт точкой не являются, отправка на них падает при
// каждом сообщении, а устройство остаётся с подпиской, которая никогда
// не заработает (docs/protocol.md, «Устройства»).
func pushPoint(s string) bool {
	raw, ok := pushKey(s, p256dhLen)
	if !ok {
		return false
	}
	_, err := ecdh.P256().NewPublicKey(raw)
	return err == nil
}

// pushKey — ключ подписки: ровно n байт base64url. Push API задаёт форму
// без паддинга, но браузер, добавивший паддинг, не должен остаться без
// уведомлений: webpush-go разбирает обе формы, и сервер принимает обе.
func pushKey(s string, n int) ([]byte, bool) {
	if raw, err := b64.DecodeString(s); err == nil {
		return raw, len(raw) == n
	}
	raw, err := base64.URLEncoding.DecodeString(s)
	return raw, err == nil && len(raw) == n
}
