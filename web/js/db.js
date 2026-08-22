// IndexedDB клиента — схема из docs/storage.md, «Клиент — IndexedDB».
//
// База `bare`, версия 1, все хранилища заводятся сразу: одна версия — одна
// схема, даже если часть хранилищ наполняется на следующих этапах.
// CryptoKey кладётся объектом: structured clone умеет их хранить,
// и приватный ключ остаётся non-extractable.

const NAME = "bare";
const VERSION = 1;

let opening = null;

export function open() {
  if (!opening) {
    opening = new Promise((resolve, reject) => {
      const request = indexedDB.open(NAME, VERSION);
      request.onupgradeneeded = () => create(request.result);
      request.onsuccess = () => {
        const db = request.result;
        // Соседняя вкладка стирает базу при выходе из аккаунта — отпускаем
        // соединение, иначе удаление зависнет заблокированным.
        db.onversionchange = () => {
          db.close();
          opening = null;
        };
        resolve(db);
      };
      request.onerror = () => reject(request.error);
      request.onblocked = () => reject(new Error("база занята другой вкладкой"));
    });
    opening.catch(() => {
      opening = null;
    });
  }
  return opening;
}

function create(db) {
  db.createObjectStore("meta"); // ключ — строка снаружи значения
  db.createObjectStore("chats", { keyPath: "id" });
  const messages = db.createObjectStore("messages", { keyPath: "id" });
  messages.createIndex("chat", ["chatId", "id"]);
  db.createObjectStore("roomKeys", { keyPath: ["roomId", "keyId"] });
  db.createObjectStore("peers", { keyPath: "nick" });
}

function done(tx) {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error ?? new Error("транзакция отменена"));
  });
}

