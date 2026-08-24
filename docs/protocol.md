# Протокол

HTTP-API под `/api/`, JSON в обе стороны, `Content-Type: application/json`. Все остальные пути — статика клиента. Время — миллисекунды Unix. Ошибка — статус и тело `{"error": "код", "message": "текст для человека"}`.

## Общие правила

- Аутентификация — cookie `bare_session` (ADR-021). Без неё — `401 unauthenticated`. Публичные: `GET /api/config`, `GET /api/kdf`, `POST /api/register`, `POST /api/login`.
- На всех запросах кроме `GET`/`HEAD` заголовок `Origin` обязан равняться `BARE_ORIGIN`, иначе `403 bad_origin`.
- Заголовок `X-Device: <deviceId>` обязателен на `/api/ack`, `/api/messages`, `/api/devices/{id}/push`; для `/api/events` устройство передаётся в query (`EventSource` не умеет заголовки). Устройство должно принадлежать пользователю сессии, иначе `403 unknown_device`. Принадлежность — право, поэтому проверяется после разбора тела и его формы (ADR-043).
- Тело запроса — до 32 КиБ, иначе `413 too_large`.
- Rate limiting — `429 rate_limited` с `Retry-After` в целых секундах, не меньше одной. Правила (ADR-021): регистрация — 5 в час на IP; вход — 10 за 10 минут на пару IP+ник; сообщения — 30 в минуту на пользователя, пакет 10; остальные изменяющие запросы — 60 в минуту на пользователя, одним ведром на все маршруты. Чтения не ограничиваются. Общий лимит отвечает раньше разбора тела; у регистрации, входа и сообщений он стоит на своём месте в порядке проверок эндпоинта (ADR-055).
- Неизвестный путь — `404 not_found`; неверный JSON — `400 bad_json`; валидация — `400 invalid` с полем `field`.
- Форма запроса проверяется раньше прав и раньше существования сущностей: `bad_json`, `too_large` и `invalid` приходят и на запрос, который отвергли бы и по правам (ADR-043).
- Сбой на стороне сервера — `500 internal`; причина остаётся в журнале сервера и клиенту не показывается (ADR-027).
- Неподдерживаемый метод на известном пути — тоже `404 not_found`: кода `405` в протоколе нет (ADR-026).

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
  keys: {keyId, from, iv, ct}[],         // завёрнутые ключи для запрашивающего, от старого к новому
  needsRekey: boolean                    // состав уменьшился, а нового ключа ещё не было (ADR-041)
}

