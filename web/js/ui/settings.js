// Настройки — docs/ui.md, «Настройки»: кто ты, уведомления, установка
// приложения, устройства, история, смена пароля, выход, удаление аккаунта.

import { ApiError } from "../api.js";
import { fingerprintGroups } from "../crypto.js";
import { ARCHIVE_EXT, ArchiveError, exportHistory, importHistory } from "../export.js";
import * as pwa from "../pwa.js";
import {
  EXPORT_FAILED,
  INSTALL_IOS,
  button,
  clear,
  confirmPanel,
  el,
  field,
  message,
  setError,
  setNote,
} from "./dom.js";

// Состояния уведомлений и инструкция установки — docs/ui.md, «Настройки».
const NOTIFICATIONS = {
  on: "включены",
  off: "выключены",
  denied: "запрещены в браузере",
};

const INSTALL_HINT = "поделиться → на экран «домой»";

// Сколько символов идентификатора устройства видно в списке (docs/ui.md,
// «Настройки»). Восьми хватает, чтобы отличить одно устройство от другого:
// идентификатор случайный.
const ID_SHOWN = 8;

// Дата появления устройства — местная, цифрами: она стоит в строке рядом
// с идентификатором, и длинная форма её бы утопила (ADR-052).
const DEVICE_DAY = new Intl.DateTimeFormat("ru-RU", {
  day: "2-digit",
  month: "2-digit",
  year: "numeric",
});

// МБ занятого места — 1024×1024 байта (ADR-052).
const MB = 1024 * 1024;

// Импорт не дошёл до базы: места на устройстве нет или ключей аккаунта
// на нём нет (docs/ui.md, «Тексты состояний»). Порча самого файла говорит
// о себе своими словами.
const IMPORT_FAILED = "импорт не удался";

