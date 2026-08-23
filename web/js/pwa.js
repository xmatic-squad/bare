// PWA: service worker, подписка на пуши и установка приложения
// (ADR-011, ADR-023, ADR-046).
//
// Экраны спрашивают отсюда состояние и сюда же отдают действия; в
// pushManager, IndexedDB и сеть они не ходят — как и с чатом, это делает
// один модуль.
//
// Пуш — сигнал: он говорит, что для устройства что-то есть, а содержимое
// приезжает очередью при подключении (ADR-011). Поэтому здесь нет ни
// сообщений, ни ключей — только подписка и разрешение.

import * as api from "./api.js";
import { NetworkError } from "./api.js";
import * as db from "./db.js";
import { b64url, unb64url } from "./crypto.js";
import { deviceId } from "./sync.js";

const WORKER = "/sw.js";

// iOS: пуши работают только у приложения, установленного на экран «Домой»
// (ADR-011). Признак — docs/ui.md, «Баннер установки»: iPhone|iPad
// и navigator.standalone !== true.
const IOS = /iPhone|iPad/;

const state = {
  // Регистрация service worker: одна на страницу, ждут её все.
  registering: null,
  // beforeinstallprompt приходит один раз и ждёт кнопки в настройках
  // (docs/ui.md, «Настройки»).
  prompt: null,
  // Разрешение спрашивается один раз за всю историю устройства
  // (docs/ui.md, «Уведомления»); флаг в памяти закрывает вкладку от
  // повторного вопроса, флаг в meta — устройство.
  asked: false,
  // Вопрос идёт прямо сейчас: два сообщения подряд не должны дать
  // два запроса разрешения.
  asking: false,
  // Браузер отказал в самой подписке: приватное окно, политика,
  // недоступный push-сервис. Кнопкой это не включить, поэтому раздел
  // показывает «запрещены в браузере» (ADR-046). Флаг живёт во вкладке:
  // перезагрузка пробует снова — причина могла уйти.
  refused: false,
};

// Приглашение установки ловится с первой секунды: браузер показывает его
// сам и только раз. Предотвращённое событие оживает кнопкой в настройках.
addEventListener("beforeinstallprompt", (event) => {
  event.preventDefault();
  state.prompt = event;
});

// --- service worker -----------------------------------------------------

// register ставит service worker. Отдаёт регистрацию или null: браузер
// без service worker — это просто клиент без кэша оболочки и пушей,
// а не сломанный клиент.
export function register() {
  if (!("serviceWorker" in navigator)) {
    return Promise.resolve(null);
  }
  if (state.registering === null) {
    state.registering = navigator.serviceWorker.register(WORKER).catch(() => null);
  }
  return state.registering;
}

// ready — регистрация с работающим service worker: подписка ставится
// только на неё. navigator.serviceWorker.ready ждёт вечно, если
// регистрации нет, — поэтому сначала register.
async function ready() {
  const registration = await register();
  if (registration === null) {
    return null;
  }
  try {
    return await navigator.serviceWorker.ready;
  } catch {
    return null;
  }
}

// --- уведомления --------------------------------------------------------

// supported — есть ли в браузере то, из чего складывается пуш. iOS вне
// установленного приложения сюда не проходит: там нет ни Notification,
// ни PushManager.
function supported() {
  return "serviceWorker" in navigator
    && "PushManager" in self
    && "Notification" in self;
}

// notifications — состояние раздела «уведомления» (docs/ui.md):
// "on" — «включены», "off" — «выключены», "denied" — «запрещены
// в браузере». В "denied" сходится всё, чего кнопкой не включить:
// отклонённое разрешение, браузер без уведомлений, сервер без
// VAPID-ключа (ADR-046).
export async function notifications(key) {
  if (!supported() || !key || Notification.permission === "denied" || state.refused) {
    return "denied";
  }
  if (await turnedOff()) {
    return "off";
  }
  const subscription = await current();
  if (subscription === null) {
    return "off";
  }
  // Подписка под прежней парой VAPID-ключей не работает и не починится
  // сама: push-сервис отвечает на неё 403, а это не 404 и не 410, и
  // сервер её не снимет. Для человека это «выключены», а кнопка
  // «включить» подпишет заново под текущим ключом (ADR-049).
  return sameKey(subscription, key) ? "on" : "off";
}

