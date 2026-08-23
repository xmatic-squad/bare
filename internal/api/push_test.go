package api_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xmatic-squad/bare/internal/config"
)

// quiet — сколько ждём, чтобы убедиться, что пуша нет. Всё локально,
// задержек быть не должно.
const quiet = 300 * time.Millisecond

// pushEnv — сервер с настоящей парой VAPID-ключей: без неё пуши выключены
// (docs/deploy.md).
func pushEnv(t *testing.T) *env { return pushEnvWith(t, true) }

// pushEnvWith — то же; local разрешает отправку на 127.0.0.1, где живёт
// подменный push-сервис. Настоящий сервер ходит только по публичным
// адресам (ADR-047), и это проверяется отдельно.
func pushEnvWith(t *testing.T, local bool) *env {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("vapid: %v", err)
	}
	return envWith(t, func(cfg *config.Config) {
		cfg.VAPIDPublic = raw64(key.PublicKey().Bytes())
		cfg.VAPIDPrivate = raw64(key.Bytes())
		cfg.VAPIDSubject = "mailto:bare@bare.test"
		// Push-сервис вендора подменён сервером на 127.0.0.1: в работе
		// отправщик ходит только по публичным адресам (ADR-047).
		cfg.PushLocal = local
	})
}

func raw64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// padded64 — то же, что bytesOf, но с паддингом: браузер вправе прислать
// ключи подписки и в такой форме.
func padded64(n int, seed byte) string {
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

// point65 — p256dh настоящей подписки: несжатая точка P-256 в 65 байтах.
// Случайные байты той же длины точкой не являются, и сервер их не примет
// (docs/protocol.md, «Устройства»).
func point65(t *testing.T) []byte {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ключ подписки: %v", err)
	}
	return key.PublicKey().Bytes()
}

// pushService — push-сервис вендора в тесте. Настоящий FCM тестам не нужен
// и не годится: проверяется, что уходит и что сервер делает с ответом.
type pushService struct {
	t      *testing.T
	url    string
	got    chan delivered
	status atomic.Int32
}

// delivered — то, что увидел push-сервис.
type delivered struct {
	device   string // хвост endpoint: по нему видно, чей это пуш
	ttl      string
	urgency  string
	encoding string
	auth     string
	record   []byte
}

func newPushService(t *testing.T) *pushService {
	t.Helper()
	open := make(chan struct{})
	close(open)
	return pushServiceWith(t, open)
}

// newSlowPushService — push-сервис, который принимает запрос и молчит,
// пока тест не отпустит его. Так видно, что делает сервер, пока отправка
// ещё идёт. Отпускать обязательно: иначе остановка сервера ждёт таймаута.
func newSlowPushService(t *testing.T) (*pushService, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	return pushServiceWith(t, gate), func() { once.Do(func() { close(gate) }) }
}

// pushServiceWith — push-сервис, отвечающий не раньше, чем закроется gate.
func pushServiceWith(t *testing.T, gate <-chan struct{}) *pushService {
	t.Helper()
	p := &pushService{t: t, got: make(chan delivered, 512)}
	p.status.Store(http.StatusCreated)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record, _ := io.ReadAll(r.Body)
		got := delivered{
			device:   strings.TrimPrefix(r.URL.Path, "/push/"),
			ttl:      r.Header.Get("TTL"),
			urgency:  r.Header.Get("Urgency"),
			encoding: r.Header.Get("Content-Encoding"),
			auth:     r.Header.Get("Authorization"),
			record:   record,
		}
		select {
		case p.got <- got:
		default:
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(int(p.status.Load()))
	}))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

// next — следующий пуш; его отсутствие — ошибка теста.
func (p *pushService) next() delivered {
	p.t.Helper()
	select {
	case got := <-p.got:
		return got
	case <-time.After(wait):
		p.t.Fatal("пуш не пришёл")
	}
	return delivered{}
}

// silent требует, чтобы других пушей не было.
func (p *pushService) silent() {
	p.t.Helper()
	select {
	case got := <-p.got:
		p.t.Fatalf("лишний пуш устройству %s", got.device)
	case <-time.After(quiet):
	}
}

