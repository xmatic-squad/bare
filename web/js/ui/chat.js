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

// Тексты нерасшифрованного — docs/ui.md, «Чат». Строк там две, а причин
// в записи три (docs/storage.md): «нет ключа комнаты» — это unknown_key
// в комнате, всё остальное сводится к «ключ изменился».
const NO_ROOM_KEY = "не удалось расшифровать: нет ключа комнаты";
const KEY_CHANGED = "не удалось расшифровать: ключ изменился";

// Сколько символов имени комнаты попадает в подсказку ввода. Строка ввода
// растёт под placeholder так же, как под текст, и имя в 64 символа (ADR-018)
// занимало бы три строки. Имя укорачивается многоточием, как в шапке, только
// разметкой: text-overflow к placeholder не применяется. Двенадцать —
// столько, чтобы «сообщение в #имя…» умещалось в одну строку на самом
// узком из целевых экранов (360 px).
const NAME_IN_HINT = 12;

// Насколько далеко от низа ленты человек ещё считается «внизу»: пришедшее
// сообщение подматывает ленту только тогда, когда он и так смотрит конец.
const NEAR_BOTTOM = 80;

// Насколько близко к верхнему краю берётся следующая страница. Запас
// в экран: страница успевает приехать до того, как человек упрётся
// в край (ADR-053).
const NEAR_TOP = 200;