// turnedOff — уведомления выключены кнопкой в настройках. Явный отказ
// сильнее любой оставшейся подписки: сама она больше не включается
// (ADR-049).
async function turnedOff() {
  try {
    return (await db.meta(["notificationsOff"])).notificationsOff === true;
  } catch {
    return false;
  }
}

// enable — «включить». Разрешение спрашивается по нажатию, подписка
// ставится после granted (docs/ui.md, «Уведомления»). Отдаёт новое
// состояние.
export async function enable(key) {
  if (!supported() || !key) {
    return "denied";
  }
  let permission;
  try {
    permission = await Notification.requestPermission();
  } catch {
    return "denied";
  }
  if (permission !== "granted") {
    return permission === "denied" ? "denied" : "off";
  }
  // Человек решил всё сам: отказа больше нет, и спрашивать после первого
  // сообщения не о чем (ADR-049).
  state.asked = true;
  await db.putMeta({ notificationsAsked: true, notificationsOff: false }).catch(() => {});
  return (await attach(key)) ? "on" : "denied";
}

// disable — «выключить». Подписка снимается у push-сервиса и у сервера:
// первое действует сразу, второе убирает мёртвую строку из базы.
//
// Отказ запоминается раньше всего остального: без него первое же
// отправленное сообщение вернуло бы подписку через askOnce, а запуск
// приложения — через refresh (ADR-049).
export async function disable() {
  await db.putMeta({ notificationsOff: true }).catch(() => {});
  const subscription = await current();
  if (subscription !== null) {
    try {
      await subscription.unsubscribe();
    } catch {
      // Отписаться не дали: сервер уберёт подписку по 404/410 (ADR-011).
    }
  }
  const device = deviceId();
  if (device) {
    await api.clearPush(device);
  }
}

// askOnce — запрос разрешения после первого успешно отправленного
// сообщения за всю историю устройства, один раз (ADR-011, docs/ui.md,
// «Уведомления»). Отказ — молча: включить можно в настройках.
//
// На iOS вне установленного приложения вопроса нет вовсе: там вместо
// него баннер установки (ADR-023), а флаг не ставится — установят,
// спросим после следующего сообщения.
export async function askOnce(key) {
  if (state.asked || state.asking) {
    return;
  }
  state.asking = true;
  try {
    let meta;
    try {
      meta = await db.meta(["notificationsAsked", "notificationsOff"]);
    } catch {
      return;
    }
    // Выключенные в настройках уведомления сами не включаются: вопрос
    // закрыт человеком, а не нами (ADR-049).
    if (meta.notificationsOff === true) {
      return;
    }
    if (meta.notificationsAsked === true) {
      state.asked = true;
      return;
    }
    if (iosBrowser() || !supported() || !key) {
      return;
    }
    // Спрашивать нечего: разрешение уже дано или уже отклонено. Данное —
    // повод поставить подписку, если её нет.
    if (Notification.permission !== "default") {
      await remember();
      if (Notification.permission === "granted") {
        await attach(key).catch(() => {});
      }
      return;
    }
    let permission;
    try {
      permission = await Notification.requestPermission();
    } catch {
      // Браузер требует нажатия, а между отправкой и ответом сервера оно
      // истекло. Вопроса не было — значит, «один раз» ещё не потрачено:
      // попробуем после следующего сообщения.
      return;
    }
    await remember();
    if (permission === "granted") {
      await attach(key).catch(() => {});
    }
  } finally {
    state.asking = false;
  }
}

// remember — вопрос задан, второй раз не спрашиваем ни в этой вкладке,
// ни на этом устройстве (docs/ui.md, «Уведомления»).
async function remember() {
  state.asked = true;
  await db.putMeta({ notificationsAsked: true }).catch(() => {});
}