// subscriber — устройство с push-подпиской. Ключи настоящие: тест
// расшифровывает пуш ровно так, как это сделал бы браузер (RFC 8291),
// и потому видит, что в нём лежит.
type subscriber struct {
	device string
	key    *ecdh.PrivateKey
	auth   []byte
}

// subscribe кладёт подписку устройства прямо в базу. Через PUT её сюда
// не поставить: тестовый push-сервис живёт на http, а эндпоинт принимает
// только https. Форму подписки проверяют TestPushSubscription
// и TestPushSubscriptionForm, правила отправки от неё не зависят.
func (e *env) subscribe(nick, device string, p *pushService) *subscriber {
	e.t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatalf("ключ подписки: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		e.t.Fatalf("секрет подписки: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"endpoint": p.url + "/push/" + device,
		"keys": map[string]string{
			"p256dh": raw64(key.PublicKey().Bytes()),
			"auth":   raw64(auth),
		},
	})
	if err != nil {
		e.t.Fatalf("подписка: %v", err)
	}
	set, err := e.st.SetPush(context.Background(), device, nick, string(raw))
	if err != nil || !set {
		e.t.Fatalf("SetPush: %v (поставлена: %v)", err, set)
	}
	return &subscriber{device: device, key: key, auth: auth}
}

// open расшифровывает пуш: aes128gcm по RFC 8291, как это делает браузер.
// Без расшифровки нельзя утверждать, что в пуше нет ничего лишнего.
func (s *subscriber) open(t *testing.T, record []byte) map[string]string {
	t.Helper()
	check := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// Заголовок записи: соль, размер записи, длина открытого ключа.
	const header = 16 + 4 + 1
	if len(record) < header {
		t.Fatalf("запись короче заголовка: %d байт", len(record))
	}
	salt := record[:16]
	keyLen := int(record[20])
	if len(record) < header+keyLen {
		t.Fatalf("запись короче ключа отправителя: %d байт", len(record))
	}
	sender, ct := record[header:header+keyLen], record[header+keyLen:]

	remote, err := ecdh.P256().NewPublicKey(sender)
	check("ключ отправителя", err)
	shared, err := s.key.ECDH(remote)
	check("ecdh", err)

	info := append([]byte("WebPush: info\x00"), s.key.PublicKey().Bytes()...)
	info = append(info, sender...)
	ikm, err := hkdf.Key(sha256.New, shared, s.auth, string(info), 32)
	check("ikm", err)
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	check("ключ записи", err)
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	check("nonce", err)

	block, err := aes.NewCipher(cek)
	check("aes", err)
	gcm, err := cipher.NewGCM(block)
	check("gcm", err)
	plain, err := gcm.Open(nil, nonce, ct, nil)
	check("расшифровка", err)

	// Хвост записи — набивка: нули после разделителя 0x02.
	plain = bytes.TrimSuffix(bytes.TrimRight(plain, "\x00"), []byte{2})
	var out map[string]string
	if err := json.Unmarshal(plain, &out); err != nil {
		t.Fatalf("нагрузка %q: %v", plain, err)
	}
	return out
}

// hasPush — что о подписке устройства говорит GET /api/devices.
func (e *env) hasPush(c *http.Cookie, device string) bool {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/devices", nil, with(c))
	expect(e.t, rec, http.StatusOK, "")
	var list []struct {
		ID      string `json:"id"`
		HasPush bool   `json:"hasPush"`
	}
	decodeBody(e.t, rec, &list)
	for _, got := range list {
		if got.ID == device {
			return got.HasPush
		}
	}
	e.t.Fatalf("устройства %s нет в списке", device)
	return false
}

// waitPushGone ждёт, пока подписка исчезнет: снимает её отправщик, уже
// после того, как push-сервис ответил.
func (e *env) waitPushGone(c *http.Cookie, device string) {
	e.t.Helper()
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); {
		if !e.hasPush(c, device) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("подписка устройства %s не снята", device)
}

// send — обычная отправка личного сообщения.
func (e *env) send(c *http.Cookie, device, to string, seed byte) {
	e.t.Helper()
	expect(e.t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), seed), to),
		with(c), withDevice(device)), http.StatusAccepted, "")
}

