// Package build отвечает на вопрос «какой код сейчас работает»: ревизия
// и время коммита, которые git оставляет в бинаре при сборке (ADR-074).
// Момента компиляции здесь нет и не будет: штамп времени сборки делал бы
// каждую пересборку одного коммита новым файлом, а подлинность бинаря
// проверяется сравнением хеша со сборкой из тега (ADR-022).
package build

import (
	"runtime/debug"
	"time"
)

// short — сколько символов ревизии показываются человеку и клиенту.
// Полный хеш не добавляет ничего: коммит опознаётся и по семи.
const short = 7

// unknown — ревизии нет вовсе (ADR-057).
const unknown = "unknown"

// Info — что бинарь знает о своём происхождении. Нулевое значение —
// сборка не из git: так выглядят `go run`, `go build -buildvcs=false`
// и тестовый бинарь.
type Info struct {
	Revision string    // vcs.revision — полный хеш коммита; пусто, если неизвестен
	Modified bool      // vcs.modified — дерево при сборке было изменено (ADR-057)
	CommitAt time.Time // vcs.time — время коммита; нулевое, если неизвестно
}

// current читается один раз: у собранного бинаря это неизменная величина.
var current = read(debug.ReadBuildInfo())

// Current — сведения о текущей сборке.
func Current() Info { return current }

// Version — ревизия строкой: семь символов хеша, «+dirty» у сборки
// из изменённого дерева (ADR-057), «unknown» без ревизии вовсе.
func (i Info) Version() string {
	if i.Revision == "" {
		return unknown
	}
	v := i.Revision
	if len(v) > short {
		v = v[:short]
	}
	if i.Modified {
		v += "+dirty"
	}
	return v
}

// CommitMilli — время коммита в миллисекундах Unix, 0 если его нет.
// Проверка отдельная: UnixMilli нулевого времени даёт не ноль, а большое
// отрицательное число.
func (i Info) CommitMilli() int64 {
	if i.CommitAt.IsZero() {
		return 0
	}
	return i.CommitAt.UnixMilli()
}

// read разбирает build info. Отдельная функция принимает результат
// debug.ReadBuildInfo как есть: только так поведение без build info
// и с неразбираемыми значениями проверяется тестом — тестовому бинарю
// vcs.* не проставляются (ADR-057).
func read(info *debug.BuildInfo, ok bool) Info {
	if !ok || info == nil {
		return Info{}
	}
	var out Info
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			out.Revision = s.Value
		case "vcs.modified":
			out.Modified = s.Value == "true"
		case "vcs.time":
			// Неразобранное время равносильно его отсутствию: соврать
			// о нём хуже, чем промолчать.
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				out.CommitAt = t
			}
		}
	}
	return out
}
