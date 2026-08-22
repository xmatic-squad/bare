// Карточка контакта — docs/ui.md, «Карточка контакта».
//
// Смена ключа собеседника и «доверять новому ключу» появятся вместе
// с TOFU (этап 3, ADR-016): до тех пор у записи peers нет pending.

import * as sync from "../sync.js";
import { fingerprintGroups } from "../crypto.js";
import { button, el, message, setError, setNote } from "./dom.js";

export function renderContact(root, ctx, nick) {
  root.append(head(ctx, nick));
  const body = el("div", "body settings");
  root.append(body);

  const card = el("section", "block block--first");
  body.append(card, remove(ctx, nick));

  // Отпечаток лежит в записи TOFU; её может ещё не быть, если чат
  // открыли до первого ключа.
  sync.peer(nick).then((record) => {
    if (!record?.fingerprint || !card.isConnected) {
      return;
    }
    const groups = fingerprintGroups(record.fingerprint);
    card.append(
      el("p", "fp", groups.slice(0, 8).join(" ")),
      el("p", "fp", groups.slice(8).join(" ")),
      el("p", "fp-hint", "сверьте с собеседником голосом или лично"),
    );
  }).catch(() => {});
}

function head(ctx, nick) {
  const bar = el("div", "head");
  const back = el("button", "back", "назад");
  back.type = "button";
  back.addEventListener("click", () => ctx.go(`#/dm/${nick}`));
  bar.append(back, el("span", "title", `@${nick}`));
  return bar;
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
