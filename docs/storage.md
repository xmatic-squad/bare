# Хранение

## Сервер — SQLite

Режим: `journal_mode=WAL`, `synchronous=NORMAL`, `foreign_keys=ON`, `busy_timeout=5000`. Версия схемы — `PRAGMA user_version`; миграции — `internal/store/migrations/NNN_*.sql`, встроены через `embed`, применяются по порядку при старте, каждая в транзакции. Время — миллисекунды Unix в `INTEGER`.

### Миграция 001

```sql
CREATE TABLE users (
  nick        TEXT PRIMARY KEY,
  auth_hash   BLOB NOT NULL,            -- argon2id(authKey), 32 байта
  auth_salt   BLOB NOT NULL,            -- 16 байт
  auth_params TEXT NOT NULL,            -- "argon2id,m=19456,t=2,p=1"
  public_key  TEXT NOT NULL,            -- JWK, JSON
  key_blob    TEXT NOT NULL,            -- непрозрачный JSON клиента
  created_at  INTEGER NOT NULL
);

CREATE TABLE devices (
  id                TEXT PRIMARY KEY,   -- base64url 16 байт, выдаёт клиент
  nick              TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  created_at        INTEGER NOT NULL,
  last_seen         INTEGER NOT NULL,
  push_subscription TEXT,               -- JSON PushSubscription или NULL
  push_pending      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX devices_nick ON devices(nick);

CREATE TABLE sessions (
  token_hash  BLOB PRIMARY KEY,         -- SHA-256(токен)
  nick        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  device_id   TEXT REFERENCES devices(id) ON DELETE CASCADE,   -- NULL до POST /api/devices
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);
CREATE INDEX sessions_nick ON sessions(nick);

CREATE TABLE contacts (
  nick        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  peer        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (nick, peer)
);

CREATE TABLE rooms (
  id          TEXT PRIMARY KEY,         -- base64url 16 байт, выдаёт сервер
  name        TEXT NOT NULL,
  owner       TEXT NOT NULL REFERENCES users(nick),
  created_at  INTEGER NOT NULL
);

CREATE TABLE room_members (
  room_id     TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  nick        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  joined_at   INTEGER NOT NULL,
  PRIMARY KEY (room_id, nick)
);
CREATE INDEX room_members_nick ON room_members(nick);

CREATE TABLE room_keys (
  room_id     TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  nick        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  key_id      TEXT NOT NULL,
  sender      TEXT NOT NULL,            -- кто завернул
  iv          TEXT NOT NULL,
  ct          TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (room_id, nick, key_id)
);

CREATE TABLE queue (
  device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  msg_id      TEXT NOT NULL,
  envelope    TEXT NOT NULL,            -- готовый JSON Envelope
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (device_id, msg_id)
);
CREATE INDEX queue_created ON queue(created_at);
```

Текущий ключ комнаты для участника — строка `room_keys` с максимальным `created_at`; `keyId` считается ключом комнаты, если есть хоть одна строка с таким `key_id` для `room_id`.

Удаление пользователя: перед `DELETE FROM users` сервер обрабатывает комнаты, где он владелец (передача или удаление), остальное — каскад.

### Фоновая чистка, раз в час

```sql
DELETE FROM queue    WHERE created_at < :now - 30 дней;
DELETE FROM devices  WHERE last_seen  < :now - 90 дней;
DELETE FROM sessions WHERE expires_at < :now;
-- room_keys: оставить два последних key_id на комнату
```

### Чего в базе нет

Истории сообщений, плейнтекста, паролей, ключей в открытом виде, IP-адресов, логов доставки.

## Клиент — IndexedDB

База `bare`, версия 1. Один аккаунт на браузерный профиль: выход из аккаунта стирает базу целиком после подтверждения (история на этом устройстве — единственная копия). Вход под другим ником стирает её так же и тоже после подтверждения — на экране входа (ADR-029).

```
meta        key: string → value
  deviceId, nick, publicKey (JWK), fingerprint,
  privateKey (CryptoKey ECDH, non-extractable),
  accountSecret (CryptoKey HKDF, non-extractable),
  notificationsAsked (bool), installBannerDismissed (bool)

chats       key: id                      // "dm:<peer>" | "room:<roomId>"
  {id, type: "dm"|"room", title, peer?, roomId?, owner?, members?: nick[],
   lastId: ULID|null, lastReadId: ULID|null, unread: number, hidden: bool}

messages    key: id (ULID)
  index "chat": [chatId, id]
  {id, chatId, from, text: string|null, ts, status: "pending"|"sent"|"failed",
   undecryptable?: "unknown_key"|"bad_aead"|"key_changed", raw?: Envelope}

roomKeys    key: [roomId, keyId]
  {roomId, keyId, key: CryptoKey AES-GCM non-extractable, from, receivedAt}

peers       key: nick
  {nick, publicKey: JWK, fingerprint, firstSeen,
   pending: {publicKey, fingerprint, seenAt} | null}     // новый ключ, ждущий подтверждения
```

Правила:

- Сообщение пишется в `messages` до ACK серверу: сначала `put`, потом `POST /api/ack`. Повтор доставки — `put` с тем же `id`, без дублей.
- Исходящее пишется со `status: "pending"` и локальным `id`, затем `POST /api/messages`; `202` → `sent`, сетевая ошибка → остаётся `pending` и повторяется при следующем подключении; `4xx` → `failed` с текстом ошибки. При каждой попытке отправки `pending` получает новый ULID (старая запись удаляется, новая пишется): сообщение ещё не покидало устройство, а его время должно совпадать с временем фактической отправки — иначе после долгого офлайна сервер ответит `clock_skew`.
- `unread` и `lastReadId` — локальные, на сервер не уходят.
- Нерасшифрованное сообщение хранит `raw` для повторной попытки после подтверждения нового ключа или получения недостающего `keyId`.
- Пагинация — курсор по индексу `chat` назад от последнего, по 50.
- При старте: `navigator.storage.persist()`; в настройках — `storage.estimate()`.

## Экспорт `.bare`

Полезная нагрузка — `chats` (без `unread`, `lastReadId`), `messages` (без `raw`, только с `text`), `peers` (без `pending`). Формат файла и шифрование — `docs/crypto.md`. Имя файла — `bare-<nick>-<YYYY-MM-DD>.bare`.
