// Каркас: сайдбар со списком чатов и место под экран — docs/ui.md, «Каркас»
// и «Список чатов».

import { el, mark } from "./dom.js";
import { mount } from "./chats.js";

// frame отдаёт корень, место под экран и отписку списка чатов.
// screen — что показывать на мобильном, где виден один экран за раз:
// "list" или "screen". active — чат, который сейчас открыт.
export function frame(ctx, screen, active = null) {
  const root = el("div", "shell");
  root.dataset.screen = screen;
  const main = el("main", "main");
  const { nav, dispose } = side(ctx, active);
  root.append(nav, main);
  return { root, main, dispose };
}

function side(ctx, active) {
  const nav = el("nav", "side");

  const brand = el("div", "brand");
  brand.append(mark(), el("span", null, "bare"));
  nav.append(brand);

  const list = el("div", "list");
  const add = el("button", "item item--new", "+ новый чат");
  add.type = "button";
  add.addEventListener("click", () => ctx.go("#/new"));
  const items = el("div");
  list.append(add, items);
  nav.append(list);

  const me = el("button", "me");
  me.type = "button";
  const dot = el("i");
  dot.setAttribute("aria-hidden", "true");
  me.append(dot, el("span", null, `ты: @${ctx.me.nick}`));
  me.addEventListener("click", () => ctx.go("#/settings"));
  nav.append(me);

  return { nav, dispose: mount(items, ctx, active) };
}
