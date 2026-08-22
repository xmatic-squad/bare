// Package push отправляет веб-пуши устройствам (ADR-011, ADR-023).
//
// Пуш — сигнал, а не транспорт: он говорит, что для устройства что-то
// есть, а содержимое устройство забирает очередью при подключении.
// Плейнтекста сервер не знает, поэтому текст пуша — константа, а не поле.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/xmatic-squad/bare/internal/config"
)

// Параметры отправки из ADR-023.
const (
	ttl     = 24 * time.Hour
	urgency = webpush.UrgencyNormal
)

// body — текст пуша. Константа, а не поле полезной нагрузки: сервер
// не знает плейнтекста сообщения и не может положить его в пуш даже
// по ошибке (ADR-011, docs/ui.md, «Уведомления»).
const body = "новое сообщение"

// Пределы отправки (ADR-048). Пуш — побочный эффект доставки, ответа на
// POST /api/messages он не ждёт, но и «выстрелил и забыл» без границ
// не годится: недоступный push-сервис держит соединение до таймаута,
// и без предела такие отправки копились бы горутинами и сокетами,
// пока хватает памяти. Поэтому фиксированная очередь, фиксированное
// число отправщиков и доля одного аккаунта в них.
const (
	workers = 8
	// queueSize — сколько пушей ждут отправщика. Переполнение означает,
	// что push-сервисы не справляются; лишний пуш отбрасывается, а не
	// копится. Потери в этом нет: право на пуш забирается перед самой
	// отправкой, поэтому у отброшенного устройства push_pending остаётся
	// нулём и следующее сообщение попробует снова.
	queueSize = 256
	// perAccount — сколько заданий одного аккаунта бывает в очереди и в
	// работе одновременно. Без этой доли аккаунт с сотней устройств на
	// молчащем эндпоинте занимал бы всех отправщиков, и пуши остальных
	// пользователей отбрасывались бы (ADR-048).
	perAccount = 4
	// requestTimeout — сколько ждём push-сервис. Вендоры отвечают за
	// секунды; всё, что дольше, — уже недоступный сервис, а таймаут
	// на задание задаёт пропускную способность отправки.
	requestTimeout = 5 * time.Second
	// dialTimeout — сколько ждём соединения с push-сервисом.
	dialTimeout = 3 * time.Second
	// storeTimeout — сколько ждём базу, когда правим подписку по итогам
	// отправки.
	storeTimeout = 5 * time.Second
	// dropEvery — как часто в журнал уходит счётчик отброшенных пушей.
	// Строка на каждый отброшенный пуш была бы усилителем заливки
	// журнала: одно сообщение аккаунту с сотней устройств давало бы
	// сотню строк (ADR-048).
	dropEvery = time.Minute
)

// Devices — что отправителю нужно от хранилища. Правило «одно молчащее
// устройство — один пуш» держится на атомарном захвате (ADR-023).
type Devices interface {
	ClaimPush(ctx context.Context, device string) (subscription string, claimed bool, err error)
	ReleasePush(ctx context.Context, device string) error
	DropPush(ctx context.Context, device string) error
}

// Payload — полезная нагрузка пуша (ADR-023). Заголовок — «@nick»
// отправителя или «#имя комнаты», chat — идентификатор чата для
// перехода: «dm:<nick>» или «room:<id>». Текста сообщения здесь нет
// и быть не может.
type Payload struct {
	Title string
	Chat  string
}

// Target — кому нужен пуш: устройство и аккаунт, которому оно
// принадлежит. Аккаунт нужен, чтобы отмерить его долю в отправке
// (ADR-048).
type Target struct {
	Device string
	Owner  string
}

// wire — полезная нагрузка на проводе.
type wire struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Chat  string `json:"chat"`
}

// Sender — очередь отправки и отправщики за ней.
type Sender struct {
	devices Devices
	// connected — держит ли устройство поток событий. Спрашивается
	// в момент захвата права на пуш, а не при постановке в очередь
	// (ADR-023).
	connected func(device string) bool
	public    string
	private   string
	subject   string
	client    *http.Client
	logw      io.Writer

	jobs chan job
	done chan struct{}
	stop sync.Once
	wg   sync.WaitGroup

	mu sync.Mutex
	// share — сколько заданий аккаунта в очереди и в работе.
	share map[string]int
	// dropped — сколько пушей отброшено с прошлой строки в журнале.
	dropped  int
	reported time.Time
}

// job — один пуш: кому, от чьего имени доля и что.
type job struct {
	device  string
	owner   string
	payload []byte
}