// renderChat рисует чат в root и отдаёт отписку.
export function renderChat(root, ctx, chatId) {
  const view = {
    ctx,
    chatId,
    me: ctx.me.nick,
    peer: sync.peerOf(chatId),
    roomId: sync.roomIdOf(chatId),
    limit: ctx.config?.maxMessageChars ?? LIMIT,
    alive: true,
    // Каким значением открыт редактируемый блок строки ввода:
    // "plaintext-only" или "true" (ADR-069). От него зависит, разбирает ли
    // браузер вставку и перенос строки сам или это делаем мы.
    edit: "true",
    // Имя комнаты; до чтения записи чата вместо него идентификатор,
    // как в db.blankChat.
    name: sync.roomIdOf(chatId),
    // Ключ собеседника изменился и ждёт подтверждения: ввод заблокирован
    // (ADR-016).
    blocked: false,
    // Комнаты у нас больше нет: вышли сами, убрал владелец, комната
    // удалена. Ввод заблокирован, лента остаётся (ADR-044).
    gone: false,
    // Лента: записи по возрастанию id и их разметка — строка и её
    // разделители, id → {node, marks}.
    items: [],
    nodes: new Map(),
    // Выше загруженного есть ещё сообщения: последняя страница пришла
    // целой (ADR-053).
    more: false,
    // Страница уже едет: событий scroll приходит много подряд.
    loading: false,
    // Граница «новых» на момент открытия: было ли непрочитанное и докуда
    // читали. Сама граница считается по всему загруженному — подгрузка
    // вверх двигает её выше (ADR-053).
    mark: { unread: false, bound: null },
    // Граница «новых»: первый непрочитанный на момент открытия.
    newId: null,
    chain: Promise.resolve(),
  };

  root.append(head(view));

  view.feed = el("div", "feed");
  view.body = el("div", "grid");
  view.body.setAttribute("aria-live", "polite");
  view.feed.append(view.body);
  // Прокрутка к верхнему краю берёт следующую страницу (ADR-053).
  view.feed.addEventListener("scroll", () => {
    if (view.feed.scrollTop <= NEAR_TOP) {
      pull(view);
    }
  });
  root.append(view.feed);

  root.append(composer(view));

  const offMessages = sync.on("messages", (detail) => {
    if (detail.chatId === view.chatId) {
      run(view, () => apply(view, detail));
    }
  });
  const offNet = sync.on("net", () => paintBar(view));
  // Доверие к ключу собеседника меняет полосу и ввод; имя комнаты приходит
  // из GET /api/rooms и события room, иногда позже первой отрисовки.
  const offPeers = sync.on("peers", (detail) => {
    if (view.peer !== null && detail.nick === view.peer) {
      run(view, () => checkPeer(view));
    }
  });
  const offRooms = sync.on("rooms", (detail) => {
    if (view.roomId !== null && detail.id === view.roomId) {
      run(view, () => refreshRoom(view));
    }
  });
  const media = matchMedia(DESKTOP);
  const onMedia = () => paint(view, true);
  media.addEventListener("change", onMedia);

  run(view, () => load(view));

  return () => {
    view.alive = false;
    offMessages();
    offNet();
    offPeers();
    offRooms();
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

// head — шапка: имя чата, по нажатию — участники комнаты или карточка
// контакта (docs/ui.md, «Чат»). «назад» слева нужен там, где виден один
// экран за раз; на десктопе его прячет CSS.
function head(view) {
  const bar = el("div", "head");
  const back = el("button", "back back--chat", "назад");
  back.type = "button";
  back.addEventListener("click", () => view.ctx.go("#/"));
  view.title = el("button", "chat-title", titleText(view));
  view.title.type = "button";
  view.title.addEventListener("click", () => view.ctx.go(view.roomId !== null
    ? `#/room/${view.roomId}/members`
    : `#/contact/${view.peer}`));
  bar.append(back, view.title);
  return bar;
}

function titleText(view) {
  return view.roomId !== null ? `#${view.name}` : `@${view.peer}`;
}

// shortName — имя комнаты для подсказки ввода: длинное обрезается
// многоточием. Считается символами, а не единицами utf-16: имя ограничено
// символами (docs/protocol.md), и разрезать пару посередине незачем.
function shortName(name) {
  const chars = Array.from(name);
  return chars.length > NAME_IN_HINT ? `${chars.slice(0, NAME_IN_HINT).join("")}…` : name;
}

// refreshRoom обновляет имя комнаты в шапке и placeholder ввода — имя
// приходит от сервера и бывает известно позже первой отрисовки — и состояние
// членства: комната, из состава которой нас больше нет, гасит ввод
// и показывает полосу (ADR-044).
async function refreshRoom(view, known) {
  if (view.roomId === null) {
    return;
  }
  let record = known;
  if (record === undefined) {
    try {
      record = await sync.chat(view.chatId);
    } catch {
      return;
    }
  }
  if (!view.alive) {
    return;
  }
  view.name = record?.title || view.roomId;
  view.title.textContent = titleText(view);
  hint(view, `сообщение в #${shortName(view.name)}`);
  // Скрытая запись комнаты — это room_left или собственный выход:
  // отправлять больше некуда, и сервер ответил бы not_member.
  view.gone = record?.hidden === true;
  allow(view, !view.gone);
  paintBar(view);
}

// composer — полоса состояния и строка ввода: рамка 1 px ink, слева «>»
// цветом mark. Enter отправляет только на десктопе; на мобильном он делает
// перенос, а отправляет кнопка «>» справа (docs/ui.md, «Чат»).
//
// Строка сообщения — редактируемый блок, а не поле формы (ADR-069): над
// клавиатурой iOS рисует полосу помощника форм, и бывает она только
// у input и textarea. Со страницы её не убрать, а в чате она занимает место
// и ничего не делает: поле на экране одно, переходить стрелками некуда.
function composer(view) {
  const form = el("form", "compose");
  form.noValidate = true;

  view.bar = el("p", "bar");
  view.bar.hidden = true;
  view.bar.setAttribute("aria-live", "polite");

  const row = el("div", "input");
  const prompt = el("span", "p", ">");
  prompt.setAttribute("aria-hidden", "true");

  view.field = el("div", "input__field input__field--text");
  view.edit = editing(view.field);
  // Поле ввода без input: экранному диктору о нём говорят роль и подпись,
  // подпись же стоит подсказкой в пустом блоке (docs/ui.md, «Доступность»).
  view.field.setAttribute("role", "textbox");
  view.field.setAttribute("aria-multiline", "true");
  hint(view, "сообщение");
  // Клавиша Enter на мобильном переносит строку, а не отправляет: отправка
  // там на кнопке «>» (docs/ui.md, «Чат»). Поэтому «enter», а не «send»:
  // подпись клавиши обещает то, что клавиша делает.
  view.field.enterKeyHint = "enter";
  // Сообщение — обычная речь, а не ник и не пароль: заглавная в начале
  // предложения и исправление опечаток тут к месту (в формах входа
  // и «нового чата» они, наоборот, выключены).
  view.field.autocapitalize = "sentences";
  view.field.setAttribute("autocorrect", "on");

  view.counter = el("span", "counter");
  view.counter.hidden = true;

  view.send = el("button", "input__send", ">");
  view.send.type = "submit";

  row.append(prompt, view.field, view.counter, el("span", "enter", "enter — отправить"), view.send);
  form.append(view.bar, row);

  // Предел держится до вставки, а не после: обрезается приходящее,
  // а набранное остаётся на месте (ADR-071).
  view.field.addEventListener("beforeinput", (event) => cap(view, event));
  view.field.addEventListener("input", (event) => {
    if (event.isComposing) {
      // Пока идёт композиция IME, содержимое не трогаем: правка оборвала
      // бы её на полуслове, а подтверждённое не попало бы в блок вовсе.
      // Подтверждённое разберёт compositionend (ADR-071).
      return;
    }
    fit(view);
    tail(view);
    count(view);
  });
  view.field.addEventListener("compositionend", () => {
    fit(view);
    tail(view);
    count(view);
  });
  view.field.addEventListener("keydown", (event) => {
    if (event.key !== "Enter" || event.isComposing) {
      return;
    }
    if (wide() && !event.shiftKey) {
      event.preventDefault();
      submit(view);
      return;
    }
    if (view.edit !== "plaintext-only") {
      // Блок без plaintext-only ставит на Enter <div> или <br>, а значение
      // читается textContent: перенос вписываем сами (ADR-069).
      event.preventDefault();
      insert(view, "\n");
    }
  });
  if (view.edit !== "plaintext-only") {
    // plaintext-only приносит из буфера и мыши только текст сам. Без него
    // в блок приезжает чужая разметка, поэтому вставка и перенос — наши.
    view.field.addEventListener("paste", (event) => {
      event.preventDefault();
      insert(view, event.clipboardData?.getData("text/plain") ?? "");
    });
    view.field.addEventListener("drop", (event) => {
      event.preventDefault();
      insert(view, event.dataTransfer?.getData("text/plain") ?? "");
    });
    // Перетаскивание изнутри блока браузер сделал бы переносом: вписал бы
    // текст на новом месте и убрал со старого. Вставку мы делаем сами,
    // отменяя действие по умолчанию, — уносить исходное браузеру нечем,
    // и перенос молча оборачивался бы удвоением. Объявляем копирование:
    // тогда это честная копия (ADR-071).
    view.field.addEventListener("dragstart", (event) => {
      if (event.dataTransfer !== null) {
        event.dataTransfer.effectAllowed = "copy";
      }
    });
  }
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    submit(view);
  });

  return form;
}

