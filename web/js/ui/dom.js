// Сборка узлов. innerHTML не используется нигде: сообщения и ники —
// пользовательские данные (docs/ui.md, CSP из ADR-021).

const SVG = "http://www.w3.org/2000/svg";

// DESKTOP — порог десктопа: сайдбар и чат рядом, один экран за раз кончается
// (docs/ui.md, «Каркас»). Экраны спрашивают ширину в момент события, а не
// перерисовываются на каждое изменение размера.
export const DESKTOP = "(min-width: 760px)";

export function wide() {
  return matchMedia(DESKTOP).matches;
}

export function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) {
    node.className = className;
  }
  if (text !== undefined) {
    node.textContent = text;
  }
  return node;
}

export function clear(node) {
  while (node.firstChild) {
    node.removeChild(node.firstChild);
  }
}

// mark — знак «скобы»: четыре угла рамки (docs/identity/brief.md).
export function mark() {
  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("class", "mark");
  svg.setAttribute("viewBox", "0 0 64 64");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "7");
  svg.setAttribute("aria-hidden", "true");
  for (const d of ["M10 26V10h16", "M38 10h16v16", "M54 38v16H38", "M26 54H10V38"]) {
    const path = document.createElementNS(SVG, "path");
    path.setAttribute("d", d);
    svg.append(path);
  }
  return svg;
}

// field — подпись и строка ввода. Отдаёт и то, и другое: подпись идёт
// в форму, ввод нужен обработчику.
export function field(label, { type = "text", name, autocomplete } = {}) {
  const wrap = el("label", "field");
  wrap.append(el("span", null, label));
  const input = el("input");
  input.type = type;
  if (name) {
    input.name = name;
  }
  if (autocomplete) {
    input.autocomplete = autocomplete;
  }
  input.autocapitalize = "off";
  input.spellcheck = false;
  wrap.append(input);
  return { wrap, input };
}

export function button(text, className = "button") {
  const node = el("button", className, text);
  node.type = "button";
  return node;
}

// confirmPanel — вопрос и две кнопки; спрятан, пока не спросили.
// Вопрос отдаётся наружу: его текст бывает известен только к моменту показа.
export function confirmPanel(question, yesLabel) {
  const root = el("div");
  root.hidden = true;
  const text = el("p", "confirm", question);
  const yes = button(yesLabel);
  const no = button("отмена");
  const row = el("div", "row");
  row.append(yes, no);
  root.append(text, row);
  return { root, text, yes, no };
}

// message — строка состояния под формой: ошибка цветом mark, ответ — mute.
export function message() {
  const node = el("p", "message");
  node.setAttribute("aria-live", "polite");
  return node;
}

export function setError(node, text) {
  node.className = "message message--error";
  node.textContent = text;
}

export function setNote(node, text) {
  node.className = "message";
  node.textContent = text;
}
