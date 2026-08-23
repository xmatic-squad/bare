package web_test

import (
	"io/fs"
	"regexp"
	"testing"

	bare "github.com/xmatic-squad/bare"
)

// shellRe — массив SHELL из web/sw.js: перечень оболочки списком строк.
var shellRe = regexp.MustCompile(`(?s)const SHELL = \[(.*?)\];`)

var pathRe = regexp.MustCompile(`"([^"]+)"`)

// Оболочка в sw.js перечислена вручную (у Cache API нет масок), и забытый
// в ней файл ломает только офлайн — молча. Поэтому список сверяется
// с содержимым embed: оболочка — всё из web/, кроме самого sw.js;
// index.html лежит в кэше под адресом «/» (ADR-023).
func TestShellCoversStatic(t *testing.T) {
	root, err := fs.Sub(bare.Web, "web")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	worker, err := fs.ReadFile(root, "sw.js")
	if err != nil {
		t.Fatalf("sw.js: %v", err)
	}
	block := shellRe.FindSubmatch(worker)
	if block == nil {
		t.Fatal("в sw.js нет массива SHELL")
	}
	shell := make(map[string]bool)
	for _, m := range pathRe.FindAllSubmatch(block[1], -1) {
		shell[string(m[1])] = true
	}

	want := make(map[string]bool)
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir(), p == "sw.js":
			// Обновление воркера ведёт браузер, в кэш он не кладётся.
			return nil
		case p == "index.html":
			want["/"] = true
		default:
			want["/"+p] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход embed: %v", err)
	}

	for p := range want {
		if !shell[p] {
			t.Errorf("%s есть в web/, но не в SHELL: офлайн он не откроется", p)
		}
	}
	for p := range shell {
		if !want[p] {
			t.Errorf("%s есть в SHELL, но не в web/: install воркера упадёт целиком", p)
		}
	}
}
