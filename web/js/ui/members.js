// Участники комнаты — docs/ui.md, «Участники».
//
// Состав, владелец и то, чей ключ мешает rekey, приходят из sync: экран
// не пишет в базу и не ходит в сеть сам.

import * as sync from "../sync.js";
import { button, clear, confirmPanel, el, message, setError, setNote } from "./dom.js";

// renderMembers рисует экран в root и отдаёт отписку.
export function renderMembers(root, ctx, roomId) {
  const view = {
    ctx,
    roomId,
    chatId: sync.roomChatId(roomId),
    me: ctx.me.nick,
    alive: true,
    // Событий «rooms» приходит больше одного подряд; рисует последнее.
    generation: 0,
  };

  view.title = el("span", "title", "#");
  root.append(head(view));

  const body = el("div", "body settings");

  // Полоса «нужен новый ключ комнаты» — единственный акцент экрана
  // (docs/identity/brief.md).
  view.warn = el("p", "bar bar--mark");
  view.warn.hidden = true;
  view.warn.setAttribute("aria-live", "polite");

  const people = el("section", "block block--first");
  view.list = el("ul", "members");
  view.note = message();
  people.append(view.list, add(view), view.note);

  body.append(view.warn, people, exit(view));
  root.append(body);

  const off = sync.on("rooms", (detail) => {
    if (detail.id === view.roomId) {
      paint(view);
    }
  });
  paint(view);

  return () => {
    view.alive = false;
    off();
  };
}

function head(view) {
  const bar = el("div", "head");
  const back = el("button", "back", "назад");
  back.type = "button";
  back.addEventListener("click", () => view.ctx.go(`#/room/${view.roomId}`));
  bar.append(back, view.title);
  return bar;
}

// --- разметка -----------------------------------------------------------

// add — строка ввода `@ник` и «добавить». Видна только владельцу: состав
// меняет он (ADR-018).
function add(view) {
  const form = el("form", "form form--row");
  form.noValidate = true;
  form.hidden = true;

  const line = el("div", "input");
  const prompt = el("span", "p", ">");
  prompt.setAttribute("aria-hidden", "true");
  const field = el("input", "input__field");
  field.type = "text";
  field.placeholder = "@ник";
  field.autocapitalize = "off";
  field.autocomplete = "off";
  field.spellcheck = false;
  line.append(prompt, field);

  const go = el("button", "button", "добавить");
  go.type = "submit";
  form.append(line, go);

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (go.disabled) {
      return;
    }
    setNote(view.note, "");
    // Ник вводят как в списке: с «@» или без. Ники строчные (ADR-019).
    const nick = field.value.trim().replace(/^@/, "").toLowerCase();
    if (nick === "") {
      field.focus();
      return;
    }
    field.value = nick;
    go.disabled = true;
    try {
      await sync.changeMembers(view.roomId, { add: [nick] });
      field.value = "";
    } catch (err) {
      setError(view.note, failure(view.ctx, err));
      field.focus();
    } finally {
      go.disabled = false;
    }
  });

  view.add = form;
  return form;
}

// exit — «выйти из комнаты» всем, «удалить комнату» владельцу
// (docs/ui.md, «Участники»).
function exit(view) {
  const box = el("section", "block");
  const note = message();

  const leave = button("выйти из комнаты");
  leave.addEventListener("click", async () => {
    leave.disabled = true;
    setNote(note, "");
    try {
      await sync.leaveRoom(view.roomId);
      view.ctx.go("#/");
    } catch (err) {
      setError(note, failure(view.ctx, err));
      leave.disabled = false;
    }
  });

  const drop = button("удалить комнату");
  drop.hidden = true;
  const panel = confirmPanel("комната будет удалена у всех участников.", "удалить");

  drop.addEventListener("click", () => {
    setNote(note, "");
    drop.hidden = true;
    panel.root.hidden = false;
    panel.yes.focus();
  });
  panel.no.addEventListener("click", () => {
    panel.root.hidden = true;
    drop.hidden = false;
    drop.focus();
  });
  panel.yes.addEventListener("click", async () => {
    panel.yes.disabled = true;
    panel.no.disabled = true;
    try {
      await sync.deleteRoom(view.roomId);
      view.ctx.go("#/");
    } catch (err) {
      setError(note, failure(view.ctx, err));
      panel.yes.disabled = false;
      panel.no.disabled = false;
      panel.root.hidden = true;
      drop.hidden = false;
    }
  });

  view.drop = drop;
  view.panel = panel.root;
  box.append(leave, drop, panel.root, note);
  return box;
}

// --- состояние ----------------------------------------------------------

// paint перечитывает комнату и рисует состав. Владение переходит к участнику
// с наименьшим joined_at (ADR-018), поэтому владельческие части экрана
// появляются и исчезают вместе с составом.
async function paint(view) {
  const mine = ++view.generation;
  let record = null;
  try {
    record = await sync.chat(view.chatId);
  } catch {
    // Базы нет — рисуем пустой состав: выйти из комнаты это не мешает.
  }
  if (!view.alive || mine !== view.generation) {
    return;
  }
  const members = record?.members ?? [];
  const owner = record?.owner ?? null;
  const mineRoom = owner === view.me;

  view.title.textContent = `#${record?.title || view.roomId}`;

  clear(view.list);
  for (const nick of members) {
    view.list.append(person(view, nick, owner, mineRoom));
  }

  view.add.hidden = !mineRoom;
  view.drop.hidden = !mineRoom || !view.panel.hidden;
  if (!mineRoom) {
    view.panel.hidden = true;
  }

  const blocked = sync.needsTrust(view.roomId);
  view.warn.textContent = blocked.length > 0 ? trustText(blocked) : "";
  view.warn.hidden = blocked.length === 0;
}

// person — строка участника: ник, пометка «владелец», «убрать» у владельца.
// Владельца убрать нельзя (docs/protocol.md, `400 owner`), поэтому кнопки
// в его строке нет.
function person(view, nick, owner, mineRoom) {
  const row = el("li", "member");
  row.append(el("span", "member__name", `@${nick}`));
  if (nick === owner) {
    row.append(el("span", "tag", "владелец"));
    return row;
  }
  if (!mineRoom) {
    return row;
  }
  const drop = el("button", "link", "убрать");
  drop.type = "button";
  drop.addEventListener("click", async () => {
    drop.disabled = true;
    setNote(view.note, "");
    try {
      await sync.changeMembers(view.roomId, { remove: [nick] });
    } catch (err) {
      setError(view.note, failure(view.ctx, err));
      drop.disabled = false;
    }
  });
  row.append(drop);
  return row;
}

// failure — текст отказа. Неподтверждённый ключ обрывает смену состава
// до запроса, и состояние у него то же, что у несделанного rekey:
// комнате нужен новый ключ, а раздать его некому (ADR-016, ADR-038).
function failure(ctx, err) {
  if (err instanceof sync.TrustNeeded) {
    return trustText(err.nicks);
  }
  return ctx.errorText(err);
}

function trustText(nicks) {
  return `нужен новый ключ комнаты: подтвердите ключ ${nicks.map((nick) => `@${nick}`).join(", ")}`;
}
