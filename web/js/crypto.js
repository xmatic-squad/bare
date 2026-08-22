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

// Длина секрета аккаунта (ADR-014) и вектора инициализации AES-GCM.
export const SECRET_LEN = 32;
const IV_LEN = 12;

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

// fingerprint — SHA-256 несжатой точки публичного ключа, 64 hex строчными.
export async function fingerprint(publicKey) {
  const raw = await subtle.exportKey("raw", publicKey);
  return hex(await subtle.digest("SHA-256", raw));
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