WrappedKey { to: nick, iv: string, ct: string }
```

`keys` — все ключи запрашивающего, которые сервер ещё держит, в `GET /api/rooms`; ровно один, только что розданный, — в событии `room` и в ответах `POST /api/rooms` и `POST /api/rooms/{id}/members` (ADR-059). Пусто, если ключа у него нет.

## Публичные

`GET /api/config` → `200 {inviteRequired: bool, vapidPublicKey: string, kdfIterations: number, maxMessageChars: 4000, version: string, commitAt: number}`

`version` — короткая ревизия сборки: семь символов хеша коммита, с суффиксом `+dirty` у бинаря из изменённого дерева (ADR-057) и `unknown` у сборки не из git. `commitAt` — время коммита в миллисекундах Unix, `0` если оно неизвестно. Это время коммита, а не момент компиляции: штамп времени сборки лишил бы смысла сверку хеша бинаря со сборкой из тега (ADR-022, ADR-074). Те же значения печатает `bare version`. Отдельного эндпоинта у них нет.

`GET /api/kdf?nick=<nick>` → `200 {iterations}`. Для неизвестного ника — `kdfIterations` из конфигурации, тем же статусом. Скрытием существования ника ответ не занимается: у аккаунта, не входившего после повышения цели, число итераций своё (ADR-062), а сам факт, что ник существует, публичен (ADR-019).

`POST /api/register {nick, authKey, publicKey: JWK, blob: string, invite?: string}` → `201 {nick}` + cookie. Ошибки: `400 invalid_nick`, `409 nick_taken`, `403 invite_required`, `403 invalid_invite`. `authKey` — base64url 32 байт, `publicKey` — JWK `kty=EC, crv=P-256` с `x`, `y` без `d`; `blob` — до 8 КиБ.

`POST /api/login {nick, authKey}` → `200 {nick, publicKey, blob}` + cookie. Ошибка одна: `401 invalid_credentials`.

## Аккаунт

`GET /api/me` → `200 {nick, publicKey, createdAt}`

`POST /api/logout` → `204`, cookie стирается.

`POST /api/password {authKey, newAuthKey, blob, logoutOthers: bool}` → `204`. `401 invalid_credentials`, если `authKey` не подходит. Хеш и блоб меняются в одной транзакции; при `logoutOthers` удаляются все сессии кроме текущей, а устройствам удалённых сессий поток событий закрывается — их следующий запрос получает `401` (ADR-058).

`DELETE /api/me {authKey}` → `204`. Удаляет пользователя каскадом; владение комнатами передаётся по ADR-018; пустые комнаты удаляются. Удаление аккаунта — выход из всех его комнат: оставшимся участникам уходит `event: room` с `needsRekey: true`, каждому со своим ключом (ADR-041).

`GET /api/users/{nick}` → `200 {nick, publicKey}` | `404 unknown_user`.

## Устройства

`POST /api/devices {id}` → `201 {id}` при создании, `200 {id}` если уже есть у этого пользователя; `409 device_conflict`, если `id` занят другим пользователем (клиент генерирует новый). Обновляет `last_seen` и привязывает текущую сессию к устройству.

`GET /api/devices` → `200 [{id, createdAt, lastSeen, hasPush, current: bool}]`.

`DELETE /api/devices/{id}` → `204`. Удаляет очередь, подписку и сессии, привязанные к устройству. Подключённому по SSE устройству поток закрывается; его следующий запрос получает `401`.

`PUT /api/devices/{id}/push {subscription}` → `204`. `subscription` — объект `PushSubscription.toJSON()`: `endpoint` — абсолютный `https`-адрес до 2 КиБ на публичный адрес (литеральные loopback, link-local и приватные адреса — `400 invalid`, ADR-047), `keys.p256dh` — точка кривой P-256 в 65 байтах base64url, `keys.auth` — 16 байт base64url; прочие поля, включая `expirationTime`, сервер не хранит. Сбрасывает `push_pending`. Устройство в пути, как и `X-Device`, обязано принадлежать пользователю сессии.

`DELETE /api/devices/{id}/push` → `204`; подписки не было — тот же `204`, чужое устройство — `403 unknown_device`.

## Контакты

`GET /api/contacts` → `200 [{nick, publicKey, createdAt}]`.

`POST /api/contacts {nick}` → `201 {nick, publicKey}` | `200` если уже есть | `404 unknown_user` | `400 self`.

`DELETE /api/contacts/{nick}` → `204`. Только своя строка; зеркальная у собеседника остаётся.

## Сообщения

`POST /api/messages {id, to, keyId, iv, ct}` → `202 {id, ts}`.

Проверки по порядку: формат полей (`400 invalid`); время ULID в пределах ±5 минут от серверного (`400 clock_skew`); для `dm` — существование ника (`404 unknown_user`), не себе (`400 self`); для `room` — членство (`403 not_member`), `keyId` среди ключей комнаты (`400 unknown_key`); лимит (`429`).

Сервер в одной транзакции: для `dm` создаёт недостающие строки `contacts` в обе стороны; вычисляет получателей (оба ника или все участники); для каждого устройства получателей, кроме `X-Device`, вставляет строку в `queue`; после коммита отдаёт конверт подключённым устройствам и шлёт пуши устройствам получателей по правилам ADR-023 и ADR-045.

`POST /api/ack {ids: string[]}` → `204`. До 500 идентификаторов. Удаляет из `queue` строки устройства `X-Device`. Клиент копит подтверждения и шлёт их пачкой, не чаще раза в две секунды: маршрут живёт в общем ведре изменяющих запросов, и запрос на конверт съедал бы его целиком (ADR-063).

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
event: room       data: Room         // создание, смена состава, rekey, выход участника (needsRekey);
                                     // keys — один новый ключ получателя либо пусто (ADR-059)
event: room_left  data: {id}         // получателя удалили или комната удалена
event: ready      data: {}
```

