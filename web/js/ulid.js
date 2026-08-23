// ULID — идентификатор сообщения: 48 бит миллисекунд и 80 бит случайности,
// Crockford base32, 26 символов (docs/crypto.md, «Идентификаторы»).
//
// Заглавные буквы обязательны: идентификатор входит в AAD шифротекста
// побайтно, и сервер строчные не принимает.
//
// Модуль не знает про DOM: его можно импортировать в node и прогнать.

// crockford — алфавит base32 без I, L, O и U.
const ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

const TIME_LEN = 10; // 50 бит, старшие два обязаны быть нулевыми
const RANDOM_LEN = 16; // 80 бит
const RANDOM_BYTES = 10;

export const ULID_LEN = TIME_LEN + RANDOM_LEN;

// MAX_TIME — предел 48 бит: дальше метка времени в ULID не помещается.
const MAX_TIME = 2 ** 48 - 1;

// Последняя выданная миллисекунда и её случайная часть. Внутри одной
// миллисекунды случайная часть инкрементируется (docs/crypto.md):
// два сообщения, набранные подряд, не получают одинаковый идентификатор
// и сортируются в порядке отправки.
let lastMs = -1;
const lastRandom = new Uint8Array(RANDOM_BYTES);

export function ulid(now = Date.now()) {
  const ms = Math.floor(now);
  if (!Number.isSafeInteger(ms) || ms < 0 || ms > MAX_TIME) {
    throw new RangeError("время вне 48 бит");
  }
  if (ms === lastMs) {
    bump(lastRandom);
  } else {
    lastMs = ms;
    globalThis.crypto.getRandomValues(lastRandom);
  }
  return encodeTime(ms) + encodeRandom(lastRandom);
}

// ulidTime — метка времени идентификатора в миллисекундах; null, если
// это не ULID. Сервер считает ту же величину и сравнивает со своими
// часами: расхождение больше пяти минут — clock_skew (ADR-017).
export function ulidTime(id) {
  if (typeof id !== "string" || id.length !== ULID_LEN) {
    return null;
  }
  let ms = 0;
  for (let i = 0; i < ULID_LEN; i += 1) {
    const value = ALPHABET.indexOf(id[i]);
    if (value < 0) {
      return null;
    }
    if (i < TIME_LEN) {
      ms = ms * 32 + value;
    }
  }
  return ms > MAX_TIME ? null : ms;
}

export function validUlid(id) {
  return ulidTime(id) !== null;
}

// bump увеличивает случайную часть на единицу. Переполнение всех 80 бит
// внутри одной миллисекунды невозможно на практике; если оно всё же
// случилось, берём новые случайные байты.
function bump(bytes) {
  for (let i = bytes.length - 1; i >= 0; i -= 1) {
    if (bytes[i] < 255) {
      bytes[i] += 1;
      return;
    }
    bytes[i] = 0;
  }
  globalThis.crypto.getRandomValues(bytes);
}

function encodeTime(ms) {
  const out = new Array(TIME_LEN);
  let rest = ms;
  for (let i = TIME_LEN - 1; i >= 0; i -= 1) {
    out[i] = ALPHABET[rest % 32];
    rest = Math.floor(rest / 32);
  }
  return out.join("");
}

// encodeRandom режет 80 бит на 16 групп по 5: остатка нет.
function encodeRandom(bytes) {
  let out = "";
  let acc = 0;
  let bits = 0;
  for (let i = 0; i < bytes.length; i += 1) {
    acc = (acc << 8) | bytes[i];
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += ALPHABET[(acc >>> bits) & 31];
    }
  }
  return out;
}
