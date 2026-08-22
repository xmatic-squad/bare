// Каркас: сайдбар со списком чатов и место под экран — docs/ui.md, «Каркас»
// и «Список чатов». Чаты появятся на этапе 2, секции пока пустые.

import { el, mark } from "./dom.js";

// frame отдаёт корень и место под экран. screen — что показывать
// на мобильном, где виден один экран за раз: "list" или "screen".
export function frame(ctx, screen) {
  const root = el("div", "shell");
  root.dataset.screen = screen;
  const main = el("main", "main");
  root.append(side(ctx), main);
  return { root, main };
}

function side(ctx) {
  const nav = el("nav", "side");

  const brand = el("div", "brand");
  brand.append(mark(), el("span", null, "bare"));
  nav.append(brand);

  const list = el("div", "list");
  for (const title of ["каналы", "личные"]) {
    list.append(el("h2", "section", title), el("ul", "items"));
  }
  nav.append(list);

  const me = el("button", "me");
  me.type = "button";
  const dot = el("i");
  dot.setAttribute("aria-hidden", "true");
  me.append(dot, el("span", null, `ты: @${ctx.me.nick}`));
  me.addEventListener("click", () => ctx.go("#/settings"));
  nav.append(me);

  return nav;
}