// refresh переставляет подписку на текущее устройство. Подписка живёт
// в браузерном профиле, а принадлежит устройству (ADR-023): deviceId
// меняется при конфликте идентификаторов и после чистки IndexedDB,
// и запуск приложения это чинит (ADR-046).
//
// Выключенные уведомления запуск не включает, а подписку под прежним
// ключом сервера не переставляет: она всё равно не работает, и место ей
// не в базе, а в кнопке «включить» (ADR-049).
export async function refresh(key) {
  if (await turnedOff()) {
    return;
  }
  const subscription = await current();
  if (subscription === null) {
    return;
  }
  if (key && !sameKey(subscription, key)) {
    return;
  }
  try {
    await put(subscription);
  } catch {
    // Не переставили — переставим при следующем запуске.
  }
}

// detach снимает подписку у push-сервиса при выходе из аккаунта
// и при его удалении: подписка принадлежит устройству, а устройство —
// аккаунту (ADR-046). Сервер не спрашиваем: сессии к этому моменту
// уже нет, а мёртвую подписку он уберёт сам по 404/410.
export async function detach() {
  const subscription = await current();
  if (subscription === null) {
    return;
  }
  try {
    await subscription.unsubscribe();
  } catch {
    // Не отписались — пуши всё равно некуда доставлять: истории на
    // устройстве больше нет.
  }
}

// current — подписка этого браузера или null.
async function current() {
  const registration = await ready();
  if (registration === null || !registration.pushManager) {
    return null;
  }
  try {
    return await registration.pushManager.getSubscription();
  } catch {
    return null;
  }
}

// attach ставит подписку и отдаёт её серверу. Ключ сервера вплетён
// в подписку: сменился ключ — прежняя подписка не годится, push-сервис
// подпишет заново.
//
// Отказ самой подписки — не сбой сервера, а браузер, который её не даёт:
// приватное окно, политика, недоступный push-сервис. Кнопкой это
// не включить, поэтому false, а раздел настроек скажет «запрещены
// в браузере» и уберёт кнопку (ADR-046).
async function attach(key) {
  const registration = await ready();
  if (registration === null || !registration.pushManager) {
    return false;
  }
  let subscription = await registration.pushManager.getSubscription();
  if (subscription !== null && !sameKey(subscription, key)) {
    try {
      await subscription.unsubscribe();
    } catch {
      // Старая подписка останется у push-сервиса; сервер её не знает.
    }
    subscription = null;
  }
  if (subscription === null) {
    try {
      subscription = await registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: unb64url(key),
      });
    } catch {
      state.refused = true;
      return false;
    }
  }
  state.refused = false;
  await put(subscription);
  return true;
}

// put отдаёт подписку серверу. Без устройства запрос невозможен:
// подписка принадлежит устройству, а его заводит подключение
// (ADR-017). Это то же состояние, что и не дошедший запрос.
async function put(subscription) {
  const device = deviceId();
  if (!device) {
    throw new NetworkError();
  }
  await api.setPush(device, subscription.toJSON());
}

// sameKey — та ли пара VAPID-ключей, под которую выдана подписка.
function sameKey(subscription, key) {
  const applied = subscription.options?.applicationServerKey;
  if (!applied) {
    return false;
  }
  try {
    return b64url(new Uint8Array(applied)) === key;
  } catch {
    return false;
  }
}

// --- установка ----------------------------------------------------------

// iosBrowser — iPhone или iPad вне установленного приложения. Ровно этот
// признак показывает баннер установки (docs/ui.md).
export function iosBrowser() {
  return IOS.test(navigator.userAgent) && navigator.standalone !== true;
}

// installable — поймано ли приглашение установки.
export function installable() {
  return state.prompt !== null;
}

// install показывает приглашение установки. Оно одноразовое: показали —
// кнопки больше нет.
export async function install() {
  const prompt = state.prompt;
  if (prompt === null) {
    return;
  }
  state.prompt = null;
  try {
    await prompt.prompt();
    await prompt.userChoice;
  } catch {
    // Приглашение протухло: браузер покажет своё, когда сочтёт нужным.
  }
}

// bannerHidden — баннер установки уже закрывали (docs/ui.md).
export async function bannerHidden() {
  try {
    return (await db.meta(["installBannerDismissed"])).installBannerDismissed === true;
  } catch {
    return true;
  }
}

// hideBanner — крестик: повтор не показывается.
export async function hideBanner() {
  await db.putMeta({ installBannerDismissed: true }).catch(() => {});
}
