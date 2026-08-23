// Криптография клиента — всё по docs/crypto.md.
//
// Только WebCrypto: crypto.subtle и crypto.getRandomValues. Строки-константы
// входят в вывод ключей, менять их нельзя. Бинарные поля — base64url без
// паддинга, пароль нормализуется в NFC.
//
// Модуль не знает про DOM: его можно импортировать в node и прогнать.

const subtle = globalThis.crypto.subtle;

// Константы вывода ключей (docs/crypto.md). Буквальные строки.
const SALT_PREFIX = "bare-v1:";
const INFO_AUTH = "bare-auth-v1";
const INFO_KEK = "bare-kek-v1";
const BLOB_AAD = "bare-blob-v1|";
const DM_SALT = "bare-dm-v1";
const WRAP_SALT = "bare-wrap-v1";
const ROOM_AAD = "bare-roomkey-v1|";
const MSG_AAD = "bare-msg-v1|";

// DM_KEY_ID — keyId личного чата: ключ выводится из ECDH, отдельного
// идентификатора у него нет (docs/crypto.md, «Сообщение»).
export const DM_KEY_ID = "dm";

// Длина секрета аккаунта (ADR-014) и вектора инициализации AES-GCM.
export const SECRET_LEN = 32;
const IV_LEN = 12;

// ROOM_KEY_LEN — ключ комнаты: 32 случайных байта (docs/crypto.md,
// «Комната»).
export const ROOM_KEY_LEN = 32;

// ID_LEN — deviceId, keyId и roomId устроены одинаково: 16 случайных
// байт base64url, 22 символа (docs/crypto.md, «Идентификаторы»).
const ID_LEN = 16;

// Границы числа итераций PBKDF2 (ADR-013, ADR-030). Число приходит от
// сервера — в ответе /api/kdf, /api/config или полем iter в блобе, — а
// считает по нему клиент, поэтому проверить его может только он.
export const MIN_ITERATIONS = 600_000;
export const MAX_ITERATIONS = 10_000_000;

export function validIterations(iterations) {
  return Number.isSafeInteger(iterations)
    && iterations >= MIN_ITERATIONS
    && iterations <= MAX_ITERATIONS;
}

// Пустая соль HKDF: «salt = пусто» из docs/crypto.md.
const EMPTY = new Uint8Array(0);
const ECDH_P256 = { name: "ECDH", namedCurve: "P-256" };

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export function utf8(text) {
  return encoder.encode(text);
}

export function random(length) {
  const bytes = new Uint8Array(length);
  globalThis.crypto.getRandomValues(bytes);
  return bytes;
}

// newId — идентификатор устройства, ключа комнаты или комнаты.
export function newId() {
  return b64url(random(ID_LEN));
}

