# Протокол

HTTP-API под `/api/`, JSON в обе стороны, `Content-Type: application/json`. Все остальные пути — статика клиента. Время — миллисекунды Unix. Ошибка — статус и тело `{"error": "код", "message": "текст для человека"}`.

## Общие правила

- Аутентификация — cookie `bare_session` (ADR-021). Без неё — `401 unauthenticated`. Публичные: `GET /api/config`, `GET /api/kdf`, `POST /api/register`, `POST /api/login`.
- На всех запросах кроме `GET`/`HEAD` заголовок `Origin` обязан равняться `BARE_ORIGIN`, иначе `403 bad_origin`.
- Заголовок `X-Device: <deviceId>` обязателен на `/api/ack`, `/api/messages`, `/api/devices/{id}/push`; для `/api/events` устройство передаётся в query (`EventSource` не умеет заголовки). Устройство должно принадлежать пользователю сессии, иначе `403 unknown_device`.
- Тело запроса — до 32 КиБ, иначе `413`.
- Rate limiting — `429` с `Retry-After` (секунды).
- Неизвестный путь — `404 not_found`; неверный JSON — `400 bad_json`; валидация — `400 invalid` с полем `field`.

## Типы

```
Envelope {
  id:    string   // ULID, 26 символов
  to:    {dm: nick} | {room: roomId}
  from:  nick     // ставит сервер
  keyId: string   // "dm" | keyId комнаты
  iv:    string   // base64url, 12 байт
  ct:    string   // base64url
  ts:    number   // ставит сервер
}

Room {
  id: roomId, name: string, owner: nick,
  members: nick[],                       // по joined_at
  createdAt: number,
  key: {keyId, from, iv, ct} | null,     // текущий завёрнутый ключ для запрашивающего
  needsRekey: boolean                    // только в событии после выхода участника
}

WrappedKey { to: nick, iv: string, ct: string }
```

## Публичные

`GET /api/config` → `200 {inviteRequired: bool, vapidPublicKey: string, kdfIterations: number, maxMessageChars: 4000}`

`GET /api/kdf?nick=<nick>` → `200 {iterations}`. Для неизвестного ника — `kdfIterations` из конфигурации, тем же статусом.

`POST /api/register {nick, authKey, publicKey: JWK, blob: string, invite?: string}` → `201 {nick}` + cookie. Ошибки: `400 invalid_nick`, `409 nick_taken`, `403 invite_required`, `403 invalid_invite`. `authKey` — base64url 32 байт, `publicKey` — JWK `kty=EC, crv=P-256` с `x`, `y` без `d`; `blob` — до 8 КиБ.

`POST /api/login {nick, authKey}` → `200 {nick, publicKey, blob}` + cookie. Ошибка одна: `401 invalid_credentials`.

## Аккаунт

`GET /api/me` → `200 {nick, publicKey, createdAt}`

`POST /api/logout` → `204`, cookie стирается.

`POST /api/password {authKey, newAuthKey, blob, logoutOthers: bool}` → `204`. `401 invalid_credentials`, если `authKey` не подходит. Хеш и блоб меняются в одной транзакции; при `logoutOthers` удаляются все сессии кроме текущей.

`DELETE /api/me {authKey}` → `204`. Удаляет пользователя каскадом; владение комнатами передаётся по ADR-018; пустые комнаты удаляются.

`GET /api/users/{nick}` → `200 {nick, publicKey}` | `404 unknown_user`.

## Устройства

`POST /api/devices {id}` → `201 {id}` при создании, `200 {id}` если уже есть у этого пользователя; `409 device_conflict`, если `id` занят другим пользователем (клиент генерирует новый). Обновляет `last_seen` и привязывает текущую сессию к устройству.

`GET /api/devices` → `200 [{id, createdAt, lastSeen, hasPush, current: bool}]`.

`DELETE /api/devices/{id}` → `204`. Удаляет очередь, подписку и сессии, привязанные к устройству. Подключённому по SSE устройству поток закрывается; его следующий запрос получает `401`.

`PUT /api/devices/{id}/push {subscription}` → `204`. `subscription` — объект `PushSubscription.toJSON()`. Сбрасывает `push_pending`.

`DELETE /api/devices/{id}/push` → `204`.

## Контакты

`GET /api/contacts` → `200 [{nick, publicKey, createdAt}]`.

`POST /api/contacts {nick}` → `201 {nick, publicKey}` | `200` если уже есть | `404 unknown_user` | `400 self`.

