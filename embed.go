// Package bare встраивает клиентскую статику в бинарь.
//
// Директива go:embed видит только каталог своего пакета и ниже, поэтому
// объявление живёт в корне модуля, а не в internal/web (ADR-025).
package bare

import "embed"

// Web — каталог web/ как есть: index.html, app.css, manifest.json, sw.js, icons/.
//
//go:embed web
var Web embed.FS
