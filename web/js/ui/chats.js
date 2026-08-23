// Список чатов в сайдбаре — docs/ui.md, «Список чатов».
//
// Секции «каналы» и «личные», порядок — по lastId по убыванию (его держит
// sync.chats). Пустая секция не рисуется.

import * as sync from "../sync.js";
import { clear, el } from "./dom.js";

const SECTIONS = [
  ["room", "каналы"],
  ["dm", "личные"],
];

// mount рисует список в root и держит его в актуальном виде, пока экран
// жив. Отдаёт отписку.
export function mount(root, ctx, active) {
  // Событий «chats» приходит больше одного подряд; рисует последнее.
  let generation = 0;
  const paint = async () => {
    const mine = ++generation;
    let list;
    try {
      list = await sync.chats();
    } catch {
      return;
    }
    if (mine !== generation) {
      return;
    }
    clear(root);
    for (const [type, title] of SECTIONS) {
      const part = list.filter((chat) => chat.type === type);
      if (part.length === 0) {
        continue;
      }
      const items = el("ul", "items");
      for (const chat of part) {
        items.append(item(ctx, chat, active));
      }
      root.append(el("h2", "section", title), items);
    }
  };
  const off = sync.on("chats", paint);
  paint();
  return off;
}

function item(ctx, chat, active) {
  const row = el("li");
  const button = el("button", "item");
  button.type = "button";
  if (chat.id === active) {
    // Активный чат — инверсия (docs/identity/brief.md).
    button.classList.add("is-active");
    button.setAttribute("aria-current", "true");
  }
  button.append(el("span", "item__name", sigil(chat) + chat.title));
  if (chat.unread > 0) {
    button.append(el("span", "n", String(chat.unread)));
  }
  button.addEventListener("click", () => ctx.go(hashOf(chat)));
  row.append(button);
  return row;
}

// Сигил ставит экран: в базе чат зовётся без «@» и «#» (docs/storage.md).
function sigil(chat) {
  return chat.type === "dm" ? "@" : "#";
}

function hashOf(chat) {
  return chat.type === "dm" ? `#/dm/${chat.peer}` : `#/room/${chat.roomId}`;
}
