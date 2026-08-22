// Транспорт и данные чата: устройство, поток событий, приём и отправка.
// Экраны берут отсюда данные и сюда же отдают действия; в db.js и api.js
// они не ходят — пишет в базу только этот модуль.
//
// Правила — docs/protocol.md («События», «Сообщения», «Комнаты»)
// и docs/storage.md: ACK уходит только после успешной записи в IndexedDB,
// исходящее живёт в pending до 202 и держится за свой ULID, пока время
// в нём годится серверу; отвергнутый по часам переиспользованный id
// меняется на свежий один раз (ADR-036).
//
// Доверие к ключам — TOFU (ADR-016): каждый публичный ключ, пришедший
// от сервера, сверяется с запомненным; изменившийся ложится в pending
// и блокирует отправку до подтверждения. Ключи комнат — docs/crypto.md,
// «Комната»: владелец заворачивает новый ключ каждому участнику, клиент
// держит все ключи комнаты и расшифровывает любым известным.

import * as api from "./api.js";
import { ApiError, NetworkError } from "./api.js";
import * as db from "./db.js";
import {
  DM_KEY_ID,
  dmKey,
  dmLabel,
  fingerprintOf,
  importRoomKey,
  newId,
  newRoomKey,
  openMessage,
  roomLabel,
  sealMessage,
  unwrapRoomKey,
  wipe,
  wrapRoomKey,
} from "./crypto.js";
import { ulid, ulidTime, validUlid } from "./ulid.js";

// Пауза перед восстановлением закрытого потока: удваивается, пока
// не упрётся в предел. Живой ready сбрасывает её обратно.
const RETRY_MIN = 1000;
const RETRY_MAX = 30000;

// Владение потоком одно на браузерный профиль: устройство у вкладок общее,
// а соединение на устройство сервер держит одно (ADR-035).
const STREAM_LOCK = "bare-stream";
const CHANNEL = "bare";

// Оба API нужны вместе: замок выбирает владельца потока, канал раздаёт
// его находки остальным вкладкам. Нет хотя бы одного — работаем как
// одна вкладка (ADR-035).
const shared = typeof BroadcastChannel === "function" && !!navigator.locks;

const state = {
  running: false,
  nick: null,
  privateKey: null,
  // Свой публичный ключ: он проверен при входе, и спрашивать его у сервера
  // незачем — заворачивание себе идёт по нему (docs/crypto.md, «Комната»).
  publicKey: null,
  device: null,
  close: null, // закрыть поток событий
  online: false,
  timer: null,
  wait: RETRY_MIN,
  // Владение потоком: release отпускает замок, claim отменяет ожидание.
  release: null,
  claim: null,
  channel: null,
  // Ключи личных чатов — только в памяти: в IndexedDB они не пишутся,
  // а выводятся заново из peers (docs/crypto.md, «Чат 1:1»).
  keys: new Map(),
  // Ключи комнат: "<roomId>|<keyId>" → CryptoKey. Это кэш над хранилищем
  // roomKeys, где ключи и живут.
  roomKeys: new Map(),
  // Комнаты, где rekey упёрся в неподтверждённый ключ: roomId → ники
  // (ADR-016). Состояние экрана, в базу не пишется.
  blocked: new Map(),
  // Комнаты, которым мы должны новый ключ: участник вышел, а rekey
  // не прошёл. Долг поднимается и из события room, и из GET /api/rooms:
  // это состояние комнаты, и офлайн владельца его не теряет (ADR-041).
  // Отдаётся после ready и после подтверждения ключа (ADR-018).
  owed: new Set(),
  // Неотправленное. Полный проход по messages делается один раз при
  // старте: индекса по статусу в схеме нет (docs/storage.md).
  pending: new Set(),
  // Конверты, пришедшие по SSE и ещё не разобранные.
  inbox: [],
  scheduled: false,
  // Отложенный разбор конвертов, которые сейчас не разобрать.
  inboxTimer: null,
  hold: RETRY_MIN,
};

// --- события для экранов -----------------------------------------------

const bus = new EventTarget();

// on подписывает обработчик и отдаёт функцию отписки. События пять:
//
//   "net"      {online}                — доходят ли запросы до сервера
//   "chats"    {}                      — список чатов изменился
//   "messages" {chatId, ids, removed, whole}
//                                      — в чате появились, изменились
//                                        или исчезли сообщения; whole
//                                        означает «перечитай ленту
//                                        целиком», без перечня (ADR-050)
//   "peers"    {nick}                  — доверие к ключу ника изменилось:
//                                        появился pending или его подтвердили
//   "rooms"    {id}                    — комната изменилась: имя, состав,
//                                        ключ, потребность в rekey
//
// removed непуст, только когда повтор отправки выдал сообщению новый
// ULID: старую запись из ленты надо убрать. Обычный повтор идёт с прежним
// идентификатором, removed пуст, и лента не перерисовывается (ADR-036).
export function on(type, handler) {
  const wrapped = (event) => handler(event.detail);
  bus.addEventListener(type, wrapped);
  return () => bus.removeEventListener(type, wrapped);
}

function emit(type, detail = {}) {
  bus.dispatchEvent(new CustomEvent(type, { detail }));
}

// notify рассылает изменения: экрану чата нужна лента, сайдбару — список.
// Те же изменения уходят соседним вкладкам: поток событий у профиля один,
// а база общая (ADR-035).
function notify(messages, removed = []) {
  const byChat = new Map();
  const slot = (chatId) => {
    if (!byChat.has(chatId)) {
      byChat.set(chatId, { chatId, ids: [], removed: [] });
    }
    return byChat.get(chatId);
  };
  for (const m of messages) {
    slot(m.chatId).ids.push(m.id);
  }
  for (const r of removed) {
    slot(r.chatId).removed.push(r.id);
  }
  const details = [...byChat.values()];
  for (const detail of details) {
    emit("messages", detail);
  }
  emit("chats");
  share({
    kind: "changed",
    details,
    // Отправленное и похороненное повтора больше не ждёт. О том, что
    // осталось pending, соседям говорит settle: пока попытка идёт,
    // повтор из соседней вкладки отправил бы то же сообщение второй
    // раз (ADR-035).
    settled: messages.filter((m) => m.status !== "pending").map((m) => m.id)
      .concat(removed.map((r) => r.id)),
  });
}

// announceChats — список чатов изменился без сообщений: прочитан чат,
// заведён или скрыт собеседник.
function announceChats() {
  emit("chats");
  share({ kind: "chats" });
}

// announcePeer — доверие к ключу ника изменилось: карточке контакта нужен
// новый отпечаток, чату — полоса про смену ключа (docs/ui.md).
function announcePeer(nick) {
  emit("peers", { nick });
  share({ kind: "peers", nick });
}

