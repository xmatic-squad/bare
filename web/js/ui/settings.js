// Настройки — docs/ui.md, «Настройки». На этом этапе только разделы,
// которые уже работают: кто ты, уведомления, установка приложения, смена
// пароля, выход, удаление аккаунта. Устройства и история — дальше по плану.

import { ApiError } from "../api.js";
import { fingerprintGroups } from "../crypto.js";
import * as pwa from "../pwa.js";
import { INSTALL_IOS, button, confirmPanel, el, field, message, setError, setNote } from "./dom.js";

// Состояния уведомлений и инструкция установки — docs/ui.md, «Настройки».
const NOTIFICATIONS = {
  on: "включены",
  off: "выключены",
  denied: "запрещены в браузере",
};

const INSTALL_HINT = "поделиться → на экран «домой»";

export function renderSettings(root, ctx) {
  root.append(head(ctx));
  const body = el("div", "body settings");
  body.append(identity(ctx), notificationsBlock(ctx));
  const install = installBlock();
  if (install !== null) {
    body.append(install);
  }
  body.append(passwordBlock(ctx), exitBlock(ctx), deleteBlock(ctx));
  root.append(body);
}

function head(ctx) {
  const bar = el("div", "head");
  const back = button("назад", "back");
  back.addEventListener("click", () => ctx.go("#/"));
  bar.append(back, el("span", "title", "настройки"));
  return bar;
}

// identity — «ты: @nick» и свой отпечаток группами по 4 в две строки.
function identity(ctx) {
  const box = el("section", "block block--first");
  box.append(el("p", "self", `ты: @${ctx.me.nick}`));
  const groups = fingerprintGroups(ctx.me.fingerprint ?? "");
  box.append(el("p", "fp", groups.slice(0, 8).join(" ")));
  box.append(el("p", "fp", groups.slice(8).join(" ")));
  return box;
}

// notificationsBlock — «уведомления»: состояние и одна кнопка
// (docs/ui.md, «Настройки»). Состояний три; «запрещены в браузере» —
// это и отклонённое разрешение, и браузер без уведомлений, и сервер без
// VAPID-ключа: включать нечем, кнопки нет (ADR-046).
function notificationsBlock(ctx) {
  const box = block("уведомления");
  const status = el("p", "state", "");
  // На iOS вне установленного приложения кнопки нет: там пуши работают
  // только у приложения на экране «Домой» (ADR-011).
  const ios = pwa.iosBrowser();
  const action = button("включить");
  action.hidden = true;
  const note = message();
  if (ios) {
    box.append(status, el("p", "install", INSTALL_IOS), note);
  } else {
    box.append(status, action, note);
  }

  let mode = "denied";
  const paint = async () => {
    if (ios) {
      status.textContent = NOTIFICATIONS.off;
      return;
    }
    mode = await pwa.notifications(await vapidKey(ctx));
    status.textContent = NOTIFICATIONS[mode];
    action.textContent = mode === "on" ? "выключить" : "включить";
    action.hidden = mode === "denied";
  };

  action.addEventListener("click", async () => {
    if (action.disabled) {
      return;
    }
    action.disabled = true;
    setNote(note, "");
    try {
      if (mode === "on") {
        await pwa.disable();
      } else {
        await pwa.enable(await vapidKey(ctx));
      }
    } catch (err) {
      setError(note, ctx.errorText(err));
    } finally {
      action.disabled = false;
      await paint();
    }
  });

  paint();
  return box;
}

// vapidKey — публичный ключ сервера для подписки (docs/protocol.md,
// «Публичные»). Конфигурации нет — подписаться нечем.
async function vapidKey(ctx) {
  try {
    return (await ctx.ensureConfig()).vapidPublicKey ?? "";
  } catch {
    return "";
  }
}

// installBlock — «установить приложение» (docs/ui.md, «Настройки»).
// Кнопка есть, если поймано beforeinstallprompt; на iOS вместо неё
// инструкция. Устанавливать нечего — раздела нет.
function installBlock() {
  const ios = pwa.iosBrowser();
  if (!ios && !pwa.installable()) {
    return null;
  }
  const box = block("установить приложение");
  if (ios) {
    box.append(el("p", "install", INSTALL_HINT));
    return box;
  }
  const action = button("установить");
  action.addEventListener("click", () => {
    // Приглашение одноразовое: показали — устанавливать этим разделом
    // больше нечего, и раздела нет (docs/ui.md, «Настройки»). Раздел
    // уходит сразу: дальше человек отвечает браузеру, а не нам, и ждать
    // его ответа кнопке незачем.
    pwa.install();
    box.remove();
  });
  box.append(action);
  return box;
}