// pushEventually шлёт сообщения, пока не придёт пуш. И разрыв потока,
// и возврат права на пуш случаются после ответа на запрос: момент их
// наступления не назначить, поэтому попытка повторяется.
func (e *env) pushEventually(p *pushService, c *http.Cookie, device, to string) delivered {
	e.t.Helper()
	for i := 0; i < 8; i++ {
		e.send(c, device, to, byte(50+i))
		select {
		case got := <-p.got:
			return got
		case <-time.After(200 * time.Millisecond):
		}
	}
	e.t.Fatal("пуш так и не пришёл")
	return delivered{}
}

// subscription — тело PUT /api/devices/{id}/push в форме
// PushSubscription.toJSON().
func subscription(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"endpoint":       "https://push.example/one",
		"expirationTime": nil,
		"keys":           map[string]string{"p256dh": raw64(point65(t)), "auth": bytesOf(16, 7)},
	}
}

// Подписка ставится и снимается, hasPush честный, чужое устройство — 403
// (docs/protocol.md, «Устройства», «Общие правила»).
func TestPushSubscription(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	body := map[string]any{"subscription": subscription(t)}

	if e.hasPush(marta, m1) {
		t.Error("hasPush до подписки: true")
	}
	expect(t, e.do(http.MethodPut, "/api/devices/"+m1+"/push", body, with(marta), withDevice(m1)),
		http.StatusNoContent, "")
	if !e.hasPush(marta, m1) {
		t.Error("hasPush после подписки: false")
	}
	// Подписка принадлежит устройству: у соседа её не появилось.
	if e.hasPush(petya, p1) {
		t.Error("подписка досталась чужому устройству")
	}

	expect(t, e.do(http.MethodDelete, "/api/devices/"+m1+"/push", nil, with(marta), withDevice(m1)),
		http.StatusNoContent, "")
	if e.hasPush(marta, m1) {
		t.Error("hasPush после снятия: true")
	}
	// Снимать нечего — тот же 204.
	expect(t, e.do(http.MethodDelete, "/api/devices/"+m1+"/push", nil, with(marta), withDevice(m1)),
		http.StatusNoContent, "")

	// Чужое устройство в пути — 403, и подписки у него не появилось.
	expect(t, e.do(http.MethodPut, "/api/devices/"+p1+"/push", body, with(marta), withDevice(m1)),
		http.StatusForbidden, "unknown_device")
	expect(t, e.do(http.MethodDelete, "/api/devices/"+p1+"/push", nil, with(marta), withDevice(m1)),
		http.StatusForbidden, "unknown_device")
	if e.hasPush(petya, p1) {
		t.Error("подписка поставлена чужому устройству")
	}

	// X-Device обязателен и обязан быть своим.
	for _, opts := range [][]func(*http.Request){
		{with(marta)},
		{with(marta), withDevice(p1)},
		{with(marta), withDevice("мусор")},
		{with(marta), withDevice(deviceOf(9))},
	} {
		expect(t, e.do(http.MethodPut, "/api/devices/"+m1+"/push", body, opts...),
			http.StatusForbidden, "unknown_device")
		expect(t, e.do(http.MethodDelete, "/api/devices/"+m1+"/push", nil, opts...),
			http.StatusForbidden, "unknown_device")
	}
	if e.hasPush(marta, m1) {
		t.Error("подписка появилась после отказа")
	}
}