// announceRoom — комната изменилась: экрану участников нужен свежий состав
// и знание, чей ключ мешает rekey. Список ников едет вместе с событием:
// он живёт в памяти вкладки, а в базе его нет.
function announceRoom(id) {
  emit("rooms", { id });
  share({ kind: "rooms", id, blocked: needsTrust(id) });
}

// imported — импорт архива влил историю в базу (ADR-050). Перечня
// добавленного в событии нет: сообщений бывает несколько тысяч и они
// старые, поэтому лента перечитывается целиком, а не строка за строкой.
// Запись сделал export.js, здесь остаётся поднять экраны — свои
// и соседних вкладок (ADR-035).
export function imported(chatIds) {
  const details = chatIds.map((chatId) => ({ chatId, ids: [], removed: [], whole: true }));
  for (const detail of details) {
    emit("messages", detail);
  }
  emit("chats");
  share({ kind: "changed", details, settled: [] });
}

// --- соседние вкладки ---------------------------------------------------

// share отдаёт изменение соседним вкладкам. Канал открыт, только пока
// синхронизация жива: после выхода база стирается, рассылать нечего.
function share(payload) {
  state.channel?.postMessage(payload);
}

function openChannel() {
  if (!shared || state.channel) {
    return;
  }
  state.channel = new BroadcastChannel(CHANNEL);
  state.channel.addEventListener("message", (event) => receive(event.data));
  // Вкладка, открытая позже владельца, состояния сети ещё не знает.
  share({ kind: "hello" });
}

function closeChannel() {
  state.channel?.close();
  state.channel = null;
}

// receive применяет чужое изменение: в базу оно уже записано той вкладкой,
// здесь остаётся поднять экраны. Рассылать это дальше нельзя — иначе
// сообщение ходило бы по кругу.
function receive(data) {
  if (!state.running || data === null || typeof data !== "object") {
    return;
  }
  switch (data.kind) {
    case "hello":
      // Отвечает владелец: только он знает, цел ли поток.
      if (state.release) {
        share({ kind: "net", online: state.online });
      }
      return;
    case "net":
      applyOnline(data.online === true);
      return;
    case "chats":
      emit("chats");
      return;
    case "peers":
      if (typeof data.nick === "string") {
        // Ключ личного чата выведен из ключа собеседника и лежит в памяти
        // этой вкладки: доверие изменилось — выводим заново из peers
        // (docs/crypto.md, «Чат 1:1»). Без этого вкладка продолжила бы
        // шифровать ключом, который человек только что отверг.
        state.keys.delete(data.nick);
        // Сохранённые raw перебирает владелец потока: конверты, разобранные
        // устаревшим ключом до сброса, иначе остались бы нерасшифрованными.
        if (state.release) {
          serial(reopen);
        }
        emit("peers", { nick: data.nick });
      }
      return;
    case "rooms":
      if (typeof data.id === "string") {
        if (Array.isArray(data.blocked) && data.blocked.length > 0) {
          state.blocked.set(data.id, data.blocked);
        } else {
          state.blocked.delete(data.id);
        }
        emit("rooms", { id: data.id });
      }
      return;
    case "pending":
      // Соседняя вкладка не отправила сообщение и повторять его не будет:
      // повторяет владелец потока.
      for (const id of data.ids ?? []) {
        state.pending.add(id);
      }
      return;
    case "changed":
      for (const id of data.settled ?? []) {
        state.pending.delete(id);
      }
      for (const detail of data.details ?? []) {
        emit("messages", detail);
      }
      emit("chats");
      return;
    default:
  }
}

// --- жизненный цикл -----------------------------------------------------

// start поднимает синхронизацию после входа или восстановления сессии.
// Ключи берутся из IndexedDB: наружу они не выходят.
export async function start() {
  if (state.running) {
    return;
  }
  let meta;
  try {
    meta = await db.meta(["nick", "privateKey", "publicKey"]);
  } catch {
    return;
  }
  if (!meta.nick || !meta.privateKey || !meta.publicKey) {
    return;
  }
  state.running = true;
  state.nick = meta.nick;
  state.privateKey = meta.privateKey;
  state.publicKey = meta.publicKey;
  openChannel();
  try {
    for (const m of await db.pendingMessages()) {
      state.pending.add(m.id);
    }
  } catch {
    // Не прочли — повторим при следующем запуске; отправка не сломана.
  }
  await connect();
}

// stop гасит синхронизацию: выход, удаление аккаунта, истёкшая сессия.
// Базу не трогает — это дело main.js.
export function stop() {
  state.running = false;
  clearTimer();
  if (state.inboxTimer !== null) {
    clearTimeout(state.inboxTimer);
    state.inboxTimer = null;
  }
  if (state.close) {
    state.close();
    state.close = null;
  }
  // Замок отпускается раньше, чем гаснет всё остальное: соседняя вкладка
  // ждёт очереди и займёт поток сразу (ADR-035).
  yieldStream();
  closeChannel();
  state.nick = null;
  state.privateKey = null;
  state.publicKey = null;
  state.device = null;
  state.keys.clear();
  state.roomKeys.clear();
  state.blocked.clear();
  state.owed.clear();
  state.pending.clear();
  state.inbox.length = 0;
  state.wait = RETRY_MIN;
  state.hold = RETRY_MIN;
  setOnline(false);
}

export function online() {
  return state.online;
}

export function nick() {
  return state.nick;
}

export function deviceId() {
  return state.device;
}

// onSent ставит обработчик успешной отправки: по первой из них клиент
// один раз просит разрешение на уведомления (docs/ui.md, «Уведомления»).
// «Первой за всю историю устройства» это делает не здесь: транспорт
// не знает ни про разрешения, ни про то, о чём уже спрашивали. Ставит
// обработчик main.js.
let sentHandler = () => {};

export function onSent(handler) {
  sentHandler = handler;
}

// --- устройство ---------------------------------------------------------

// ensureDevice — deviceId устройства: 16 случайных байт base64url,
// заводится при первом входе и живёт в IndexedDB (ADR-017).
// 409 device_conflict означает, что идентификатор занят другим аккаунтом:
// берём новый.
async function ensureDevice() {
  let id = (await db.meta(["deviceId"])).deviceId ?? null;
  for (let attempt = 0; attempt < 3; attempt += 1) {
    if (!id) {
      id = newId();
      await db.putMeta({ deviceId: id });
    }
    try {
      await api.registerDevice(id);
      return id;
    } catch (err) {
      if (err instanceof ApiError && err.code === "device_conflict") {
        id = null;
        continue;
      }
      throw err;
    }
  }
  throw new Error("не удалось завести устройство");
}

// --- поток событий ------------------------------------------------------