`DELETE /api/contacts/{nick}` → `204`. Только своя строка; зеркальная у собеседника остаётся.

## Сообщения

`POST /api/messages {id, to, keyId, iv, ct}` → `202 {id, ts}`.

Проверки по порядку: формат полей (`400 invalid`); время ULID в пределах ±5 минут от серверного (`400 clock_skew`); для `dm` — существование ника (`404 unknown_user`), не себе (`400 self`); для `room` — членство (`403 not_member`), `keyId` среди ключей комнаты (`400 unknown_key`); лимит (`429`).

Сервер в одной транзакции: для `dm` создаёт недостающие строки `contacts` в обе стороны; вычисляет получателей (оба ника или все участники); для каждого устройства получателей, кроме `X-Device`, вставляет строку в `queue`; после коммита отдаёт конверт подключённым устройствам и шлёт пуши по правилам ADR-023.

`POST /api/ack {ids: string[]}` → `204`. До 500 идентификаторов. Удаляет из `queue` строки устройства `X-Device`.

## События

`GET /api/events?device=<deviceId>` → `text/event-stream`. Заголовки ответа: `Cache-Control: no-cache`, `X-Accel-Buffering: no`. Одно соединение на устройство: новое закрывает предыдущее.

Порядок после подключения:

1. `push_pending` устройства сбрасывается, `last_seen` обновляется.
2. Все строки `queue` устройства по `created_at, msg_id` — каждая как `event: msg`.
3. `event: ready` с данными `{}`.
4. Живые события.
5. Каждые 20 секунд — строка `: ping`.

События:

```
event: msg        data: Envelope
event: room       data: Room         // создание, смена состава, rekey, выход участника (needsRekey)
event: room_left  data: {id}         // получателя удалили или комната удалена
event: ready      data: {}
```

`msg` идёт через очередь и требует ACK. `room` и `room_left` в очередь не кладутся: клиент после каждого `ready` перечитывает `GET /api/rooms` и `GET /api/contacts`, поэтому пропуск события во время офлайна ничего не ломает.

`id` в SSE не используется; `Last-Event-ID` игнорируется — повторная выдача очереди после реконнекта и есть механизм восстановления.

## Комнаты

`GET /api/rooms` → `200 Room[]` — комнаты, где пользователь участник, с его текущим ключом.

`POST /api/rooms {name, keyId, keys: WrappedKey[]}` → `201 Room`. `keys` — ровно одна запись, `to` равен нику создателя. Всем устройствам создателя кроме `X-Device` (если передан) уходит `event: room`.

`POST /api/rooms/{id}/members {add: nick[], remove: nick[], keyId, keys: WrappedKey[]}` → `200 Room`. Только владелец (`403 not_owner`). Проверки: все `add` существуют (`404 unknown_user`), `remove` — участники, владельца удалить нельзя (`400 owner`), `keyId` новый для комнаты (`409 key_exists`), множество `keys[].to` равно итоговому составу (`400 keys_mismatch`). Пустые `add` и `remove` — чистый rekey. В одной транзакции: состав, `room_keys` для каждого участника, удаление ключей и членства удалённых, обрезка до двух последних `keyId`. После коммита: `event: room` всем участникам (каждому — с его ключом), `event: room_left` удалённым.

`POST /api/rooms/{id}/leave` → `204`. Удаляет членство и ключи вышедшего. Если вышел владелец — владение получает участник с наименьшим `joined_at`; если никого не осталось — комната удаляется. Остальным — `event: room` с `needsRekey: true`.

`DELETE /api/rooms/{id}` → `204`. Только владелец. Всем участникам — `event: room_left`.

## Коды ошибок

`unauthenticated`, `bad_origin`, `unknown_device`, `bad_json`, `invalid`, `invalid_nick`, `nick_taken`, `invite_required`, `invalid_invite`, `invalid_credentials`, `unknown_user`, `self`, `device_conflict`, `clock_skew`, `not_member`, `unknown_key`, `not_owner`, `owner`, `key_exists`, `keys_mismatch`, `not_found`, `rate_limited`.

## Статика и служебное

- `GET /` → `index.html`; `/app.css`, `/js/*.js`, `/sw.js`, `/manifest.json`, `/icons/*` — из `embed`, с `ETag` и `Cache-Control: no-cache`. `sw.js` — дополнительно `Service-Worker-Allowed: /`.
- Заголовки безопасности на всех ответах — ADR-021.
- `GET /healthz` → `200 ok`, без аутентификации, для проверок после деплоя.
