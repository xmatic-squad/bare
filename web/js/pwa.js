// PWA: service worker, самообновление оболочки, подписка на пуши
// и установка приложения (ADR-011, ADR-023, ADR-046, ADR-068).
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

// Слово, по которому service worker включает установленную версию
// (ADR-068). Решение принимает страница, а не воркер.
const SKIP = "skip-waiting";

// Слово, которым страница спрашивает воркера, какую версию оболочки он
// держит. Этим версии и различаются: новый выпуск меняет её, а тот же файл
// воркера, отданный сервером заново, — нет (ADR-070).
const ASK = "version";

// Сколько ждать ответа о версии. Отвечают сразу — вопрос стоит одного
// сообщения; молчит воркер прежнего выпуска, который такого вопроса
// не знает, и тогда версия остаётся неизвестной.
const ASK_WAIT = 3 * 1000;

// Как часто спрашивать сервер об обновлении: при запуске и при каждом
// возвращении в приложение, но не чаще раза в минуту. Проверка — один
// условный запрос за /sw.js, и дёргать сеть на каждое переключение
// приложений незачем (ADR-068).
const CHECK_EVERY = 60 * 1000;

// Сколько ждать между двумя перезагрузками ради обновления в одной
// вкладке. Пауза постоянная и не растёт: петлю закрывает сверка версии
// оболочки (ADR-070), а это предел частоты — на случай сервера, который
// отдаёт разные версии на каждый запрос. Отметка живёт в sessionStorage:
// она переживает перезагрузку и умирает вместе с вкладкой.
const RELOAD_KEY = "bare-updated-at";
const RELOAD_APART = 30 * 1000;

// Как часто возвращаться к отложенному, пока в полях набранное. Событие
// input ловит набор, но опустеть поле умеет и молча: отправленное
// сообщение чистит строку присваиванием, а уход с экрана уносит её
// из документа вместе с текстом — событий не случается ни там, ни там.
// Обход полей раз в полминуты ничего не стоит и заводится только тогда,
// когда обновление уже ждёт (ADR-072).
const DRAFT_AGAIN = 30 * 1000;

// Поля, куда набирают текст. Проверяется то, что считается набранным,
// а не список исключений: у скрытого поля, переключателя и кнопки
// значение непусто по определению, и одно такое поле молча запретило бы
// обновление навсегда (ADR-068).
const TYPED = new Set(["text", "password", "search", "email", "url", "tel", "number"]);

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
  // Обновление (ADR-068). controlled — управляет ли страницей service
  // worker. У такой страницы смену контроллёра разбирает fresh: другая
  // версия оболочки стоит перезагрузки, та же — нет (ADR-070). У первого
  // захода контроллёр появляется и без обновления: его приход сменой
  // не считается, но запоминается — дальше страница управляемая.
  controlled: "serviceWorker" in navigator && navigator.serviceWorker.controller !== null,
  // Новую версию попросили включиться мы сами. Тогда смена контроллёра —
  // ответ на нашу просьбу, и перезагружаться надо, даже если при загрузке
  // воркера у страницы ещё не было: в первый заход он ставится тут же,
  // а обновление может приехать в тот же сеанс. Имя своё: у вопроса про
  // уведомления рядом живёт свой флаг, и один на двоих означал бы, что
  // обновление закрывает вопрос про уведомления, а вопрос — обновление.
  requested: false,
  // Оболочка сменилась, а перезагрузка отложена набранным текстом.
  // Просить включения могла и соседняя вкладка с пустыми полями
  // (ADR-035): контроллёр меняется у всех сразу, а платить за это
  // черновиком этой вкладки не должен никто. Перезагрузимся, когда
  // терять станет нечего.
  changed: false,
  // Перезагрузка началась: controllerchange приходит один раз, но цена
  // ошибки — бесконечный цикл.
  reloading: false,
  // Новая версия установлена и ждёт включения. Ждать её заставляет
  // набранный текст: перезагрузка унесла бы его.
  waiting: null,
  // Когда последний раз спрашивали сервер об обновлении.
  checked: 0,
  // Версия оболочки, код которой исполняет эта страница, — обещание
  // ответа воркера. С ней сравнивается версия включившейся: совпали —
  // перезагружаться незачем, оболочка та же (ADR-070).
  shell: Promise.resolve(null),
  // Отложенная до конца паузы попытка: другого повода вернуться к ней
  // может и не быть.
  timer: 0,
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

// --- обновление ---------------------------------------------------------

