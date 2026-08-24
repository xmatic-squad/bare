// Service worker: кэш оболочки, пуши и переход по уведомлению (ADR-023).
//
// Оболочка отдаётся stale-while-revalidate: сначала из кэша, следом —
// проверка у сервера. Всё под /api/ не кэшируется никогда: ни ответы,
// ни ошибки — это чужая почта, а не оболочка.
//
// Имя кэша содержит версию; версия — константа, она меняется при релизе,
// и старые кэши уходят в activate.

const VERSION = "v7";
const CACHE = `bare-${VERSION}`;

// Оболочка — всё, из чего клиент поднимается без сети. Список явный:
// у Cache API нет масок, а угадывать нечего — файлов немного и они
// перечислены в docs/plan.md (ADR-001).
const SHELL = [
  "/",
  "/app.css",
  "/manifest.json",
  "/js/api.js",
  "/js/crypto.js",
  "/js/db.js",
  "/js/export.js",
  "/js/main.js",
  "/js/pwa.js",
  "/js/sync.js",
  "/js/ulid.js",
  "/js/zoom.js",
  "/js/ui/auth.js",
  "/js/ui/chat.js",
  "/js/ui/chats.js",
  "/js/ui/contact.js",
  "/js/ui/dom.js",
  "/js/ui/members.js",
  "/js/ui/new.js",
  "/js/ui/settings.js",
  "/js/ui/shell.js",
  "/icons/icon.svg",
  "/icons/mark.svg",
  "/icons/icon-180.png",
  "/icons/icon-192.png",
  "/icons/icon-512.png",
];

// Иконка уведомления — знак из /icons/ (ADR-023).
const ICON = "/icons/icon-192.png";

// Идентификатор чата из нагрузки пуша (ADR-023) и маршруты клиента
// (docs/ui.md, «Каркас») — разные формы одного и того же; перевод
// одной в другую — ADR-046.
const DM = /^dm:([a-z0-9_]{2,32})$/;
const ROOM = /^room:([A-Za-z0-9_-]{22})$/;

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((cache) => cache.addAll(SHELL)));
});

// Установленная версия ждёт очереди, пока страница не попросит её включить
// (ADR-068). Просит именно страница: в установленном на «Домой»
// приложении перезагрузить оболочку больше нечем, а терять набранное
// в строке ввода нельзя — знает об этом только она.
//
// На «version» воркер называет свою версию. По ней страница отличает новую
// оболочку от того же файла воркера, отданного сервером заново: первая
// стоит перезагрузки, второй — нет (ADR-070). Ответ уходит каналом
// вопроса: вопросов бывает два подряд, к разным воркерам.
self.addEventListener("message", (event) => {
  if (event.data === "skip-waiting") {
    self.skipWaiting();
    return;
  }
  if (event.data === "version") {
    event.ports[0]?.postMessage(VERSION);
  }
});

// activate уносит кэши прежних версий: имя кэша содержит версию, и всё,
// что названо иначе, — прошлый релиз. clients.claim берёт под контроль
// уже открытую страницу: без этого первый запуск остался бы без кэша
// и без перехода по уведомлению.
self.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    for (const name of await caches.keys()) {
      if (name !== CACHE) {
        await caches.delete(name);
      }
    }
    await self.clients.claim();
  })());
});

self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.method !== "GET") {
    return;
  }
  const url = new URL(request.url);
  if (url.origin !== self.location.origin || !shell(url.pathname)) {
    return;
  }
  event.respondWith(revalidate(event, request, url.origin + url.pathname + url.search));
});

// shell — что относится к оболочке. Всё прочее идёт в сеть мимо кэша:
// /api/ — потому что это данные (ADR-023), /sw.js — потому что его
// обновление ведёт браузер, /healthz — потому что он про сервер.
function shell(path) {
  return path === "/"
    || path === "/app.css"
    || path === "/manifest.json"
    || path.startsWith("/js/")
    || path.startsWith("/icons/");
}