`msg` идёт через очередь и требует ACK. `room` и `room_left` в очередь не кладутся: клиент после каждого `ready` перечитывает `GET /api/rooms` и `GET /api/contacts`, поэтому пропуск события во время офлайна ничего не ломает. Всё, что несёт событие `room`, включая `needsRekey` и ключ, есть и в `GET /api/rooms` (ADR-041, ADR-059).

`id` в SSE не используется; `Last-Event-ID` игнорируется — повторная выдача очереди после реконнекта и есть механизм восстановления.

## Комнаты

`GET /api/rooms` → `200 Room[]` — комнаты, где пользователь участник, со всеми его ключами, которые сервер ещё держит (до двух, ADR-018), и признаком `needsRekey`: он состояние комнаты, а не свойство события, и переживает офлайн владельца (ADR-041). Ключи идут от старого к новому; последний — текущий. Участник, пропустивший rekey в офлайне, добирает пропущенный `keyId` только отсюда: события в очередь не кладутся, а запроса ключа по идентификатору в протоколе нет (ADR-059).

`POST /api/rooms {id, name, keyId, keys: WrappedKey[]}` → `201 Room`. `id` — 16 случайных байт base64url, генерирует клиент (ADR-037): ключ комнаты заворачивается до запроса и привязан к идентификатору. Занятый `id` — `409 room_conflict`, клиент берёт новый. `keys` — ровно одна запись, `to` равен нику создателя. Всем устройствам создателя кроме `X-Device` (если передан) уходит `event: room`.

`POST /api/rooms/{id}/members {add: nick[], remove: nick[], keyId, keys: WrappedKey[]}` → `200 Room`. Только владелец (`403 not_owner`). Проверки: все `add` существуют (`404 unknown_user`), `remove` — участники, владельца удалить нельзя (`400 owner`), `keyId` новый для комнаты (`409 key_exists`), множество `keys[].to` равно итоговому составу (`400 keys_mismatch`; повтор ника в `keys[].to` — тот же код). Форма `add` и `remove` проверяется раньше прав: ник не по форме — `400 invalid` с этим полем. Пустые `add` и `remove` — чистый rekey. В одной транзакции: состав, `room_keys` для каждого участника, удаление ключей и членства удалённых, обрезка до двух последних `keyId`, снятие `needsRekey`. После коммита: `event: room` всем участникам (каждому — с его новым ключом), `event: room_left` удалённым.

`POST /api/rooms/{id}/leave` → `204`. Удаляет членство и ключи вышедшего. Если вышел владелец — владение получает участник с наименьшим `joined_at`; если никого не осталось — комната удаляется. Остальным — `event: room` с `needsRekey: true`; другим устройствам вышедшего, кроме отправившего запрос, — `event: room_left` (ADR-041).

`DELETE /api/rooms/{id}` → `204`. Только владелец. Всем участникам — `event: room_left`.

## Коды ошибок

`unauthenticated`, `bad_origin`, `unknown_device`, `bad_json`, `invalid`, `invalid_nick`, `nick_taken`, `invite_required`, `invalid_invite`, `invalid_credentials`, `unknown_user`, `self`, `device_conflict`, `room_conflict`, `clock_skew`, `not_member`, `unknown_key`, `not_owner`, `owner`, `key_exists`, `keys_mismatch`, `not_found`, `rate_limited`, `too_large`, `internal`.

## Статика и служебное

- `GET /` → `index.html`; `/app.css`, `/js/*.js`, `/sw.js`, `/manifest.json`, `/icons/*` — из `embed`, с `ETag` и `Cache-Control: no-cache`. `sw.js` — дополнительно `Service-Worker-Allowed: /`.
- Заголовки безопасности на всех ответах — ADR-021.
- `GET /healthz` → `200 ok`, без аутентификации, для проверок после деплоя.