function value(request) {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

// get и put — одна запись одного хранилища. Ключ у chats, messages
// и peers лежит внутри значения (keyPath), поэтому put берёт запись целиком.
async function get(name, key) {
  const db = await open();
  return value(db.transaction(name, "readonly").objectStore(name).get(key));
}

async function put(name, record) {
  const db = await open();
  const tx = db.transaction(name, "readwrite");
  tx.objectStore(name).put(record);
  await done(tx);
}

// meta читает несколько ключей одной транзакцией.
export async function meta(keys) {
  const db = await open();
  const store = db.transaction("meta", "readonly").objectStore("meta");
  const out = {};
  await Promise.all(keys.map(async (key) => {
    out[key] = await value(store.get(key));
  }));
  return out;
}

// putMeta пишет пары «ключ — значение» одной транзакцией.
export async function putMeta(entries) {
  const db = await open();
  const tx = db.transaction("meta", "readwrite");
  const store = tx.objectStore("meta");
  for (const [key, item] of Object.entries(entries)) {
    store.put(item, key);
  }
  await done(tx);
}

// --- чаты ---------------------------------------------------------------

// PAGE — страница ленты: 50 сообщений (docs/storage.md).
export const PAGE = 50;

const DM = "dm:";
const ROOM = "room:";

// Ключ чата — "dm:<собеседник>" или "room:<roomId>" (docs/storage.md).
// Это не метка чата в AAD сообщения: там у личного чата оба ника.
export function dmChatId(peer) {
  return DM + peer;
}

export function roomChatId(roomId) {
  return ROOM + roomId;
}

// peerOf — с кем личный чат; у комнаты собеседника нет.
export function peerOf(chatId) {
  return chatId.startsWith(DM) ? chatId.slice(DM.length) : null;
}

// blankChat — пустая запись чата по её ключу. title — имя без «@» и «#»:
// сигил ставит экран. Комнате имя приходит из GET /api/rooms (этап 3),
// до этого вместо имени стоит идентификатор.
export function blankChat(id) {
  const base = { id, title: "", lastId: null, lastReadId: null, unread: 0, hidden: false };
  const peer = peerOf(id);
  if (peer !== null) {
    return { ...base, type: "dm", title: peer, peer };
  }
  const roomId = id.slice(ROOM.length);
  return { ...base, type: "room", title: roomId, roomId };
}

// chats — список чатов в порядке docs/ui.md: по lastId по убыванию.
// Скрытые («убрать из списка») не отдаются, пока их не попросят.
export async function chats({ hidden = false } = {}) {
  const db = await open();
  const store = db.transaction("chats", "readonly").objectStore("chats");
  const list = await value(store.getAll());
  return list.filter((c) => hidden || !c.hidden).sort(byLastId);
}

function byLastId(a, b) {
  if (a.lastId !== b.lastId) {
    if (!a.lastId) {
      return 1;
    }
    if (!b.lastId) {
      return -1;
    }
    return a.lastId < b.lastId ? 1 : -1;
  }
  return a.id < b.id ? -1 : 1;
}

export function chat(id) {
  return get("chats", id);
}

export function putChat(record) {
  return put("chats", record);
}

// markRead — чат прочитан: счётчик обнуляется, граница «новых» уезжает
// к последнему сообщению. Обе величины локальные, на сервер не уходят
// (docs/storage.md).
export async function markRead(chatId) {
  const db = await open();
  const tx = db.transaction("chats", "readwrite");
  const store = tx.objectStore("chats");
  const record = await value(store.get(chatId));
  if (record) {
    record.unread = 0;
    record.lastReadId = record.lastId;
    store.put(record);
  }
  await done(tx);
  return record ?? null;
}

// hideChat прячет чат из списка или возвращает его туда. История
// не трогается: «убрать из списка» — не удаление (ADR-019).
export async function hideChat(chatId, hidden) {
  const db = await open();
  const tx = db.transaction("chats", "readwrite");
  const store = tx.objectStore("chats");
  const record = (await value(store.get(chatId))) ?? blankChat(chatId);
  record.hidden = hidden;
  store.put(record);
  await done(tx);
  return record;
}

// --- сообщения ----------------------------------------------------------

export function message(id) {
  return get("messages", id);
}

// saveMessages пишет сообщения и обновляет их чаты одной транзакцией.
// ACK серверу уходит только после успешной записи (docs/storage.md),
// поэтому лента и счётчик непрочитанных не должны расходиться.
//
// remove — идентификаторы, которые надо убрать: устаревший ULID
// неотправленного сообщения меняется на свежий, и старая запись уходит
// (ADR-036).
// me — собственный ник: свои сообщения непрочитанными не считаются.
// incoming — сообщения пришли из потока событий: известный id
// игнорируется целиком, перезаписи нет (ADR-034).
//
// Отдаёт ключи затронутых чатов.
export async function saveMessages({
  messages = [],
  remove = [],
  me = null,
  incoming = false,
} = {}) {
  if (messages.length === 0 && remove.length === 0) {
    return [];
  }
  const db = await open();
  const tx = db.transaction(["messages", "chats"], "readwrite");
  const store = tx.objectStore("messages");
  const chatStore = tx.objectStore("chats");

  for (const id of remove) {
    store.delete(id);
  }
  // Оба чтения — запросы этой же транзакции: она живёт, пока их ждут.
  const known = await Promise.all(messages.map((m) => value(store.get(m.id))));
  const ids = [...new Set(messages.map((m) => m.chatId))];
  const records = await Promise.all(ids.map((id) => value(chatStore.get(id))));

  const touched = new Map();
  ids.forEach((id, i) => touched.set(id, records[i] ?? blankChat(id)));

  // Повтор доставки не должен ни дублировать ленту, ни двигать счётчик:
  // сервер выдаёт очередь заново при каждом подключении и вправе
  // прислать конверт дважды в одной пачке (ADR-017). Дубли внутри пачки
  // видны только здесь: known собран до первого put.
  const seen = new Set();
  messages.forEach((m, i) => {
    const twice = seen.has(m.id);
    seen.add(m.id);
    // Входящее с уже известным id игнорируется целиком: id открыт
    // в конверте, и перезапись отдала бы собеседнику чужую запись
    // в истории (ADR-034). Исходящее по своему id пишется всегда —
    // это переход pending → sent/failed.
    if (twice || (incoming && known[i] !== undefined)) {
      return;
    }
    store.put(m);
    const record = touched.get(m.chatId);
    if (!record.lastId || record.lastId < m.id) {
      record.lastId = m.id;
    }
    if (known[i] !== undefined) {
      return;
    }
    if (m.from !== me && (!record.lastReadId || record.lastReadId < m.id)) {
      record.unread += 1;
    }
    // Новое сообщение возвращает скрытый чат в список.
    record.hidden = false;
  });

  for (const record of touched.values()) {
    chatStore.put(record);
  }
  await done(tx);
  return [...touched.keys()];
}

// messagesBefore — страница ленты назад от before, не включая его,
// по индексу "chat" (docs/storage.md). Отдаёт по возрастанию id.
export async function messagesBefore(chatId, before = null, limit = PAGE) {
  const db = await open();
  const store = db.transaction("messages", "readonly").objectStore("messages");
  // Ключ индекса — [chatId, id]. Массив больше любой строки, поэтому
  // [chatId, []] — верхняя граница всех сообщений чата, а [chatId] —
  // нижняя: короткий массив идёт раньше своих продолжений.
  const range = before
    ? IDBKeyRange.bound([chatId], [chatId, before], false, true)
    : IDBKeyRange.bound([chatId], [chatId, []]);
  const out = [];
  await cursor(store.index("chat").openCursor(range, "prev"), (record) => {
    out.push(record);
    return out.length < limit;
  });
  out.reverse();
  return out;
}

// pendingMessages — неотправленное по возрастанию id. Индекса по статусу
// в схеме нет (docs/storage.md), поэтому это проход курсором: он делается
// один раз при старте, дальше отправитель ведёт свой список.
export async function pendingMessages() {
  const db = await open();
  const store = db.transaction("messages", "readonly").objectStore("messages");
  const out = [];
  await cursor(store.openCursor(), (record) => {
    if (record.status === "pending") {
      out.push(record);
    }
    return true;
  });
  return out;
}

// cursor обходит курсор, пока step не скажет «хватит».
function cursor(request, step) {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => {
      const current = request.result;
      if (!current || !step(current.value)) {
        resolve();
        return;
      }
      current.continue();
    };
    request.onerror = () => reject(request.error);
  });
}

// --- собеседники --------------------------------------------------------

// peers — доверие к ключам, TOFU (ADR-016). Запись заводится при первом
// получении ключа; сверка изменившегося ключа и pending — этап 3.
export function peer(nick) {
  return get("peers", nick);
}

export function putPeer(record) {
  return put("peers", record);
}

// persist просит браузер не вычищать базу: история на устройстве —
// единственная копия (docs/storage.md).
export async function persist() {
  if (!navigator.storage?.persist) {
    return false;
  }
  try {
    return await navigator.storage.persist();
  } catch {
    return false;
  }
}

// destroy стирает базу целиком: выход из аккаунта уносит историю
// (docs/storage.md, один аккаунт на браузерный профиль).
export async function destroy() {
  const db = await open().catch(() => null);
  if (db) {
    db.close();
  }
  opening = null;
  await new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(NAME);
    request.onsuccess = () => resolve();
    request.onerror = () => reject(request.error);
    // Блокировка — не ответ: соседние вкладки закрывают соединение
    // по versionchange, после чего удаление доходит до success.
    // Сказать «история удалена», не удалив её, нельзя.
    request.onblocked = () => {};
  });
}
