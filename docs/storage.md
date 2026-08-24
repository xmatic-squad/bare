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
  id          TEXT PRIMARY KEY,         -- base64url 16 байт, выдаёт клиент
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

### Миграция 002

```sql
ALTER TABLE rooms ADD COLUMN needs_rekey INTEGER NOT NULL DEFAULT 0;
```

Признак «состав уменьшился, нового ключа ещё не было» (ADR-041): ставится при выходе участника и удалении аккаунта, снимается при `POST /api/rooms/{id}/members`, отдаётся полем `needsRekey`.

Текущий ключ комнаты для участника — строка `room_keys` с максимальным `created_at`; `keyId` считается ключом комнаты, если есть хоть одна строка с таким `key_id` для `room_id`. Участнику отдаются все его удерживаемые ключи, а не только текущий: пропущенный в офлайне `keyId` иначе не добыть ничем — события в очередь не кладутся, а запроса ключа по идентификатору в протоколе нет (ADR-059).

Время записи `room_keys` строго больше времени всех прежних ключей той же комнаты; при равенстве порядок доопределяется по `key_id` (ADR-042). Два rekey подряд укладываются в одну миллисекунду, поэтому `created_at` ключа — не в точности миллисекунды Unix, а миллисекунды, сдвинутые вперёд ровно настолько, чтобы «последний» был однозначен.

Удаление пользователя: перед `DELETE FROM users` сервер обрабатывает его комнаты — убирает членство и ключи, передаёт владение или удаляет опустевшую комнату, ставит `needs_rekey` там, где участники остались (ADR-041), — остальное уносит каскад.

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
  notificationsAsked (bool), notificationsOff (bool), installBannerDismissed (bool)

chats       key: id                      // "dm:<peer>" | "room:<roomId>"
  {id, type: "dm"|"room", title, peer?, roomId?, owner?, members?: nick[],
   lastId: ULID|null, lastReadId: ULID|null, unread: number, hidden: bool}

messages    key: id (ULID)
  index "chat": [chatId, id]
  {id, chatId, from, text: string|null, ts, status: "pending"|"sent"|"failed",
   error?: string,                                        // текст отказа у failed
   undecryptable?: "unknown_key"|"bad_aead"|"key_changed", raw?: Envelope}

roomKeys    key: [roomId, keyId]
  {roomId, keyId, key: CryptoKey AES-GCM non-extractable, from, receivedAt}
  // receivedAt строго больше receivedAt всех прежних ключей той же комнаты;
  // текущий ключ — последний по нему, то есть в порядке получения (ADR-042)

peers       key: nick
  {nick, publicKey: JWK, fingerprint, firstSeen,
   pending: {publicKey, fingerprint, seenAt} | null}     // новый ключ, ждущий подтверждения
```

Правила:

- Сообщение пишется в `messages` до ACK серверу: сначала `put`, потом `POST /api/ack`. Подтверждения копятся и уходят пачкой, не чаще раза в две секунды: в накопитель идёт только записанное, а запрос на конверт тратил бы общее ведро лимита одними подтверждениями (ADR-063).
- Входящее сообщение с уже известным `id` игнорируется целиком: ни записи, ни счётчика непрочитанных (ADR-034). Повтор доставки не даёт ни дубля в ленте, ни второго непрочитанного; `id` открыт в конверте, и перезапись отдала бы собеседнику чужую строку истории. Перезапись по `id` остаётся у исходящего: переход `pending → sent/failed`.
- Исходящее пишется со `status: "pending"` и локальным `id`, затем `POST /api/messages`; `202` → `sent`, сетевая ошибка и `500` → остаётся `pending` и повторяется при следующем подключении; прочие `4xx` → `failed` с текстом отказа в поле `error` (ADR-033). Повтор отправки идёт с прежним ULID, пока время в нём разошлось с текущим меньше чем на четыре минуты: ответ на `POST` мог потеряться уже после того, как сервер сообщение принял, а повтор с тем же `id` получатель игнорирует (ADR-034). Идентификатор старше запаса заменяется свежим — старая запись удаляется, новая пишется: время в `id` должно совпадать с временем фактической отправки, иначе после долгого офлайна сервер ответит `clock_skew`. `clock_skew` на переиспользованном `id` отменяет переиспользование: попытка идёт второй раз со свежим `id`, ровно один раз; такой же отказ на свежем `id` — `failed` с текстом про часы (ADR-036).
- Исключение среди `4xx` одно: `403 unknown_device` — не отказ сообщению, а потерянное устройство. Запись остаётся `pending`, клиент заводит устройство заново при переподключении и повторяет её после `ready` (ADR-060).
- `unread` и `lastReadId` — локальные, на сервер не уходят.
- Нерасшифрованное сообщение хранит `raw` для повторной попытки после подтверждения нового ключа или получения недостающего `keyId`.
- Пагинация — курсор по индексу `chat` назад от последнего, по 50.
- При старте: `navigator.storage.persist()`; в настройках — `storage.estimate()`.
- Кроме IndexedDB клиент держит один ключ в `sessionStorage` — `bare-updated-at`, время последней перезагрузки ради обновления оболочки. Он живёт не дольше вкладки и задаёт предел частоты: между двумя такими перезагрузками не меньше 30 секунд. Петлю закрывает не он, а сверка версии оболочки (ADR-070).

## Экспорт `.bare`

Полезная нагрузка — `chats` (без `lastId`, `unread`, `lastReadId`, `hidden`), `messages` (без `raw` и `error`), `peers` (без `pending`). Уносится только отправленное — `sent`. Неотправленное и отвергнутое остаются устройству: `pending` и `failed` — незаконченная и отвергнутая попытки, привязанные к своему ULID (ADR-036), а не история. Нерасшифрованное не уносится: без текста от записи остаётся один заголовок, а `raw` — служебное поле. Показания устройства — место чата в списке, счётчики и «убрано из списка» — не уносятся тоже (ADR-050). Формат файла и шифрование — `docs/crypto.md`. Имя файла — `bare-<nick>-<YYYY-MM-DD>.bare`.

Архив — недоверенный ввод: форму каждой записи клиент проверяет сам, до записи в базу (ADR-054). Ник — `[a-z0-9_]{2,32}`, `roomId` — 22 символа base64url, `id` сообщения — ULID, `ts` — целое в пределах `Date`; автор сообщения в личном чате — свой ник или ник собеседника. Что не по форме, до базы не доходит: пропускается запись целиком, а не поле.

Импорт вливает архив одной транзакцией и не трогает то, что уже лежит (ADR-050): сообщение и чат с известным `id` остаются как есть, `unread` и `hidden` не меняются, запись `peers` добавляется только для ника, которого в TOFU ещё нет. У чата двигается `lastId` — под самое новое из добавленного, — и вместе с ним граница «новых»: `lastReadId` уезжает под новый `lastId`, пока непрочитанных у чата нет. Ответ — число добавленных сообщений; повторный импорт того же файла добавляет ноль.