async function connect() {
  if (!state.running) {
    return;
  }
  clearTimer();
  try {
    state.device = await ensureDevice();
  } catch (err) {
    // 401 unauthenticated уже увёл на экран входа и остановил нас.
    if (err instanceof NetworkError) {
      // Запрос не дошёл — это и есть «нет соединения» (ADR-028).
      setOnline(false);
    }
    if (transient(err)) {
      retryLater();
    }
    return;
  }
  if (state.release) {
    // Поток уже наш: переподключение идёт под тем же замком.
    openStream();
    return;
  }
  claimStream();
}

// claimStream берёт владение потоком. Устройство у вкладок одного профиля
// общее (ADR-017), а соединение на устройство сервер держит одно: без
// арбитража вкладки бесконечно отбирали бы поток друг у друга. Замок
// держится, пока жива синхронизация; ожидающие вкладки живут на
// broadcast от владельца (ADR-035).
function claimStream() {
  if (state.claim) {
    return;
  }
  if (!shared) {
    openStream();
    return;
  }
  const claim = new AbortController();
  state.claim = claim;
  navigator.locks.request(STREAM_LOCK, { signal: claim.signal }, () => new Promise((release) => {
    state.claim = null;
    if (!state.running) {
      release();
      return;
    }
    state.release = release;
    openStream();
  })).catch(() => {
    // Ожидание отменено выходом или замок не дался — потока у нас нет.
    if (state.claim === claim) {
      state.claim = null;
    }
  });
}

// yieldStream отпускает владение: соседняя вкладка займёт поток сразу.
function yieldStream() {
  if (state.claim) {
    state.claim.abort();
    state.claim = null;
  }
  if (state.release) {
    state.release();
    state.release = null;
  }
}

function openStream() {
  if (state.close) {
    state.close();
  }
  state.close = api.stream(state.device, {
    msg: (envelope) => {
      if (envelope) {
        state.inbox.push(envelope);
        schedule();
      }
    },
    // Комнаты разбираются в общей очереди работ: приём сообщений и раздача
    // ключей не должны перемешиваться.
    room: (room) => {
      if (room) {
        serial(() => applyRoom(room));
      }
    },
    roomLeft: (data) => {
      if (typeof data?.id === "string") {
        serial(() => forgetRoom(data.id));
      }
    },
    ready: () => {
      state.wait = RETRY_MIN;
      setOnline(true);
      serial(afterReady);
    },
    error: (closed) => {
      setOnline(false);
      // Браузер переподключается сам, пока поток не закрыт насовсем.
      if (closed) {
        retryLater();
      }
    },
  });
}

function retryLater() {
  if (state.timer !== null || !state.running) {
    return;
  }
  const delay = state.wait;
  state.wait = Math.min(delay * 2, RETRY_MAX);
  state.timer = setTimeout(() => {
    state.timer = null;
    recover();
  }, delay);
}

function clearTimer() {
  if (state.timer !== null) {
    clearTimeout(state.timer);
    state.timer = null;
  }
}

// recover разбирает окончательно закрытый поток. Причин две: сессии
// больше нет — это увидит GET /api/me и уведёт на экран входа; или
// устройства больше нет — тогда его надо завести заново.
async function recover() {
  if (!state.running) {
    return;
  }
  try {
    await api.me();
  } catch (err) {
    if (err instanceof NetworkError) {
      retryLater();
    }
    return;
  }
  await connect();
}

// setOnline — состояние сети этой вкладки. Владелец потока рассказывает
// о нём соседям: своего потока у них нет (ADR-035).
function setOnline(value) {
  if (state.online === value) {
    return;
  }
  applyOnline(value);
  if (state.release) {
    share({ kind: "net", online: value });
  }
}

function applyOnline(value) {
  if (state.online === value) {
    return;
  }
  state.online = value;
  emit("net", { online: value });
}

// --- очередь работ ------------------------------------------------------

// serial выстраивает работу с базой в очередь: приём, отправка и повтор
// не должны идти одновременно.
let chain = Promise.resolve();

function serial(task) {
  const next = chain.then(() => task());
  chain = next.catch(() => {});
  return next;
}

// schedule откладывает разбор входящих на следующий такт: очередь при
// подключении приходит событием на конверт, а записать её и подтвердить
// лучше пачкой. Разбор забирает всё, что успело накопиться.
function schedule() {
  if (state.scheduled) {
    return;
  }
  state.scheduled = true;
  setTimeout(() => {
    state.scheduled = false;
    serial(flush);
  }, 0);
}

// --- приём --------------------------------------------------------------

async function flush() {
  const batch = state.inbox.splice(0, state.inbox.length);
  if (batch.length === 0) {
    return;
  }
  const messages = [];
  const acked = [];
  const kept = [];
  for (const envelope of batch) {
    if (!usable(envelope)) {
      // Разобрать нечего, но и держать это в очереди сервера незачем.
      if (typeof envelope?.id === "string") {
        acked.push(envelope.id);
      }
      continue;
    }
    const record = await decode(envelope);
    if (record === null) {
      // Ключа сейчас не добыть по причине, которая пройдёт: конверт
      // остаётся у нас и разбирается заново. Ждать переподключения
      // нельзя — поток цел и рваться не собирается.
      kept.push(envelope);
      continue;
    }
    messages.push(record);
    acked.push(record.id);
  }
  if (messages.length > 0) {
    await db.saveMessages({ messages, me: state.nick, incoming: true });
    notify(messages);
  }
  if (kept.length > 0) {
    state.inbox.unshift(...kept);
    postpone();
  } else {
    state.hold = RETRY_MIN;
  }
  // ACK — только после успешной записи (docs/storage.md).
  await ackAll(acked);
}

// postpone откладывает повторный разбор: причина, по которой конверт не
// разобрался, проходит сама, но сообщать о себе не умеет. Пауза
// удваивается, удачный разбор возвращает её к минимуму.
function postpone() {
  if (state.inboxTimer !== null || !state.running) {
    return;
  }
  const delay = state.hold;
  state.hold = Math.min(delay * 2, RETRY_MAX);
  state.inboxTimer = setTimeout(() => {
    state.inboxTimer = null;
    schedule();
  }, delay);
}

// usable — форма конверта (docs/protocol.md, «Типы»). Сервер её проверяет,
// но запись в базу собирается из этих полей, и мусор до неё не доходит.
function usable(e) {
  return e !== null && typeof e === "object"
    && typeof e.id === "string" && validUlid(e.id)
    && typeof e.from === "string"
    && typeof e.keyId === "string"
    && typeof e.iv === "string" && typeof e.ct === "string"
    && Number.isFinite(e.ts)
    && (typeof e.to?.dm === "string") !== (typeof e.to?.room === "string");
}

