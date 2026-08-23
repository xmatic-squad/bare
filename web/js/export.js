// Архив `.bare` — экспорт и импорт истории.
//
// История живёт только на устройстве (ADR-009), и архив — единственный
// способ перенести её на другое (ADR-010). Файл привязан к аккаунту
// криптографически: ключ выводится из секрета аккаунта, и у чужого клиента
// его нет (ADR-014). Формат — docs/crypto.md, «Экспорт .bare», состав
// полезной нагрузки — docs/storage.md.
//
// Модуль работает и без сессии: и история, и секрет аккаунта лежат
// на устройстве. Это и есть смысл кнопки «экспортировать» в подтверждении
// выхода и в подтверждении входа под другим ником (docs/ui.md).

import * as db from "./db.js";
import * as sync from "./sync.js";
import {
  fingerprintBytes,
  fingerprintOf,
  importPublic,
  openArchive,
  parseArchive,
  publicJwk,
  sameBytes,
  sealArchive,
  utf8,
  wipe,
} from "./crypto.js";
import { validUlid } from "./ulid.js";

// Версия полезной нагрузки — поле v внутри шифротекста (docs/crypto.md).
// Версия самого файла живёт в заголовке и считается отдельно.
const PAYLOAD_VERSION = 1;

// Тексты отказа — docs/ui.md, «Настройки», раздел «история».
const BROKEN = "файл повреждён";
const FOREIGN = "архив создан другим аккаунтом";

// Файл отдаётся как двоичный: своего типа у .bare нет и заводить его
// незачем.
const MIME = "application/octet-stream";

// ARCHIVE_EXT — расширение файла (docs/storage.md). Оно же уходит в accept
// выбора файла: предлагать человеку всё подряд незачем.
export const ARCHIVE_EXT = ".bare";

// Временный адрес живёт до конца скачивания: браузер читает Blob по нему
// уже после click. Минута — с запасом на медленный диск.
const REVOKE_AFTER = 60_000;

const decoder = new TextDecoder();

// ArchiveError — отказ импорта. Сообщение уже пригодно для показа
// человеку (ADR-028): причин у отказа ровно две, и обе — в docs/ui.md.
export class ArchiveError extends Error {
  constructor(text) {
    super(text);
    this.name = "ArchiveError";
  }
}

// --- экспорт ------------------------------------------------------------

// exportHistory собирает архив и отдаёт его браузеру на скачивание.
export async function exportHistory() {
  const { nick, publicKey, accountSecret } = await db.meta(["nick", "publicKey", "accountSecret"]);
  if (!nick || !publicKey || !accountSecret) {
    throw new Error("на устройстве нет ключей аккаунта");
  }
  const fingerprint = await fingerprintBytes(await importPublic(publicKey));
  const payload = utf8(JSON.stringify(await collect()));
  let file;
  try {
    file = await sealArchive(accountSecret, fingerprint, payload);
  } finally {
    // Плейнтекст истории в памяти дальше не нужен.
    wipe(payload);
  }
  save(file, fileName(nick));
}

// collect — полезная нагрузка (docs/storage.md, «Экспорт .bare»).
async function collect() {
  const [chats, messages, peers] = await Promise.all([
    // Скрытые чаты — тоже история: «убрать из списка» не удаление (ADR-019).
    db.chats({ hidden: true }),
    db.allMessages(),
    db.allPeers(),
  ]);
  return {
    v: PAYLOAD_VERSION,
    exportedAt: Date.now(),
    chats: chats.map(chatRecord),
    messages: messages.filter(archivable).map(messageRecord),
    peers: peers.map(peerRecord),
  };
}

// archivable — что из ленты попадает в архив: только отправленное.
// Нерасшифрованное не уносится: без текста в архиве от него остался бы один
// заголовок, а raw — служебное поле. Незаконченная и отвергнутая попытки
// не уносятся тоже: pending и failed привязаны к устройству и к своему
// ULID (ADR-036) — на другом устройстве «повторить» отправило бы то же
// сообщение вторым, а запись, ушедшая после повтора под свежим id,
// вернулась бы из архива дублем (ADR-050).
function archivable(record) {
  return typeof record?.text === "string" && record.status === "sent";
}