// Форма подписки: абсолютный https-адрес и два ключа нужной длины.
func TestPushSubscriptionForm(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	_, p1 := e.join("petya", 2)

	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"нет endpoint", func(s map[string]any) { delete(s, "endpoint") }},
		{"endpoint без tls", func(s map[string]any) { s["endpoint"] = "http://push.example/one" }},
		{"endpoint без хоста", func(s map[string]any) { s["endpoint"] = "https:///one" }},
		{"endpoint не url", func(s map[string]any) { s["endpoint"] = "какой же это url" }},
		{"нет ключей", func(s map[string]any) { delete(s, "keys") }},
		{"endpoint на loopback", func(s map[string]any) { s["endpoint"] = "https://127.0.0.1:9/push" }},
		{"endpoint на link-local", func(s map[string]any) {
			s["endpoint"] = "https://169.254.169.254/latest/meta-data/"
		}},
		{"endpoint в приватной сети", func(s map[string]any) { s["endpoint"] = "https://10.0.0.1/push" }},
		{"endpoint на ::1", func(s map[string]any) { s["endpoint"] = "https://[::1]:8411/api/me" }},
		{"endpoint длиннее 2 КиБ", func(s map[string]any) {
			s["endpoint"] = "https://push.example/" + strings.Repeat("a", 2048)
		}},
		{"p256dh не 65 байт", func(s map[string]any) {
			s["keys"] = map[string]string{"p256dh": bytesOf(32, 5), "auth": bytesOf(16, 7)}
		}},
		{"p256dh не точка на кривой", func(s map[string]any) {
			s["keys"] = map[string]string{"p256dh": bytesOf(65, 5), "auth": bytesOf(16, 7)}
		}},
		{"auth не 16 байт", func(s map[string]any) {
			s["keys"] = map[string]string{"p256dh": bytesOf(65, 5), "auth": bytesOf(32, 7)}
		}},
		{"ключ не base64url", func(s map[string]any) {
			s["keys"] = map[string]string{"p256dh": strings.Repeat("!", 87), "auth": bytesOf(16, 7)}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub := subscription(t)
			c.change(sub)
			rec := e.do(http.MethodPut, "/api/devices/"+m1+"/push",
				map[string]any{"subscription": sub}, with(marta), withDevice(m1))
			expect(t, rec, http.StatusBadRequest, "invalid")
			var field struct {
				Field string `json:"field"`
			}
			decodeBody(t, rec, &field)
			if field.Field != "subscription" {
				t.Errorf("field: получено %q, ожидалось \"subscription\"", field.Field)
			}
		})
	}
	if e.hasPush(marta, m1) {
		t.Error("подписка не по форме поставилась")
	}

	// Паддинг в base64url тоже принимается: браузер вправе его добавить.
	padded := subscription(t)
	padded["keys"] = map[string]string{
		"p256dh": base64.URLEncoding.EncodeToString(point65(t)),
		"auth":   padded64(16, 7),
	}
	expect(t, e.do(http.MethodPut, "/api/devices/"+m1+"/push",
		map[string]any{"subscription": padded}, with(marta), withDevice(m1)), http.StatusNoContent, "")

	// Форма проверяется раньше прав: на запрос к чужому устройству
	// приходит отказ по форме, а не по правам (ADR-043).
	expect(t, e.do(http.MethodPut, "/api/devices/"+p1+"/push", "не json", with(marta), withDevice(m1)),
		http.StatusBadRequest, "bad_json")
	broken := subscription(t)
	broken["endpoint"] = "http://push.example/one"
	expect(t, e.do(http.MethodPut, "/api/devices/"+p1+"/push",
		map[string]any{"subscription": broken}, with(marta), withDevice(m1)), http.StatusBadRequest, "invalid")
}

// Пуш уходит только отключённому устройству с подпиской (ADR-023).
// Он несёт заголовок, «новое сообщение» и адрес чата — и ничего больше.
func TestPushToSilentDevice(t *testing.T) {
	svc := newPushService(t)
	e := pushEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	p2 := e.addDevice(petya, deviceOf(3))
	e.subscribe("petya", p1, svc)           // подключено по SSE
	silent := e.subscribe("petya", p2, svc) // молчит

	stream := e.open(p1, petya)
	stream.untilReady()
	e.send(marta, m1, "petya", 4)

	got := svc.next()
	if got.device != p2 {
		t.Fatalf("пуш ушёл устройству %s, ожидалось %s", got.device, p2)
	}
	// TTL сутки, urgency normal (ADR-023).
	if got.ttl != "86400" {
		t.Errorf("TTL: получено %q, ожидалось \"86400\"", got.ttl)
	}
	if got.urgency != "normal" {
		t.Errorf("Urgency: получено %q, ожидалось \"normal\"", got.urgency)
	}
	if got.encoding != "aes128gcm" {
		t.Errorf("Content-Encoding: получено %q, ожидалось \"aes128gcm\"", got.encoding)
	}
	if !strings.HasPrefix(got.auth, "vapid t=") {
		t.Errorf("Authorization: получено %q, ожидался vapid", got.auth)
	}

	payload := silent.open(t, got.record)
	if len(payload) != 3 {
		t.Errorf("поля нагрузки: %v", payload)
	}
	if payload["title"] != "@marta" {
		t.Errorf("title: получено %q, ожидалось \"@marta\"", payload["title"])
	}
	if payload["body"] != "новое сообщение" {
		t.Errorf("body: получено %q, ожидалось \"новое сообщение\"", payload["body"])
	}
	if payload["chat"] != "dm:marta" {
		t.Errorf("chat: получено %q, ожидалось \"dm:marta\"", payload["chat"])
	}
	// Шифротекста сообщения в пуше нет ни в каком виде: сервер его
	// не пересылает, а плейнтекста он и не знает (ADR-011).
	if bytes.Contains(got.record, []byte(bytesOf(48, 23))) {
		t.Error("шифротекст сообщения попал в пуш")
	}
	svc.silent()
}

