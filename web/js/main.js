// Загрузка, роутинг по hash и состояние аккаунта.
//
// Экраны из js/ui/ занимаются только разметкой: всё, что меняет состояние —
// вход, регистрация, смена пароля, выход, удаление аккаунта — живёт здесь
// и отдаётся экранам через ctx.

import * as api from "./api.js";
import * as db from "./db.js";
import {
  deriveAccountKeys,
  exportPrivateJwk,
  exportPublicJwk,
  fingerprintOf,
  generateIdentity,
  importPrivate,
  importSecret,
  openBlob,
  parseBlob,
  publicJwk,
  sealBlob,
  validIterations,
  wipe,
} from "./crypto.js";
import { clear } from "./ui/dom.js";
import { renderAuth } from "./ui/auth.js";
import { renderSettings } from "./ui/settings.js";
import { frame } from "./ui/shell.js";

// Минимальная длина пароля — ADR-013.
const MIN_PASSWORD = 12;

// Ник — ADR-019. Клиент проверяет ту же форму, что и сервер.
const NICK = /^[a-z0-9_]{2,32}$/;

const state = { config: null, me: null };

// AccountError — то, что случилось с ключевым материалом, а не с сетью.
// Сообщение уже пригодно для показа человеку (ADR-028).
class AccountError extends Error {
  constructor(text) {
    super(text);
    this.name = "AccountError";
  }
}

const ctx = {
  minPassword: MIN_PASSWORD,
  validNick: (nick) => NICK.test(nick),
  storedNick,
  get config() {
    return state.config;
  },
  get me() {
    return state.me;
  },
  errorText,
  ensureConfig,
  signUp,
  signIn,
  changePassword,
  deleteAccount,
  signOut,
  go,
};

// --- роутинг -----------------------------------------------------------

// render рисует экран под текущий hash. Маршруты — docs/ui.md, «Каркас»;
// на этом этапе есть только список и настройки, остальные ведут в пустой
// список: чатов, контактов и комнат ещё нет.
function render() {
  const app = document.getElementById("app");
  clear(app);
  if (!state.me) {
    renderAuth(app, ctx);
    return;
  }
  const settings = (location.hash || "#/") === "#/settings";
  const { root, main } = frame(ctx, settings ? "screen" : "list");
  if (settings) {
    renderSettings(main, ctx);
  }
  app.append(root);
}

function go(hash) {
  if (location.hash === hash) {
    render();
  } else {
    location.hash = hash;
  }
}

// --- состояние ---------------------------------------------------------

async function ensureConfig() {
  if (!state.config) {
    state.config = await api.config();
  }
  return state.config;
}

// restore отвечает на вопрос «вошли ли мы»: сессия у сервера и ключи
// на устройстве нужны вместе. Ключей нет — нужен вход, он их и вернёт.
async function restore() {
  let who;
  try {
    who = await api.me();
  } catch {
    return null;
  }
  let meta;
  try {
    meta = await db.meta(["nick", "publicKey", "fingerprint", "privateKey"]);
  } catch {
    return null;
  }
  if (!meta.privateKey || meta.nick !== who.nick) {
    return null;
  }
  return { nick: meta.nick, publicKey: meta.publicKey, fingerprint: meta.fingerprint };
}

// storedNick — чьи ключи лежат на устройстве. Вход под другим ником стирает
// базу, поэтому экран входа сначала спрашивает (ADR-029).
async function storedNick() {
  try {
    const meta = await db.meta(["nick"]);
    return meta.nick ?? null;
  } catch {
    return null;
  }
}

// --- аккаунт -----------------------------------------------------------

// derive — вывод ключей по числу итераций, пришедшему от сервера. Границы
// проверяются до PBKDF2: authKey уходит на сервер сразу после вычисления,
// а слишком большой iter не заканчивается вовсе (ADR-030).
async function derive(nick, password, iterations) {
  if (!validIterations(iterations)) {
    throw new AccountError("параметры ключа не совпали");
  }
  return deriveAccountKeys(nick, password, iterations);
}

async function signUp(nick, password, invite) {
  const config = await ensureConfig();
  const iterations = config.kdfIterations;
  const { authKey, kek } = await derive(nick, password, iterations);
  const { keyPair, secret } = await generateIdentity();
  try {
    const priv = await exportPrivateJwk(keyPair.privateKey);
    const body = {
      nick,
      authKey,
      publicKey: await exportPublicJwk(keyPair.publicKey),
      blob: await sealBlob({ nick, iterations, kek, priv, secret }),
    };
    if (invite) {
      body.invite = invite;
    }
    await api.register(body);
    await adopt(nick, priv, secret);
  } finally {
    wipe(secret);
  }
}

async function signIn(nick, password) {
  const config = await ensureConfig();
  const { authKey, iterations, priv, secret } = await unlock(nick, password);
  try {
    await adopt(nick, priv, secret);
    if (iterations < config.kdfIterations) {
      await raise(nick, password, authKey, priv, secret, config.kdfIterations);
    }
  } finally {
    wipe(secret);
  }
}

