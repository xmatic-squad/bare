package push

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

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
