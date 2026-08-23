# ADR-022: Деплой — nginx, systemd, кросс-сборка

Уточнён [ADR-032](032-state-permissions.md) (`StateDirectoryMode` и `UMask` в юните), [ADR-056](056-nginx-access-log-off.md) (`access_log off`) и [ADR-057](057-version-marks-dirty-tree.md) (`bare version` помечает сборку из изменённого дерева).

## Контекст

Целевой сервер (`ssh xmatic`, Ubuntu 22.04) уже держит nginx на 80/443 с десятком сайтов и certbot. Go на сервере нет. HTTPS обязателен (ADR-002), но TLS в самом бинаре означал бы либо `autocert` — четвёртую зависимость, — либо конфликт за 443 с nginx.

## Решение

- TLS терминирует nginx. Bare слушает `127.0.0.1:8411` (порт свободен; 8090 занят PocketBase). Сертификат — certbot для `bare.xmatic.team`, как у остальных сайтов на машине.
- nginx проксирует всё на бинарь; для `/api/events` — `proxy_buffering off`, `proxy_read_timeout 1h`, HTTP/1.1 к апстриму. Сервер дополнительно шлёт `X-Accel-Buffering: no`. Конфиг — `docs/deploy.md`.
- Бинарь под systemd: пользователь `bare`, `/opt/bare/bare`, база в `/var/lib/bare/bare.db`, секреты в `/etc/bare/env` (режим 0600). Юнит с `ProtectSystem=strict`, `ProtectHome=yes`, `NoNewPrivileges=yes`.
- Конфигурация — переменные окружения с префиксом `BARE_`: `ADDR`, `DB`, `ORIGIN`, `VAPID_PUBLIC`, `VAPID_PRIVATE`, `VAPID_SUBJECT`, `INVITE_CODE`. Подкоманда `bare vapid` генерирует пару ключей. Подкоманда `bare serve` запускает сервер.
- Клиентская статика встроена в бинарь через `embed`: артефакт деплоя — ровно один файл, и обещание ADR-001 «код в продакшене байт в байт совпадает с репозиторием» проверяется сравнением с тегом.
- Сборка локально: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build`. Деплой — `scripts/deploy.sh`: сборка, `scp`, `install`, `systemctl restart`. Без контейнеров.

## Следствия

- Проверка подлинности клиента сводится к проверке бинаря: хеш файла на сервере против сборки из тега.
- Зависимость от чужого nginx на той же машине — осознанная: он уже там и уже умеет сертификаты.
- Статику отдаёт Go, не nginx: заголовки безопасности и ETag в одном месте.