// editing включает редактирование блока и отдаёт значение, которое
// применилось. Нужен plaintext-only: в нём браузер кладёт в блок только
// текст, чем бы ни был буфер обмена, и разметке взяться неоткуда.
//
// Знают его не все браузеры, и незнакомое значение атрибута делает блок
// нередактируемым вовсе — то есть строка ввода перестала бы работать молча.
// Поэтому оно не назначается, а проверяется чтением: не применилось —
// остаётся "true", а вставку, перетаскивание и перенос строки разбирает
// composer (ADR-069).
function editing(field) {
  try {
    field.contentEditable = "plaintext-only";
  } catch {
    // Браузер, которому это значение незнакомо, бросает SyntaxError.
  }
  if (field.contentEditable === "plaintext-only") {
    return "plaintext-only";
  }
  field.contentEditable = "true";
  return "true";
}

// hint — подсказка в пустой строке ввода (docs/ui.md, «Чат»). placeholder
// у редактируемого блока не бывает: текст кладётся атрибутом, а рисует его
// правило CSS через :empty::before. Тот же текст — подпись для экранного
// диктора: другой у строки ввода нет.
function hint(view, text) {
  view.field.dataset.hint = text;
  view.field.setAttribute("aria-label", text);
}

// allow открывает и закрывает ввод: предупреждение о ключе и уход
// из комнаты гасят и блок, и кнопку «>» (ADR-016, ADR-044). У блока нет
// disabled — закрытый перестаёт быть редактируемым и говорит об этом
// экранному диктору.
function allow(view, on) {
  view.field.contentEditable = on ? view.edit : "false";
  if (on) {
    view.field.removeAttribute("aria-disabled");
  } else {
    view.field.setAttribute("aria-disabled", "true");
  }
  view.send.disabled = !on;
}

