// Экран чата — docs/ui.md, «Чат»; вид — docs/identity/screens.html.
//
// Данные и действия идут только через sync.js: экран не пишет в базу
// и не ходит в сеть сам.

import * as sync from "../sync.js";
import { DESKTOP, clear, el, wide } from "./dom.js";

// Предел текста и порог счётчика — docs/ui.md, «Чат».
const LIMIT = 4000;
const COUNTER_AT = 3500;

// Разделители дат: на десктопе — полная дата, на мобильном — короткая,
// как в эталоне. Время — ЧЧ:ММ в локальной зоне.
const DAY_LONG = new Intl.DateTimeFormat("ru-RU", { weekday: "long", day: "numeric", month: "long" });
const DAY_SHORT = new Intl.DateTimeFormat("ru-RU", { day: "numeric", month: "short" });
const TIME = new Intl.DateTimeFormat("ru-RU", { hour: "2-digit", minute: "2-digit" });

// Тексты нерасшифрованного — docs/ui.md, «Чат». Ключа комнаты нет —
// это про комнату; всё остальное в личном чате означает чужой ключ.
const NO_ROOM_KEY = "не удалось расшифровать: нет ключа комнаты";
const KEY_CHANGED = "не удалось расшифровать: ключ изменился";

// Насколько далеко от низа ленты человек ещё считается «внизу»: пришедшее
// сообщение подматывает ленту только тогда, когда он и так смотрит конец.
const NEAR_BOTTOM = 80;

// renderChat рисует чат в root и отдаёт отписку.
export function renderChat(root, ctx, chatId) {
  const view = {
    ctx,
    chatId,
    me: ctx.me.nick,
    peer: sync.peerOf(chatId),
    limit: ctx.config?.maxMessageChars ?? LIMIT,
    alive: true,
    // Лента: записи по возрастанию id и их строки в разметке.
    items: [],
    nodes: new Map(),
    // Граница «новых»: первый непрочитанный на момент открытия.
    newId: null,
    chain: Promise.resolve(),
  };

  root.append(head(view));

  view.feed = el("div", "feed");
  view.body = el("div", "grid");
  view.body.setAttribute("aria-live", "polite");
  view.feed.append(view.body);
  root.append(view.feed);

  root.append(composer(view));

  const offMessages = sync.on("messages", (detail) => {
    if (detail.chatId === view.chatId) {
      run(view, () => apply(view, detail));
    }
  });
  const offNet = sync.on("net", () => paintBar(view));
  const media = matchMedia(DESKTOP);
  const onMedia = () => paint(view, true);
  media.addEventListener("change", onMedia);

  run(view, () => load(view));

  return () => {
    view.alive = false;
    offMessages();
    offNet();
    media.removeEventListener("change", onMedia);
  };
}

// run выстраивает работу экрана в очередь: загрузка и приходящие события
// не должны перемешиваться.
function run(view, task) {
  view.chain = view.chain.then(task).catch(() => {});
  return view.chain;
}

// --- разметка -----------------------------------------------------------

// head — шапка: имя чата, по нажатию — карточка контакта. «назад» слева
// нужен там, где виден один экран за раз; на десктопе его прячет CSS.
function head(view) {
  const bar = el("div", "head");
  const back = el("button", "back back--chat", "назад");
  back.type = "button";
  back.addEventListener("click", () => view.ctx.go("#/"));
  const title = el("button", "chat-title", `@${view.peer}`);
  title.type = "button";
  title.addEventListener("click", () => view.ctx.go(`#/contact/${view.peer}`));
  bar.append(back, title);
  return bar;
}

// composer — полоса состояния и строка ввода: рамка 1 px ink, слева «>»
// цветом mark. Enter отправляет только на десктопе; на мобильном он делает
// перенос, а отправляет кнопка «>» справа (docs/ui.md, «Чат»).
function composer(view) {
  const form = el("form", "compose");
  form.noValidate = true;

  view.bar = el("p", "bar");
  view.bar.hidden = true;
  view.bar.setAttribute("aria-live", "polite");

  const row = el("div", "input");
  const prompt = el("span", "p", ">");
  prompt.setAttribute("aria-hidden", "true");

  view.field = el("textarea", "input__field");
  view.field.rows = 1;
  view.field.placeholder = "сообщение";
  view.field.maxLength = view.limit;

  view.counter = el("span", "counter");
  view.counter.hidden = true;

  const send = el("button", "input__send", ">");
  send.type = "submit";

  row.append(prompt, view.field, view.counter, el("span", "enter", "enter — отправить"), send);
  form.append(view.bar, row);

  view.field.addEventListener("input", () => count(view));
  view.field.addEventListener("keydown", (event) => {
    if (event.key !== "Enter" || event.shiftKey || event.isComposing) {
      return;
    }
    if (!wide()) {
      return;
    }
    event.preventDefault();
    submit(view);
  });
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    submit(view);
  });

  return form;
}

