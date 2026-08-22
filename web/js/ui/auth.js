// Экран входа и регистрации — docs/ui.md, «Вход и регистрация».

import { clear, confirmPanel, el, field, mark, message, setError, setNote } from "./dom.js";

const HINT = "пароль — это ключ шифрования, а не запись в базе. восстановления нет. "
  + "не короче 12 символов; лучше — фраза из нескольких слов.";

const MODES = [
  ["in", "вход"],
  ["up", "регистрация"],
];

export function renderAuth(root, ctx) {
  // Состояние экрана переживает перерисовку: режим, введённый ник и ошибка.
  const view = { mode: "in", nick: "", error: "" };
  const paint = () => {
    clear(root);
    root.append(screen(ctx, view, paint));
  };
  paint();
}

function screen(ctx, view, paint) {
  const main = el("main", "auth");
  const inner = el("div", "auth__inner");
  main.append(inner);

  const brand = el("div", "auth__brand");
  brand.append(mark(), el("span", null, "bare"));
  inner.append(brand);
  inner.append(tabs(view, paint));

  const form = el("form", "form");
  form.noValidate = true;

  const register = view.mode === "up";
  const nick = field("ник", { name: "nick", autocomplete: "username" });
  nick.input.value = view.nick;
  const password = field("пароль", {
    type: "password",
    name: "password",
    autocomplete: register ? "new-password" : "current-password",
  });
  form.append(nick.wrap, password.wrap);

  let invite = null;
  if (register) {
    form.append(el("p", "hint", HINT));
    if (ctx.config?.inviteRequired) {
      invite = field("инвайт-код", { name: "invite", autocomplete: "off" });
      form.append(invite.wrap);
    }
  }

  const label = register ? "регистрация" : "вход";
  const submit = el("button", "button", label);
  submit.type = "submit";
  form.append(submit);

  // На устройстве могут лежать ключи другого ника: вход под этим сотрёт
  // историю прежнего, поэтому сначала подтверждение (ADR-029).
  const wipe = confirmPanel("", "удалить");
  form.append(wipe.root);

  const note = message();
  form.append(note);
  if (view.error) {
    setError(note, view.error);
  }

  const fail = (text) => {
    view.error = text;
    if (text) {
      setError(note, text);
    } else {
      setNote(note, "");
    }
  };

  // run — то, ради чего экран: PBKDF2 и запрос к серверу. Вызывается либо
  // сразу, либо после подтверждения стирания.
  const run = async () => {
    submit.disabled = true;
    submit.textContent = "вычисляем ключ…";
    const pass = password.input.value;
    try {
      if (register) {
        await ctx.signUp(view.nick, pass, invite ? invite.input.value.trim() : "");
      } else {
        await ctx.signIn(view.nick, pass);
      }
      ctx.go("#/");
    } catch (err) {
      submit.disabled = false;
      submit.textContent = label;
      fail(ctx.errorText(err));
    }
  };

  const hideWipe = () => {
    wipe.root.hidden = true;
    submit.hidden = false;
  };
  wipe.no.addEventListener("click", () => {
    hideWipe();
    submit.focus();
  });
  wipe.yes.addEventListener("click", () => {
    hideWipe();
    run();
  });

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (submit.disabled) {
      return;
    }
    view.nick = nick.input.value.trim().toLowerCase();
    nick.input.value = view.nick;
    const pass = password.input.value;
    fail("");
    hideWipe();

    if (!ctx.validNick(view.nick)) {
      fail("ник: 2–32 символа, a–z, 0–9, _");
      nick.input.focus();
      return;
    }
    // Длина пароля проверяется в обоих режимах: короче минимума пароля
    // нет ни у одного аккаунта, считать по нему PBKDF2 незачем (docs/ui.md).
    if ([...pass].length < ctx.minPassword) {
      fail(`пароль: не короче ${ctx.minPassword} символов`);
      password.input.focus();
      return;
    }
    try {
      await ctx.ensureConfig();
    } catch (err) {
      fail(ctx.errorText(err));
      return;
    }
    // Поле инвайта могло не появиться: конфигурация приехала только что.
    if (register && ctx.config.inviteRequired && invite === null) {
      view.error = "нужен инвайт-код";
      paint();
      return;
    }

    const other = await ctx.storedNick();
    if (other && other !== view.nick) {
      wipe.text.textContent = `на этом устройстве история @${other}. вход под другим ником удалит её.`;
      wipe.root.hidden = false;
      submit.hidden = true;
      wipe.yes.focus();
      return;
    }
    await run();
  });

  inner.append(form);
  return main;
}

// tabs — переключатель «вход / регистрация».
function tabs(view, paint) {
  const wrap = el("div", "tabs");
  MODES.forEach(([mode, text], index) => {
    if (index > 0) {
      wrap.append(el("span", "tabs__sep", "/"));
    }
    const tab = el("button", "tab", text);
    tab.type = "button";
    const on = view.mode === mode;
    if (on) {
      tab.classList.add("is-on");
    }
    tab.setAttribute("aria-pressed", String(on));
    tab.addEventListener("click", () => {
      if (view.mode !== mode) {
        view.mode = mode;
        view.error = "";
        paint();
      }
    });
    wrap.append(tab);
  });
  return wrap;
}