// cap держит предел до вставки: обрезается то, что приходит в блок,
// а набранное остаётся на месте — так же, как считал maxLength у textarea
// (ADR-071). Влезающее браузер вставляет сам: тогда и отмена (cmd+z)
// остаётся его, а не нашей.
//
// Композицию IME не трогаем вовсе: отменённая на полуслове, она уносит
// с собой и подтверждённый ввод. Лишнее в ней снимет fit на compositionend.
function cap(view, event) {
  if (event.isComposing || event.inputType === "insertCompositionText") {
    return;
  }
  if (!event.inputType.startsWith("insert")) {
    return;
  }
  // Перенос строки и прочее без текста считаем одним символом: тогда cap
  // вмешивается только тогда, когда места не осталось совсем.
  const text = event.data ?? event.dataTransfer?.getData("text/plain") ?? "";
  const range = target(view, event);
  if ((text === "" ? 1 : text.length) <= free(view, range)) {
    return;
  }
  event.preventDefault();
  insert(view, text, range);
}

// target — куда пойдёт вставка: диапазон, который браузер собирается
// заменить (beforeinput знает его точно — это не всегда выделение:
// вставка мышью идёт туда, куда отпустили), иначе выделение в блоке.
function target(view, event = null) {
  const ranges = event?.getTargetRanges?.() ?? [];
  if (ranges.length > 0) {
    const range = document.createRange();
    range.setStart(ranges[0].startContainer, ranges[0].startOffset);
    range.setEnd(ranges[0].endContainer, ranges[0].endOffset);
    return view.field.contains(range.commonAncestorContainer) ? range : null;
  }
  const selection = getSelection();
  const now = selection !== null && selection.rangeCount > 0 ? selection.getRangeAt(0) : null;
  return now !== null && view.field.contains(now.commonAncestorContainer) ? now : null;
}

// free — сколько символов ещё влезет туда, куда идёт вставка: предел минус
// то, что останется от набранного, когда заменяемое уйдёт.
function free(view, range) {
  return view.limit - (value(view).length - (range === null ? 0 : range.toString().length));
}

// value — набранное так, как оно уедет собеседнику. В plaintext-only
// браузер держит последнюю строку вторым «\n» в самом конце: в textContent
// он виден, а в сообщении его нет — его снимает trim в sync.send. Считаем
// и режем по тому же, что отправляем (ADR-071). В запасном пути последнюю
// строку держит <br>, которого в textContent нет вовсе, и перенос в конце
// там настоящий.
function value(view) {
  const text = view.field.textContent;
  return view.edit === "plaintext-only" && text.endsWith("\n") ? text.slice(0, -1) : text;
}

// cut режет текст по пределу, не разрывая суррогатную пару: половина пары
// уехала бы собеседнику битым символом.
function cut(text, limit) {
  if (limit <= 0) {
    return "";
  }
  if (text.length <= limit) {
    return text;
  }
  const last = text.charCodeAt(limit - 1);
  return text.slice(0, last >= 0xd800 && last <= 0xdbff ? limit - 1 : limit);
}