// watch включает самообновление: проверку при запуске и при возвращении
// в приложение, включение установленной версии и перезагрузку страницы
// (ADR-068). Установленное на «Домой» приложение обновить иначе нечем:
// ни строки адреса, ни кнопки перезагрузки в нём нет.
export function watch() {
  if (!("serviceWorker" in navigator)) {
    return;
  }
  // Версия оболочки, код которой исполняет эта страница. Спрашивается
  // сразу: воркер, который её держит, скоро сменится, а сравнивать надо
  // с тем, чей код уже загружен (ADR-070).
  state.shell = ask(navigator.serviceWorker.controller);
  // Контроллёр сменился — работает другая версия оболочки, а страница
  // исполняет прежнюю. Перезагрузка одна за жизнь страницы.
  //
  // Первая установка её не считается: там контроллёр появляется впервые
  // (clients.claim), а страница и так свежая. Эту смену замок съедает
  // и запоминает — вместе с версией, которую новый контроллёр держит:
  // она и есть версия загруженной оболочки.
  //
  // Перезагружаемся не отсюда: включить новую версию могла соседняя
  // вкладка, у которой поля пусты, а у этой в них набранное. Отмечаем
  // и ждём — apply перезагрузит, когда терять станет нечего.
  navigator.serviceWorker.addEventListener("controllerchange", () => {
    if (state.reloading) {
      return;
    }
    if (!state.requested && !state.controlled) {
      state.controlled = true;
      state.shell = ask(navigator.serviceWorker.controller);
      return;
    }
    fresh();
  });
  // Возвращение в приложение — единственный момент, когда о нём вспоминают
  // на телефоне: вкладка не закрывается неделями. Событие приходит
  // документу, ему и слушаем.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") {
      check();
    }
  });
  // Набранное стёрли — терять стало нечего, и отложенное обновление
  // можно включать.
  addEventListener("input", () => apply(), true);
  register().then((registration) => {
    if (registration === null) {
      return;
    }
    registration.addEventListener("updatefound", () => track(registration.installing));
    // Версия, установившаяся в прошлый заход и не дождавшаяся своей
    // очереди: вкладку закрыли раньше.
    track(registration.waiting);
    check();
  });
}

// check спрашивает сервер, нет ли новой версии. Сети нет — молча:
// офлайн не сбой, спросим при следующем возвращении (ADR-068).
async function check() {
  if (Date.now() - state.checked < CHECK_EVERY) {
    // Спрашивать рано, но отложенное обновление могло дождаться своего
    // часа: набранное стёрли или отправили.
    apply();
    return;
  }
  state.checked = Date.now();
  const registration = await register();
  if (registration === null) {
    return;
  }
  try {
    await registration.update();
  } catch {
    // Сервер молчит или сети нет.
  }
  // Ждущая версия берётся и отсюда, а не только из updatefound: событие
  // случается один раз, а включить её могло не выйти — страница была
  // занята набранным текстом или сообщение до воркера не дошло.
  track(registration.waiting);
  apply();
}

// track следит за устанавливающейся версией: как только она встала
// и ждёт, её можно включать.
function track(worker) {
  if (!worker || worker === state.waiting) {
    return;
  }
  const look = () => {
    if (worker.state === "installed") {
      state.waiting = worker;
      apply();
    }
  };
  worker.addEventListener("statechange", look);
  look();
}

// fresh разбирает смену контроллёра: включилась другая версия оболочки
// или тот же файл воркера, отданный сервером заново. Перезагрузка нужна
// только в первом случае: во втором она вернула бы ту же самую страницу,
// и следующий запрос за sw.js начал бы всё сначала — это и есть петля
// (ADR-070).
//
// Неизвестная версия считается другой: молчит воркер прежнего выпуска,
// и обновление до выпуска, который отвечает, важнее. Повторяться этому
// не с чего — после перезагрузки отвечают оба.
async function fresh() {
  const [was, now] = await Promise.all([state.shell, ask(navigator.serviceWorker.controller)]);
  if (state.reloading) {
    return;
  }
  if (was !== null && now !== null && was === now) {
    return;
  }
  state.changed = true;
  apply();
}

