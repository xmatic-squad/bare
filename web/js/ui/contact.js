// Карточка контакта — docs/ui.md, «Карточка контакта».
//
// Отпечаток доверенного ключа, а если ключ ника изменился и ждёт
// подтверждения — оба отпечатка и «доверять новому ключу» (ADR-016).

import * as sync from "../sync.js";
import { fingerprintGroups } from "../crypto.js";
import { button, clear, el, message, setError, setNote } from "./dom.js";

// renderContact рисует карточку в root и отдаёт отписку.
export function renderContact(root, ctx, nick) {
  const view = {
    ctx,
    nick,
    alive: true,
    // Событий «peers» приходит больше одного подряд; рисует последнее.
    generation: 0,
  };
  root.append(head(ctx, nick));
  const body = el("div", "body settings");
  view.card = el("section", "block block--first");
  body.append(view.card, remove(ctx, nick));
  root.append(body);

  const off = sync.on("peers", (detail) => {
    if (detail.nick === view.nick) {
      paint(view);
    }
  });
  paint(view);

  return () => {
    view.alive = false;
    off();
  };
}

function head(ctx, nick) {
  const bar = el("div", "head");
  const back = el("button", "back", "назад");
  back.type = "button";
  back.addEventListener("click", () => ctx.go(`#/dm/${nick}`));
  bar.append(back, el("span", "title", `@${nick}`));
  return bar;
}

// paint рисует отпечатки. Записи TOFU может ещё не быть: чат открыли
// раньше, чем пришёл первый ключ.
async function paint(view) {
  const mine = ++view.generation;
  let record = null;
  try {
    record = await sync.peer(view.nick);
  } catch {
    // Базы нет — показывать нечего.
  }
  if (!view.alive || mine !== view.generation || !record?.fingerprint) {
    return;
  }
  clear(view.card);
  if (!record.pending) {
    fingerprint(view.card, record.fingerprint, null);
    view.card.append(el("p", "fp-hint", "сверьте с собеседником голосом или лично"));
    return;
  }
  // Ключ изменился: показываем оба отпечатка с пометками, чтобы
  // подтверждали не наугад (ADR-038).
  fingerprint(view.card, record.fingerprint, "старый");
  fingerprint(view.card, record.pending.fingerprint, "новый");
  view.card.append(el("p", "fp-hint", "сверьте с собеседником голосом или лично"), trust(view));
}

// fingerprint — 64 hex группами по 4 в две строки (ADR-016).
function fingerprint(box, value, label) {
  if (label) {
    box.append(el("p", "fp-label", label));
  }
  const groups = fingerprintGroups(value);
  box.append(el("p", "fp", groups.slice(0, 8).join(" ")), el("p", "fp", groups.slice(8).join(" ")));
}

// trust — «доверять новому ключу»: ключ из pending становится основным,
// и всё, что в него упиралось, повторяется (ADR-016).
function trust(view) {
  const yes = button("доверять новому ключу");
  yes.addEventListener("click", async () => {
    yes.disabled = true;
    try {
      await sync.trustKey(view.nick);
    } catch {
      // Не вышло — запись цела, экран перерисуется с теми же ключами.
      yes.disabled = false;
    }
  });
  return yes;
}

// remove — «убрать из списка»: строка контакта уходит с сервера, история
// на устройстве остаётся (ADR-019).
function remove(ctx, nick) {
  const box = el("section", "block");
  const note = message();
  const drop = button("убрать из списка");
  drop.addEventListener("click", async () => {
    drop.disabled = true;
    setNote(note, "");
    try {
      await sync.forgetChat(sync.dmChatId(nick));
      ctx.go("#/");
    } catch (err) {
      setError(note, ctx.errorText(err));
      drop.disabled = false;
    }
  });
  box.append(drop, note);
  return box;
}
