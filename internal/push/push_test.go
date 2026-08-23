package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xmatic-squad/bare/internal/config"
)

// sender без VAPID-ключей: отправщиков он не заводит, а клиент собирает —
// именно клиент здесь и проверяется.
func client(t *testing.T, local bool) *http.Client {
	t.Helper()
	return New(&config.Config{PushLocal: local}, nil, nil, nil).client
}

// Публичный адрес отличается от того, по которому push-сервиса не бывает
// (ADR-047).
func TestPublicAddress(t *testing.T) {
	cases := []struct {
		addr   string
		public bool
	}{
		{"93.184.216.34", true},
		{"2606:2800:220:1:248:1893:25c8:1946", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.0.0.1", false},
		{"172.16.5.4", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"fe80::1", false},
		{"fc00::1", false},
		{"0.0.0.0", false},
		{"::", false},
		{"224.0.0.1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
	}
	for _, c := range cases {
		ip, err := netip.ParseAddr(c.addr)
		if err != nil {
			t.Fatalf("%s: %v", c.addr, err)
		}
		if got := Public(ip); got != c.public {
			t.Errorf("Public(%s): получено %v, ожидалось %v", c.addr, got, c.public)
		}
	}
}

// Соединения с непубличным адресом не случается: проверка стоит на самом
// dial, поэтому её не обойти ни именем, ни редиректом (ADR-047).
func TestClientRefusesLocalAddress(t *testing.T) {
	got := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- struct{}{}
	}))
	defer srv.Close()

	resp, err := client(t, false).Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("соединение с 127.0.0.1 состоялось")
	}
	if !errors.Is(err, errLocalAddress) {
		t.Errorf("ошибка: %v, ожидался отказ по адресу", err)
	}
	if reason(err) != "адрес подписки не публичный" {
		t.Errorf("класс отказа: %q", reason(err))
	}
	select {
	case <-got:
		t.Error("внутренняя служба получила запрос")
	default:
	}
}

// Редиректы push-сервиса не выполняются: иначе один 307 уводил бы запрос
// вместе с VAPID-заголовком куда угодно, и проверка «endpoint — https»
// не значила бы ничего (ADR-047).
func TestClientDoesNotFollowRedirect(t *testing.T) {
	inside := make(chan struct{}, 1)
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inside <- struct{}{}
		w.WriteHeader(http.StatusGone)
	}))
	defer internal.Close()

	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/latest/meta-data/", http.StatusTemporaryRedirect)
	}))
	defer vendor.Close()

	// local: сами тестовые серверы живут на 127.0.0.1, проверяется здесь
	// именно политика редиректов.
	resp, err := client(t, true).Get(vendor.URL + "/push")
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("статус: получено %d, ожидалось 307", resp.StatusCode)
	}
	select {
	case <-inside:
		t.Error("запрос ушёл по редиректу на внутренний адрес")
	default:
	}
}

// Отказ отправки сводится к классу: адреса подписки в журнале нет
// (docs/deploy.md, «Логи»).
func TestReasonWithoutEndpoint(t *testing.T) {
	const endpoint = "secret-host.push.example"
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("dial: %w", errLocalAddress), "адрес подписки не публичный"},
		{fmt.Errorf("post: %w", context.DeadlineExceeded), "таймаут"},
		{&url.Error{Op: "Post", URL: "https://" + endpoint + "/x",
			Err: &net.DNSError{Err: "no such host", Name: endpoint}}, "имя не разрешилось"},
		{&url.Error{Op: "Post", URL: "https://" + endpoint + "/x",
			Err: &net.OpError{Op: "read", Net: "tcp",
				Addr: &net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 443},
				Err:  errors.New("connection reset by peer")}}, "отправка не удалась"},
	}
	for _, c := range cases {
		got := reason(c.err)
		if got != c.want {
			t.Errorf("reason(%v): получено %q, ожидалось %q", c.err, got, c.want)
		}
		if strings.Contains(got, endpoint) || strings.Contains(got, "10.1.2.3") {
			t.Errorf("адрес подписки попал в журнал: %q", got)
		}
	}
}

