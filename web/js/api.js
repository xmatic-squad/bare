// Обёртки над fetch и поток событий. Форма запросов и ответов —
// docs/protocol.md: JSON в обе стороны, cookie сессии, ошибка —
// {error, message}.

// MAX_ACK — сколько идентификаторов принимает один POST /api/ack
// (docs/protocol.md, «Сообщения»).
export const MAX_ACK = 500;

// ApiError — ответ сервера с кодом из перечня docs/protocol.md.
export class ApiError extends Error {
  constructor(code, message, status, field) {
    super(message || code);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
    this.field = field;
  }
}

// NetworkError — запрос не дошёл: сети нет, сервер не ответил.
// Это состояние клиента, а не код протокола.
export class NetworkError extends Error {
  constructor() {
    super("нет соединения");
    this.name = "NetworkError";
  }
}

// Тексты состояний и ошибок — docs/ui.md и ADR-028.
const TEXT = {
  invalid_credentials: "неверный ник или пароль",
  nick_taken: "ник занят",
  invalid_nick: "ник: 2–32 символа, a–z, 0–9, _",
  invite_required: "нужен инвайт-код",
  invalid_invite: "инвайт-код не подходит",
  rate_limited: "слишком часто, попробуйте позже",
  unknown_user: "такого ника нет",
  self: "нельзя писать себе",
  clock_skew: "проверьте часы на устройстве: расхождение больше 5 минут",
};

export function errorText(err) {
  if (err instanceof NetworkError) {
    return "нет соединения";
  }
  if (err instanceof ApiError && TEXT[err.code]) {
    return TEXT[err.code];
  }
  return "сервер не справился, попробуйте позже";
}

// expired вызывается, когда сервер сказал «нужен вход»: сессия истекла
// или её завершили с другого устройства. IndexedDB при этом не трогается
// (docs/ui.md, «Сеть и состояния»).
let expired = () => {};

export function onSessionExpired(handler) {
  expired = handler;
}

// quiet: не звать expired() на 401 unauthenticated. Нужно ровно там, где
// «сессии нет» — не конец сеанса, а ожидаемый ответ (dropSession).
// device: заголовок X-Device — он обязателен там, где важно, с какого
// устройства пришёл запрос (docs/protocol.md, «Общие правила»).
async function request(method, path, body, { quiet = false, device = null } = {}) {
  const init = { method, credentials: "same-origin", cache: "no-store" };
  const headers = {};
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  if (device) {
    headers["X-Device"] = device;
  }
  if (Object.keys(headers).length > 0) {
    init.headers = headers;
  }
  let response;
  try {
    response = await fetch(path, init);
  } catch {
    throw new NetworkError();
  }

  let data = null;
  if ((response.headers.get("Content-Type") ?? "").startsWith("application/json")) {
    data = await response.json().catch(() => null);
  }
  if (response.ok) {
    return data;
  }

  const code = typeof data?.error === "string" ? data.error : "internal";
  // Отличаем истёкшую сессию от неверного пароля: 401 invalid_credentials —
  // обычная ошибка формы входа, 401 unauthenticated — выход на экран входа.
  if (code === "unauthenticated" && !quiet) {
    expired();
  }
  throw new ApiError(code, data?.message, response.status, data?.field);
}

export function config() {
  return request("GET", "/api/config");
}

export function kdf(nick) {
  return request("GET", `/api/kdf?nick=${encodeURIComponent(nick)}`);
}

export function register(body) {
  return request("POST", "/api/register", body);
}

export function login(nick, authKey) {
  return request("POST", "/api/login", { nick, authKey });
}

export function me() {
  return request("GET", "/api/me");
}

// dropSession — служебный выход перед повторным входом (ADR-031). Смена
// пароля и удаление аккаунта входят заново, а вход перезаписывает cookie:
// прежнюю сессию закрываем сами, пока её токен ещё при нас.
//
// 401 unauthenticated здесь означает «сессии и так нет» — это успех, а не
// конец сеанса: следующим шагом идёт login, он заведёт новую. Остальные
// отказы поднимаются наверх: при живой сессии входить заново нельзя,
// её строка осталась бы на сервере без владельца.
export async function dropSession() {
  try {
    await request("POST", "/api/logout", undefined, { quiet: true });
  } catch (err) {
    if (!(err instanceof ApiError) || err.code !== "unauthenticated") {
      throw err;
    }
  }
}

export function password(body) {
  return request("POST", "/api/password", body);
}

export function deleteMe(authKey) {
  return request("DELETE", "/api/me", { authKey });
}

