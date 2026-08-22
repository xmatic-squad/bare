// Новый чат — docs/ui.md, «Новый чат». Две строки ввода: `@ник` открывает
// личный чат, `#имя комнаты` заводит комнату.

import * as sync from "../sync.js";
import { el, message, setError, setNote } from "./dom.js";

// Имя комнаты — до 64 символов (ADR-018, ADR-021). maxLength считает
// единицы UTF-16, а сервер — руны: за предел это не выпустит.
const ROOM_NAME_MAX = 64;

export function renderNew(root, ctx) {
  root.append(head(ctx));

  const body = el("div", "body");
  // Место под ошибку одно на оба поля: строка состояния у экрана одна
  // (ADR-028).
  const note = message();

  const dm = row("@ник");
  const room = row("#имя комнаты");
  room.field.maxLength = ROOM_NAME_MAX;

  dm.form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (dm.go.disabled) {
      return;
    }
    setNote(note, "");
    // Ник вводят как в списке: с «@» или без. Регистр не хранится —
    // ники строчные (ADR-019).
    const nick = dm.field.value.trim().replace(/^@/, "").toLowerCase();
    if (nick === "") {
      dm.field.focus();
      return;
    }
    dm.field.value = nick;
    dm.go.disabled = true;
    try {
      await sync.openDm(nick);
      ctx.go(`#/dm/${nick}`);
    } catch (err) {
      setError(note, ctx.errorText(err));
      dm.field.focus();
    } finally {
      dm.go.disabled = false;
    }
  });

  room.form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (room.go.disabled) {
      return;
    }
    setNote(note, "");
    // Имя вводят как в списке: с «#» или без. Регистр имени комнаты
    // сохраняется — это её название, а не идентификатор.
    const name = room.field.value.trim().replace(/^#/, "").trim();
    if (name === "") {
      room.field.focus();
      return;
    }
    room.go.disabled = true;
    try {
      const chatId = await sync.createRoom(name);
      if (chatId === null) {
        room.field.focus();
        return;
      }
      ctx.go(`#/room/${sync.roomIdOf(chatId)}`);
    } catch (err) {
      setError(note, ctx.errorText(err));
      room.field.focus();
    } finally {
      room.go.disabled = false;
    }
  });

  body.append(dm.form, room.form, note);
  root.append(body);
  dm.field.focus();
}

// row — строка ввода в стиле чата: рамка 1 px ink, слева «>» цветом mark
// (docs/identity/brief.md, «Компоновка»).
function row(placeholder) {
  const form = el("form", "form form--row");
  form.noValidate = true;

  const line = el("div", "input");
  const prompt = el("span", "p", ">");
  prompt.setAttribute("aria-hidden", "true");
  const field = el("input", "input__field");
  field.type = "text";
  field.placeholder = placeholder;
  field.autocapitalize = "off";
  field.autocomplete = "off";
  field.spellcheck = false;
  const go = el("button", "input__send", ">");
  go.type = "submit";
  line.append(prompt, field, go);

  form.append(line);
  return { form, field, go };
}

function head(ctx) {
  const bar = el("div", "head");
  const back = el("button", "back", "назад");
  back.type = "button";
  back.addEventListener("click", () => ctx.go("#/"));
  bar.append(back, el("span", "title", "новый чат"));
  return bar;
}