// fileName — bare-<nick>-<YYYY-MM-DD>.bare (docs/storage.md). Дата местная:
// это день человека, а не UTC.
function fileName(nick) {
  const now = new Date();
  const day = [
    String(now.getFullYear()).padStart(4, "0"),
    String(now.getMonth() + 1).padStart(2, "0"),
    String(now.getDate()).padStart(2, "0"),
  ].join("-");
  return `bare-${nick}-${day}${ARCHIVE_EXT}`;
}

// save отдаёт файл браузеру: Blob, временный адрес и <a download>. Ни
// inline-скриптов, ни атрибутов-обработчиков это не требует, а CSP
// default-src 'self' скачиванию не мешает: сохранение файла — не подгрузка
// ресурса страницы (ADR-021).
function save(bytes, name) {
  const url = URL.createObjectURL(new Blob([bytes], { type: MIME }));
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.hidden = true;
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), REVOKE_AFTER);
}

// --- импорт -------------------------------------------------------------

// importHistory разбирает выбранный файл и вливает его в базу. Порядок —
// docs/crypto.md: магия и версия, потом отпечаток владельца, и только потом
// ключ. Чужой архив не расшифровывается вовсе: сверка отпечатка — вежливость,
// настоящая защита в том, что секрета аккаунта у чужого клиента нет (ADR-014).
//
// Отдаёт число добавленных сообщений.
export async function importHistory(file) {
  const { nick, publicKey, accountSecret } = await db.meta(["nick", "publicKey", "accountSecret"]);
  if (!nick || !publicKey || !accountSecret) {
    throw new Error("на устройстве нет ключей аккаунта");
  }
  let bytes;
  try {
    bytes = new Uint8Array(await file.arrayBuffer());
  } catch {
    // Файл не прочитался: для человека это то же самое, что порча.
    throw new ArchiveError(BROKEN);
  }
  const archive = parseArchive(bytes);
  if (archive === null) {
    throw new ArchiveError(BROKEN);
  }
  const mine = await fingerprintBytes(await importPublic(publicKey));
  if (!sameBytes(archive.fingerprint, mine)) {
    throw new ArchiveError(FOREIGN);
  }
  const payload = await unpack(accountSecret, archive);
  const { added, chats } = await db.mergeArchive({
    chats: list(payload.chats).filter(usableChat).map(chatRecord),
    messages: list(payload.messages).filter((record) => usableMessage(record, nick)).map(messageRecord),
    peers: await peersOf(payload.peers),
  });
  if (chats.length > 0) {
    sync.imported(chats);
  }
  return added;
}

// unpack расшифровывает и разбирает нагрузку. Порча заголовка, порча
// шифротекста и мусор внутри — одно и то же для человека: файл повреждён.
async function unpack(secret, archive) {
  let payload;
  try {
    payload = JSON.parse(decoder.decode(await openArchive(secret, archive)));
  } catch {
    throw new ArchiveError(BROKEN);
  }
  if (payload === null || typeof payload !== "object" || payload.v !== PAYLOAD_VERSION) {
    throw new ArchiveError(BROKEN);
  }
  return payload;
}

function list(value) {
  return Array.isArray(value) ? value : [];
}

// --- записи -------------------------------------------------------------
//
// Один и тот же отбор полей работает в обе стороны: что уходит в архив,
// то и приходит из него. Всё, чего в этих функциях нет, до базы не доходит.
//
// Место чата в списке, счётчик непрочитанных, граница «новых» и «убрано
// из списка» — показания устройства, а не история: lastId, unread,
// lastReadId и hidden в архив не пишутся (ADR-050). У lastId причина
// вторая: он указывает на последнюю строку чата, а ею бывает и та,
// которой в архиве нет, — неотправленная или нерасшифрованная. Устройство,
// принявшее архив, считает его само — по тому, что действительно добавило.

function chatRecord(chat) {
  const record = {
    id: chat.id,
    type: chat.type,
    title: chat.title,
  };
  if (chat.type === "dm") {
    record.peer = chat.peer;
  } else {
    record.roomId = chat.roomId;
  }
  // Владелец и состав есть только у комнаты и приходят от сервера: пока
  // комната не перечитана, их может не быть вовсе.
  if (typeof chat.owner === "string") {
    record.owner = chat.owner;
  }
  if (Array.isArray(chat.members)) {
    record.members = chat.members.filter((nick) => typeof nick === "string");
  }
  return record;
}