export function b64url(input) {
  const bytes = input instanceof Uint8Array ? input : new Uint8Array(input);
  let binary = "";
  for (let i = 0; i < bytes.length; i += 1) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

export function unb64url(text) {
  if (typeof text !== "string" || /[^A-Za-z0-9_-]/.test(text)) {
    throw new Error("не base64url");
  }
  const padded = text.replaceAll("-", "+").replaceAll("_", "/");
  const binary = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

export function hex(input) {
  const bytes = input instanceof Uint8Array ? input : new Uint8Array(input);
  let out = "";
  for (let i = 0; i < bytes.length; i += 1) {
    out += bytes[i].toString(16).padStart(2, "0");
  }
  return out;
}

// fingerprintGroups режет отпечаток на группы по 4 символа: так его
// показывают человеку (ADR-016).
export function fingerprintGroups(fingerprint) {
  return fingerprint.match(/.{1,4}/g) ?? [];
}

// sameBytes — побайтное сравнение. Постоянного времени здесь не нужно:
// сравниваются отпечатки публичных ключей, а они не секрет.
export function sameBytes(a, b) {
  if (a.length !== b.length) {
    return false;
  }
  for (let i = 0; i < a.length; i += 1) {
    if (a[i] !== b[i]) {
      return false;
    }
  }
  return true;
}

// wipe затирает сырые байты, когда они больше не нужны.
export function wipe(bytes) {
  if (bytes instanceof Uint8Array) {
    bytes.fill(0);
  }
}

// accountSalt — соль KDF: детерминированная, известна до входа.
export async function accountSalt(nick) {
  return new Uint8Array(await subtle.digest("SHA-256", utf8(SALT_PREFIX + nick)));
}

// deriveAccountKeys выводит мастер-ключ из пароля и два независимых ключа
// из него: authKey уходит на сервер, kek шифрует ключевой блоб.
// Пароль и мастер остаются в памяти и затираются здесь же.
export async function deriveAccountKeys(nick, password, iterations) {
  // Ослабленный или неподъёмный iter не должен доходить до PBKDF2:
  // authKey, выведенный по нему, уходит на сервер (ADR-030).
  if (!validIterations(iterations)) {
    throw new Error("iter вне границ");
  }
  const salt = await accountSalt(nick);
  const secret = utf8(password.normalize("NFC"));
  const material = await subtle.importKey("raw", secret, "PBKDF2", false, ["deriveBits"]);
  wipe(secret);

  const masterBits = await subtle.deriveBits(
    { name: "PBKDF2", salt, iterations, hash: "SHA-256" },
    material,
    256,
  );
  const master = await subtle.importKey("raw", masterBits, "HKDF", false, ["deriveBits", "deriveKey"]);
  wipe(new Uint8Array(masterBits));

  const authBits = await subtle.deriveBits(
    { name: "HKDF", hash: "SHA-256", salt: EMPTY, info: utf8(INFO_AUTH) },
    master,
    256,
  );
  const kek = await subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt: EMPTY, info: utf8(INFO_KEK) },
    master,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
  const authKey = b64url(authBits);
  wipe(new Uint8Array(authBits));
  return { authKey, kek };
}

// generateIdentity — ключевая пара аккаунта и секрет для экспорта истории.
// Пара extractable: приватный ключ нужно упаковать в блоб.
export async function generateIdentity() {
  const keyPair = await subtle.generateKey(ECDH_P256, true, ["deriveBits"]);
  return { keyPair, secret: random(SECRET_LEN) };
}

// publicJwk — публичный ключ ровно в той форме, в какой его хранит сервер.
export function publicJwk(jwk) {
  return { kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y };
}

export async function exportPublicJwk(publicKey) {
  return publicJwk(await subtle.exportKey("jwk", publicKey));
}

export async function exportPrivateJwk(privateKey) {
  return subtle.exportKey("jwk", privateKey);
}

// importPublic — чужой или свой публичный ключ из JWK. Extractable: из него
// считается отпечаток по сырой точке.
export async function importPublic(jwk) {
  return subtle.importKey("jwk", publicJwk(jwk), ECDH_P256, true, []);
}

// importPrivate — приватный ключ после расшифровки блоба: наружу больше
// не выходит (docs/crypto.md, «Хранение на устройстве»).
export async function importPrivate(jwk) {
  return subtle.importKey("jwk", jwk, ECDH_P256, false, ["deriveBits"]);
}

// importSecret — секрет аккаунта как ключ HKDF, тоже non-extractable.
export async function importSecret(bytes) {
  return subtle.importKey("raw", bytes, "HKDF", false, ["deriveKey", "deriveBits"]);
}

// fingerprintBytes — отпечаток сырыми байтами: SHA-256 несжатой точки
// публичного ключа, 32 байта. В заголовке архива лежат именно они,
// а не hex-строка (ADR-014).
export async function fingerprintBytes(publicKey) {
  const raw = await subtle.exportKey("raw", publicKey);
  return new Uint8Array(await subtle.digest("SHA-256", raw));
}

// fingerprint — тот же отпечаток для человека: 64 hex строчными.
export async function fingerprint(publicKey) {
  return hex(await fingerprintBytes(publicKey));
}

export async function fingerprintOf(jwk) {
  return fingerprint(await importPublic(jwk));
}

// sealBlob собирает ключевой блоб: приватный ключ и секрет аккаунта под kek.
// Ник в AAD — блоб одного аккаунта не подходит другому.
export async function sealBlob({ nick, iterations, kek, priv, secret }) {
  const plain = utf8(JSON.stringify({ priv, secret: b64url(secret) }));
  const iv = random(IV_LEN);
  const ct = await subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: utf8(BLOB_AAD + nick) },
    kek,
    plain,
  );
  wipe(plain);
  return JSON.stringify({ v: 1, iter: iterations, iv: b64url(iv), ct: b64url(ct) });
}