// decode превращает конверт в запись messages. null означает «сейчас
// не разобрать по причине, которая пройдёт»: конверт остаётся и у нас,
// и в очереди сервера — ACK по нему не уходит. Ошибка AEAD
// и неизвестный keyId причиной не являются —
// сообщение сохраняется нерасшифрованным (docs/crypto.md, «Сообщение»)
// вместе с raw: по нему попытка повторяется, когда ключ появится
// или когда новый ключ собеседника подтвердят (docs/storage.md).
async function decode(envelope) {
  const me = state.nick;
  const room = typeof envelope.to.room === "string" ? envelope.to.room : null;
  const peer = room === null
    ? (envelope.from === me ? envelope.to.dm : envelope.from)
    : null;
  const base = {
    id: envelope.id,
    chatId: room === null ? db.dmChatId(peer) : db.roomChatId(room),
    from: envelope.from,
    text: null,
    ts: envelope.ts,
    status: "sent",
  };

  if (room !== null) {
    // Комната расшифровывается любым известным ключом по keyId конверта:
    // клиент держит все ключи комнаты (ADR-018).
    let key;
    try {
      key = await roomKeyOf(room, envelope.keyId);
    } catch {
      return null;
    }
    if (key === null) {
      return { ...base, undecryptable: "unknown_key", raw: envelope };
    }
    try {
      return { ...base, text: await openMessage(key, { ...envelope, chat: roomLabel(room) }) };
    } catch {
      return { ...base, undecryptable: "bad_aead", raw: envelope };
    }
  }

  if (envelope.keyId !== DM_KEY_ID) {
    return { ...base, undecryptable: "unknown_key", raw: envelope };
  }
  let key;
  try {
    key = await chatKey(peer);
  } catch (err) {
    if (transient(err)) {
      return null;
    }
    // Ник исчез: публичного ключа не будет и позже, но raw остаётся.
    return { ...base, undecryptable: "unknown_key", raw: envelope };
  }
  try {
    const text = await openMessage(key, { ...envelope, chat: dmLabel(me, peer) });
    return { ...base, text };
  } catch {
    // Расшифровка идёт доверенным ключом. Не сошлось, а у ника ждёт
    // подтверждения новый, — сообщение зашифровано им (ADR-016).
    const known = await db.peer(peer).catch(() => null);
    return {
      ...base,
      undecryptable: known?.pending ? "key_changed" : "bad_aead",
      raw: envelope,
    };
  }
}

// reopen — повторная расшифровка сохранённого raw: пришёл недостающий ключ
// комнаты или подтверждён новый ключ собеседника (docs/storage.md).
// Записи свои, а не входящие: перезапись по id здесь законна (ADR-034).
async function reopen() {
  let list;
  try {
    list = await db.undecryptable();
  } catch {
    return;
  }
  const messages = [];
  for (const record of list) {
    if (!usable(record.raw)) {
      continue;
    }
    const fresh = await decode(record.raw);
    if (fresh === null || fresh.text === null) {
      continue;
    }
    messages.push(fresh);
  }
  if (messages.length === 0) {
    return;
  }
  await db.saveMessages({ messages, me: state.nick });
  notify(messages);
}

async function ackAll(ids) {
  for (let i = 0; i < ids.length; i += api.MAX_ACK) {
    try {
      await api.ack(state.device, ids.slice(i, i + api.MAX_ACK));
    } catch {
      // Не подтвердили — сервер выдаст конверты заново, а put по тому же
      // id дублей не создаст (ADR-017).
      return;
    }
  }
}

// --- после ready --------------------------------------------------------

// afterReady — очередь выдана целиком. Клиент перечитывает контакты
// и комнаты и повторяет неотправленное (docs/ui.md, «Сеть и состояния»):
// события room и room_left в очередь не кладутся, и пропущенное во время
// офлайна восстанавливается только этим (docs/protocol.md, «События»).
async function afterReady() {
  await refreshContacts();
  await refreshRooms();
  await payRekeys();
  await retryPending();
}

async function refreshContacts() {
  let list;
  try {
    list = await api.contacts();
  } catch {
    return;
  }
  let changed = false;
  for (const contact of list) {
    // Список контактов — главное место сверки TOFU: ключи всех собеседников
    // приходят от сервера после каждого ready (ADR-016).
    await seePeer(contact.nick, contact.publicKey, contact.createdAt).catch(() => {});
    const chatId = db.dmChatId(contact.nick);
    if (!(await db.chat(chatId))) {
      await db.putChat(db.blankChat(chatId));
      changed = true;
    }
  }
  if (changed) {
    announceChats();
  }
}

// retryPending повторяет неотправленное после подключения. Идёт прямо,
// без serial: afterReady уже внутри очереди.
async function retryPending() {
  for (const id of [...state.pending]) {
    let record;
    try {
      record = await db.message(id);
    } catch {
      return;
    }
    if (!record || record.status !== "pending") {
      state.pending.delete(id);
      continue;
    }
    await attempt(record, record.id);
  }
}

// --- собеседники --------------------------------------------------------

// chatKey — ключ личного чата из памяти или выведенный заново. Выводится
// он из доверенного ключа: ждущий подтверждения в дело не идёт (ADR-016).
async function chatKey(peer) {
  const cached = state.keys.get(peer);
  if (cached) {
    return cached;
  }
  const record = await knownPeer(peer);
  const key = await dmKey(state.privateKey, record.publicKey, state.nick, peer);
  state.keys.set(peer, key);
  return key;
}

// knownPeer — запись TOFU. Ника ещё нет — берём ключ у сервера: первый
// ключ запоминается молча, trust on first use (ADR-016).
async function knownPeer(nick) {
  const known = await db.peer(nick);
  if (known) {
    return known;
  }
  const user = await api.user(nick);
  return seePeer(user.nick, user.publicKey);
}

// seePeer — сверка TOFU. Зовётся при каждом получении публичного ключа ника,
// откуда бы он ни пришёл: GET /api/users, список контактов, состав комнаты,
// отправитель завёрнутого ключа (ADR-016).
//
// Первый ключ ника запоминается молча. Совпавший — ничего не меняет.
// Изменившийся ложится в pending: отправка этому нику блокируется, входящее
// его ключом остаётся нерасшифрованным, rekey ему не выполняется — всё
// до явного «доверять новому ключу».
//
// Сервер, вернувшийся к доверенному ключу, снимает pending: смены не
// случилось, а подтверждать было бы уже отозванный ключ (ADR-040,
// docs/ui.md, «Карточка контакта»).
async function seePeer(nick, publicKey, firstSeen = Date.now()) {
  const fingerprint = await fingerprintOf(publicKey);
  const known = await db.peer(nick);
  if (!known) {
    const record = { nick, publicKey, fingerprint, firstSeen, pending: null };
    await db.putPeer(record);
    return record;
  }
  if (known.fingerprint === fingerprint) {
    if (!known.pending) {
      return known;
    }
    const record = { ...known, pending: null };
    await db.putPeer(record);
    announcePeer(nick);
    return record;
  }
  if (known.pending?.fingerprint === fingerprint) {
    return known;
  }
  const record = { ...known, pending: { publicKey, fingerprint, seenAt: Date.now() } };
  await db.putPeer(record);
  announcePeer(nick);
  return record;
}

