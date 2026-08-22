package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// room — комната из трёх ников для тестов хранилища.
func newTestStore(t *testing.T, nicks ...string) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "bare.db"))
	for i, nick := range nicks {
		err := s.CreateUser(ctx, User{
			Nick:      nick,
			Cred:      Credential{Hash: []byte("hash"), Salt: []byte("salt"), Params: "argon2id,m=19456,t=2,p=1"},
			PublicKey: `{"kty":"EC"}`,
			KeyBlob:   `{"v":1}`,
			CreatedAt: int64(i + 1),
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", nick, err)
		}
	}
	return s, ctx
}

// wrap — завёрнутый ключ участнику: содержимое хранилищу безразлично.
func wrap(nicks ...string) []WrappedKey {
	out := make([]WrappedKey, 0, len(nicks))
	for _, nick := range nicks {
		out = append(out, WrappedKey{To: nick, IV: "iv-" + nick, CT: "ct-" + nick})
	}
	return out
}

// rekey — смена состава и раздача нового ключа.
func rekey(t *testing.T, s *Store, ctx context.Context, room, owner, keyID string, add, remove, to []string, now int64) RoomChange {
	t.Helper()
	change, err := s.UpdateMembers(ctx, MembersChange{
		RoomID: room,
		Owner:  owner,
		Add:    add,
		Remove: remove,
		KeyID:  keyID,
		Keys:   wrap(to...),
		Now:    now,
	})
	if err != nil {
		t.Fatalf("UpdateMembers %s: %v", keyID, err)
	}
	return change
}