// fit — последняя страховка предела (docs/ui.md, «Чат»): лишнее в блок
// попадает мимо cap — композицией IME или вставкой, о которой браузер
// не рассказал. Снимается ровно хвост сверх предела, а не переписывается
// блок целиком: правка узлов оставляет курсор на месте и не сносит стек
// отмены. Опустевший блок остаётся без узлов: подсказка стоит правилом
// :empty, а браузер оставляет в опустевшем блоке <br>, и с ним подсказки
// не видно.
function fit(view) {
  const raw = view.field.textContent;
  const text = value(view);
  const keep = cut(text, view.limit);
  if (keep.length < text.length) {
    // Вместе с лишним уходит и заполнитель последней строки, если он есть:
    // он в самом конце, а браузер ставит его снова, когда понадобится.
    trim(view.field, raw.length - keep.length);
    return;
  }
  if (raw === "" && view.field.firstChild !== null) {
    clear(view.field);
  }
}

// trim снимает с хвоста блока лишние единицы текста, не трогая остального:
// textContent = … переписал бы блок целиком и унёс бы и курсор, и отмену.
function trim(field, extra) {
  const walker = document.createTreeWalker(field, NodeFilter.SHOW_TEXT);
  const nodes = [];
  while (walker.nextNode() !== null) {
    nodes.push(walker.currentNode);
  }
  let left = extra;
  for (let i = nodes.length - 1; i >= 0 && left > 0; i -= 1) {
    const node = nodes[i];
    const take = Math.min(left, node.length);
    node.deleteData(node.length - take, take);
    left -= take;
  }
}

// insert вписывает текст туда, куда идёт вставка, обрезая его по свободному
// месту. Своими руками, а не execCommand: тот в Chrome разбирает перенос
// строки в <div>, а в блоке живёт только текст.
function insert(view, text, where) {
  // where — диапазон, который назвал beforeinput. Его не передали — берём
  // выделение; выделения в блоке нет — вписываем в конец.
  const range = where === undefined ? target(view) : where;
  const fitted = cut(text, free(view, range));
  if (fitted === "") {
    return;
  }
  const node = document.createTextNode(fitted);
  const selection = getSelection();
  if (range === null) {
    view.field.append(node);
  } else {
    range.deleteContents();
    range.insertNode(node);
  }
  if (selection !== null) {
    const at = document.createRange();
    at.setStartAfter(node);
    at.collapse(true);
    selection.removeAllRanges();
    selection.addRange(at);
  }
  // Вставка делит текст на два-три соседних узла; курсор normalize
  // переставляет сам.
  view.field.normalize();
  // Своё изменение блока события input не порождает: счётчик, предел
  // и последнюю строку трогаем руками.
  fit(view);
  tail(view);
  count(view);
}

// tail держит последнюю пустую строку видимой. Перенос в самом конце
// браузер не рисует — строки после него нет, — и курсор оставался бы
// на прежней строке, а набранное дальше уезжало бы перед переносом.
// Пустую строку держит <br>: тот самый заполнитель, который браузер сам
// ставит в опустевший блок. В textContent его нет, и на отправляемый текст
// он не влияет.
//
// В plaintext-only это не нужно: там последнюю строку браузер ведёт сам,
// своим переносом, и лишний заполнитель дал бы вторую пустую строку.
function tail(view) {
  if (view.edit === "plaintext-only") {
    return;
  }
  const last = view.field.lastChild;
  const wants = view.field.textContent.endsWith("\n");
  if (wants && (last === null || last.nodeName !== "BR")) {
    view.field.append(document.createElement("br"));
    return;
  }
  if (!wants && last !== null && last.nodeName === "BR") {
    last.remove();
  }
}

// count — счётчик остатка: появляется после порога (docs/ui.md, «Чат»).
// Считается то, что уедет: заполнитель последней строки в счёт не идёт
// (ADR-071).
function count(view) {
  const length = value(view).length;
  view.counter.textContent = String(view.limit - length);
  view.counter.hidden = length <= COUNTER_AT;
}