// trustKey — «доверять новому ключу» из карточки контакта (docs/ui.md).
// Ключ из pending становится основным, pending чистится, и всё, что
// упиралось в старый ключ, повторяется: завёрнутые ключи комнат от этого
// ника, сохранённые raw и неотправленное.
//
// Отдаёт, было ли что подтверждать.
export function trustKey(nick) {
  return serial(async () => {
    if (!state.running) {
      return false;
    }
    const known = await db.peer(nick);
    if (!known?.pending) {
      return false;
    }
    await db.putPeer({
      nick: known.nick,
      publicKey: known.pending.publicKey,
      fingerprint: known.pending.fingerprint,
      // firstSeen — когда ник встретился впервые, а не когда сменил ключ.
      firstSeen: known.firstSeen,
      pending: null,
    });
    // Ключ личного чата выводится из ключа собеседника — выводим заново.
    state.keys.delete(nick);
    announcePeer(nick);
    await refreshRooms();
    // Владелец, чей rekey упирался в этот ключ, доводит его до конца.
    await payRekeys();
    await reopen();
    await retryPending();
    return true;
  });
}

// --- комнаты ------------------------------------------------------------

// TrustNeeded — rekey не выполняется участнику с изменившимся и
// неподтверждённым ключом (ADR-016). nicks — чьи ключи ждут подтверждения;
// владелец повторяет операцию после «доверять новому ключу».
export class TrustNeeded extends Error {
  constructor(nicks) {
    super("нужно подтвердить ключ");
    this.name = "TrustNeeded";
    this.nicks = nicks;
  }
}

// needsTrust — чьи ключи мешают rekey комнаты (docs/ui.md, «Участники»).
export function needsTrust(roomId) {
  return state.blocked.get(roomId) ?? [];
}

// usableRoom и usableKey — форма Room и завёрнутого ключа
// (docs/protocol.md, «Типы»). Сервер её держит, но записи собираются
// из этих полей, и мусор до базы не доходит.
function usableRoom(r) {
  return r !== null && typeof r === "object"
    && typeof r.id === "string" && r.id !== ""
    && typeof r.name === "string"
    && typeof r.owner === "string"
    && Array.isArray(r.members) && r.members.every((nick) => typeof nick === "string")
    && Number.isFinite(r.createdAt)
    && (r.key === null || r.key === undefined || usableKey(r.key));
}

function usableKey(k) {
  return k !== null && typeof k === "object"
    && typeof k.keyId === "string" && k.keyId !== ""
    && typeof k.from === "string"
    && typeof k.iv === "string" && typeof k.ct === "string";
}

// roomKeyOf — ключ комнаты по keyId конверта; null, если такого нет.
async function roomKeyOf(roomId, keyId) {
  const at = `${roomId}|${keyId}`;
  const cached = state.roomKeys.get(at);
  if (cached) {
    return cached;
  }
  const record = await db.roomKey(roomId, keyId);
  if (!record) {
    return null;
  }
  state.roomKeys.set(at, record.key);
  return record.key;
}

// currentKeyId — текущий ключ комнаты: последний полученный этим
// устройством. Им шифруется исходящее. Порядок — получения, а не сервера:
// номера ключа протокол не несёт, и в гонке двух rekey эти порядки могут
// разойтись (ADR-042). Оба ключа при этом живы, сервер принимает любой.
async function currentKeyId(roomId) {
  const list = await db.roomKeysOf(roomId);
  return list.length > 0 ? list[list.length - 1].keyId : null;
}

// saveRoom кладёт комнату в список чатов. Имя, владелец и состав приходят
// от сервера; лента, счётчик непрочитанных и граница «новых» — местные.
function saveRoom(room) {
  return db.mergeChat(db.roomChatId(room.id), {
    type: "room",
    roomId: room.id,
    title: room.name,
    owner: room.owner,
    members: [...room.members],
    // Комната в списке есть, пока мы её участники.
    hidden: false,
  });
}

// senderKey — публичный ключ того, кто завернул ключ комнаты. Свой берётся
// с устройства: он проверен при входе и от сервера не зависит. Чужой
// приходит от сервера и проходит через TOFU; ключ, ждущий подтверждения, —
// null: разворачивать им нельзя (ADR-016).
async function senderKey(nick) {
  if (nick === state.nick) {
    return state.publicKey;
  }
  const user = await api.user(nick);
  const record = await seePeer(nick, user.publicKey);
  return record.pending ? null : record.publicKey;
}

// takeRoomKey разворачивает завёрнутый нам ключ комнаты и кладёт его
// в roomKeys вместе с from и receivedAt (docs/crypto.md, «Комната»).
// Отдаёт, появился ли новый ключ.
//
// Уже известный keyId не трогается: клиент держит все ключи комнаты.
// Не развернувшийся не теряется — сервер отдаёт его снова с каждым
// GET /api/rooms.
//
// Заворачивает ключ участник комнаты — владелец или тот, кто им был
// до передачи владения (ADR-018). Ключ от постороннего ника отвергается
// до запроса его публичного ключа: TOFU запоминает первый ключ молча,
// поэтому незнакомый распространитель — это подмена, а не первый
// контакт (ADR-039).
async function takeRoomKey(room) {
  const wrapped = room.key;
  if (!usableKey(wrapped) || !room.members.includes(wrapped.from)) {
    return false;
  }
  if (await db.roomKey(room.id, wrapped.keyId)) {
    return false;
  }
  let publicKey;
  try {
    publicKey = await senderKey(wrapped.from);
  } catch {
    // Ключа отправителя сейчас не добыть: попробуем при следующем ready.
    return false;
  }
  if (publicKey === null) {
    return false;
  }
  let key;
  try {
    key = await unwrapRoomKey(state.privateKey, publicKey, {
      roomId: room.id,
      keyId: wrapped.keyId,
      from: wrapped.from,
      to: state.nick,
      iv: wrapped.iv,
      ct: wrapped.ct,
    });
  } catch {
    return false;
  }
  const stored = await db.saveRoomKey({
    roomId: room.id,
    keyId: wrapped.keyId,
    key,
    from: wrapped.from,
  });
  if (stored) {
    state.roomKeys.set(`${room.id}|${wrapped.keyId}`, key);
  }
  return stored;
}

