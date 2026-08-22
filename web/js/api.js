// Обёртки над fetch. Форма запросов и ответов — docs/protocol.md:
// JSON в обе стороны, cookie сессии, ошибка — {error, message}.
// SSE и ACK появятся на этапе 2.

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
async function request(method, path, body, { quiet = false } = {}) {
  const init = { method, credentials: "same-origin", cache: "no-store" };
  if (body !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
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
