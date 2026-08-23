package build

import (
	"runtime/debug"
	"testing"
	"time"
)

const hash = "9f2c1ab7d3e4c5061728394a5b6c7d8e9f001122"

// Формат версии: семь символов ревизии, «+dirty» у изменённого дерева,
// «unknown» без ревизии (ADR-057, ADR-065).
func TestVersion(t *testing.T) {
	cases := []struct {
		info Info
		want string
	}{
		{Info{Revision: hash}, "9f2c1ab"},
		{Info{Revision: hash, Modified: true}, "9f2c1ab+dirty"},
		// Ревизия короче семи символов остаётся как есть.
		{Info{Revision: "9f2c"}, "9f2c"},
		{Info{Revision: "9f2c", Modified: true}, "9f2c+dirty"},
		// Сборка не из git: «unknown» и без «+dirty» — помечать нечего.
		{Info{}, unknown},
		{Info{Modified: true}, unknown},
	}
	for _, c := range cases {
		if got := c.info.Version(); got != c.want {
			t.Errorf("Version() для %+v: получено %q, ожидалось %q", c.info, got, c.want)
		}
	}
}

// Время коммита — миллисекунды Unix; неизвестное время даёт ноль,
// а не большое отрицательное число.
func TestCommitMilli(t *testing.T) {
	at := time.Date(2026, 8, 22, 9, 14, 3, 0, time.UTC)
	if got := (Info{CommitAt: at}).CommitMilli(); got != at.UnixMilli() {
		t.Errorf("CommitMilli(): получено %d, ожидалось %d", got, at.UnixMilli())
	}
	if got := (Info{}).CommitMilli(); got != 0 {
		t.Errorf("CommitMilli() без времени: получено %d, ожидался 0", got)
	}
}

func TestRead(t *testing.T) {
	at := time.Date(2026, 8, 22, 9, 14, 3, 0, time.UTC)
	cases := []struct {
		name     string
		settings []debug.BuildSetting
		want     Info
	}{
		{
			name: "чистое дерево",
			settings: []debug.BuildSetting{
				{Key: "-compiler", Value: "gc"},
				{Key: "vcs", Value: "git"},
				{Key: "vcs.revision", Value: hash},
				{Key: "vcs.time", Value: "2026-08-22T09:14:03Z"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: Info{Revision: hash, CommitAt: at},
		},
		{
			name: "изменённое дерево",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: hash},
				{Key: "vcs.time", Value: "2026-08-22T09:14:03Z"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: Info{Revision: hash, Modified: true, CommitAt: at},
		},
		{
			// Тестовый бинарь и `go build -buildvcs=false` выглядят так.
			name:     "build info без vcs",
			settings: []debug.BuildSetting{{Key: "-compiler", Value: "gc"}, {Key: "GOOS", Value: "linux"}},
			want:     Info{},
		},
		{
			// Время не по RFC 3339 — то же, что его отсутствие.
			name: "время не разбирается",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: hash},
				{Key: "vcs.time", Value: "вчера"},
			},
			want: Info{Revision: hash},
		},
	}
	for _, c := range cases {
		got := read(&debug.BuildInfo{Settings: c.settings}, true)
		if got.Revision != c.want.Revision || got.Modified != c.want.Modified || !got.CommitAt.Equal(c.want.CommitAt) {
			t.Errorf("read(%s): получено %+v, ожидалось %+v", c.name, got, c.want)
		}
	}
}

// Build info недоступен вовсе: ревизия — «unknown», время — 0.
// Это же состояние у любой сборки не из git, включая тестовую.
func TestReadWithoutBuildInfo(t *testing.T) {
	for _, got := range []Info{read(nil, false), read(nil, true), read(&debug.BuildInfo{}, true)} {
		if got != (Info{}) {
			t.Errorf("получено %+v, ожидалось нулевое", got)
		}
		if v := got.Version(); v != unknown {
			t.Errorf("Version(): получено %q, ожидалось %q", v, unknown)
		}
		if ms := got.CommitMilli(); ms != 0 {
			t.Errorf("CommitMilli(): получено %d, ожидался 0", ms)
		}
	}
}
