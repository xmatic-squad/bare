// Новый чат — docs/ui.md, «Новый чат». Строка `#имя комнаты` появится
// вместе с комнатами (этап 3): создавать пока нечего.

import * as sync from "../sync.js";
import { el, message, setError, setNote } from "./dom.js";

export function renderNew(root, ctx) {
  root.append(head(ctx));

  const body = el("div", "body");
  const form = el("form", "form");
  form.noValidate = true;

  const row = el("div", "input");
  const prompt = el("span", "p", ">");
  prompt.setAttribute("aria-hidden", "true");
  const field = el("input", "input__field");
  field.type = "text";
  field.placeholder = "@ник";
  field.autocapitalize = "off";
  field.autocomplete = "off";
  field.spellcheck = false;
  const go = el("button", "input__send", ">");
  go.type = "submit";
  row.append(prompt, field, go);

  const note = message();
  form.append(row, note);

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (go.disabled) {
      return;
    }
    setNote(note, "");
    // Ник вводят как в списке: с «@» или без. Регистр не хранится —
    // ники строчные (ADR-019).
    const nick = field.value.trim().replace(/^@/, "").toLowerCase();
    if (nick === "") {
      field.focus();
      return;
    }
    field.value = nick;
    go.disabled = true;
    try {
      await sync.openDm(nick);
      ctx.go(`#/dm/${nick}`);
    } catch (err) {
      setError(note, ctx.errorText(err));
      field.focus();
    } finally {
      go.disabled = false;
    }
  });

  body.append(form);
  root.append(body);
  field.focus();
}

function head(ctx) {
  const bar = el("div", "head");
  const back = el("button", "back", "назад");
  back.type = "button";
  back.addEventListener("click", () => ctx.go("#/"));
  bar.append(back, el("span", "title", "новый чат"));
  return bar;
}