function messageRecord(record) {
  return {
    id: record.id,
    chatId: record.chatId,
    from: record.from,
    text: record.text,
    ts: record.ts,
    status: record.status,
  };
}

function peerRecord(record) {
  return {
    nick: record.nick,
    publicKey: publicJwk(record.publicKey),
    fingerprint: record.fingerprint,
    firstSeen: record.firstSeen,
  };
}

// peersOf — записи TOFU из архива. Отпечаток считается заново из ключа:
// человек сверяет голосом именно его, и брать его на веру из файла рядом
// с ключом нельзя (ADR-016). Ключ, из которого отпечаток не считается, —
// не ключ, такая запись пропускается.
async function peersOf(peers) {
  const out = [];
  for (const record of list(peers)) {
    if (!usablePeer(record)) {
      continue;
    }
    try {
      out.push({
        ...peerRecord(record),
        fingerprint: await fingerprintOf(record.publicKey),
        // Ждущий подтверждения ключ в архив не пишется (docs/storage.md).
        pending: null,
      });
    } catch {
      // Не ключ.
    }
  }
  return out;
}

// --- разбор архива ------------------------------------------------------
//
// Архив собрал владелец аккаунта — чужой его не соберёт (ADR-014), — но
// разбирается он на устройстве и ложится в базу рядом с настоящей историей.
// На сетевом пути форму держит сервер (internal/api/valid.go), поэтому
// sync.js обходится проверкой типа; у файла с диска такой опоры нет, и
// форму проверяет клиент, до записи (ADR-054). Запись, которую потом
// нельзя ни открыть, ни убрать, лежала бы в базе навсегда.

// Ник — форма ADR-019; roomId — 16 случайных байт base64url, 22 символа
// (docs/crypto.md, «Идентификаторы»).
const NICK = /^[a-z0-9_]{2,32}$/;
const ROOM_ID = /^[A-Za-z0-9_-]{22}$/;

// MAX_TS — предел Date: дальше `new Date(ts)` не дата вовсе, а лента
// падает на такой строке целиком (ADR-054).
const MAX_TS = 8.64e15;

// known — ключ чата по форме docs/storage.md: «dm:<ник>» или «room:<id>».
function known(chatId) {
  if (typeof chatId !== "string") {
    return false;
  }
  const peer = db.peerOf(chatId);
  if (peer !== null) {
    return NICK.test(peer);
  }
  const roomId = db.roomIdOf(chatId);
  return roomId !== null && ROOM_ID.test(roomId);
}

function usableChat(chat) {
  if (chat === null || typeof chat !== "object" || !known(chat.id)) {
    return false;
  }
  const peer = db.peerOf(chat.id);
  // Вид чата задаёт его ключ: «dm:<ник>» или «room:<id>» (docs/storage.md).
  if (chat.type !== (peer !== null ? "dm" : "room")) {
    return false;
  }
  if (peer !== null ? chat.peer !== peer : chat.roomId !== db.roomIdOf(chat.id)) {
    return false;
  }
  return typeof chat.title === "string";
}

// usableMessage — строка истории. Автор в личном чате — свой ник или ник
// собеседника: третьего в переписке двоих не бывает. В комнате автором
// бывает и вышедший участник, поэтому там сверяется только форма ника.
function usableMessage(record, me) {
  if (record === null || typeof record !== "object" || !known(record.chatId)) {
    return false;
  }
  const peer = db.peerOf(record.chatId);
  if (peer !== null && record.from !== peer && record.from !== me) {
    return false;
  }
  return typeof record.id === "string" && validUlid(record.id)
    && typeof record.from === "string" && NICK.test(record.from)
    && typeof record.text === "string"
    && Number.isSafeInteger(record.ts) && record.ts >= 0 && record.ts <= MAX_TS
    && record.status === "sent";
}

function usablePeer(record) {
  return record !== null && typeof record === "object"
    && typeof record.nick === "string" && NICK.test(record.nick)
    && record.publicKey !== null && typeof record.publicKey === "object"
    && Number.isFinite(record.firstSeen);
}