// parseBlob читает форму блоба, не расшифровывая: iter нужен до того,
// как появится kek.
export function parseBlob(text) {
  const blob = JSON.parse(text);
  if (blob === null || typeof blob !== "object") {
    throw new Error("блоб — не объект");
  }
  if (blob.v !== 1) {
    throw new Error("версия блоба не 1");
  }
  if (!validIterations(blob.iter)) {
    throw new Error("iter блоба вне границ");
  }
  if (typeof blob.iv !== "string" || typeof blob.ct !== "string") {
    throw new Error("iv или ct блоба — не строка");
  }
  return blob;
}

// openBlob расшифровывает блоб и отдаёт сырые JWK приватного ключа
// и байты секрета. Жить им — до импорта в CryptoKey.
export async function openBlob(blob, kek, nick) {
  const iv = unb64url(blob.iv);
  const ct = unb64url(blob.ct);
  if (iv.length !== IV_LEN) {
    throw new Error("iv блоба — не 12 байт");
  }
  const plain = await subtle.decrypt(
    { name: "AES-GCM", iv, additionalData: utf8(BLOB_AAD + nick) },
    kek,
    ct,
  );
  const bytes = new Uint8Array(plain);
  const parsed = JSON.parse(decoder.decode(bytes));
  wipe(bytes);
  if (parsed === null || typeof parsed !== "object" || parsed.priv === null || typeof parsed.priv !== "object") {
    throw new Error("в блобе нет приватного ключа");
  }
  const secret = unb64url(parsed.secret);
  if (secret.length !== SECRET_LEN) {
    throw new Error("секрет аккаунта — не 32 байта");
  }
  return { priv: parsed.priv, secret };
}

// --- чат 1:1 -----------------------------------------------------------

// order — ники пары по возрастанию. Сравниваются кодовые единицы, а не
// буквы языка: ник — это [a-z0-9_], и порядок обязан совпасть у обеих
// сторон побайтно (docs/crypto.md, «Чат 1:1»).
export function order(a, b) {
  return a < b ? [a, b] : [b, a];
}

// dmLabel — метка чата для AAD сообщения: "dm:" + a + ":" + b.
// Это не ключ хранилища chats: там чат зовётся "dm:<собеседник>".
export function dmLabel(a, b) {
  const [first, second] = order(a, b);
  return `dm:${first}:${second}`;
}

// dmKey выводит ключ личного чата (docs/crypto.md, «Чат 1:1»).
// Ключ симметричен для обеих сторон и всех их устройств; в IndexedDB
// не пишется — выводится заново из peers.
export async function dmKey(privateKey, peerPublicJwk, me, peer) {
  const [a, b] = order(me, peer);
  const publicKey = await importPublic(peerPublicJwk);
  const shared = await subtle.deriveBits({ name: "ECDH", public: publicKey }, privateKey, 256);
  const material = await subtle.importKey("raw", shared, "HKDF", false, ["deriveKey"]);
  wipe(new Uint8Array(shared));
  return subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt: utf8(DM_SALT), info: utf8(`${a}\0${b}`) },
    material,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

// --- комната -----------------------------------------------------------

// roomLabel — метка комнаты для AAD сообщения: "room:" + roomId. Совпадает
// с ключом чата в IndexedDB, но собирается здесь: crypto.js не знает о базе.
export function roomLabel(roomId) {
  return `room:${roomId}`;
}

// newRoomKey — новый ключ комнаты: 32 случайных байта и случайный keyId
// (docs/crypto.md, «Комната»). Сырые байты живут до конца заворачивания,
// потом распространитель импортирует их себе non-extractable и затирает.
export function newRoomKey() {
  return { keyId: newId(), bytes: random(ROOM_KEY_LEN) };
}

