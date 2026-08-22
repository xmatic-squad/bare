package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/xmatic-squad/bare/internal/config"
)

// Сервер не умеет и не пытается проверять шифротексты. Он проверяет форму:
// base64url, длины, версии (docs/crypto.md, «Что сервер проверяет»).
const (
	authKeyLen = 32      // байт
	ivLen      = 12      // байт
	minCTLen   = 16      // байт: короче тега AES-GCM шифротекста не бывает
	maxBlob    = 8 << 10 // ключевой блоб, docs/protocol.md
)

// b64 — кодировка бинарных полей протокола: base64url без паддинга.
var b64 = base64.RawURLEncoding

// nickRe — ник по ADR-019: только строчные, без регистровых коллизий.
var nickRe = regexp.MustCompile(`^[a-z0-9_]{2,32}$`)

func validNick(nick string) bool { return nickRe.MatchString(nick) }

// decodeExactly разбирает base64url и требует ровно n байт.
func decodeExactly(s string, n int) ([]byte, bool) {
	raw, err := b64.DecodeString(s)
	if err != nil || len(raw) != n {
		return nil, false
	}
	return raw, true
}

// authKey разбирает authKey клиента: base64url ровно 32 байта.
func authKey(s string) ([]byte, bool) { return decodeExactly(s, authKeyLen) }

// jwkPublic — публичный ключ в том виде, в каком сервер его хранит
// и отдаёт: четыре поля и ничего больше.
type jwkPublic struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKeyJSON проверяет JWK и отдаёт его канонический JSON.
//
// Поле d — приватный ключ. Его наличие означает, что клиент собирается
// отдать серверу материал, которого у сервера не должно быть ни при каких
// условиях, поэтому такой запрос отвергается целиком, а не чистится молча.
// Всё, что не kty, crv, x и y, отбрасывается: хранится ровно то, что нужно.
func publicKeyJSON(raw json.RawMessage) (string, error) {
	var in struct {
		Kty string          `json:"kty"`
		Crv string          `json:"crv"`
		X   string          `json:"x"`
		Y   string          `json:"y"`
		D   json.RawMessage `json:"d"`
	}
	if len(raw) == 0 {
		return "", errors.New("нет публичного ключа")
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", errors.New("публичный ключ — не jwk")
	}
	if in.D != nil {
		return "", errors.New("приватному ключу на сервере не место")
	}
	if in.Kty != "EC" || in.Crv != "P-256" {
		return "", errors.New("ожидается ключ ec p-256")
	}
	if _, ok := decodeExactly(in.X, 32); !ok {
		return "", errors.New("x — не 32 байта base64url")
	}
	if _, ok := decodeExactly(in.Y, 32); !ok {
		return "", errors.New("y — не 32 байта base64url")
	}
	out, err := json.Marshal(jwkPublic{Kty: in.Kty, Crv: in.Crv, X: in.X, Y: in.Y})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// blobIterations проверяет форму ключевого блоба (docs/crypto.md,
// «Ключевой блоб») и отдаёт iter. Это единственное поле блоба, которое
// сервер читает: его же отдаёт GET /api/kdf. Всё остальное — непрозрачный
// шифротекст.
func blobIterations(blob string) (int, error) {
	if blob == "" {
		return 0, errors.New("нет ключевого блоба")
	}
	if len(blob) > maxBlob {
		return 0, errors.New("ключевой блоб больше 8 КиБ")
	}
	var b struct {
		V    int    `json:"v"`
		Iter int    `json:"iter"`
		IV   string `json:"iv"`
		CT   string `json:"ct"`
	}
	if err := json.Unmarshal([]byte(blob), &b); err != nil {
		return 0, errors.New("ключевой блоб — не json")
	}
	if b.V != 1 {
		return 0, fmt.Errorf("версия блоба %d, ожидается 1", b.V)
	}
	if b.Iter < config.KDFMinIterations || b.Iter > config.KDFMaxIterations {
		return 0, fmt.Errorf("iter вне границ %d…%d", config.KDFMinIterations, config.KDFMaxIterations)
	}
	if _, ok := decodeExactly(b.IV, ivLen); !ok {
		return 0, errors.New("iv — не 12 байт base64url")
	}
	if ct, err := b64.DecodeString(b.CT); err != nil || len(ct) < minCTLen {
		return 0, errors.New("ct — не base64url или слишком короткий")
	}
	return b.Iter, nil
}