// revalidate — stale-while-revalidate. Ответ из кэша уходит сразу, запрос
// к серверу идёт своим ходом и обновляет кэш. Сервер отдаёт статику
// с ETag и Cache-Control: no-cache (docs/protocol.md), поэтому обычный
// fetch — это условный запрос: неизменившийся файл стоит одного 304.
//
// Ключ кэша — адрес без фрагмента: у навигационного запроса в url лежит
// маршрут (`/#/dm/marta`), и по самому запросу запись `/` подменялась бы
// адресом последней перезагрузки. Сопоставление фрагмент и так
// игнорирует, а вот caches.keys() должен говорить правду о том, что
// лежит в оболочке (ADR-001).
function revalidate(event, request, key) {
  return caches.open(CACHE).then(async (cache) => {
    const cached = await cache.match(key);
    const network = fetch(request).then((response) => {
      // Кладём только цельный свой ответ: чужие и частичные в оболочке
      // не бывают.
      if (response.ok && response.type === "basic") {
        cache.put(key, response.clone());
      }
      return response;
    });
    if (cached) {
      // Проверка переживёт ответ: без waitUntil браузер вправе усыпить
      // service worker сразу после отдачи страницы.
      event.waitUntil(network.catch(() => {}));
      return cached;
    }
    return network;
  });
}

// push → уведомление. Содержимого сообщения в нагрузке нет и быть
// не может: сервер его не знает (ADR-011). tag — идентификатор чата:
// новое уведомление заменяет старое в том же чате (ADR-023).
self.addEventListener("push", (event) => {
  const data = payload(event);
  if (data === null) {
    return;
  }
  event.waitUntil(self.registration.showNotification(data.title, {
    body: data.body,
    tag: data.chat,
    data: { chat: data.chat },
    icon: ICON,
    lang: "ru",
  }));
});

// payload разбирает нагрузку пуша: {title, body, chat} (ADR-023).
// Чужого здесь не бывает — пуш подписан ключом сервера, — но показывать
// неразобранное всё равно нечем.
function payload(event) {
  let data = null;
  try {
    data = event.data ? event.data.json() : null;
  } catch {
    return null;
  }
  if (data === null || typeof data !== "object") {
    return null;
  }
  const { title, body, chat } = data;
  if (typeof title !== "string" || typeof body !== "string" || typeof chat !== "string") {
    return null;
  }
  if (title === "" || body === "" || chat === "") {
    return null;
  }
  return { title, body, chat };
}

// notificationclick — фокус уже открытого окна с переходом на нужный чат
// либо открытие нового (ADR-023).
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  event.waitUntil(open(route(event.notification.data?.chat)));
});

// route переводит идентификатор чата в маршрут клиента (ADR-046).
// Идентификатор не той формы открывает список.
function route(chat) {
  if (typeof chat === "string") {
    const dm = DM.exec(chat);
    if (dm) {
      return `/#/dm/${dm[1]}`;
    }
    const room = ROOM.exec(chat);
    if (room) {
      return `/#/room/${room[1]}`;
    }
  }
  return "/#/";
}

async function open(path) {
  const target = new URL(path, self.location.origin);
  const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  for (const client of windows) {
    if (new URL(client.url).origin !== target.origin) {
      continue;
    }
    try {
      await client.focus();
    } catch {
      // Поднять окно не дали. Чат всё равно откроем: человек вернётся
      // в приложение сам и увидит его открытым там, где нужно.
    }
    try {
      // Маршрут живёт в hash: переход к нему — не перезагрузка,
      // а событие hashchange, которое разбирает роутер клиента.
      if (new URL(client.url).hash !== target.hash && typeof client.navigate === "function") {
        await client.navigate(target.href);
      }
    } catch {
      // Окно не наше или им уже распоряжаются: останется, где было.
    }
    return;
  }
  await self.clients.openWindow(target.href);
}