// count — счётчик остатка: появляется после порога (docs/ui.md, «Чат»).
function count(view) {
  const length = view.field.value.length;
  view.counter.textContent = String(view.limit - length);
  view.counter.hidden = length <= COUNTER_AT;
}

function submit(view) {
  const text = view.field.value;
  if (text.trim() === "") {
    return;
  }
  view.field.value = "";
  count(view);
  run(view, () => sync.send(view.chatId, text));
}

// --- лента --------------------------------------------------------------

async function load(view) {
  let record = null;
  let list = [];
  try {
    record = await sync.chat(view.chatId);
    list = await sync.messagesBefore(view.chatId);
  } catch {
    // Базы нет — рисуем пустую ленту: отправка от этого не ломается.
  }
  if (!view.alive) {
    return;
  }
  view.items = list;
  view.newId = firstUnread(record, list, view.me);
  paint(view, true);
  // Фокус в строку ввода при открытии чата на десктопе (docs/ui.md,
  // «Доступность»); на мобильном это подняло бы клавиатуру на весь экран.
  if (wide()) {
    view.field.focus();
  }
  await read(view);
}

// firstUnread — граница «новых»: первый чужой непрочитанный. Своё
// непрочитанным не бывает, поэтому и границей не становится.
function firstUnread(record, list, me) {
  if (!record || record.unread <= 0) {
    return null;
  }
  const bound = record.lastReadId;
  const found = list.find((m) => m.from !== me && (!bound || m.id > bound));
  return found ? found.id : null;
}

// read помечает чат прочитанным — после отрисовки: до этого lastReadId
// и есть граница «новых» (docs/storage.md).
async function read(view) {
  try {
    await sync.markRead(view.chatId);
  } catch {
    // Счётчик непрочитанных подождёт до следующего раза.
  }
}

// apply разбирает изменения ленты. Дописать в конец дешевле, чем
// перерисовать: лента — живая область, и перерисовка заставила бы
// экранного диктора зачитать её целиком.
async function apply(view, detail) {
  const incoming = [];
  for (const id of detail.ids ?? []) {
    let record = null;
    try {
      record = await sync.message(id);
    } catch {
      return;
    }
    if (record && record.chatId === view.chatId) {
      incoming.push(record);
    }
  }
  if (!view.alive) {
    return;
  }
  const bottom = atBottom(view);
  let whole = false;
  let added = 0;

  for (const id of detail.removed ?? []) {
    const at = view.items.findIndex((m) => m.id === id);
    if (at >= 0) {
      view.items.splice(at, 1);
      whole = true;
    }
  }
  incoming.sort((a, b) => (a.id < b.id ? -1 : 1));
  for (const record of incoming) {
    const at = view.items.findIndex((m) => m.id === record.id);
    if (at >= 0) {
      // Та же запись в новом состоянии: pending стал sent или failed.
      view.items[at] = record;
      if (!whole) {
        redraw(view, record);
      }
      continue;
    }
    const last = view.items[view.items.length - 1];
    if (last && last.id > record.id) {
      // Из очереди сервера пришло то, что старше уже нарисованного.
      view.items.splice(view.items.findIndex((m) => m.id > record.id), 0, record);
      whole = true;
      continue;
    }
    view.items.push(record);
    if (!whole) {
      line(view, record, view.items[view.items.length - 2] ?? null);
    }
    added += 1;
  }

  if (whole) {
    paint(view, bottom);
  } else {
    if (bottom && added > 0) {
      down(view);
    }
    paintBar(view);
  }
  if (whole || added > 0) {
    await read(view);
  }
}

// paint рисует ленту заново.
function paint(view, bottom) {
  clear(view.body);
  view.nodes.clear();
  let previous = null;
  for (const record of view.items) {
    line(view, record, previous);
    previous = record;
  }
  paintBar(view);
  if (bottom) {
    down(view);
  }
}