// unlock скачивает ключевой блоб и расшифровывает его. Блоб отдаёт только
// вход — другого источника в протоколе нет, поэтому смена пароля тоже
// проходит через login (docs/crypto.md, «Повышение итераций и смена пароля»).
async function unlock(nick, password) {
  const { iterations } = await api.kdf(nick);
  const { authKey, kek } = await derive(nick, password, iterations);
  const account = await api.login(nick, authKey);

  let blob;
  try {
    blob = parseBlob(account.blob);
  } catch {
    throw new AccountError("ключ аккаунта повреждён");
  }
  // Расшифровка идёт по iter из блоба; расхождение с ответом /api/kdf
  // означает несогласованность данных (docs/crypto.md, «Ключевой блоб»).
  if (blob.iter !== iterations) {
    throw new AccountError("параметры ключа не совпали");
  }
  let opened;
  try {
    opened = await openBlob(blob, kek, nick);
  } catch {
    throw new AccountError("ключ аккаунта повреждён");
  }
  // Публичный ключ, который сервер раздаёт собеседникам, обязан
  // соответствовать приватному из блоба.
  if (opened.priv.x !== account.publicKey?.x || opened.priv.y !== account.publicKey?.y) {
    wipe(opened.secret);
    throw new AccountError("ключ аккаунта повреждён");
  }
  return { authKey, iterations, priv: opened.priv, secret: opened.secret };
}

// adopt кладёт ключи на устройство. Сырые байты дальше не идут:
// в IndexedDB попадают только non-extractable CryptoKey (docs/crypto.md).
async function adopt(nick, priv, secret) {
  const previous = await db.meta(["nick"]);
  if (previous.nick && previous.nick !== nick) {
    // На устройстве история другого аккаунта: один аккаунт на браузерный
    // профиль (docs/storage.md). Согласие на стирание спрашивает экран
    // входа до вычисления ключа (ADR-029) — здесь оно уже получено.
    await db.destroy();
  }
  const publicKey = publicJwk(priv);
  const fingerprint = await fingerprintOf(publicKey);
  await db.putMeta({
    nick,
    publicKey,
    fingerprint,
    privateKey: await importPrivate(priv),
    accountSecret: await importSecret(secret),
  });
  state.me = { nick, publicKey, fingerprint };
}

// raise — автоматическое повышение итераций сразу после входа, молча
// и без завершения чужих сессий (docs/crypto.md).
async function raise(nick, password, authKey, priv, secret, iterations) {
  try {
    const next = await derive(nick, password, iterations);
    await api.password({
      authKey,
      newAuthKey: next.authKey,
      blob: await sealBlob({ nick, iterations, kek: next.kek, priv, secret }),
      logoutOthers: false,
    });
  } catch {
    // Не вышло — вход уже состоялся, повторим при следующем.
  }
}

async function changePassword(current, next, logoutOthers) {
  const config = await ensureConfig();
  const nick = state.me.nick;
  // unlock входит заново — иначе не добыть блоб, — а вход заводит новую
  // сессию и перезаписывает cookie. Прежнюю закрываем сами и до входа:
  // иначе её строка осталась бы жить на сервере, а токена от неё нет уже
  // ни у кого. Сорвавшийся unlock после этого оставляет клиент без сессии,
  // но не на экране входа: следующая попытка начинается с того же
  // служебного выхода и проходит целиком (ADR-031).
  await api.dropSession();
  const { authKey, priv, secret } = await unlock(nick, current);
  try {
    const iterations = config.kdfIterations;
    const derived = await derive(nick, next, iterations);
    await api.password({
      authKey,
      newAuthKey: derived.authKey,
      blob: await sealBlob({ nick, iterations, kek: derived.kek, priv, secret }),
      logoutOthers,
    });
  } finally {
    wipe(secret);
  }
}

// deleteAccount входит заново тем же порядком, что и смена пароля: сессии
// может уже не быть — её закрывает сорвавшаяся смена пароля, — а DELETE
// /api/me без сессии не проходит. Блоб здесь не нужен, поэтому вход прямой,
// без unlock: аккаунт с испорченным блобом обязан удаляться (ADR-031).
async function deleteAccount(password) {
  const nick = state.me.nick;
  const { iterations } = await api.kdf(nick);
  const { authKey } = await derive(nick, password, iterations);
  await api.dropSession();
  await api.login(nick, authKey);
  await api.deleteMe(authKey);
  await forget();
}

async function signOut() {
  try {
    await api.dropSession();
  } catch {
    // Сети нет или сервер не справился; база стирается в любом случае.
  }
  await forget();
}

// forget уносит историю: она на этом устройстве единственная копия
// (docs/storage.md, docs/ui.md).
async function forget() {
  await db.destroy();
  state.me = null;
}

function errorText(err) {
  if (err instanceof AccountError) {
    return err.message;
  }
  return api.errorText(err);
}

// --- старт -------------------------------------------------------------

async function boot() {
  db.persist();
  try {
    await ensureConfig();
  } catch {
    // Ошибку покажем на первой попытке входа: экран рисуется и без конфигурации.
  }
  state.me = await restore();
  render();
  addEventListener("hashchange", render);
  // 401 unauthenticated на любом запросе — на экран входа, IndexedDB цела.
  api.onSessionExpired(() => {
    if (state.me) {
      state.me = null;
      render();
    }
  });
}

boot();
