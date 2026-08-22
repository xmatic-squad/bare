# Криптография

Все операции — WebCrypto (`crypto.subtle`), все случайные байты — `crypto.getRandomValues`. Кодировка бинарных полей в JSON — base64url без паддинга. Строки в UTF-8, пароль нормализуется в NFC. Названия констант — буквальные строки, они входят в вывод ключей и менять их нельзя.

## Аккаунт

### Мастер-ключ и два ключа из него

```
salt    = SHA-256(utf8("bare-v1:" + nick))                       // 32 байта
master  = PBKDF2-HMAC-SHA256(utf8(NFC(password)), salt, iter, 256 бит)
authKey = HKDF-SHA256(master, salt = пусто, info = "bare-auth-v1", 32 байта) → base64url
kek     = HKDF-SHA256(master, salt = пусто, info = "bare-kek-v1") → AES-GCM-256
```

`iter` — из `GET /api/kdf?nick=` перед входом, из `GET /api/config` при регистрации. Целевое значение сервера — 1 000 000, нижняя граница — 600 000 (ADR-013). WebCrypto: `deriveBits` из PBKDF2, результат импортируется `importKey("raw", …, "HKDF")`, дальше `deriveBits`/`deriveKey`.

`authKey` — единственное, что уходит на сервер. Пароль и `master` не покидают память клиента и не пишутся в IndexedDB.

### Ключевая пара и секрет аккаунта

- Пара — `generateKey({name: "ECDH", namedCurve: "P-256"}, extractable: true, ["deriveBits"])`. Экспорт: публичный — JWK, приватный — JWK (только для упаковки в блоб).
- Секрет аккаунта — 32 случайных байта.
- Отпечаток — `SHA-256(exportKey("raw", publicKey))`, 65-байтовая несжатая точка. Показывается как 64 hex-символа группами по 4, нижний регистр.

### Ключевой блоб

```
plain = JSON {"priv": <JWK приватного ключа>, "secret": <base64url 32 байта>}
iv    = 12 случайных байт
ct    = AES-GCM(kek, iv, utf8(plain), AAD = utf8("bare-blob-v1|" + nick))
blob  = JSON {"v": 1, "iter": iter, "iv": iv, "ct": ct}
```

Сервер хранит `blob` как непрозрачную строку. При входе клиент читает `iter` из блоба, а не из ответа `/api/kdf`: расхождение означает несогласованность данных и показывается как ошибка.

### Хранение на устройстве

После расшифровки блоба:

- приватный ключ — `importKey("jwk", priv, ECDH P-256, extractable: false, ["deriveBits"])`, объект `CryptoKey` кладётся в IndexedDB `meta.privateKey`;
- секрет — `importKey("raw", secret, "HKDF", extractable: false, ["deriveKey", "deriveBits"])` → `meta.accountSecret`;
- публичный ключ — JWK → `meta.publicKey`; отпечаток → `meta.fingerprint`.

Сырые байты приватного ключа и секрета живут в памяти только во время входа, регистрации и смены пароля.

### Повышение итераций и смена пароля

Одна процедура. Вход: старый `authKey` уже вычислен. Клиент выводит `master'` с новым паролем или новым `iter`, получает `authKey'` и `kek'`, собирает новый `blob` из сырых байт (они есть: при входе — только что расшифрованы; при смене пароля — блоб скачивается и расшифровывается старым `kek` заново). Отправляет `POST /api/password {authKey, newAuthKey, blob, logoutOthers}`.

Автоматическое повышение происходит, когда `blob.iter < config.kdfIterations`, сразу после входа, с `logoutOthers: false`.

## Чат 1:1

```
shared = ECDH.deriveBits(myPrivate, peerPublic, 256)
dmKey  = HKDF-SHA256(shared, salt = utf8("bare-dm-v1"), info = utf8(a + "\0" + b)) → AES-GCM-256
```

`a`, `b` — ники пары по возрастанию. Ключ симметричен для обеих сторон и всех их устройств. Кэшируется в памяти, в IndexedDB не пишется — выводится заново из `peers`.

## Комната

### Ключ

`roomKey` — 32 случайных байта, `keyId` — 16 случайных байт base64url. Распространитель держит сырые байты только до конца заворачивания, потом импортирует себе non-extractable AES-GCM-256.

### Заворачивание участнику

```
shared = ECDH.deriveBits(distributorPrivate, memberPublic, 256)
wrapK  = HKDF-SHA256(shared, salt = utf8("bare-wrap-v1"), info = utf8("bare-roomkey-v1|" + roomId + "|" + keyId)) → AES-GCM-256
iv     = 12 случайных байт
ct     = AES-GCM(wrapK, iv, roomKey, AAD = utf8("bare-roomkey-v1|" + roomId + "|" + keyId + "|" + from + "|" + to))
```

Запись `{to, iv, ct}` уходит на сервер в `keys[]`. Участник разворачивает той же схемой со своим приватным и публичным ключом `from`, импортирует `roomKey` как non-extractable AES-GCM-256 и хранит в IndexedDB `roomKeys[roomId, keyId]`.

Публичный ключ `from` проходит через TOFU как любой другой. Заворачивание самому себе — `ECDH(myPrivate, myPublic)`, без исключений в коде.

## Сообщение

```
chat  = "dm:" + a + ":" + b   | "room:" + roomId
aad   = utf8("bare-msg-v1|" + id + "|" + chat + "|" + from + "|" + keyId)
plain = JSON {"t": text}
iv    = 12 случайных байт
ct    = AES-GCM(key, iv, utf8(plain), aad)
```

`key` — `dmKey` при `keyId = "dm"`, иначе `roomKeys[roomId, keyId]`. `from` — собственный ник отправителя; сервер проставляет то же значение из сессии, поэтому AAD сходится у получателя. Расшифровка с неизвестным `keyId` или ошибкой AEAD не является фатальной: сообщение сохраняется как нерасшифрованное с кодом причины.

## Экспорт `.bare`

```
salt      = 16 случайных байт
exportKey = HKDF-SHA256(accountSecret, salt, info = utf8("bare-export-v1")) → AES-GCM-256
iv        = 12 случайных байт
payload   = JSON {"v": 1, "exportedAt": ms, "chats": [...], "messages": [...], "peers": [...]}
ct        = AES-GCM(exportKey, iv, utf8(payload), AAD = header)
file      = header || ct
header    = "BARE" (4) || version u8 = 1 || salt (16) || fingerprint (32) || iv (12)     // 65 байт
```

Импорт: проверить magic и версию, сравнить `fingerprint` со своим — при несовпадении показать «архив создан другим аккаунтом» и остановиться, иначе вывести ключ и расшифровать. Слияние — идемпотентное по `id` сообщений и `id` чатов; записи `peers` из архива добавляются только для ников, которых в локальном TOFU ещё нет.

## Идентификаторы

- ULID: 48 бит миллисекунд + 80 бит случайности, Crockford base32, 26 символов. Внутри одной миллисекунды на одном клиенте случайная часть инкрементируется.
- `deviceId`, `keyId`, `roomId` — 16 случайных байт base64url (22 символа).
- Сессионный токен — 32 случайных байта, на сервере хранится `SHA-256`.

## Что сервер проверяет, а что нет

Сервер не умеет и не пытается проверять шифротексты. Он проверяет форму: base64url, длины (`iv` = 12 байт, `ct` не короче 16), существование `keyId` для комнаты, формат ULID и его время.