// importRoomKey — ключ комнаты как non-extractable AES-GCM-256. Наружу
// он больше не выходит: в IndexedDB кладётся объект CryptoKey.
export function importRoomKey(bytes) {
  return subtle.importKey("raw", bytes, { name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
}

// roomAad и wrapKey — ровно то, что написано в docs/crypto.md,
// «Заворачивание участнику». Заворачивание самому себе идёт этим же кодом:
// ECDH(myPrivate, myPublic), без исключений.
function roomAad({ roomId, keyId, from, to }) {
  return utf8(`${ROOM_AAD}${roomId}|${keyId}|${from}|${to}`);
}

async function wrapKey(privateKey, peerPublicJwk, roomId, keyId) {
  const publicKey = await importPublic(peerPublicJwk);
  const shared = await subtle.deriveBits({ name: "ECDH", public: publicKey }, privateKey, 256);
  const material = await subtle.importKey("raw", shared, "HKDF", false, ["deriveKey"]);
  wipe(new Uint8Array(shared));
  return subtle.deriveKey(
    {
      name: "HKDF",
      hash: "SHA-256",
      salt: utf8(WRAP_SALT),
      info: utf8(`${ROOM_AAD}${roomId}|${keyId}`),
    },
    material,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

// wrapRoomKey заворачивает сырые байты ключа комнаты участнику. Отдаёт
// запись keys[] запроса: {to, iv, ct} (docs/protocol.md, «Типы»).
export async function wrapRoomKey(privateKey, memberPublicJwk, { roomId, keyId, from, to }, roomKey) {
  const key = await wrapKey(privateKey, memberPublicJwk, roomId, keyId);
  const iv = random(IV_LEN);
  const ct = await subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: roomAad({ roomId, keyId, from, to }) },
    key,
    roomKey,
  );
  return { to, iv: b64url(iv), ct: b64url(ct) };
}

// unwrapRoomKey разворачивает завёрнутый нам ключ той же схемой и сразу
// импортирует его non-extractable: сырые байты дальше не идут.
// Публичный ключ отправителя проходит через TOFU до вызова (ADR-016).
export async function unwrapRoomKey(privateKey, senderPublicJwk, { roomId, keyId, from, to, iv, ct }) {
  const key = await wrapKey(privateKey, senderPublicJwk, roomId, keyId);
  const nonce = unb64url(iv);
  if (nonce.length !== IV_LEN) {
    throw new Error("iv — не 12 байт");
  }
  const plain = await subtle.decrypt(
    { name: "AES-GCM", iv: nonce, additionalData: roomAad({ roomId, keyId, from, to }) },
    key,
    unb64url(ct),
  );
  const bytes = new Uint8Array(plain);
  if (bytes.length !== ROOM_KEY_LEN) {
    wipe(bytes);
    throw new Error("ключ комнаты — не 32 байта");
  }
  const imported = await importRoomKey(bytes);
  wipe(bytes);
  return imported;
}

// --- сообщение ---------------------------------------------------------

// messageAad привязывает открытые поля конверта к шифротексту: подмена
// любого из них ломает расшифровку (docs/crypto.md, «Сообщение»).
function messageAad({ id, chat, from, keyId }) {
  return utf8(`${MSG_AAD}${id}|${chat}|${from}|${keyId}`);
}

// sealMessage шифрует текст сообщения. plain — JSON {"t": текст};
// ничего кроме текста внутрь не кладётся.
export async function sealMessage(key, { id, chat, from, keyId, text }) {
  const iv = random(IV_LEN);
  const plain = utf8(JSON.stringify({ t: text }));
  const ct = await subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: messageAad({ id, chat, from, keyId }) },
    key,
    plain,
  );
  wipe(plain);
  return { iv: b64url(iv), ct: b64url(ct) };
}

// openMessage расшифровывает конверт и отдаёт текст. Бросает при любой
// порче: не тот ключ, изменившиеся открытые поля, битый base64url.
// Для вызывающего это не фатально — сообщение сохраняется нерасшифрованным
// с кодом причины (docs/storage.md).
export async function openMessage(key, { id, chat, from, keyId, iv, ct }) {
  const nonce = unb64url(iv);
  if (nonce.length !== IV_LEN) {
    throw new Error("iv — не 12 байт");
  }
  const plain = await subtle.decrypt(
    { name: "AES-GCM", iv: nonce, additionalData: messageAad({ id, chat, from, keyId }) },
    key,
    unb64url(ct),
  );
  const bytes = new Uint8Array(plain);
  const parsed = JSON.parse(decoder.decode(bytes));
  wipe(bytes);
  if (parsed === null || typeof parsed !== "object" || typeof parsed.t !== "string") {
    throw new Error("в сообщении нет текста");
  }
  return parsed.t;
}

