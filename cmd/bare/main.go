// Команда bare: сервер чата одним бинарём.
//
//	bare serve     запустить http-сервер
//	bare vapid     напечатать пару vapid-ключей
//	bare version   напечатать ревизию сборки
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
	"runtime/debug"
	"syscall"
	"time"

	"github.com/xmatic-squad/bare/internal/api"
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
  bare version   напечатать ревизию сборки

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
	srv.RegisterOnShutdown(h.Close)

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

func version() {
	fmt.Println(revision())
}

func revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return "unknown"
}
