// Package web отдаёт клиентскую статику из embed (ADR-025).
//
// Файлы читаются в память один раз при старте: их немного и они неизменны.
// Ни листинга каталогов, ни доступа к файловой системе сервера здесь нет —
// отдаётся только то, что вкомпилировано в бинарь.
package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	bare "github.com/xmatic-squad/bare"
	"github.com/xmatic-squad/bare/internal/api"
)

// Handler — карта «путь в URL → файл».
type Handler struct {
	files map[string]file
}

type file struct {
	data  []byte
	etag  string // сильный, SHA-256 содержимого, в кавычках
	ctype string
}

// New читает web/ из embed и готовит ответы.
func New() (*Handler, error) {
	root, err := fs.Sub(bare.Web, "web")
	if err != nil {
		return nil, err
	}
	h := &Handler{files: make(map[string]file)}
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		h.files["/"+p] = file{
			data:  data,
			etag:  `"` + hex.EncodeToString(sum[:]) + `"`,
			ctype: contentType(path.Ext(p)),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	index, ok := h.files["/index.html"]
	if !ok {
		return nil, errors.New("web: в embed нет index.html")
	}
	h.files["/"] = index
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Статика читается только чтением. Для прочих методов путь считается
	// неизвестным: кода 405 в протоколе нет (ADR-026).
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		api.NotFound(w)
		return
	}
	f, ok := h.files[r.URL.Path]
	if !ok {
		api.NotFound(w)
		return
	}

	head := w.Header()
	head.Set("ETag", f.etag)
	head.Set("Cache-Control", "no-cache")
	if r.URL.Path == "/sw.js" {
		head.Set("Service-Worker-Allowed", "/")
	}
	if match(r.Header.Get("If-None-Match"), f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	head.Set("Content-Type", f.ctype)
	head.Set("Content-Length", strconv.Itoa(len(f.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(f.data)
}

// match разбирает If-None-Match: список тегов, «*» или слабые формы W/"...".
func match(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

func contentType(ext string) string {
	switch ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}