// --- архив .bare -------------------------------------------------------

// Формат файла — docs/crypto.md, «Экспорт .bare»:
//
//   header = "BARE" (4) || version u8 = 1 || salt (16) || fingerprint (32)
//            || iv (12)                                          // 65 байт
//   file   = header || AES-GCM(exportKey, iv, payload, AAD = header)
//
// Заголовок открыт и целиком входит в AAD: подмена любого его байта ломает
// расшифровку. Ника владельца в нём нет — это лишняя утечка (ADR-014).

const EXPORT_INFO = "bare-export-v1";
const MAGIC = "BARE";

// ARCHIVE_VERSION — версия формата файла. Не версия полезной нагрузки:
// та лежит внутри, полем v, и считается отдельно.
const ARCHIVE_VERSION = 1;

const SALT_LEN = 16;
const FP_LEN = 32;
const MAGIC_AT = 0;
const VERSION_AT = 4;
const SALT_AT = 5;
const FP_AT = SALT_AT + SALT_LEN;
const IV_AT = FP_AT + FP_LEN;

// HEADER_LEN — 65 байт, ровно как в docs/crypto.md.
const HEADER_LEN = IV_AT + IV_LEN;

// Тег AES-GCM — 16 байт: короче шифротекста не бывает даже у пустого архива.
const TAG_LEN = 16;

// archiveKey — ключ одного экспорта: HKDF из секрета аккаунта со случайной
// солью (ADR-014). Секрет — non-extractable CryptoKey типа HKDF; сырых байт
// у клиента нет и быть не должно.
function archiveKey(secret, salt) {
  return subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt, info: utf8(EXPORT_INFO) },
    secret,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

function archiveHeader(salt, fingerprint, iv) {
  const header = new Uint8Array(HEADER_LEN);
  header.set(utf8(MAGIC), MAGIC_AT);
  header[VERSION_AT] = ARCHIVE_VERSION;
  header.set(salt, SALT_AT);
  header.set(fingerprint, FP_AT);
  header.set(iv, IV_AT);
  return header;
}

// sealArchive шифрует полезную нагрузку и собирает файл целиком.
// fingerprint — 32 сырых байта отпечатка владельца.
export async function sealArchive(secret, fingerprint, payload) {
  if (fingerprint.length !== FP_LEN) {
    throw new Error("отпечаток — не 32 байта");
  }
  const salt = random(SALT_LEN);
  const iv = random(IV_LEN);
  const header = archiveHeader(salt, fingerprint, iv);
  const ct = await subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: header },
    await archiveKey(secret, salt),
    payload,
  );
  const file = new Uint8Array(HEADER_LEN + ct.byteLength);
  file.set(header);
  file.set(new Uint8Array(ct), HEADER_LEN);
  return file;
}

// parseArchive читает заголовок, ничего не расшифровывая: отпечаток
// владельца сверяется до вывода ключа (ADR-014). Чужая магия, чужая версия
// и файл короче заголовка с тегом — null.
export function parseArchive(bytes) {
  if (!(bytes instanceof Uint8Array) || bytes.length < HEADER_LEN + TAG_LEN) {
    return null;
  }
  const magic = utf8(MAGIC);
  for (let i = 0; i < magic.length; i += 1) {
    if (bytes[MAGIC_AT + i] !== magic[i]) {
      return null;
    }
  }
  if (bytes[VERSION_AT] !== ARCHIVE_VERSION) {
    return null;
  }
  return {
    header: bytes.subarray(0, HEADER_LEN),
    salt: bytes.subarray(SALT_AT, FP_AT),
    fingerprint: bytes.subarray(FP_AT, IV_AT),
    iv: bytes.subarray(IV_AT, HEADER_LEN),
    ct: bytes.subarray(HEADER_LEN),
  };
}

// openArchive расшифровывает разобранный файл. Ошибка AEAD — единственный
// признак порчи: заголовок целиком в AAD, а всё остальное под тегом.
export async function openArchive(secret, archive) {
  const plain = await subtle.decrypt(
    { name: "AES-GCM", iv: archive.iv, additionalData: archive.header },
    await archiveKey(secret, archive.salt),
    archive.ct,
  );
  return new Uint8Array(plain);
}