// makeRoom — комната с одним владельцем и его ключом.
func makeRoom(t *testing.T, s *Store, ctx context.Context, owner string, now int64) RoomChange {
	t.Helper()
	change, err := s.CreateRoom(ctx, NewRoom{
		ID:    "room-1",
		Name:  "общая",
		Owner: owner,
		KeyID: "k1",
		Key:   wrap(owner)[0],
		Now:   now,
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	return change
}

// Занятый идентификатор комнаты — ErrRoomExists, и ни одной строки
// существующая комната при этом не теряет (ADR-037).
func TestCreateRoomTakenID(t *testing.T) {
	s, ctx := newTestStore(t, "marta", "petya")
	makeRoom(t, s, ctx, "marta", 1000)

	_, err := s.CreateRoom(ctx, NewRoom{ID: "room-1", Name: "чужая", Owner: "petya",
		KeyID: "k2", Key: wrap("petya")[0], Now: 2000})
	if !errors.Is(err, ErrRoomExists) {
		t.Fatalf("CreateRoom с занятым id: получено %v, ожидалось ErrRoomExists", err)
	}
	rooms, err := s.Rooms(ctx, "marta")
	if err != nil {
		t.Fatalf("Rooms: %v", err)
	}
	if len(rooms) != 1 || rooms[0].Name != "общая" || rooms[0].Owner != "marta" ||
		rooms[0].Key == nil || rooms[0].Key.KeyID != "k1" {
		t.Errorf("комната после отказа: %+v", rooms)
	}
	if got, err := s.Rooms(ctx, "petya"); err != nil || len(got) != 0 {
		t.Errorf("занятый id присоединил чужого: %+v, %v", got, err)
	}
}

// У комнаты живут два последних keyId; обрезка при rekey и фоновая чистка
// держат одни и те же ключи и не трогают текущий ключ участника (ADR-018).
func TestRoomKeysTrimmedToTwo(t *testing.T) {
	s, ctx := newTestStore(t, "marta", "petya", "kolya")
	makeRoom(t, s, ctx, "marta", 1000)
	rekey(t, s, ctx, "room-1", "marta", "k2", []string{"petya"}, nil, []string{"marta", "petya"}, 2000)
	rekey(t, s, ctx, "room-1", "marta", "k3", []string{"kolya"}, nil, []string{"marta", "petya", "kolya"}, 3000)

	if got := ids(t, s, `SELECT DISTINCT key_id FROM room_keys ORDER BY key_id`); !equal(got, []string{"k2", "k3"}) {
		t.Errorf("ключи после rekey: получено %v, ожидалось [k2 k3]", got)
	}
	// Фоновая чистка ничего не добавляет к обрезке: запрос у них один.
	if err := s.Cleanup(ctx, time.Now()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if got := ids(t, s, `SELECT DISTINCT key_id FROM room_keys ORDER BY key_id`); !equal(got, []string{"k2", "k3"}) {
		t.Errorf("ключи после чистки: получено %v, ожидалось [k2 k3]", got)
	}
	// Текущий ключ есть у каждого участника, и он последний.
	for _, nick := range []string{"marta", "petya", "kolya"} {
		rooms, err := s.Rooms(ctx, nick)
		if err != nil {
			t.Fatalf("Rooms %s: %v", nick, err)
		}
		if len(rooms) != 1 || rooms[0].Key == nil {
			t.Fatalf("комнаты %s: %+v", nick, rooms)
		}
		if rooms[0].Key.KeyID != "k3" || rooms[0].Key.CT != "ct-"+nick {
			t.Errorf("ключ %s: %+v", nick, rooms[0].Key)
		}
	}
}

// Два rekey в одну миллисекунду: текущим остаётся последний розданный ключ,
// и обрезка его не выбрасывает (docs/storage.md, «Текущий ключ комнаты»).
// Идентификаторы ключей случайны, поэтому свежий вполне может оказаться
// меньше прежнего по порядку сортировки — на этом и построен случай.
func TestRoomKeysWithinOneMillisecond(t *testing.T) {
	s, ctx := newTestStore(t, "marta", "petya")
	if _, err := s.CreateRoom(ctx, NewRoom{ID: "room-1", Name: "общая", Owner: "marta",
		KeyID: "ccc", Key: wrap("marta")[0], Now: 1000}); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	rekey(t, s, ctx, "room-1", "marta", "bbb", []string{"petya"}, nil, []string{"marta", "petya"}, 1000)
	rekey(t, s, ctx, "room-1", "marta", "aaa", nil, nil, []string{"marta", "petya"}, 1000)

	if got := ids(t, s, `SELECT DISTINCT key_id FROM room_keys ORDER BY key_id`); !equal(got, []string{"aaa", "bbb"}) {
		t.Errorf("ключи: получено %v, ожидалось [aaa bbb]", got)
	}
	for _, nick := range []string{"marta", "petya"} {
		rooms, err := s.Rooms(ctx, nick)
		if err != nil || len(rooms) != 1 || rooms[0].Key == nil {
			t.Fatalf("комнаты %s: %+v, %v", nick, rooms, err)
		}
		if rooms[0].Key.KeyID != "aaa" {
			t.Errorf("текущий ключ %s: получено %q, ожидалось \"aaa\"", nick, rooms[0].Key.KeyID)
		}
	}
	// Свежим ключом можно писать: он остался ключом комнаты.
	access, err := s.RoomAccess(ctx, "room-1", "marta", "aaa")
	if err != nil || !access.Member || !access.KnownKey {
		t.Errorf("доступ по свежему ключу: %+v, %v", access, err)
	}
	if access.Name != "общая" {
		t.Errorf("имя комнаты: получено %q, ожидалось \"общая\"", access.Name)
	}
}

// Убранный участник теряет и членство, и все свои ключи.
func TestRoomRemoveDropsKeys(t *testing.T) {
	s, ctx := newTestStore(t, "marta", "petya")
	makeRoom(t, s, ctx, "marta", 1000)
	rekey(t, s, ctx, "room-1", "marta", "k2", []string{"petya"}, nil, []string{"marta", "petya"}, 2000)

	change := rekey(t, s, ctx, "room-1", "marta", "k3", nil, []string{"petya"}, []string{"marta"}, 3000)
	if len(change.Left) != 1 || change.Left[0].Nick != "petya" {
		t.Errorf("выбывшие: %+v", change.Left)
	}
	if got := ids(t, s, `SELECT nick FROM room_keys ORDER BY nick, key_id`); !equal(got, []string{"marta", "marta"}) {
		t.Errorf("ключи после удаления участника: %v", got)
	}
	rooms, err := s.Rooms(ctx, "petya")
	if err != nil || len(rooms) != 0 {
		t.Errorf("комнаты убранного: %+v, %v", rooms, err)
	}
}

// Отказ в середине не оставляет следов: состав и ключи те же (docs/protocol.md).
func TestRoomChangeIsAtomic(t *testing.T) {
	s, ctx := newTestStore(t, "marta", "petya", "kolya")
	makeRoom(t, s, ctx, "marta", 1000)
	rekey(t, s, ctx, "room-1", "marta", "k2", []string{"petya"}, nil, []string{"marta", "petya"}, 2000)

	cases := []struct {
		name string
		c    MembersChange
		want error
	}{
		{"не владелец", MembersChange{RoomID: "room-1", Owner: "petya", KeyID: "k3",
			Keys: wrap("marta", "petya"), Now: 3000}, ErrNotOwner},
		{"нет комнаты", MembersChange{RoomID: "нет", Owner: "marta", KeyID: "k3",
			Keys: wrap("marta", "petya"), Now: 3000}, ErrNotOwner},
		{"нет ника", MembersChange{RoomID: "room-1", Owner: "marta", Add: []string{"никого"}, KeyID: "k3",
			Keys: wrap("marta", "petya", "никого"), Now: 3000}, ErrUnknownUser},
		{"не участник", MembersChange{RoomID: "room-1", Owner: "marta", Remove: []string{"kolya"}, KeyID: "k3",
			Keys: wrap("marta", "petya"), Now: 3000}, ErrNotMember},
		{"владелец", MembersChange{RoomID: "room-1", Owner: "marta", Remove: []string{"marta"}, KeyID: "k3",
			Keys: wrap("petya"), Now: 3000}, ErrOwnerRemoval},
		{"ключ уже был", MembersChange{RoomID: "room-1", Owner: "marta", Add: []string{"kolya"}, KeyID: "k2",
			Keys: wrap("marta", "petya", "kolya"), Now: 3000}, ErrKeyExists},
		{"ключи не по составу", MembersChange{RoomID: "room-1", Owner: "marta", Add: []string{"kolya"}, KeyID: "k3",
			Keys: wrap("marta", "petya"), Now: 3000}, ErrKeysMismatch},
		// Два ключа одному вместо ключа другому: длина сходится, состав — нет.
		{"два ключа одному", MembersChange{RoomID: "room-1", Owner: "marta", KeyID: "k3",
			Keys: wrap("marta", "marta"), Now: 3000}, ErrKeysMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.UpdateMembers(ctx, c.c); !errors.Is(err, c.want) {
				t.Fatalf("получено %v, ожидалось %v", err, c.want)
			}
			if got := ids(t, s, `SELECT nick FROM room_members WHERE room_id = 'room-1' ORDER BY nick`); !equal(got, []string{"marta", "petya"}) {
				t.Errorf("состав: получено %v, ожидалось [marta petya]", got)
			}
			if got := ids(t, s, `SELECT DISTINCT key_id FROM room_keys ORDER BY key_id`); !equal(got, []string{"k1", "k2"}) {
				t.Errorf("ключи: получено %v, ожидалось [k1 k2]", got)
			}
		})
	}
}

// Удаление аккаунта передаёт владение участнику с наименьшим joined_at,
// а комнату без участников удаляет (ADR-018).
func TestDeleteUserRooms(t *testing.T) {
	s, ctx := newTestStore(t, "marta", "petya", "kolya")
	makeRoom(t, s, ctx, "marta", 1000)
	rekey(t, s, ctx, "room-1", "marta", "k2", []string{"kolya"}, nil, []string{"marta", "kolya"}, 2000)
	rekey(t, s, ctx, "room-1", "marta", "k3", []string{"petya"}, nil, []string{"marta", "kolya", "petya"}, 3000)

	// Вторая комната — только владелец.
	if _, err := s.CreateRoom(ctx, NewRoom{ID: "room-2", Name: "своя", Owner: "marta",
		KeyID: "k1", Key: wrap("marta")[0], Now: 4000}); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	if _, err := s.DeleteUser(ctx, "marta"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if got := ids(t, s, `SELECT id FROM rooms ORDER BY id`); !equal(got, []string{"room-1"}) {
		t.Errorf("комнаты: получено %v, ожидалось [room-1]", got)
	}
	if got := ids(t, s, `SELECT owner FROM rooms`); !equal(got, []string{"kolya"}) {
		t.Errorf("владелец: получено %v, ожидалось [kolya]", got)
	}
	if got := ids(t, s, `SELECT nick FROM room_members ORDER BY nick`); !equal(got, []string{"kolya", "petya"}) {
		t.Errorf("состав: получено %v, ожидалось [kolya petya]", got)
	}
	if got := ids(t, s, `SELECT DISTINCT nick FROM room_keys ORDER BY nick`); !equal(got, []string{"kolya", "petya"}) {
		t.Errorf("ключи: получено %v, ожидалось [kolya petya]", got)
	}
}

// Удаление аккаунта простого участника чужую комнату не трогает: членство
// и ключи уносит каскад, владелец и остальные на месте.
func TestDeleteUserMember(t *testing.T) {
	s, ctx := newTestStore(t, "marta", "petya")
	if _, err := s.CreateRoom(ctx, NewRoom{ID: "room-1", Name: "общая", Owner: "petya",
		KeyID: "k1", Key: wrap("petya")[0], Now: 1000}); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	rekey(t, s, ctx, "room-1", "petya", "k2", []string{"marta"}, nil, []string{"petya", "marta"}, 2000)

	if _, err := s.DeleteUser(ctx, "marta"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if got := ids(t, s, `SELECT owner FROM rooms`); !equal(got, []string{"petya"}) {
		t.Errorf("комнаты: получено %v, ожидалось [petya]", got)
	}
	if got := ids(t, s, `SELECT nick FROM room_members`); !equal(got, []string{"petya"}) {
		t.Errorf("состав: получено %v, ожидалось [petya]", got)
	}
	if got := ids(t, s, `SELECT DISTINCT nick FROM room_keys`); !equal(got, []string{"petya"}) {
		t.Errorf("ключи: получено %v, ожидалось [petya]", got)
	}
}

// Выход владельца передаёт владение; выход последнего удаляет комнату.
func TestLeaveRoom(t *testing.T) {
	s, ctx := newTestStore(t, "marta", "petya")
	makeRoom(t, s, ctx, "marta", 1000)
	rekey(t, s, ctx, "room-1", "marta", "k2", []string{"petya"}, nil, []string{"marta", "petya"}, 2000)

	// Не участник — ErrNotFound, комната не тронута.
	if _, err := s.LeaveRoom(ctx, "нет", "marta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("выход из несуществующей комнаты: %v", err)
	}

	change, err := s.LeaveRoom(ctx, "room-1", "marta")
	if err != nil {
		t.Fatalf("LeaveRoom: %v", err)
	}
	if change.Room.Owner != "petya" {
		t.Errorf("владелец: получено %q, ожидалось \"petya\"", change.Room.Owner)
	}
	if len(change.Members) != 1 || change.Members[0].Nick != "petya" {
		t.Fatalf("оставшиеся: %+v", change.Members)
	}
	if change.Members[0].Key == nil || change.Members[0].Key.KeyID != "k2" {
		t.Errorf("ключ оставшегося: %+v", change.Members[0].Key)
	}
	if !change.Room.NeedsRekey {
		t.Error("needsRekey после выхода участника")
	}
	if len(change.Left) != 1 || change.Left[0].Nick != "marta" {
		t.Errorf("room_left вышедшему: %+v", change.Left)
	}
	if got := ids(t, s, `SELECT DISTINCT nick FROM room_keys`); !equal(got, []string{"petya"}) {
		t.Errorf("ключи после выхода: %v", got)
	}

	last, err := s.LeaveRoom(ctx, "room-1", "petya")
	if err != nil {
		t.Fatalf("LeaveRoom: %v", err)
	}
	// Комнаты больше нет: room слать некому, room_left уходит другим
	// устройствам вышедшего (ADR-041).
	if len(last.Members) != 0 {
		t.Errorf("получатели после выхода последнего: %+v", last.Members)
	}
	if len(last.Left) != 1 || last.Left[0].Nick != "petya" {
		t.Errorf("room_left после выхода последнего: %+v", last.Left)
	}
	if got := ids(t, s, `SELECT id FROM rooms`); len(got) != 0 {
		t.Errorf("пустая комната осталась: %v", got)
	}
	if got := ids(t, s, `SELECT DISTINCT nick FROM room_keys`); len(got) != 0 {
		t.Errorf("ключи удалённой комнаты остались: %v", got)
	}
}