// apply двигает обновление вперёд — если сейчас есть чем платить.
// Набранное откладывает и просьбу включиться, и перезагрузку: и то,
// и другое кончается новой оболочкой, а она уносит поля (ADR-068).
//
// Первая установка до просьбы не доходит: страницей ещё никто
// не управляет, включать нечего, а воркер и так активируется сам.
// Через «installed» он при этом проходит — без проверки контроллёра
// первый же запуск перезагружал бы себя сам.
function apply() {
  if (state.reloading) {
    return;
  }
  if (typed()) {
    // Набранное откладывает обновление, а поводов вернуться к нему мало:
    // событие input случается при наборе, но не при отправке сообщения
    // и не при уходе с экрана — там поле пустеет и пропадает молча.
    // Поэтому ждём ещё и по таймеру (ADR-072).
    if (state.changed || state.waiting !== null) {
      later(DRAFT_AGAIN);
    }
    return;
  }
  if (state.changed) {
    // Оболочка уже сменилась — просить больше нечего, осталось
    // перезагрузиться. Слишком часто подряд не перезагружаемся:
    // пауза кончится, и apply вернётся сюда сам.
    const wait = pause();
    if (wait > 0) {
      later(wait);
      return;
    }
    state.reloading = true;
    markReload();
    location.reload();
    return;
  }
  const worker = state.waiting;
  if (worker === null || navigator.serviceWorker.controller === null) {
    return;
  }
  state.waiting = null;
  state.requested = true;
  worker.postMessage(SKIP);
}

// typed — есть ли на экране набранное. Проверка общая на весь документ,
// а не на строку ввода: пароль в форме входа теряется от перезагрузки
// так же, как черновик сообщения. Пустое поле под курсором ничего
// не стоит: оно и после перезагрузки пустое.
//
// Набранным считается только то, куда набирают: textarea и поля из
// TYPED. Чекбокс, файл, скрытое поле и кнопка непусты сами по себе,
// а обновление они бы запретили насовсем.
//
// Строка сообщения в чате — редактируемый блок, а не поле формы
// (ADR-069): value у него нет, набранное лежит в textContent. Считается
// и закрытый блок: ввод бывает заблокирован предупреждением о ключе,
// а недописанное в нём остаётся.
function typed() {
  for (const node of document.querySelectorAll("input, textarea")) {
    if (node.value === "") {
      continue;
    }
    if (node.tagName === "TEXTAREA" || TYPED.has(node.type)) {
      return true;
    }
  }
  for (const node of document.querySelectorAll("[contenteditable]")) {
    if (node.textContent !== "") {
      return true;
    }
  }
  return false;
}

// ask спрашивает у воркера версию оболочки, которую он держит. Ответ идёт
// своим каналом: вопросов бывает два подряд, а перепутать ответы нельзя.
//
// Молчание — не сбой: так отвечает воркер прежнего выпуска, который такого
// вопроса не знает. Тогда версия неизвестна (null), и решение принимается
// в пользу обновления (ADR-070).
function ask(worker) {
  return new Promise((done) => {
    if (!worker) {
      done(null);
      return;
    }
    const channel = new MessageChannel();
    const timer = setTimeout(() => {
      channel.port1.close();
      done(null);
    }, ASK_WAIT);
    channel.port1.onmessage = (event) => {
      clearTimeout(timer);
      channel.port1.close();
      done(typeof event.data === "string" && event.data !== "" ? event.data : null);
    };
    try {
      worker.postMessage(ASK, [channel.port2]);
    } catch {
      clearTimeout(timer);
      done(null);
    }
  });
}

// pause — сколько ещё ждать до перезагрузки ради обновления. Петлю она
// не закрывает — это делает сверка версии оболочки (ADR-070); здесь только
// предел частоты на случай сервера, который отдаёт разные версии на каждый
// запрос. Отметки нет — значит, и повода ждать нет.
function pause() {
  const at = Number(session(RELOAD_KEY));
  if (!Number.isFinite(at) || at <= 0) {
    return 0;
  }
  const left = RELOAD_APART - (Date.now() - at);
  return left > 0 ? Math.min(left, RELOAD_APART) : 0;
}

// later возвращается к отложенному сам: другого повода может и не быть —
// вкладка открыта, обновление ждёт, а ждать его заставляет то пауза между
// перезагрузками (ADR-070), то набранное в полях (ADR-072).
function later(ms) {
  if (state.timer !== 0) {
    return;
  }
  state.timer = setTimeout(() => {
    state.timer = 0;
    apply();
  }, ms);
}

function markReload() {
  try {
    sessionStorage.setItem(RELOAD_KEY, String(Date.now()));
  } catch {
    // Хранилище закрыто политикой браузера: обойдёмся без страховки.
  }
}

function session(key) {
  try {
    return sessionStorage.getItem(key);
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