function submit(view) {
  const text = view.field.textContent;
  if (view.blocked || view.gone || text.trim() === "") {
    return;
  }
  view.field.textContent = "";
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
  await refreshRoom(view, record ?? null);
  await checkPeer(view);
  if (!view.alive) {
    return;
  }
  view.items = list;
  // Страница пришла целой — выше есть ещё (ADR-053).
  view.more = list.length >= sync.PAGE;
  view.mark = { unread: (record?.unread ?? 0) > 0, bound: record?.lastReadId ?? null };
  view.newId = firstUnread(view.mark, list, view.me);
  paint(view, true);
  // Фокус в строку ввода при открытии чата на десктопе (docs/ui.md,
  // «Доступность»); на мобильном это подняло бы клавиатуру на весь экран.
  if (wide()) {
    view.field.focus();
  }
  reach(view);
  await read(view);
}

// firstUnread — граница «новых»: первый чужой непрочитанный. Своё
// непрочитанным не бывает, поэтому и границей не становится.
//
// Считается по всему загруженному: чат с сотней непрочитанных открывается
// последней страницей, и первый из них лежит выше — граница находится,
// когда до него домотают (ADR-053).
function firstUnread(mark, list, me) {
  if (!mark.unread) {
    return null;
  }
  const found = list.find((m) => m.from !== me && (!mark.bound || m.id > mark.bound));
  return found ? found.id : null;
}

// --- страницы -----------------------------------------------------------

// pull просит следующую страницу. Событий scroll приходит много подряд,
// поэтому вход закрывается до постановки в очередь.
function pull(view) {
  if (!view.more || view.loading || !view.alive) {
    return;
  }
  view.loading = true;
  run(view, () => older(view));
}

// older дописывает страницу сверху: курсор по индексу «chat» назад
// от самого старого загруженного, по 50 (docs/storage.md, ADR-053).
// Страница короче полной означает, что выше ничего нет.
async function older(view) {
  const first = view.items[0] ?? null;
  try {
    const list = await sync.messagesBefore(view.chatId, first ? first.id : null);
    if (!view.alive) {
      return;
    }
    if (list.length < sync.PAGE) {
      view.more = false;
    }
    if (list.length === 0) {
      return;
    }
    view.items = [...list, ...view.items];
    keep(view, () => grow(view, list, first));
  } catch {
    // Базы нет — оставляем то, что уже загружено.
  } finally {
    view.loading = false;
    reach(view);
  }
}

// grow дописывает страницу сверху, не пересобирая ленту: нарисованное
// переживает подгрузку — выделение текста не пропадает, а живая область
// не зачитывается экранным диктором заново (ADR-053). Заново считаются две
// строки: та, что держала линию «новые», если граница уехала выше, и бывшая
// первая — у неё появился сосед сверху, а от соседа зависят разделитель
// даты и повтор автора.
function grow(view, list, head) {
  const was = view.newId;
  view.newId = firstUnread(view.mark, view.items, view.me);
  const page = document.createDocumentFragment();
  let previous = null;
  for (const record of list) {
    line(view, record, previous, page);
    previous = record;
  }
  view.body.insertBefore(page, view.body.firstChild);
  if (was !== null && was !== view.newId && (head === null || was !== head.id)) {
    const at = view.items.findIndex((m) => m.id === was);
    if (at > 0) {
      reline(view, view.items[at], view.items[at - 1]);
    }
  }
  if (head !== null) {
    reline(view, head, previous);
  }
}

// reline перерисовывает одну строку вместе с её разделителями: у неё
// сменился сосед сверху или уехала линия «новые».
function reline(view, record, previous) {
  const old = view.nodes.get(record.id);
  if (!old) {
    return;
  }
  const next = old.node.nextSibling;
  for (const node of old.marks) {
    node.remove();
  }
  old.node.remove();
  const box = document.createDocumentFragment();
  line(view, record, previous, box);
  view.body.insertBefore(box, next);
}

// keep сохраняет расстояние до низа ленты: подгрузка вверх не должна
// двигать то, что человек читает.
function keep(view, draw) {
  const feed = view.feed;
  const bottom = feed.scrollHeight - feed.scrollTop;
  draw();
  feed.scrollTop = feed.scrollHeight - bottom;
}

