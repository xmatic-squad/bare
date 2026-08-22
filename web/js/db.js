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
