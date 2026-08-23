package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// contactOut — строка ответа GET /api/contacts.
type contactOut struct {
	Nick      string          `json:"nick"`
	PublicKey json.RawMessage `json:"publicKey"`
	CreatedAt int64           `json:"createdAt"`
}

func (e *env) contacts(c *http.Cookie) []contactOut {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/contacts", nil, with(c))
	expect(e.t, rec, http.StatusOK, "")
	var out []contactOut
	decodeBody(e.t, rec, &out)
	return out
}

func TestContacts(t *testing.T) {
	e := newEnv(t)
	marta := e.signUp("marta")
	petya := e.signUp("petya")

	if got := e.contacts(marta); len(got) != 0 {
		t.Fatalf("контакты нового аккаунта: %+v", got)
	}

	rec := e.do(http.MethodPost, "/api/contacts", map[string]any{"nick": "petya"}, with(marta))
	expect(t, rec, http.StatusCreated, "")
	var added struct {
		Nick      string          `json:"nick"`
		PublicKey json.RawMessage `json:"publicKey"`
	}
	decodeBody(t, rec, &added)
	if added.Nick != "petya" || len(added.PublicKey) == 0 {
		t.Errorf("ответ: %s", rec.Body.String())
	}

	// Повтор — 200 и та же строка.
	expect(t, e.do(http.MethodPost, "/api/contacts", map[string]any{"nick": "petya"}, with(marta)), http.StatusOK, "")
	if got := e.contacts(marta); len(got) != 1 || got[0].Nick != "petya" {
		t.Errorf("контакты marta: %+v", got)
	}
	// Зеркальной строки POST не заводит: она появляется при первом
	// сообщении (ADR-019).
	if got := e.contacts(petya); len(got) != 0 {
		t.Errorf("контакты petya: %+v", got)
	}

	expect(t, e.do(http.MethodPost, "/api/contacts", map[string]any{"nick": "marta"}, with(marta)),
		http.StatusBadRequest, "self")
	expect(t, e.do(http.MethodPost, "/api/contacts", map[string]any{"nick": "kolya"}, with(marta)),
		http.StatusNotFound, "unknown_user")
	expect(t, e.do(http.MethodPost, "/api/contacts", map[string]any{"nick": "МАРТА"}, with(marta)),
		http.StatusNotFound, "unknown_user")
}

// Удаляется только своя строка: зеркальная у собеседника остаётся,
// это не блокировка (ADR-019).
func TestDeleteContact(t *testing.T) {
	e := newEnv(t)
	marta, m1 := e.join("marta", 1)
	petya, _ := e.join("petya", 2)

	expect(t, e.do(http.MethodPost, "/api/messages", message(ulid(nowMillis(), 3), "petya"),
		with(marta), withDevice(m1)), http.StatusAccepted, "")

	expect(t, e.do(http.MethodDelete, "/api/contacts/petya", nil, with(marta)), http.StatusNoContent, "")
	if got := e.contacts(marta); len(got) != 0 {
		t.Errorf("контакты marta: %+v", got)
	}
	if got := e.contacts(petya); len(got) != 1 || got[0].Nick != "marta" {
		t.Errorf("контакты petya: %+v", got)
	}

	// Удалять нечего — тот же ответ.
	expect(t, e.do(http.MethodDelete, "/api/contacts/petya", nil, with(marta)), http.StatusNoContent, "")
	expect(t, e.do(http.MethodDelete, "/api/contacts/kolya", nil, with(marta)), http.StatusNoContent, "")
}