// reach берёт следующую страницу, когда прокручивать нечего: лента короче
// окна, события scroll не будет, а сообщения выше есть.
function reach(view) {
  if (view.feed.scrollHeight <= view.feed.clientHeight) {
    pull(view);
  }
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
  // Импорт архива приносит недостающую историю пачкой и в середину ленты:
  // перечитать её целиком дешевле, чем вставлять сообщение за сообщением
  // (ADR-050).
  if (detail.whole === true) {
    await load(view);
    return;
  }
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

// line дописывает сообщение в конец parent вместе с разделителями, которые
// перед ним нужны. Разделители принадлежат строке: подгрузка страницы
// сверху перерисовывает строку вместе с ними, а не всю ленту.
function line(view, record, previous, parent = view.body) {
  const marks = [];
  if (!previous || dayOf(previous.ts) !== dayOf(record.ts)) {
    marks.push(divider(label(record.ts), false));
  }
  if (record.id === view.newId) {
    marks.push(divider("новые", true));
  }
  // Подряд идущие сообщения одного автора — без повтора автора.
  const first = marks.length > 0 || !previous || previous.from !== record.from;
  const node = el("div", first ? "line is-head" : "line");
  node.append(author(view, record, first), text(view, record));
  parent.append(...marks, node);
  view.nodes.set(record.id, { node, marks });
}

// redraw обновляет одну строку на месте: автор и группировка от состояния
// сообщения не зависят.
function redraw(view, record) {
  const known = view.nodes.get(record.id);
  if (!known) {
    return;
  }
  const first = known.node.classList.contains("is-head");
  clear(known.node);
  known.node.append(author(view, record, first), text(view, record));
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
    node.textContent = view.roomId !== null && record.undecryptable === "unknown_key"
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

// checkPeer — состояние доверия к ключу собеседника. Ключ изменился
// и ждёт подтверждения — ввод заблокирован до «доверять новому ключу»
// (docs/ui.md, «Чат»). В комнате блокировать нечего: ключ там симметричный,
// а чьи ключи мешают rekey, показывает экран участников.
async function checkPeer(view) {
  if (view.peer === null) {
    return;
  }
  let record = null;
  try {
    record = await sync.peer(view.peer);
  } catch {
    // Базы нет — считаем ключ прежним: отправка не блокируется.
  }
  if (!view.alive) {
    return;
  }
  view.blocked = !!record?.pending;
  // Ввод заблокирован целиком: и блок, и кнопка «>» на мобильном.
  allow(view, !view.blocked);
  paintBar(view);
}

// paintBar — полоса над вводом. Причина одна за раз (ADR-033). Порядок:
// комнаты у нас больше нет — перебивает всё, отправлять некуда (ADR-044);
// предупреждение о ключе — только оно блокирует ввод в личном чате,
// и пока оно висит, повторять отправку нечем (ADR-038); отказ отправки
// перебивает «нет соединения», потому что он про конкретное сообщение
// и уходит при следующей попытке.
function paintBar(view) {
  if (view.gone) {
    band(view, "bar bar--mark", "вы больше не участник комнаты", false);
    return;
  }
  if (view.blocked) {
    band(view, "bar bar--mark", `ключ @${view.peer} изменился. сверьте отпечаток лично.`, true);
    return;
  }
  const failed = lastFailed(view);
  if (failed) {
    band(view, "bar bar--mark", failed.error, false);
    return;
  }
  if (!sync.online()) {
    band(view, "bar", "нет соединения", false);
    return;
  }
  clear(view.bar);
  view.bar.hidden = true;
}

// band — сама полоса. Кнопка «доверять новому ключу» стоит в ней же:
// подтверждение — единственный выход из состояния (ADR-016).
function band(view, className, caption, trust) {
  clear(view.bar);
  view.bar.className = className;
  view.bar.append(el("span", null, caption));
  if (trust) {
    const yes = el("button", "link", "доверять новому ключу");
    yes.type = "button";
    yes.addEventListener("click", () => {
      yes.disabled = true;
      run(view, async () => {
        try {
          await sync.trustKey(view.peer);
        } finally {
          // Удалось — полосу перерисует событие «peers»; нет — кнопка
          // снова готова к нажатию.
          yes.disabled = false;
        }
      });
    });
    view.bar.append(yes);
  }
  view.bar.hidden = false;
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