export function renderSettings(root, ctx) {
  root.append(head(ctx));
  const body = el("div", "body settings settings--account");
  const general = el("div", "settings__column");
  general.append(notificationsBlock(ctx));
  const install = installBlock();
  if (install !== null) {
    general.append(install);
  }
  general.append(devicesBlock(ctx), historyBlock());

  const security = el("div", "settings__column");
  security.append(passwordBlock(ctx), exitBlock(ctx), deleteBlock(ctx));

  const grid = el("div", "settings__grid");
  grid.append(general, security);
  body.append(identity(ctx), grid);
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

// devicesBlock — «устройства»: список идентификаторов, дата появления
// и «удалить» (docs/ui.md, «Настройки»).
//
// У своей строки кнопки нет: там пометка «это устройство». Удаление уносит
// сессии устройства, и на своём это оставило бы человека на экране входа
// с целой историей и без объяснения; отцепляет своё устройство «выйти»
// (ADR-052).
function devicesBlock(ctx) {
  const box = block("устройства");
  const list = el("ul", "devices");
  const note = message();
  box.append(list, note);

  const row = (item) => {
    const line = el("li", "device");
    line.append(el("span", "device__id", item.id.slice(0, ID_SHOWN)));
    if (Number.isFinite(item.createdAt)) {
      line.append(el("span", "tag", DEVICE_DAY.format(item.createdAt)));
    }
    if (item.current) {
      line.append(el("span", "tag", "это устройство"));
      return line;
    }
    const drop = el("button", "link", "удалить");
    drop.type = "button";
    drop.addEventListener("click", async () => {
      drop.disabled = true;
      setNote(note, "");
      try {
        await ctx.removeDevice(item.id);
        await paint();
      } catch (err) {
        setError(note, ctx.errorText(err));
        drop.disabled = false;
      }
    });
    line.append(drop);
    return line;
  };

  // paint перечитывает список: он же и есть ответ на удаление.
  async function paint() {
    let items;
    try {
      items = await ctx.devices();
    } catch (err) {
      setError(note, ctx.errorText(err));
      return;
    }
    setNote(note, "");
    clear(list);
    for (const item of items) {
      list.append(row(item));
    }
  }

  paint();
  return box;
}

// historyBlock — «история»: занятое место, экспорт и импорт архива
// (docs/ui.md, «Настройки»).
function historyBlock() {
  const box = block("история");
  // Место занято не только сообщениями: считает его браузер, а не мы
  // (ADR-009). Браузер, который считать не умеет, строки не получает —
  // писать в неё нечего (ADR-052).
  const used = el("p", "state", "");
  used.hidden = true;
  // Строка перечитывается и после импорта: «добавлено N сообщений» рядом
  // с прежней цифрой — две строки об одном действии, говорящие разное.
  const showSpace = async () => {
    const bytes = await space();
    if (bytes === null) {
      return;
    }
    used.textContent = `занято ${megabytes(bytes)} МБ`;
    used.hidden = false;
  };
  showSpace();
  const out = button("экспорт");
  const take = button("импорт");
  // Выбор файла — обычный <input type="file">, спрятанный за кнопкой:
  // системный вид ему тут не к месту, а поведение нужно родное.
  const picker = el("input");
  picker.type = "file";
  picker.accept = ARCHIVE_EXT;
  picker.hidden = true;
  const note = message();
  box.append(used, out, take, picker, note);

  out.addEventListener("click", () => runExport(out, note));
  take.addEventListener("click", () => {
    if (take.disabled) {
      return;
    }
    setNote(note, "");
    picker.click();
  });
  picker.addEventListener("change", async () => {
    const file = picker.files?.[0] ?? null;
    // Тот же файл, выбранный второй раз подряд, обязан считаться выбором:
    // без сброса change не приходит.
    picker.value = "";
    if (file === null) {
      return;
    }
    take.disabled = true;
    try {
      const added = await importHistory(file);
      setNote(note, `добавлено ${plural(added)}`);
      await showSpace();
    } catch (err) {
      setError(note, err instanceof ArchiveError ? err.message : IMPORT_FAILED);
    } finally {
      take.disabled = false;
    }
  });
  return box;
}

// runExport — «экспорт» в разделе истории и «экспортировать» в подтверждении
// выхода: действие одно, кнопки две. Удача говорит сама за себя — браузер
// сохраняет файл, писать об этом нечего.
async function runExport(action, note) {
  if (action.disabled) {
    return;
  }
  action.disabled = true;
  setNote(note, "");
  try {
    await exportHistory();
  } catch {
    setError(note, EXPORT_FAILED);
  } finally {
    action.disabled = false;
  }
}

// space — занятое место в байтах или null, если браузер его не считает
// (docs/storage.md, ADR-009). Это оценка происхождения целиком: индексы
// и служебные страницы IndexedDB тоже занимают место.
async function space() {
  if (!navigator.storage?.estimate) {
    return null;
  }
  try {
    const { usage } = await navigator.storage.estimate();
    return Number.isFinite(usage) ? usage : null;
  } catch {
    return null;
  }
}

// megabytes — «занято N МБ» человеческим числом: до десятых, пока меньше
// десяти, дальше целые (ADR-052). Десятые округляются вверх: пара сотен
// килобайт — это не «0 МБ».
function megabytes(bytes) {
  const value = bytes / MB;
  if (value >= 10) {
    return String(Math.round(value));
  }
  return (Math.ceil(value * 10) / 10).toLocaleString("ru-RU");
}

// plural склоняет «сообщение» с числом: «добавлено 1 сообщение»,
// «добавлено 2 сообщения», «добавлено 5 сообщений» (docs/ui.md).
function plural(count) {
  const tail = count % 100;
  const last = count % 10;
  if (tail < 11 || tail > 14) {
    if (last === 1) {
      return `${count} сообщение`;
    }
    if (last >= 2 && last <= 4) {
      return `${count} сообщения`;
    }
  }
  return `${count} сообщений`;
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
  // История на этом устройстве — единственная копия, поэтому подтверждение
  // предлагает сначала сохранить её (docs/ui.md, ADR-029).
  const panel = confirmPanel(
    "история на этом устройстве будет удалена. экспортировать сначала?",
    "выйти",
    "экспортировать",
  );
  const note = message();

  start.addEventListener("click", () => {
    start.hidden = true;
    panel.root.hidden = false;
    panel.extra.focus();
  });
  // Экспорт подтверждение не закрывает: человек скачивает архив и решает
  // дальше сам.
  panel.extra.addEventListener("click", () => runExport(panel.extra, note));
  panel.no.addEventListener("click", () => {
    panel.root.hidden = true;
    setNote(note, "");
    start.hidden = false;
    start.focus();
  });
  panel.yes.addEventListener("click", async () => {
    panel.yes.disabled = true;
    panel.no.disabled = true;
    panel.extra.disabled = true;
    await ctx.signOut();
    ctx.go("#/");
  });

  box.append(start, panel.root, note);
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
