// Запрет масштабирования — страховка к строке viewport (ADR-075).
//
// Основное средство — `maximum-scale=1, user-scalable=no` в index.html
// и `touch-action: manipulation` в app.css. Safari вправе не послушаться:
// он и раньше игнорировал `user-scalable=no` целиком. Здесь то, что
// работает независимо от его настроения, — и ничего больше: прокрутка,
// свайпы и обычные нажатия проходят как проходили.

// Щипок в Safari — это gesturestart/gesturechange/gestureend. В других
// браузерах их не бывает вовсе, и подписка ничего не стоит.
const GESTURES = ["gesturestart", "gesturechange", "gestureend"];

// Два тапа подряд ближе этого срока и этого расстояния друг к другу —
// двойной тап, то есть масштабирование. Пороги браузерные: свои цифры
// здесь всё равно ничем не проверить.
const TAP_APART = 350;
const TAP_NEAR = 40;

// Прошлый тап. До первого касания его не было вовсе: иначе тап в углу
// в первые триста миллисекунд жизни страницы сошёл бы за второй.
const last = { at: -Infinity, x: 0, y: 0 };

// lock вешает страховку. Слушатели непассивные: пассивному
// preventDefault не даёт ничего.
export function lock() {
  for (const name of GESTURES) {
    document.addEventListener(name, stop, { passive: false });
  }
  document.addEventListener("touchend", tap, { passive: false });
}

function stop(event) {
  event.preventDefault();
}

// tap гасит второй тап подряд — он и масштабирует. Первый доходит
// до страницы целиком, поэтому нажатия, свайпы и прокрутка не меняются.
function tap(event) {
  // Второй палец на экране — это щипок или прокрутка двумя, не тап.
  if (event.touches.length > 0 || event.changedTouches.length !== 1) {
    last.at = -Infinity;
    return;
  }
  const touch = event.changedTouches[0];
  const quick = event.timeStamp - last.at <= TAP_APART;
  const near = Math.abs(touch.clientX - last.x) <= TAP_NEAR
    && Math.abs(touch.clientY - last.y) <= TAP_NEAR;
  last.at = event.timeStamp;
  last.x = touch.clientX;
  last.y = touch.clientY;
  if (!quick || !near || control(event.target)) {
    return;
  }
  event.preventDefault();
}

// control — элемент управления. Второй тап по нему не гасится:
// preventDefault на touchend уносит с собой click, а нажать кнопку
// дважды подряд — обычное дело. Цена мала: до элементов управления зум
// по двойному тапу не доходит и без страховки — `touch-action:
// manipulation` стоит на документе.
function control(target) {
  return typeof target?.closest === "function"
    && target.closest("button, input, textarea, select, label, a") !== null;
}