// applyRoom разбирает событие room: создание, смена состава, rekey, выход
// участника (docs/protocol.md, «События»).
async function applyRoom(room) {
  if (!state.running || !usableRoom(room)) {
    return;
  }
  const changed = await saveRoom(room);
  const fresh = await takeRoomKey(room);
  if (changed) {
    announceChats();
  }
  announceRoom(room.id);
  if (fresh) {
    await reopen();
    await retryPending();
  }
  // Участник вышел — комната осталась на ключе, который он знает. Новый
  // раздаёт владелец тем же запросом с пустыми add и remove (ADR-018).
  if (room.needsRekey === true && room.owner === state.nick) {
    await rekey(room.id);
  }
}

// refreshRooms перечитывает комнаты после каждого ready: события room
// и room_left в очередь не кладутся (docs/protocol.md, «События»).
async function refreshRooms() {
  // Список известных комнат читается до запроса: комната, заведённая
  // соседней вкладкой, пока ответ летел, в него не попадёт, а прятать
  // её нельзя — она есть и на сервере, и в базе (ADR-035).
  let known;
  try {
    known = await db.chats();
  } catch {
    known = [];
  }
  let list;
  try {
    list = await api.rooms();
  } catch {
    return;
  }
  if (!Array.isArray(list)) {
    return;
  }
  let changed = false;
  let fresh = false;
  const seen = new Set();
  for (const room of list) {
    if (!usableRoom(room)) {
      continue;
    }
    seen.add(room.id);
    const moved = await saveRoom(room);
    const key = await takeRoomKey(room);
    changed = changed || moved;
    fresh = fresh || key;
    if (moved || key) {
      announceRoom(room.id);
    }
    // Долг по ключу — состояние комнаты, а не свойство события (ADR-041):
    // владелец поднимает его и после офлайна, и после перезагрузки вкладки.
    // Отдаёт долг payRekeys — он идёт следом за refreshRooms.
    if (room.needsRekey === true && room.owner === state.nick) {
      state.owed.add(room.id);
    }
  }
  // Комнату, из которой нас убрали, пока мы были офлайн, видно только так:
  // события мы не получили, а в списке её больше нет.
  for (const chat of known) {
    if (chat.type === "room" && !seen.has(chat.roomId)) {
      await db.hideChat(chat.id, true);
      state.blocked.delete(chat.roomId);
      state.owed.delete(chat.roomId);
      announceRoom(chat.roomId);
      changed = true;
    }
  }
  if (changed) {
    announceChats();
  }
  // retryPending зовёт afterReady следом — второй раз не нужно.
  if (fresh) {
    await reopen();
  }
}

// forgetRoom убирает комнату из списка: нас удалили, комната удалена или
// мы вышли сами. История на устройстве не трогается — она единственная
// копия, а новых сообщений в этой комнате нам уже не доставят.
async function forgetRoom(roomId) {
  state.blocked.delete(roomId);
  state.owed.delete(roomId);
  const chatId = db.roomChatId(roomId);
  const record = await db.chat(chatId);
  if (!record || record.hidden) {
    return;
  }
  await db.hideChat(chatId, true);
  announceChats();
  announceRoom(roomId);
}

// memberKeys — публичные ключи итогового состава с проверкой TOFU
// (ADR-016, ADR-018). Ник с неподтверждённым ключом останавливает всю
// операцию: rekey ему не выполняется, а состав без ключа невозможен.
async function memberKeys(members) {
  const keys = new Map();
  const blocked = [];
  for (const nick of members) {
    if (nick === state.nick) {
      keys.set(nick, state.publicKey);
      continue;
    }
    const user = await api.user(nick);
    const record = await seePeer(nick, user.publicKey);
    if (record.pending) {
      blocked.push(nick);
      continue;
    }
    keys.set(nick, record.publicKey);
  }
  if (blocked.length > 0) {
    throw new TrustNeeded(blocked);
  }
  return keys;
}

// distribute генерирует ключ комнаты и заворачивает его каждому участнику,
// включая себя: заворачивание себе — ECDH(myPrivate, myPublic), тем же кодом
// (docs/crypto.md, «Комната»). Сырые байты живут до конца заворачивания,
// потом импортируются non-extractable и затираются.
async function distribute(roomId, members, keys) {
  const { keyId, bytes } = newRoomKey();
  try {
    const wrapped = [];
    for (const nick of members) {
      wrapped.push(await wrapRoomKey(
        state.privateKey,
        keys.get(nick),
        { roomId, keyId, from: state.nick, to: nick },
        bytes,
      ));
    }
    return { keyId, wrapped, key: await importRoomKey(bytes) };
  } finally {
    wipe(bytes);
  }
}

// keepRoomKey кладёт свой же розданный ключ: у распространителя он
// не разворачивается, а берётся из сырых байт до их затирания.
async function keepRoomKey(roomId, keyId, key) {
  const stored = await db.saveRoomKey({ roomId, keyId, key, from: state.nick });
  if (stored) {
    state.roomKeys.set(`${roomId}|${keyId}`, key);
  }
  return stored;
}

// createRoom заводит комнату. Идентификатор генерирует клиент: ключ
// заворачивается до запроса и привязан к roomId (ADR-037). Отдаёт chatId.
export function createRoom(name) {
  return serial(async () => {
    const title = String(name ?? "").trim();
    if (!state.running || title === "") {
      return null;
    }
    const keys = await memberKeys([state.nick]);
    for (let attempt = 0; attempt < 3; attempt += 1) {
      const roomId = newId();
      const { keyId, wrapped, key } = await distribute(roomId, [state.nick], keys);
      let room;
      try {
        room = await api.createRoom(state.device, {
          id: roomId,
          name: title,
          keyId,
          keys: wrapped,
        });
      } catch (err) {
        if (err instanceof ApiError && err.code === "room_conflict") {
          continue;
        }
        throw err;
      }
      await keepRoomKey(roomId, keyId, key);
      // Форма ответа так же непроверена, как форма события. Чужой
      // идентификатор в ответе означал бы ключ, привязанный не к той
      // комнате: roomId вплетён в info и AAD (ADR-037).
      if (usableRoom(room) && room.id === roomId) {
        await saveRoom(room);
      }
      announceChats();
      announceRoom(roomId);
      return db.roomChatId(roomId);
    }
    throw new Error("не удалось завести комнату");
  });
}

// changeMembers — смена состава и rekey одним запросом (ADR-018): владелец
// получает публичные ключи итогового состава с проверкой TOFU, генерирует
// ключ, заворачивает каждому и только потом отправляет.
//
// Ники приходят уже приведёнными к форме ADR-019: их проверяет экран.
export function changeMembers(roomId, { add = [], remove = [] } = {}) {
  return serial(() => changeRoom(roomId, add, remove));
}

// rekey — новый ключ прежнему составу: тот же запрос с пустыми add
// и remove (ADR-018). Зовётся у владельца, получившего room с needsRekey.
// Неудача долг не снимает: попытка повторится после ready.
async function rekey(roomId) {
  state.owed.add(roomId);
  try {
    await changeRoom(roomId, [], []);
  } catch (err) {
    if (err instanceof TrustNeeded) {
      // Владелец видит, чей ключ надо подтвердить, и повторяет операцию
      // после подтверждения (ADR-016).
      state.blocked.set(roomId, err.nicks);
      announceRoom(roomId);
      return;
    }
    // Сеть или отказ сервера: попытка повторится после следующего ready.
  }
}