// line дописывает сообщение в конец ленты вместе с разделителями,
// которые перед ним нужны.
function line(view, record, previous) {
  const day = !previous || dayOf(previous.ts) !== dayOf(record.ts);
  if (day) {
    view.body.append(divider(label(record.ts), false));
  }
  const fresh = record.id === view.newId;
  if (fresh) {
    view.body.append(divider("новые", true));
  }
  // Подряд идущие сообщения одного автора — без повтора автора.
  const first = day || fresh || !previous || previous.from !== record.from;
  const node = el("div", first ? "line is-head" : "line");
  node.append(author(view, record, first), text(view, record));
  view.body.append(node);
  view.nodes.set(record.id, node);
}

// redraw обновляет одну строку на месте: автор и группировка от состояния
// сообщения не зависят.
function redraw(view, record) {
  const node = view.nodes.get(record.id);
  if (!node) {
    return;
  }
  const first = node.classList.contains("is-head");
  clear(node);
  node.append(author(view, record, first), text(view, record));
}

function divider(caption, fresh) {
  const node = el("div", fresh ? "divider divider--new" : "divider");
  node.append(el("span", null, caption));
  return node;
}

// author — колонка автора: ник и время. Свой ник — цветом mark.
function author(view, record, first) {
  const node = el("div", record.from === view.me ? "author author--me" : "author");
  if (!first) {
    return node;
  }
  node.append(el("span", null, record.from), el("span", "t", TIME.format(record.ts)));
  return node;
}

// text — само сообщение. Нерасшифрованное — курсивом с причиной, pending —
// цветом stone, failed — с пометкой «не отправлено · повторить».
function text(view, record) {
  const node = el("div", "text");
  if (record.text === null) {
    node.classList.add("text--none");
    node.textContent = view.peer === null && record.undecryptable === "unknown_key"
      ? NO_ROOM_KEY
      : KEY_CHANGED;
    return node;
  }
  if (record.status === "pending") {
    node.classList.add("text--pending");
  }
  node.append(el("p", "text__body", record.text));
  if (record.status === "failed") {
    const note = el("p", "fail");
    const again = el("button", "link", "повторить");
    again.type = "button";
    again.addEventListener("click", () => run(view, () => sync.retry(record.id)));
    note.append(el("span", null, "не отправлено ·"), again);
    node.append(note);
  }
  return node;
}

// paintBar — полоса над вводом. Причина одна за раз: отказ отправки
// перебивает «нет соединения», потому что он про конкретное сообщение
// и уходит при следующей попытке (ADR-033).
function paintBar(view) {
  const failed = lastFailed(view);
  if (failed) {
    view.bar.className = "bar bar--mark";
    view.bar.textContent = failed.error;
    view.bar.hidden = false;
    return;
  }
  if (!sync.online()) {
    view.bar.className = "bar";
    view.bar.textContent = "нет соединения";
    view.bar.hidden = false;
    return;
  }
  view.bar.hidden = true;
  view.bar.textContent = "";
}

// lastFailed — последнее своё неотправленное сообщение с текстом отказа
// (docs/ui.md, «Чат»). Смотреть на состояние последнего своего нельзя:
// лента отсортирована по ULID, а время в нём — часы отправителя. Отставшие
// часы ставят новое сообщение перед его же старыми, и последним своим
// остаётся давно отправленное — ровно в том случае, ради которого текст
// про часы и заведён (ADR-033).
function lastFailed(view) {
  for (let i = view.items.length - 1; i >= 0; i -= 1) {
    const record = view.items[i];
    if (record.from === view.me && record.status === "failed" && record.error) {
      return record;
    }
  }
  return null;
}

// --- прокрутка и даты ---------------------------------------------------

function atBottom(view) {
  const feed = view.feed;
  return feed.scrollHeight - feed.scrollTop - feed.clientHeight < NEAR_BOTTOM;
}

function down(view) {
  view.feed.scrollTop = view.feed.scrollHeight;
}

function dayOf(ts) {
  const date = new Date(ts);
  return `${date.getFullYear()}-${date.getMonth()}-${date.getDate()}`;
}

// label — дата разделителя. Короткая форма на мобильном без точки
// сокращения: так в эталоне.
function label(ts) {
  if (wide()) {
    return DAY_LONG.format(ts);
  }
  return DAY_SHORT.format(ts).replace(/\.$/, "");
}