// Одно молчащее устройство получает один пуш, а не ленту (ADR-023).
func TestPushOncePerSilentDevice(t *testing.T) {
	svc := newPushService(t)
	e := pushEnv(t)
	marta, m1 := e.join("marta", 1)
	_, p1 := e.join("petya", 2)
	e.subscribe("petya", p1, svc)

	e.send(marta, m1, "petya", 3)
	if got := svc.next(); got.device != p1 {
		t.Fatalf("пуш ушёл устройству %s, ожидалось %s", got.device, p1)
	}
	// Второе и третье сообщение подряд пуша не порождают.
	e.send(marta, m1, "petya", 4)
	e.send(marta, m1, "petya", 5)
	svc.silent()
}

// Подключение по SSE сбрасывает неотработанный пуш: следующее сообщение
// молчащему устройству снова даёт пуш (ADR-023).
func TestPushAgainAfterStream(t *testing.T) {
	svc := newPushService(t)
	e := pushEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	e.subscribe("petya", p1, svc)

	e.send(marta, m1, "petya", 3)
	svc.next()
	e.send(marta, m1, "petya", 4)
	svc.silent()

	stream := e.open(p1, petya)
	stream.untilReady()
	stream.close()

	if got := e.pushEventually(svc, marta, m1, "petya"); got.device != p1 {
		t.Fatalf("пуш ушёл устройству %s, ожидалось %s", got.device, p1)
	}
}

// 404 и 410 от push-сервиса означают, что подписки больше нет (ADR-011).
func TestPushDeadSubscription(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			svc := newPushService(t)
			svc.status.Store(int32(status))
			e := pushEnv(t)
			marta, m1 := e.join("marta", 1)
			petya, p1 := e.join("petya", 2)
			e.subscribe("petya", p1, svc)

			e.send(marta, m1, "petya", 3)
			svc.next()
			e.waitPushGone(petya, p1)
		})
	}
}

// Прочие отказы push-сервиса подписку не трогают и доставку сообщения
// не роняют. Право на пуш при этом возвращается: иначе одна ошибка
// затыкала бы уведомления устройства до самого подключения.
func TestPushServiceError(t *testing.T) {
	svc := newPushService(t)
	svc.status.Store(http.StatusInternalServerError)
	e := pushEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	e.subscribe("petya", p1, svc)

	e.send(marta, m1, "petya", 3)
	svc.next()
	if !e.hasPush(petya, p1) {
		t.Error("подписка снята по ответу 500")
	}

	svc.status.Store(http.StatusCreated)
	if got := e.pushEventually(svc, marta, m1, "petya"); got.device != p1 {
		t.Fatalf("пуш ушёл устройству %s, ожидалось %s", got.device, p1)
	}
	if !e.hasPush(petya, p1) {
		t.Error("подписка снята после успешного пуша")
	}
}