// В VAPID-токен уходит субъект ровно в той форме, которую требует
// RFC 8292: у «mailto:» одна схема, а не две. webpush-go приписывает
// «mailto:» всему, что не начинается с «https:», поэтому готовый URI
// приходится отдавать ему без схемы — см. vapidSubscriber. Без этого
// APNs отвечает 403 BadJwtToken на каждый пуш.
func TestVAPIDSubjectInToken(t *testing.T) {
	cases := []struct{ subject, want string }{
		{"mailto:admin@xmatic.team", "mailto:admin@xmatic.team"},
		{"https://bare.xmatic.team", "https://bare.xmatic.team"},
		{"admin@xmatic.team", "mailto:admin@xmatic.team"},
	}
	for _, c := range cases {
		t.Run(c.subject, func(t *testing.T) {
			got := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				select {
				case got <- r.Header.Get("Authorization"):
				default:
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			s, _ := sender(t, c.subject, srv.URL, nil)
			defer s.Close()
			s.Send([]Target{{Device: "d1", Owner: "marta"}}, Payload{Title: "@marta", Chat: "dm:marta"})

			var header string
			select {
			case header = <-got:
			case <-time.After(wait):
				t.Fatal("push-сервис не получил запроса")
			}
			if sub := subClaim(t, header); sub != c.want {
				t.Errorf("sub: получено %q, ожидалось %q", sub, c.want)
			}
		})
	}
}

// Отказ push-сервиса читается по журналу: статус и код причины из тела
// (ADR-064). Без кода 403 от APNs неотличим от любого другого отказа.
func TestServiceStatusInLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"reason":"BadJwtToken"}`)
	}))
	defer srv.Close()

	log := &logbuf{}
	s, d := sender(t, "mailto:admin@xmatic.team", srv.URL, log)
	defer s.Close()
	s.Send([]Target{{Device: "d1", Owner: "marta"}}, Payload{Title: "@marta", Chat: "dm:marta"})

	const want = "пуш: push-сервис ответил 403 (BadJwtToken)"
	deadline := time.Now().Add(wait)
	for !strings.Contains(log.String(), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if line := log.String(); !strings.Contains(line, want) {
		t.Errorf("журнал: %q, ожидалась строка %q", line, want)
	}
	if strings.Contains(log.String(), srv.URL) || strings.Contains(log.String(), "d1") {
		t.Errorf("в журнал попал адрес подписки или устройство: %q", log.String())
	}
	if d.drops() != 0 {
		t.Error("подписка снята по ответу 403")
	}
}

// Код причины берётся из тела ответа, но телом распоряжается чужая
// сторона: всё, что на короткий код не похоже, в журнал не идёт вовсе
// (ADR-064, docs/deploy.md, «Логи»).
func TestServiceReason(t *testing.T) {
	const endpoint = "web.push.apple.com"
	cases := []struct{ body, want string }{
		{`{"reason":"BadJwtToken"}`, "BadJwtToken"},
		{`{"reason":"TooManyRequests"}`, "TooManyRequests"},
		{`{"code":404,"errno":103,"error":"Not Found"}`, "Not Found"},
		{"Unauthorized registration", "Unauthorized registration"},
		{`{"error":{"code":403,"status":"UNAUTHENTICATED"}}`, ""},
		{`{"reason":"unknown push endpoint https://` + endpoint + `/QK"}`, ""},
		{"gone: " + endpoint, ""},
		{`{"message":"subscription ` + endpoint + ` expired"}`, ""},
		{strings.Repeat("A", maxReason+1), ""},
		{"", ""},
		// Многострочное тело: в журнал идёт первая строка, остальное
		// отбрасывается вместе с адресом.
		{"BadJwtToken\nendpoint: " + endpoint, "BadJwtToken"},
	}
	for _, c := range cases {
		got := serviceReason([]byte(c.body))
		if got != c.want {
			t.Errorf("serviceReason(%q): получено %q, ожидалось %q", c.body, got, c.want)
		}
		if strings.Contains(got, endpoint) {
			t.Errorf("адрес подписки попал в журнал: %q", got)
		}
	}
}

// Статус без разобранного кода причины остаётся статусом.
func TestStatusWithoutReason(t *testing.T) {
	if got := status(500, nil); got != "500" {
		t.Errorf("status: получено %q, ожидалось %q", got, "500")
	}
	if got := status(403, []byte(`{"reason":"BadJwtToken"}`)); got != "403 (BadJwtToken)" {
		t.Errorf("status: получено %q, ожидалось %q", got, "403 (BadJwtToken)")
	}
}

// wait — сколько ждём отправку. Всё локально, задержек быть не должно.
const wait = 5 * time.Second

// sender — отправитель с настоящей парой VAPID-ключей и одной подпиской
// на подменный push-сервис. Без ключей отправщики не заводятся.
func sender(t *testing.T, subject, endpoint string, logw io.Writer) (*Sender, *devices) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("vapid: %v", err)
	}
	d := &devices{subscription: subscription(t, endpoint)}
	cfg := &config.Config{
		VAPIDPublic:  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		VAPIDPrivate: base64.RawURLEncoding.EncodeToString(key.Bytes()),
		VAPIDSubject: subject,
		// Подменный push-сервис живёт на 127.0.0.1; в работе отправщик
		// ходит только по публичным адресам (ADR-047).
		PushLocal: true,
	}
	return New(cfg, d, nil, logw), d
}

// subscription — подписка устройства с настоящими ключами: webpush-go
// шифрует ими нагрузку, случайных байт ему мало.
func subscription(t *testing.T, endpoint string) string {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ключ подписки: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("секрет подписки: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"endpoint": endpoint + "/push",
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(auth),
		},
	})
	if err != nil {
		t.Fatalf("подписка: %v", err)
	}
	return string(raw)
}

// subClaim достаёт sub из VAPID-заголовка: «vapid t=<jwt>, k=<ключ>».
// JWT разбирается руками — библиотека здесь и проверяется.
func subClaim(t *testing.T, header string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		t.Fatalf("заголовок не vapid: %q", header)
	}
	token, _, ok := strings.Cut(rest, ",")
	if !ok {
		t.Fatalf("в заголовке нет ключа: %q", header)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("не JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	return claims.Sub
}

// devices — хранилище устройств в тесте: право на пуш даётся всегда,
// возвраты и снятия считаются.
type devices struct {
	subscription string

	mu        sync.Mutex
	released  int
	dropcount int
}

func (d *devices) ClaimPush(context.Context, string) (string, bool, error) {
	return d.subscription, true, nil
}

func (d *devices) ReleasePush(context.Context, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.released++
	return nil
}

func (d *devices) DropPush(context.Context, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropcount++
	return nil
}

func (d *devices) drops() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropcount
}

// logbuf — журнал теста. Пишет в него отправщик, читает тест, поэтому
// с замком.
type logbuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logbuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logbuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
