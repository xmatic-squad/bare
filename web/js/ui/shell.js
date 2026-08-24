// Каркас: сайдбар со списком чатов и место под экран — docs/ui.md, «Каркас»
// и «Список чатов».

import * as pwa from "../pwa.js";
import { INSTALL_IOS, clear, el, mark } from "./dom.js";
import { mount } from "./chats.js";

// Время коммита — в местной зоне и коротко: день с месяцем и часы
// с минутами (ADR-067). Год не показывается: версия отвечает на вопрос
// «что сейчас работает», а не ведёт летопись.
const BUILD_DAY = new Intl.DateTimeFormat("ru-RU", { day: "2-digit", month: "2-digit" });
const BUILD_TIME = new Intl.DateTimeFormat("ru-RU", { hour: "2-digit", minute: "2-digit" });

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

  // Баннер установки — над списком чатов (docs/ui.md, «Баннер установки»).
  // Место под него занимается сразу, содержимое приезжает из meta.
  const place = el("div", "banner-slot");
  nav.append(place);
  banner(place);

  const list = el("div", "list");
  const add = el("button", "item item--new", "+ новый чат");
  add.type = "button";
  add.addEventListener("click", () => ctx.go("#/new"));
  const items = el("div");
  list.append(add, items);
  nav.append(list);

  const foot = el("div", "foot");
  const me = el("button", "me");
  me.type = "button";
  me.setAttribute("aria-label", `настройки: @${ctx.me.nick}`);
  const profile = el("img", "me__icon");
  profile.src = "/icons/profile.svg";
  profile.alt = "";
  profile.draggable = false;
  me.append(profile, el("span", "me__nick", `@${ctx.me.nick}`));
  me.addEventListener("click", () => ctx.go("#/settings"));
  foot.append(me);
  const version = build(ctx.config);
  if (version !== null) {
    foot.append(el("span", "build", version));
  }
  nav.append(foot);

  return { nav, dispose: mount(items, ctx, active) };
}

// build — версия и время коммита из GET /api/config (ADR-074, ADR-067).
// Конфигурации нет — офлайн-старт до первого ответа сервера — значит,
// и строки нет: выдумывать версию не из чего. Время без версии не бывает:
// её сервер отдаёт всегда, хотя бы как «unknown».
function build(config) {
  const version = config?.version;
  if (typeof version !== "string" || version === "") {
    return null;
  }
  const at = config?.commitAt;
  if (!Number.isFinite(at) || at <= 0) {
    return version;
  }
  return `${version} · ${BUILD_DAY.format(at)} ${BUILD_TIME.format(at)}`;
}

// banner — баннер установки на iOS: пуши там работают только
// у установленного приложения (ADR-011). Крестик закрывает его насовсем.
async function banner(place) {
  if (!pwa.iosBrowser() || await pwa.bannerHidden()) {
    return;
  }
  const box = el("div", "banner");
  box.append(el("p", "banner__text", INSTALL_IOS));
  const close = el("button", "banner__close", "×");
  close.type = "button";
  close.setAttribute("aria-label", "закрыть");
  close.addEventListener("click", () => {
    clear(place);
    pwa.hideBanner();
  });
  box.append(close);
  place.append(box);
}
