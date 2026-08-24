// Команда bare: сервер чата одним бинарём.
//
//	bare serve     запустить http-сервер
//	bare vapid     напечатать пару vapid-ключей
//	bare version   напечатать ревизию и время коммита
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xmatic-squad/bare/internal/api"
	"github.com/xmatic-squad/bare/internal/build"
	"github.com/xmatic-squad/bare/internal/config"
	"github.com/xmatic-squad/bare/internal/store"
	"github.com/xmatic-squad/bare/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "vapid":
		err = vapid()
	case "version":
		version()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bare:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `bare — сервер чата

использование:
  bare serve     запустить http-сервер
  bare vapid     напечатать пару vapid-ключей
  bare version   напечатать ревизию и время коммита

настройка — переменные окружения BARE_*, см. docs/deploy.md
`)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	static, err := web.New()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, name := range st.Applied() {
		fmt.Printf("bare применил миграцию %s\n", name)
	}

	h := api.New(cfg, st, static, os.Stdout)
	// Отправщики пушей дописывают начатое и пишут результат в базу, поэтому
	// остановить их надо раньше, чем закроется st. defer выстроен на это:
	// h.Close отложен позже st.Close и выполнится раньше него.
	defer h.Close()

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// OPTIONS * иначе обслуживает net/http сам, в обход middleware:
		// ответ уходил бы без заголовков безопасности (ADR-021).
		DisableGeneralOptionsHandler: true,
		// WriteTimeout не задаётся: впереди SSE с долгими ответами (ADR-004).
	}

	// Потоки событий не заканчиваются сами: без этого Shutdown ждал бы,
	// пока подключённые клиенты уйдут, до самого таймаута (ADR-004).
	// Здесь только закрытие потоков: колбэк крутится в своей горутине,
	// и Shutdown его не дожидается — дождаться отправки пушей отсюда
	// нельзя. Их останавливает h.Close, когда Shutdown уже вернулся
	// и обработчики отработали.
	srv.RegisterOnShutdown(h.CloseStreams)

	// Сначала bind, потом сообщение: строка в журнале означает, что порт занят
	// нами, а не то, что мы собирались его занять.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Фоновая чистка живёт столько же, сколько сервер (docs/storage.md).
	go st.RunCleanup(ctx, func(err error) {
		fmt.Fprintln(os.Stderr, "bare:", err)
	})

	failed := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()
	fmt.Printf("bare слушает %s, origin %s\n", ln.Addr(), cfg.Origin)
	// Молчащие пуши — худший вид поломки: снаружи она не видна вовсе.
	if cfg.VAPIDPublic == "" || cfg.VAPIDPrivate == "" || cfg.VAPIDSubject == "" {
		fmt.Println("bare: пуши выключены — нужны BARE_VAPID_PUBLIC, BARE_VAPID_PRIVATE и BARE_VAPID_SUBJECT")
	}

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	// Второй сигнал больше не перехватываем: он завершает процесс сразу.
	stop()
	fmt.Println("bare завершается")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// vapid печатает пару ключей P-256 в формате, который ждёт webpush-go:
// приватный — 32 байта скаляра, публичный — 65 байт несжатой точки,
// оба base64url без паддинга.
func vapid() error {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	b64 := base64.RawURLEncoding
	fmt.Printf("BARE_VAPID_PUBLIC=%s\n", b64.EncodeToString(priv.PublicKey().Bytes()))
	fmt.Printf("BARE_VAPID_PRIVATE=%s\n", b64.EncodeToString(priv.Bytes()))
	return nil
}

// version печатает, какой код собран в этот бинарь: короткую ревизию
// и время коммита. Время коммита, а не компиляции: штамп момента сборки
// делал бы каждую пересборку одного коммита новым файлом, и сверка хеша
// со сборкой из тега перестала бы что-либо значить (ADR-022, ADR-074).
// Ревизию, которой нет, и время, которого нет, бинарь не выдумывает
// (ADR-057).
func version() {
	info := build.Current()
	line := info.Version()
	if !info.CommitAt.IsZero() {
		line += " " + info.CommitAt.UTC().Format(time.RFC3339)
	}
	fmt.Println(line)
}
