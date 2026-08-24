// Новый чат — docs/ui.md, «Новый чат». Это выбор одного действия:
// личный чат по нику или новая приватная комната (ADR-078).

import * as sync from "../sync.js";
import { el, message, setError, setNote } from "./dom.js";

// Имя комнаты — до 64 символов (ADR-018, ADR-021). maxLength считает
// единицы UTF-16, а сервер — руны: за предел это не выпустит.
const ROOM_NAME_MAX = 64;

const MODES = [
  ["dm", "личный чат"],
  ["room", "новая комната"],
];

export function renderNew(root, ctx) {
  root.append(head(ctx));

  const body = el("div", "body new");
  const dm = direct(ctx);
  const room = roomForm(ctx);
  const panels = { dm, room };
  let mode = "dm";

  const tabs = el("div", "tabs new__tabs");
  tabs.setAttribute("role", "group");
  tabs.setAttribute("aria-label", "что открыть");

  const buttons = new Map();
  const select = (next, focus = false) => {
    mode = next;
    for (const [name, panel] of Object.entries(panels)) {
      const on = name === mode;
      panel.root.hidden = !on;
      const tab = buttons.get(name);
      tab.classList.toggle("is-on", on);
      tab.setAttribute("aria-pressed", String(on));
    }
    if (focus) {
      panels[mode].field.focus();
    }
  };

  MODES.forEach(([name, text], index) => {
    if (index > 0) {
      const sep = el("span", "tabs__sep", "/");
      sep.setAttribute("aria-hidden", "true");
      tabs.append(sep);
    }
    const tab = el("button", "tab", text);
    tab.type = "button";
    tab.addEventListener("click", () => select(name, true));
    buttons.set(name, tab);
    tabs.append(tab);
  });

  body.append(tabs, dm.root, room.root);
  root.append(body);
  select(mode);
}

function direct(ctx) {
  const panel = section("напишите человеку по нику.");
  const input = field("ник человека", "@ник");
  const go = el("button", "button", "написать");
  go.type = "submit";
  const note = message();
  panel.form.append(input.wrap, go, note);

  panel.form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (go.disabled) {
      return;
    }
    setNote(note, "");
    // Ник вводят как в списке: с «@» или без. Регистр не хранится —
    // ники строчные (ADR-019).
    const nick = input.field.value.trim().replace(/^@/, "").toLowerCase();
    if (nick === "") {
      input.field.focus();
      return;
    }
    input.field.value = nick;
    go.disabled = true;
    try {
      await sync.openDm(nick);
      ctx.go(`#/dm/${nick}`);
    } catch (err) {
      setError(note, ctx.errorText(err));
      input.field.focus();
    } finally {
      go.disabled = false;
    }
  });

  return { ...panel, field: input.field };
}

function roomForm(ctx) {
  const panel = section(
    "создайте новую приватную комнату. сначала в ней будете только вы; участников добавите следующим шагом.",
  );
  panel.copy.append(el(
    "span",
    "new__aside",
    "если вас добавят в чужую комнату, она появится в списке сама.",
  ));
  const input = field("название новой комнаты", "#название");
  input.field.maxLength = ROOM_NAME_MAX;
  const go = el("button", "button", "создать комнату");
  go.type = "submit";
  const note = message();
  panel.form.append(input.wrap, go, note);

  panel.form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (go.disabled) {
      return;
    }
    setNote(note, "");
    // Имя вводят как в списке: с «#» или без. Регистр имени комнаты
    // сохраняется — это её название, а не идентификатор.
    const name = input.field.value.trim().replace(/^#/, "").trim();
    if (name === "") {
      input.field.focus();
      return;
    }
    go.disabled = true;
    try {
      const chatId = await sync.createRoom(name);
      if (chatId === null) {
        input.field.focus();
        return;
      }
      // Новая комната начинается с одного владельца. Первый полезный
      // экран — её состав: там сразу видны владелец и добавление людей.
      ctx.go(`#/room/${sync.roomIdOf(chatId)}/members`);
    } catch (err) {
      setError(note, ctx.errorText(err));
      input.field.focus();
    } finally {
      go.disabled = false;
    }
  });

  return { ...panel, field: input.field };
}

function section(text) {
  const root = el("section", "new__panel");
  const copy = el("p", "new__copy", text);
  const form = el("form", "form");
  form.noValidate = true;
  root.append(copy, form);
  return { root, copy, form };
}

function field(label, placeholder) {
  const wrap = el("label", "field");
  wrap.append(el("span", null, label));
  const input = el("input");
  input.type = "text";
  input.placeholder = placeholder;
  input.autocapitalize = "off";
  input.autocomplete = "off";
  input.spellcheck = false;
  wrap.append(input);
  return { wrap, field: input };
}

function head(ctx) {
  const bar = el("div", "head");
  const back = el("button", "back", "назад");
  back.type = "button";
  back.addEventListener("click", () => ctx.go("#/"));
  bar.append(back, el("span", "title", "новый чат"));
  return bar;
}