// New собирает отправителя. Без полной пары VAPID-ключей и subject пуши
// выключены: отправлять их всё равно нечем (ADR-022, docs/deploy.md).
// Выключенный отправитель не заводит горутин и молча ничего не делает.
//
// connected отвечает, подключено ли устройство по SSE; nil означает
// «никто не подключён».
func New(cfg *config.Config, devices Devices, connected func(device string) bool, logw io.Writer) *Sender {
	if connected == nil {
		connected = func(string) bool { return false }
	}
	s := &Sender{
		devices:   devices,
		connected: connected,
		public:    cfg.VAPIDPublic,
		private:   cfg.VAPIDPrivate,
		subject:   cfg.VAPIDSubject,
		client: &http.Client{
			Timeout: requestTimeout,
			// Push-сервисы редиректов не шлют. Следование за ними
			// означало бы, что проверка «endpoint — https» ничего
			// не значит: один 307 уводит запрос вместе с VAPID-заголовком
			// куда угодно, в том числе на plain http внутрь периметра
			// (ADR-047).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     transport(cfg.PushLocal),
		},
		logw:  logw,
		share: make(map[string]int),
	}
	if !s.on() {
		return s
	}
	s.jobs = make(chan job, queueSize)
	s.done = make(chan struct{})
	s.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go s.work()
	}
	return s
}

// on — есть ли чем подписывать пуши.
func (s *Sender) on() bool {
	return s.public != "" && s.private != "" && s.subject != ""
}

// Send ставит пуш каждому из устройств в очередь отправки и возвращается
// сразу: конверт уже в очереди устройства, ответ на POST /api/messages
// пуша не ждёт (ADR-023).
//
// Заданий одного аккаунта в работе не больше perAccount: лишние
// отбрасываются здесь же, не занимая отправщика (ADR-048).
func (s *Sender) Send(targets []Target, p Payload) {
	if !s.on() || len(targets) == 0 {
		return
	}
	raw, err := json.Marshal(wire{Title: p.Title, Body: body, Chat: p.Chat})
	if err != nil {
		s.report("сборка нагрузки: %v", err)
		return
	}
	for _, t := range targets {
		if !s.reserve(t.Owner) {
			continue
		}
		// У каждого задания своя копия нагрузки: webpush-go дописывает
		// набивку прямо в переданный срез, а одно сообщение уходит сразу
		// нескольким устройствам и в разных отправщиках.
		select {
		case s.jobs <- job{device: t.Device, owner: t.Owner, payload: bytes.Clone(raw)}:
		default:
			s.free(t.Owner)
			s.countDrop()
		}
	}
	s.reportDrops(dropEvery)
}

// Close останавливает отправщиков и дожидается начатых отправок.
func (s *Sender) Close() {
	if !s.on() {
		return
	}
	s.stop.Do(func() { close(s.done) })
	s.wg.Wait()
	s.reportDrops(0)
}

func (s *Sender) work() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case j := <-s.jobs:
			s.deliver(j)
			s.free(j.owner)
		}
	}
}

// deliver забирает право на пуш и отправляет его. Контекст здесь свой:
// запрос, породивший пуш, к этому моменту давно отвечен.
func (s *Sender) deliver(j job) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	// Подключённому устройству пуш не нужен, и права на пуш ему брать
	// нельзя: захваченное право сбрасывается только подключением, и на
	// подключённом устройстве оно провисело бы всю сессию, съев пуш
	// после ухода в офлайн. Поэтому проверка идёт здесь, рядом
	// с захватом, а не при постановке в очередь (ADR-023).
	if s.connected(j.device) {
		return
	}
	subscription, claimed, err := s.devices.ClaimPush(ctx, j.device)
	if err != nil {
		s.report("захват: %v", err)
		return
	}
	// Права нет: устройство без подписки или с неотработанным пушем.
	// Одно молчащее устройство получает один пуш, не ленту (ADR-023).
	if !claimed {
		return
	}
	// Между проверкой и захватом устройство успевает подключиться:
	// подключение сбрасывает право, а мы забрали его следом.
	if s.connected(j.device) {
		s.release(j.device)
		return
	}

	var to webpush.Subscription
	if err := json.Unmarshal([]byte(subscription), &to); err != nil {
		// Подписку в таком виде мог записать только сервер, и всё же:
		// неразбираемая подписка не заработает никогда, снимаем.
		s.report("подписка не разобрана")
		s.drop(j.device)
		return
	}

	resp, err := webpush.SendNotificationWithContext(ctx, j.payload, &to, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.subject,
		VAPIDPublicKey:  s.public,
		VAPIDPrivateKey: s.private,
		TTL:             int(ttl.Seconds()),
		Urgency:         urgency,
	})
	if err != nil {
		s.report("отправка: %s", reason(err))
		s.release(j.device)
		return
	}
	defer resp.Body.Close()
	// Тело ответа push-сервиса нам не нужно, но дочитать его стоит:
	// иначе соединение не переиспользуется.
	io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode < 300:
		// Пуш принят: у устройства висит неотработанный пуш. Если оно
		// успело подключиться, пока шла отправка, право возвращается:
		// подключение сбрасывает его раньше, чем мы поставили.
		if s.connected(j.device) {
			s.release(j.device)
		}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// Подписки больше нет — чистим мёртвую (ADR-011).
		s.drop(j.device)
	default:
		s.report("push-сервис ответил %d", resp.StatusCode)
		s.release(j.device)
	}
}

