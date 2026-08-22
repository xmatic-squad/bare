// Транспорт и данные чата: устройство, поток событий, приём и отправка.
// Экраны берут отсюда данные и сюда же отдают действия; в db.js и api.js
// они не ходят — пишет в базу только этот модуль.
//
// Правила — docs/protocol.md («События», «Сообщения») и docs/storage.md:
// ACK уходит только после успешной записи в IndexedDB, исходящее живёт
// в pending до 202 и держится за свой ULID, пока время в нём годится
// серверу; отвергнутый по часам переиспользованный id меняется на свежий
// один раз (ADR-036).

import * as api from "./api.js";
import { ApiError, NetworkError } from "./api.js";
import * as db from "./db.js";
import {
  DM_KEY_ID,
  dmKey,
  dmLabel,
  fingerprintOf,
  newId,
  openMessage,
  sealMessage,
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

// on подписывает обработчик и отдаёт функцию отписки. События три:
//
//   "net"      {online}                — доходят ли запросы до сервера
//   "chats"    {}                      — список чатов изменился
//   "messages" {chatId, ids, removed}  — в чате появились, изменились
//                                        или исчезли сообщения
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
    meta = await db.meta(["nick", "privateKey"]);
  } catch {
    return;
  }
  if (!meta.nick || !meta.privateKey) {
    return;
  }
  state.running = true;
  state.nick = meta.nick;
  state.privateKey = meta.privateKey;
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
  state.device = null;
  state.keys.clear();
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
// сообщение сохраняется нерасшифрованным (docs/crypto.md, «Сообщение»).
async function decode(envelope) {
  const me = state.nick;
  const peer = envelope.to.dm
    ? (envelope.from === me ? envelope.to.dm : envelope.from)
    : null;
  const base = {
    id: envelope.id,
    chatId: peer === null ? db.roomChatId(envelope.to.room) : db.dmChatId(peer),
    from: envelope.from,
    text: null,
    ts: envelope.ts,
    status: "sent",
  };
  // Комнаты — этап 3: ключа комнаты на устройстве ещё нет.
  if (peer === null || envelope.keyId !== DM_KEY_ID) {
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
    // Смену ключа собеседника разбирает TOFU (ADR-016) — этап 3;
    // до тех пор любая неудача AEAD выглядит одинаково.
    return { ...base, undecryptable: "bad_aead", raw: envelope };
  }
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
// и повторяет неотправленное (docs/ui.md, «Сеть и состояния»).
// Комнаты — этап 3.
async function afterReady() {
  await refreshContacts();
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
    await rememberPeer(contact.nick, contact.publicKey, contact.createdAt);
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

// chatKey — ключ личного чата из памяти или выведенный заново.
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

// knownPeer — запись TOFU. Ключа нет — берём у сервера и запоминаем
// как есть: сверка изменившегося ключа — этап 3 (ADR-016).
async function knownPeer(nick) {
  const known = await db.peer(nick);
  if (known) {
    return known;
  }
  const user = await api.user(nick);
  return rememberPeer(user.nick, user.publicKey);
}

// rememberPeer запоминает ключ при первом контакте. Уже знакомый ник
// не трогается: смена ключа — состояние, а не перезапись (ADR-016).
async function rememberPeer(nick, publicKey, firstSeen = Date.now()) {
  const known = await db.peer(nick);
  if (known) {
    return known;
  }
  const record = {
    nick,
    publicKey,
    fingerprint: await fingerprintOf(publicKey),
    firstSeen,
    pending: null,
  };
  await db.putPeer(record);
  return record;
}

// --- отправка -----------------------------------------------------------

// send — новое исходящее сообщение. Пустая строка не отправляется;
// предел в maxMessageChars держит строка ввода (docs/ui.md, «Чат»).
// Отдаёт id записи или null, если отправлять нечего.
export function send(chatId, text) {
  const body = String(text ?? "").trim();
  if (!state.running || body === "" || db.peerOf(chatId) === null) {
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
  if (peer === null) {
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
  const err = await post(message, peer);
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

// post шифрует и отдаёт конверт серверу. from в AAD — собственный ник:
// сервер проставит то же значение из сессии, и AAD сойдётся у получателя
// (docs/crypto.md, «Сообщение»).
//
// Отдаёт null при 202 и отказ, если он был: судьбу отказа решает attempt —
// clock_skew на переиспользованном идентификаторе кончается не полосой,
// а второй попыткой.
async function post(message, peer) {
  let envelope;
  try {
    const sealed = await sealMessage(await chatKey(peer), {
      id: message.id,
      chat: dmLabel(state.nick, peer),
      from: state.nick,
      keyId: DM_KEY_ID,
      text: message.text,
    });
    envelope = {
      id: message.id,
      to: { dm: peer },
      keyId: DM_KEY_ID,
      iv: sealed.iv,
      ct: sealed.ct,
    };
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

// transient — отказ, который пройдёт сам: запрос не дошёл или сервер
// не справился. Повтор допустим (ADR-027).
function transient(err) {
  return err instanceof NetworkError || (err instanceof ApiError && err.status >= 500);
}

// --- действия экранов ---------------------------------------------------

// openDm заводит личный чат с ником и отдаёт chatId. Строку списка
// заводит сервер (ADR-019), публичный ключ приходит тем же ответом.
// Ошибки — 404 unknown_user и 400 self (docs/ui.md, «Новый чат»).
export async function openDm(peer) {
  const answer = await api.addContact(peer);
  await rememberPeer(answer.nick, answer.publicKey);
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
// состояние, живёт здесь. dmChatId и peerOf — форма ключа чата
// (docs/storage.md): экраны собирают её из ника маршрута, а не из строки.
export { chats, chat, message, messagesBefore, peer, dmChatId, peerOf, PAGE } from "./db.js";