// payRekeys отдаёт долги по ключам комнат. Комната, которой мы больше
// не владеем или из которой ушли, долг снимает: новый ключ раздаёт
// её владелец.
async function payRekeys() {
  for (const roomId of [...state.owed]) {
    let record;
    try {
      record = await db.chat(db.roomChatId(roomId));
    } catch {
      return;
    }
    if (!record || record.type !== "room" || record.hidden || record.owner !== state.nick) {
      state.owed.delete(roomId);
      if (state.blocked.delete(roomId)) {
        announceRoom(roomId);
      }
      continue;
    }
    await rekey(roomId);
  }
}

async function changeRoom(roomId, add, remove) {
  if (!state.running) {
    return null;
  }
  const record = await db.chat(db.roomChatId(roomId));
  if (!record || record.type !== "room") {
    throw new Error("нет такой комнаты");
  }
  const members = finalMembers(record.members ?? [], add, remove);
  const keys = await memberKeys(members);
  const { keyId, wrapped, key } = await distribute(roomId, members, keys);
  const room = await api.changeMembers(roomId, { add, remove, keyId, keys: wrapped });
  const stored = await keepRoomKey(roomId, keyId, key);
  // Ключ роздан всему составу: долг закрыт, и подтверждать больше нечего.
  state.owed.delete(roomId);
  state.blocked.delete(roomId);
  if (usableRoom(room)) {
    await saveRoom(room);
  }
  announceChats();
  announceRoom(roomId);
  if (stored) {
    await reopen();
    await retryPending();
  }
  return room;
}

// finalMembers — итоговый состав: текущий без remove плюс add, без повторов
// и с сохранением порядка.
function finalMembers(current, add, remove) {
  const gone = new Set(remove);
  const seen = new Set();
  const out = [];
  for (const nick of [...current.filter((nick) => !gone.has(nick)), ...add]) {
    if (seen.has(nick)) {
      continue;
    }
    seen.add(nick);
    out.push(nick);
  }
  return out;
}

// leaveRoom — «выйти из комнаты». Владение уходит участнику с наименьшим
// joined_at, опустевшая комната удаляется — это дело сервера (ADR-018).
export function leaveRoom(roomId) {
  return serial(async () => {
    await api.leaveRoom(state.device, roomId);
    await forgetRoom(roomId);
  });
}

// deleteRoom — «удалить комнату», только у владельца. Участникам уходит
// room_left.
export function deleteRoom(roomId) {
  return serial(async () => {
    await api.removeRoom(roomId);
    await forgetRoom(roomId);
  });
}

// --- отправка -----------------------------------------------------------

// Postponed — отправить сейчас нечем, но причина пройдёт: нет ключа комнаты
// или ключ собеседника изменился и ждёт подтверждения (ADR-016). Сообщение
// остаётся pending и уходит, когда причина уйдёт.
class Postponed extends Error {
  constructor() {
    super("отправка отложена");
    this.name = "Postponed";
  }
}

// send — новое исходящее сообщение. Пустая строка не отправляется;
// предел в maxMessageChars держит строка ввода (docs/ui.md, «Чат»).
// Отдаёт id записи или null, если отправлять нечего.
export function send(chatId, text) {
  const body = String(text ?? "").trim();
  const known = db.peerOf(chatId) !== null || db.roomIdOf(chatId) !== null;
  if (!state.running || body === "" || !known) {
    return Promise.resolve(null);
  }
  return serial(() => attempt({ chatId, text: body }, null));
}

// retry — повтор с пометки «не отправлено».
export function retry(id) {
  if (!state.running) {
    return Promise.resolve(null);
  }
  return serial(async () => {
    const record = await db.message(id);
    if (!record || record.status === "sent") {
      return null;
    }
    return attempt(record, record.id);
  });
}

// REUSE — запас под окно часов сервера: он принимает сообщение, пока время
// в ULID расходится с его часами не больше чем на пять минут (ADR-017).
// Идентификатор переиспользуется, пока до края окна остаётся минута: за неё
// успевают шифрование, очередь работ и сама сеть, так что дошедший запрос
// застаёт окно ещё открытым.
const REUSE = 4 * 60 * 1000;

// attempt — одна попытка отправки. Прежний ULID сохраняется, пока его время
// годится серверу: ответ на POST мог потеряться после того, как сервер
// сообщение принял, и повтор с тем же идентификатором получатель молча
// пропустит (ADR-034), а повтор с новым лёг бы у него вторым сообщением
// (ADR-036). Идентификатор старше запаса заменяется свежим, и тогда старая
// запись удаляется: время в id должно совпадать с временем фактической
// отправки — иначе после долгого офлайна сервер ответит clock_skew.
//
// fresh требует свежий идентификатор, каким бы годным ни выглядел прежний:
// так возвращается попытка, у которой переиспользованный id сервер отверг
// по часам.
async function attempt(source, previousId, fresh = false) {
  const peer = db.peerOf(source.chatId);
  const roomId = db.roomIdOf(source.chatId);
  if (peer === null && roomId === null) {
    state.pending.delete(previousId);
    return null;
  }
  const keep = !fresh && previousId !== null && reusable(previousId);
  const message = {
    id: keep ? previousId : ulid(),
    chatId: source.chatId,
    from: state.nick,
    text: source.text,
    // Время показа идёт за идентификатором: сохранённый id оставляет
    // и прежнее ts — до 202, которое принесёт серверное.
    ts: keep ? source.ts : Date.now(),
    status: "pending",
  };
  const stale = previousId !== null && !keep;
  if (stale) {
    state.pending.delete(previousId);
  }
  state.pending.add(message.id);
  await db.saveMessages({
    messages: [message],
    remove: stale ? [previousId] : [],
    me: state.nick,
  });
  notify([message], stale ? [{ chatId: source.chatId, id: previousId }] : []);
  const err = await post(message, peer, roomId);
  if (err === null) {
    return message.id;
  }
  // Возраст переиспользованного id сервер считает по своим часам: к времени,
  // проведённому в pending, добавляется расхождение часов. Отставание в пару
  // минут выводит за окно идентификатор, который клиенту кажется свежим.
  // Это ровно та причина, ради которой id и меняется, — берём свежий и идём
  // второй раз. Второго круга нет: fresh снимает переиспользование, и такой
  // же отказ на свежем id означает, что часы врут по-настоящему (ADR-036).
  if (keep && err instanceof ApiError && err.code === "clock_skew") {
    return attempt(message, message.id, true);
  }
  await settle(message, err);
  return message.id;
}

