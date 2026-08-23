package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/xmatic-squad/bare/internal/store"
)

func TestParams(t *testing.T) {
	if got := Current.String(); got != "argon2id,m=19456,t=2,p=1" {
		t.Errorf("запись параметров: получено %q", got)
	}
	got, err := ParseParams("argon2id,m=19456,t=2,p=1")
	if err != nil || got != Current {
		t.Errorf("разбор параметров: получено %+v, %v", got, err)
	}
	for _, bad := range []string{"", "argon2id", "argon2i,m=1,t=1,p=1", "argon2id,m=0,t=2,p=1", "argon2id,m=19456,t=2,p=1,junk"} {
		if _, err := ParseParams(bad); err == nil {
			t.Errorf("%q разобрано, ожидалась ошибка", bad)
		}
	}
}

func TestHashVerify(t *testing.T) {
	key := []byte("тридцать два байта authKey, ну почти")
	cred, err := Hash(key)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(cred.Hash) != KeyLen || len(cred.Salt) != SaltLen || cred.Params != Current.String() {
		t.Fatalf("хеш: %d байт, соль %d байт, параметры %q", len(cred.Hash), len(cred.Salt), cred.Params)
	}
	if ok, rehash := Verify(key, cred); !ok || rehash {
		t.Errorf("верный authKey: ok=%v rehash=%v", ok, rehash)
	}
	if ok, _ := Verify([]byte("другой ключ"), cred); ok {
		t.Error("неверный authKey принят")
	}
	// Битая строка параметров — не повод считать хеш подошедшим.
	broken := cred
	broken.Params = "argon2id"
	if ok, _ := Verify(key, broken); ok {
		t.Error("хеш с неразобранными параметрами принят")
	}
}

func TestVerifyAsksForRehash(t *testing.T) {
	key := []byte("authKey")
	old := Params{Memory: 8192, Time: 1, Threads: 1}
	cred := store.Credential{
		Hash:   derive(key, make([]byte, SaltLen), old),
		Salt:   make([]byte, SaltLen),
		Params: old.String(),
	}
	ok, rehash := Verify(key, cred)
	if !ok || !rehash {
		t.Errorf("устаревшие параметры: ok=%v rehash=%v, ожидалось true/true", ok, rehash)
	}
}

func TestToken(t *testing.T) {
	token, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != TokenLen {
		t.Fatalf("токен: %q (%v)", token, err)
	}
	sum := sha256.Sum256(raw)
	if string(hash) != string(sum[:]) {
		t.Error("в базу уходит не sha-256 токена")
	}
	got, ok := TokenHash(token)
	if !ok || string(got) != string(hash) {
		t.Error("TokenHash не совпал с NewToken")
	}
	for _, bad := range []string{"", "не base64!", base64.RawURLEncoding.EncodeToString([]byte("коротко"))} {
		if _, ok := TokenHash(bad); ok {
			t.Errorf("мусор %q принят за токен", bad)
		}
	}
}