// reserve занимает долю аккаунта в отправке. Доля израсходована — пуш
// отбрасывается: устройству от этого ничего не грозит, право на пуш
// ещё не забрано (ADR-048).
func (s *Sender) reserve(owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.share[owner] >= perAccount {
		s.dropped++
		return false
	}
	s.share[owner]++
	return true
}

// free возвращает долю аккаунта.
func (s *Sender) free(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.share[owner]; n > 1 {
		s.share[owner] = n - 1
	} else {
		delete(s.share, owner)
	}
}

// countDrop считает отброшенный пуш.
func (s *Sender) countDrop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped++
}

// reportDrops пишет счётчик отброшенных пушей, но не чаще чем раз
// в every (ADR-048).
func (s *Sender) reportDrops(every time.Duration) {
	s.mu.Lock()
	n := s.dropped
	if n == 0 || time.Since(s.reported) < every {
		s.mu.Unlock()
		return
	}
	s.dropped = 0
	s.reported = time.Now()
	s.mu.Unlock()
	s.report("отброшено пушей: %d", n)
}

// release возвращает право на пуш: отправка не состоялась, ждать
// устройству нечего. Контекст здесь свой: отправка могла кончиться
// именно таймаутом, а на просроченном контексте запись не прошла бы
// и push_pending остался бы висеть.
func (s *Sender) release(device string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := s.devices.ReleasePush(ctx, device); err != nil {
		s.report("возврат: %v", err)
	}
}

// drop снимает подписку по той же причине со своим контекстом.
func (s *Sender) drop(device string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := s.devices.DropPush(ctx, device); err != nil {
		s.report("снятие подписки: %v", err)
	}
}

// report пишет строку в журнал. Ни идентификатора устройства, ни адреса
// подписки в ней нет: и то и другое — данные пользователя
// (docs/deploy.md, «Логи»).
func (s *Sender) report(format string, args ...any) {
	if s.logw == nil {
		return
	}
	fmt.Fprintf(s.logw, "%s пуш: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// errLocalAddress — попытка соединиться с непубличным адресом (ADR-047).
var errLocalAddress = errors.New("push: адрес не публичный")

// reason сводит отказ отправки к классу. Текст ошибки транспорта
// в журнал не идёт вовсе: внутри него лежит адрес подписки — host, порт
// или имя, — а это данные пользователя (docs/deploy.md, «Логи»). Класс
// отвечает на вопрос «что чинить», адрес для этого не нужен.
func reason(err error) string {
	if errors.Is(err, errLocalAddress) {
		return "адрес подписки не публичный"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "таймаут"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "имя не разрешилось"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "таймаут сети"
	}
	return "отправка не удалась"
}

// transport — транспорт отправщика. Адрес push-сервиса выбирает браузер
// получателя, а сервер стоит во внутренней сети за nginx (ADR-022):
// без проверки любой вошедший пользователь заставил бы его стучаться
// внутрь периметра. Проверяется адрес соединения, то есть уже
// разрешённое имя, — подмена DNS не помогает (ADR-047).
//
// local снимает проверку и включается только в тестах: настоящий
// push-сервис в них подменён сервером на 127.0.0.1. Из окружения этот
// флаг не читается.
func transport(local bool) http.RoundTripper {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	if !local {
		dialer.Control = onlyPublic
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dialer.DialContext
	return t
}

// onlyPublic отказывает в соединении с непубличным адресом.
func onlyPublic(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errLocalAddress
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errLocalAddress
	}
	if !Public(ip) {
		return errLocalAddress
	}
	return nil
}

// Public — публичный ли адрес. Непубличными считаются loopback,
// link-local, приватные сети (RFC 1918 и RFC 4193), multicast
// и неопределённый адрес: push-сервиса по таким адресам не бывает,
// а внутренние службы бывают (ADR-047).
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	return !ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsInterfaceLocalMulticast() &&
		!ip.IsMulticast() &&
		!ip.IsUnspecified()
}