// reusable — годится ли прежний идентификатор для новой попытки. Часы
// сравниваются со своими же: других у клиента нет, и первый ULID берётся
// из них же. Часы, врущие сверх окна, отсекает сервер: clock_skew на
// переиспользованном id разбирает attempt, на свежем — settle.
function reusable(id) {
  const ms = ulidTime(id);
  return ms !== null && Math.abs(Date.now() - ms) < REUSE;
}

// dmEnvelope — конверт личного чата. Ключ собеседника изменился и ждёт
// подтверждения — отправка блокируется (ADR-016): сообщение остаётся
// pending и уходит после «доверять новому ключу».
async function dmEnvelope(message, peer) {
  const known = await db.peer(peer);
  if (known?.pending) {
    throw new Postponed();
  }
  const sealed = await sealMessage(await chatKey(peer), {
    id: message.id,
    chat: dmLabel(state.nick, peer),
    from: state.nick,
    keyId: DM_KEY_ID,
    text: message.text,
  });
  return { id: message.id, to: { dm: peer }, keyId: DM_KEY_ID, iv: sealed.iv, ct: sealed.ct };
}

// roomEnvelope — конверт комнаты: keyId текущего ключа, chat — "room:<id>"
// (docs/crypto.md, «Сообщение»). Ключа ещё нет — отправка откладывается
// до его прихода.
async function roomEnvelope(message, roomId) {
  const keyId = await currentKeyId(roomId);
  const key = keyId === null ? null : await roomKeyOf(roomId, keyId);
  if (key === null) {
    throw new Postponed();
  }
  const sealed = await sealMessage(key, {
    id: message.id,
    chat: roomLabel(roomId),
    from: state.nick,
    keyId,
    text: message.text,
  });
  return { id: message.id, to: { room: roomId }, keyId, iv: sealed.iv, ct: sealed.ct };
}

// post шифрует и отдаёт конверт серверу. from в AAD — собственный ник:
// сервер проставит то же значение из сессии, и AAD сойдётся у получателя
// (docs/crypto.md, «Сообщение»).
//
// Отдаёт null при 202 и отказ, если он был: судьбу отказа решает attempt —
// clock_skew на переиспользованном идентификаторе кончается не полосой,
// а второй попыткой.
async function post(message, peer, roomId) {
  let envelope;
  try {
    envelope = peer !== null
      ? await dmEnvelope(message, peer)
      : await roomEnvelope(message, roomId);
  } catch (err) {
    return err;
  }
  try {
    const answer = await api.sendMessage(state.device, envelope);
    // Запрос дошёл: сеть есть, что бы ни думал поток событий (ADR-028).
    setOnline(true);
    state.pending.delete(message.id);
    const sent = { ...message, status: "sent", ts: answer?.ts ?? message.ts };
    await db.saveMessages({ messages: [sent], me: state.nick });
    notify([sent]);
    // Сообщение ушло: обработчик решает, спрашивать ли разрешение
    // на уведомления. Отправку он не задерживает и сорвать не может.
    try {
      sentHandler();
    } catch {
      // Дело обработчика; отправка состоялась.
    }
    return null;
  } catch (err) {
    // Ответ с кодом — то же доказательство, что запрос дошёл, что и 202:
    // сеть есть, что бы ни думал поток событий (ADR-028). Ошибка шифрования
    // сюда не попадает — она случается до запроса. 401 unauthenticated уже
    // увёл на экран входа: состояние сети там ничьё.
    if (err instanceof ApiError && state.running) {
      setOnline(true);
    }
    return err;
  }
}

// settle разбирает отказ. Сеть и 500 сообщение не хоронят: оно остаётся
// pending и повторится при следующем подключении (ADR-027). Удалённое
// устройство чинится тем же способом — переподключением. Остальные 4xx —
// failed с текстом отказа (ADR-033).
async function settle(message, err) {
  if (err instanceof NetworkError) {
    // Поток событий молчания сети не замечает: у EventSource нет
    // таймаута на тишину. Не дошедший запрос — та же полоса «нет
    // соединения» (docs/ui.md, «Сеть и состояния», ADR-028).
    setOnline(false);
  }
  if (transient(err)) {
    share({ kind: "pending", ids: [message.id] });
    return;
  }
  if (err instanceof ApiError && err.code === "unknown_device") {
    share({ kind: "pending", ids: [message.id] });
    retryLater();
    return;
  }
  state.pending.delete(message.id);
  const failed = { ...message, status: "failed", error: api.errorText(err) };
  await db.saveMessages({ messages: [failed], me: state.nick });
  notify([failed]);
}

// transient — отказ, который пройдёт сам: запрос не дошёл, сервер
// не справился или шифровать пока нечем. Повтор допустим (ADR-027).
function transient(err) {
  return err instanceof NetworkError
    || err instanceof Postponed
    || (err instanceof ApiError && err.status >= 500);
}

// --- действия экранов ---------------------------------------------------

// openDm заводит личный чат с ником и отдаёт chatId. Строку списка
// заводит сервер (ADR-019), публичный ключ приходит тем же ответом.
// Ошибки — 404 unknown_user и 400 self (docs/ui.md, «Новый чат»).
export async function openDm(peer) {
  const answer = await api.addContact(peer);
  await seePeer(answer.nick, answer.publicKey);
  const chatId = db.dmChatId(answer.nick);
  const existing = await db.chat(chatId);
  if (!existing || existing.hidden) {
    // hideChat читает и пишет одной транзакцией и заводит недостающую
    // запись: приём сообщений идёт своим чередом и в неё не врезается.
    await db.hideChat(chatId, false);
    announceChats();
  }
  return chatId;
}

// forgetChat — «убрать из списка» в карточке контакта. Строка на сервере
// уходит, зеркальная у собеседника остаётся: это не блокировка (ADR-019).
// История на устройстве не трогается — чат прячется.
export async function forgetChat(chatId) {
  const peer = db.peerOf(chatId);
  if (peer !== null) {
    await api.removeContact(peer);
  }
  await db.hideChat(chatId, true);
  announceChats();
}

// markRead — чат прочитан. Граница «новых» и счётчик локальные, на сервер
// не уходят (docs/storage.md).
export async function markRead(chatId) {
  const record = await db.markRead(chatId);
  announceChats();
  return record;
}

// Чтение для экранов. Писать в базу им не нужно: всё, что меняет
// состояние, живёт здесь. dmChatId, roomChatId, peerOf и roomIdOf — форма
// ключа чата (docs/storage.md): экраны собирают её из ника или
// идентификатора маршрута, а не из строки.
export {
  chats,
  chat,
  message,
  messagesBefore,
  peer,
  dmChatId,
  roomChatId,
  peerOf,
  roomIdOf,
  PAGE,
} from "./db.js";