// Отправитель пуша о собственном сообщении не получает — ни на то
// устройство, с которого писал, ни на остальные свои (ADR-045).
func TestPushNotToSender(t *testing.T) {
	svc := newPushService(t)
	e := pushEnv(t)
	marta, m1 := e.join("marta", 1)
	m2 := e.addDevice(marta, deviceOf(2))
	_, p1 := e.join("petya", 3)
	e.subscribe("marta", m1, svc)
	e.subscribe("marta", m2, svc)
	to := e.subscribe("petya", p1, svc)

	e.send(marta, m1, "petya", 4)

	got := svc.next()
	if got.device != p1 {
		t.Fatalf("пуш ушёл устройству отправителя %s", got.device)
	}
	svc.silent()
	if payload := to.open(t, got.record); payload["chat"] != "dm:marta" {
		t.Errorf("chat: получено %q, ожидалось \"dm:marta\"", payload["chat"])
	}
}

// Пуш из комнаты: заголовок — имя комнаты, адрес чата — её идентификатор
// (ADR-023).
func TestPushFromRoom(t *testing.T) {
	svc := newPushService(t)
	e := pushEnv(t)
	marta, m1 := e.join("marta", 1)
	_, p1 := e.join("petya", 2)
	room := e.makeRoom(marta, "marta", "общая", 40, withDevice(m1))
	expect(t, e.changeMembers(marta, room.ID, []string{"petya"}, nil, []string{"marta", "petya"}, 41),
		http.StatusOK, "")
	member := e.subscribe("petya", p1, svc)

	expect(t, e.do(http.MethodPost, "/api/messages",
		roomMessage(ulid(nowMillis(), 5), room.ID, keyID(41)), with(marta), withDevice(m1)),
		http.StatusAccepted, "")

	got := svc.next()
	if got.device != p1 {
		t.Fatalf("пуш ушёл устройству %s, ожидалось %s", got.device, p1)
	}
	payload := member.open(t, got.record)
	if payload["title"] != "#общая" {
		t.Errorf("title: получено %q, ожидалось \"#общая\"", payload["title"])
	}
	if payload["chat"] != "room:"+room.ID {
		t.Errorf("chat: получено %q, ожидалось %q", payload["chat"], "room:"+room.ID)
	}
	if payload["body"] != "новое сообщение" {
		t.Errorf("body: получено %q", payload["body"])
	}
	svc.silent()
}

// Без VAPID-ключей пуши выключены: подписка ставится, отправки нет.
func TestPushOffWithoutKeys(t *testing.T) {
	svc := newPushService(t)
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	_, p1 := e.join("petya", 2)
	e.subscribe("petya", p1, svc)

	e.send(marta, m1, "petya", 3)
	svc.silent()
}

// Аккаунт с молчащим push-сервисом не отбирает отправку у остальных:
// доля одного аккаунта в отправщиках ограничена (ADR-048).
func TestPushShareBetweenAccounts(t *testing.T) {
	e := pushEnv(t)
	stuck, release := newSlowPushService(t)
	defer release()
	live := newPushService(t)

	marta, m1 := e.join("marta", 1)
	greedy, g1 := e.join("greedy", 2)
	e.subscribe("greedy", g1, stuck)
	for seed := byte(10); seed < 30; seed++ {
		e.subscribe("greedy", e.addDevice(greedy, deviceOf(seed)), stuck)
	}
	_, c1 := e.join("carol", 3)
	e.subscribe("carol", c1, live)

	// Двадцать одно молчащее устройство одного аккаунта: часть заданий
	// отбрасывается сразу, остальные занимают не больше своей доли.
	e.send(marta, m1, "greedy", 40)
	e.send(marta, m1, "carol", 41)

	select {
	case got := <-live.got:
		if got.device != c1 {
			t.Fatalf("пуш ушёл устройству %s, ожидалось %s", got.device, c1)
		}
	case <-time.After(wait):
		t.Fatal("пуш постороннему аккаунту не ушёл: отправщики заняты чужим")
	}
}