function passwordBlock(ctx) {
  const box = block("сменить пароль");
  const form = el("form", "form");
  form.noValidate = true;

  const current = field("старый", { type: "password", autocomplete: "current-password" });
  const next = field("новый", { type: "password", autocomplete: "new-password" });
  const again = field("повтор", { type: "password", autocomplete: "new-password" });
  form.append(current.wrap, next.wrap, again.wrap);

  const check = el("label", "check");
  const others = el("input");
  others.type = "checkbox";
  // Смена пароля по желанию завершает остальные сессии (ADR-015);
  // снять галочку можно, но это осознанный выбор, а не умолчание.
  others.checked = true;
  check.append(others, el("span", null, "выйти на других устройствах"));
  form.append(check);

  const submit = el("button", "button", "сменить пароль");
  submit.type = "submit";
  const note = message();
  form.append(submit, note);

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (submit.disabled) {
      return;
    }
    setNote(note, "");
    if (next.input.value !== again.input.value) {
      setError(note, "пароли не совпадают");
      return;
    }
    if ([...next.input.value].length < ctx.minPassword) {
      setError(note, `пароль: не короче ${ctx.minPassword} символов`);
      return;
    }
    submit.disabled = true;
    submit.textContent = "вычисляем ключ…";
    try {
      await ctx.changePassword(current.input.value, next.input.value, others.checked);
      for (const input of [current.input, next.input, again.input]) {
        input.value = "";
      }
      setNote(note, "пароль изменён");
    } catch (err) {
      setError(note, passwordError(ctx, err));
    } finally {
      submit.disabled = false;
      submit.textContent = "сменить пароль";
    }
  });

  box.append(form);
  return box;
}

function exitBlock(ctx) {
  const box = block("выйти");
  const start = button("выйти");
  // Кнопки «экспортировать» пока нет: экспорт — этап 5 (docs/plan.md).
  const panel = confirmPanel("история на этом устройстве будет удалена. экспортировать сначала?", "выйти");

  start.addEventListener("click", () => {
    start.hidden = true;
    panel.root.hidden = false;
    panel.yes.focus();
  });
  panel.no.addEventListener("click", () => {
    panel.root.hidden = true;
    start.hidden = false;
    start.focus();
  });
  panel.yes.addEventListener("click", async () => {
    panel.yes.disabled = true;
    panel.no.disabled = true;
    await ctx.signOut();
    ctx.go("#/");
  });

  box.append(start, panel.root);
  return box;
}

function deleteBlock(ctx) {
  const box = block("удалить аккаунт");
  const form = el("form", "form");
  form.noValidate = true;

  const password = field("пароль", { type: "password", autocomplete: "current-password" });
  const submit = el("button", "button", "удалить аккаунт");
  submit.type = "submit";
  const panel = confirmPanel("аккаунт и вся история будут удалены навсегда.", "удалить");
  const note = message();
  form.append(password.wrap, submit, panel.root, note);

  const back = () => {
    panel.root.hidden = true;
    panel.yes.disabled = false;
    panel.no.disabled = false;
    panel.yes.textContent = "удалить";
    submit.hidden = false;
  };

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    setNote(note, "");
    submit.hidden = true;
    panel.root.hidden = false;
    panel.yes.focus();
  });
  panel.no.addEventListener("click", () => {
    back();
    submit.focus();
  });
  panel.yes.addEventListener("click", async () => {
    panel.yes.disabled = true;
    panel.no.disabled = true;
    panel.yes.textContent = "вычисляем ключ…";
    try {
      await ctx.deleteAccount(password.input.value);
      ctx.go("#/");
    } catch (err) {
      back();
      setError(note, passwordError(ctx, err));
    }
  });

  box.append(form);
  return box;
}

function block(title) {
  const box = el("section", "block");
  box.append(el("h2", "section", title));
  return box;
}

// passwordError: в настройках 401 означает ровно одно — не тот пароль.
function passwordError(ctx, err) {
  if (err instanceof ApiError && err.code === "invalid_credentials") {
    return "неверный пароль";
  }
  return ctx.errorText(err);
}