export function user(nick) {
  return request("GET", `/api/users/${encodeURIComponent(nick)}`);
}

// --- устройства --------------------------------------------------------

// registerDevice — 201 при создании, 200 если устройство уже наше,
// 409 device_conflict, если идентификатор занят другим (ADR-017).
export function registerDevice(id) {
  return request("POST", "/api/devices", { id });
}

export function devices() {
  return request("GET", "/api/devices");
}

export function removeDevice(id) {
  return request("DELETE", `/api/devices/${encodeURIComponent(id)}`);
}

// setPush и clearPush — push-подписка устройства (ADR-023). Подписка
// принадлежит устройству, поэтому устройство идёт и в пути, и в заголовке:
// чужому подписку не поставить (docs/protocol.md, «Устройства»).
export function setPush(device, subscription) {
  return request("PUT", `/api/devices/${encodeURIComponent(device)}/push`, { subscription }, { device });
}

export function clearPush(device) {
  return request("DELETE", `/api/devices/${encodeURIComponent(device)}/push`, undefined, { device });
}

// --- контакты ----------------------------------------------------------

export function contacts() {
  return request("GET", "/api/contacts");
}

// addContact заводит строку списка чатов и отдаёт публичный ключ
// собеседника: 404 unknown_user, 400 self (ADR-019).
export function addContact(nick) {
  return request("POST", "/api/contacts", { nick });
}

export function removeContact(nick) {
  return request("DELETE", `/api/contacts/${encodeURIComponent(nick)}`);
}

// --- комнаты -----------------------------------------------------------

// rooms — комнаты, где мы участники, каждая с нашим текущим завёрнутым
// ключом (docs/protocol.md, «Комнаты»).
export function rooms() {
  return request("GET", "/api/rooms");
}

// createRoom заводит комнату. Идентификатор генерирует клиент: ключ
// заворачивается до запроса и привязан к roomId (ADR-037). Занятый
// идентификатор — 409 room_conflict, берётся новый.
//
// X-Device передаётся, чтобы это же устройство не получило комнату ещё
// и событием: она приходит ответом (docs/protocol.md, «Комнаты»).
export function createRoom(device, body) {
  return request("POST", "/api/rooms", body, { device });
}

// changeMembers — смена состава и rekey одним запросом (ADR-018).
export function changeMembers(id, body) {
  return request("POST", `/api/rooms/${encodeURIComponent(id)}/members`, body);
}

// leaveRoom — выход из комнаты. X-Device передаётся по той же причине,
// что и при создании: комната уходит из списка здесь же, а другим
// устройствам вышедшего сервер шлёт room_left (ADR-041).
export function leaveRoom(device, id) {
  return request("POST", `/api/rooms/${encodeURIComponent(id)}/leave`, undefined, { device });
}

export function removeRoom(id) {
  return request("DELETE", `/api/rooms/${encodeURIComponent(id)}`);
}

// --- сообщения ---------------------------------------------------------

// sendMessage отдаёт конверт серверу; from и ts он поставит сам (ADR-017).
// Ответ — 202 {id, ts}.
export function sendMessage(device, envelope) {
  return request("POST", "/api/messages", envelope, { device });
}

// ack подтверждает запись сообщений в IndexedDB: сервер убирает их
// из очереди устройства (docs/storage.md). Не больше MAX_ACK за раз.
export function ack(device, ids) {
  return request("POST", "/api/ack", { ids }, { device });
}

// --- события -----------------------------------------------------------

// stream открывает поток событий устройства (docs/protocol.md, «События»).
// Устройство передаётся в query: EventSource не умеет заголовки.
//
// Переподключение делает браузер сам. Ответ не 200 он считает
// окончательным отказом и больше не подключается — это видно
// по readyState CLOSED и передаётся в handlers.error вторым состоянием.
//
// Отдаёт функцию закрытия потока.
export function stream(device, handlers) {
  const source = new EventSource(`/api/events?device=${encodeURIComponent(device)}`);
  source.addEventListener("msg", (event) => handlers.msg(parse(event.data)));
  // room и room_left в очередь не кладутся: пропуск во время офлайна
  // чинится перечитыванием GET /api/rooms после ready (docs/protocol.md).
  source.addEventListener("room", (event) => handlers.room(parse(event.data)));
  source.addEventListener("room_left", (event) => handlers.roomLeft(parse(event.data)));
  source.addEventListener("ready", () => handlers.ready());
  source.addEventListener("error", () => handlers.error(source.readyState === EventSource.CLOSED));
  return () => source.close();
}

function parse(data) {
  try {
    return JSON.parse(data);
  } catch {
    return null;
  }
}