// Устройство, подключившееся по SSE во время отправки, не остаётся
// с неотработанным пушем: право возвращается, и следующее сообщение
// после ухода в офлайн снова даёт пуш (ADR-023).
func TestPushReleasedWhenDeviceConnects(t *testing.T) {
	e := pushEnv(t)
	svc, release := newSlowPushService(t)
	defer release()

	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	e.subscribe("petya", p1, svc)

	e.send(marta, m1, "petya", 3)
	// Отправка уже началась: push-сервис получил запрос и держит его.
	if got := svc.next(); got.device != p1 {
		t.Fatalf("пуш ушёл устройству %s, ожидалось %s", got.device, p1)
	}
	// Пока пуш в пути, устройство подключилось: подключение сбрасывает
	// неотработанный пуш, а отправщик поставил его позже.
	stream := e.open(p1, petya)
	stream.untilReady()
	release()

	// Право на пуш свободно: захват удаётся.
	for deadline := time.Now().Add(wait); ; {
		claimed, ok, err := e.st.ClaimPush(context.Background(), p1)
		if err != nil {
			t.Fatalf("ClaimPush: %v", err)
		}
		if ok {
			if claimed == "" {
				t.Error("подписка пуста")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("неотработанный пуш остался висеть на подключённом устройстве")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Пуш на непубличный адрес не уходит вовсе: соединения не случается,
// право на пуш возвращается, а адрес подписки в журнал не попадает
// (ADR-047, docs/deploy.md, «Логи»).
func TestPushSkipsLocalEndpoint(t *testing.T) {
	svc := newPushService(t)
	e := pushEnvWith(t, false)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	e.subscribe("petya", p1, svc)

	e.send(marta, m1, "petya", 3)
	svc.silent()

	if !e.hasPush(petya, p1) {
		t.Error("подписка снята, хотя push-сервис не отвечал")
	}
	// Право на пуш вернулось: следующее сообщение попробует снова.
	claimed, ok, err := e.st.ClaimPush(context.Background(), p1)
	if err != nil {
		t.Fatalf("ClaimPush: %v", err)
	}
	if !ok || claimed == "" {
		t.Error("право на пуш осталось захваченным")
	}
	log := e.log.String()
	if !strings.Contains(log, "адрес подписки не публичный") {
		t.Errorf("в журнале нет причины отказа: %q", log)
	}
	if strings.Contains(log, "127.0.0.1") || strings.Contains(log, strings.TrimPrefix(svc.url, "http://")) {
		t.Errorf("адрес подписки попал в журнал: %q", log)
	}
}

// Одно сообщение — несколько молчащих устройств: каждое получает свою
// расшифровываемую нагрузку. Нагрузка на всех одна (ADR-045), но
// шифруется она для каждой подписки отдельно.
func TestPushPayloadPerDevice(t *testing.T) {
	svc := newPushService(t)
	e := pushEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	p2 := e.addDevice(petya, deviceOf(3))
	p3 := e.addDevice(petya, deviceOf(4))
	subs := map[string]*subscriber{
		p1: e.subscribe("petya", p1, svc),
		p2: e.subscribe("petya", p2, svc),
		p3: e.subscribe("petya", p3, svc),
	}

	e.send(marta, m1, "petya", 5)
	seen := make(map[string]bool)
	for i := 0; i < len(subs); i++ {
		got := svc.next()
		to, ok := subs[got.device]
		if !ok {
			t.Fatalf("пуш ушёл неизвестному устройству %s", got.device)
		}
		if seen[got.device] {
			t.Fatalf("устройство %s получило второй пуш", got.device)
		}
		seen[got.device] = true
		payload := to.open(t, got.record)
		if payload["title"] != "@marta" || payload["chat"] != "dm:marta" || payload["body"] != "новое сообщение" {
			t.Errorf("нагрузка устройства %s: %v", got.device, payload)
		}
	}
	svc.silent()
}

// Остановка обработчика дожидается начатых отправок. Отправщик пишет
// результат в базу, поэтому закрывать её раньше нельзя, а колбэк
// http.Server.RegisterOnShutdown для этого не годится: сервер запускает
// его в своей горутине и ничего не ждёт. Потоки событий там закрывает
// CloseStreams, отправку останавливает Close — после Shutdown.
func TestCloseWaitsForPush(t *testing.T) {
	svc, release := newSlowPushService(t)
	e := pushEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, p1 := e.join("petya", 2)
	e.subscribe("petya", p1, svc)
	_ = petya

	e.send(marta, m1, "petya", 5)
	// Отправка началась и висит на медленном сервисе.
	svc.next()

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.h.Close()
	}()
	select {
	case <-done:
		t.Fatal("остановка не дождалась начатой отправки")
	case <-time.After(quiet):
	}

	release()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("остановка не закончилась после отправки")
	}
}
