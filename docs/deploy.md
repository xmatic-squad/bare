# Деплой

Цель — `ssh xmatic` (Ubuntu 22.04, x86_64), домен `bare.xmatic.team`, A-запись на IP сервера. Решения — ADR-022.

## Сборка

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bare ./cmd/bare
```

Версия бинаря — `vcs.revision` из `debug.ReadBuildInfo()`, печатается по `bare version`. `/healthz` отвечает только `ok`.

## Первичная настройка сервера (один раз)

```sh
sudo useradd --system --home /var/lib/bare --shell /usr/sbin/nologin bare
sudo mkdir -p /opt/bare /var/lib/bare /etc/bare
sudo chown bare:bare /var/lib/bare
sudo chmod 0700 /var/lib/bare /etc/bare
```

Права закрыты намеренно (ADR-032): в базе лежат `argon2id(authKey)` и ключевые блобы, машина общая.

`/etc/bare/env` (владелец root, режим 0600):

```
BARE_ADDR=127.0.0.1:8411
BARE_DB=/var/lib/bare/bare.db
BARE_ORIGIN=https://bare.xmatic.team
BARE_VAPID_PUBLIC=<из bare vapid>
BARE_VAPID_PRIVATE=<из bare vapid>
BARE_VAPID_SUBJECT=mailto:admin@xmatic.team
BARE_INVITE_CODE=<пусто или код>
```

`bare vapid` печатает пару ключей; выполняется локально один раз, результат вписывается в файл.

`/etc/systemd/system/bare.service`:

```ini
[Unit]
Description=Bare chat
After=network-online.target
Wants=network-online.target

[Service]
User=bare
Group=bare
EnvironmentFile=/etc/bare/env
ExecStart=/opt/bare/bare serve
Restart=on-failure
RestartSec=2
StateDirectory=bare
StateDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/bare

[Install]
WantedBy=multi-user.target
```

`/etc/nginx/sites-available/bare.xmatic.team` (затем симлинк в `sites-enabled`):

```nginx
server {
    listen 80;
    listen [::]:80;
    server_name bare.xmatic.team;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name bare.xmatic.team;

    ssl_certificate     /etc/letsencrypt/live/bare.xmatic.team/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/bare.xmatic.team/privkey.pem;
    add_header Strict-Transport-Security "max-age=31536000" always;

    client_max_body_size 64k;

    location /api/events {
        proxy_pass http://127.0.0.1:8411;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
        gzip off;
        proxy_read_timeout 1h;
    }

    location / {
        proxy_pass http://127.0.0.1:8411;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header Connection "";
    }
}
```

Сертификат: сначала временный конфиг только с блоком `:80` (без `return`, с `root` для ACME) или `certbot --nginx -d bare.xmatic.team` — на машине certbot уже обслуживает соседние сайты, использовать тот же способ, что у них (`ls /etc/letsencrypt/renewal/` показывает, какой плагин).

```sh
sudo nginx -t && sudo systemctl reload nginx
sudo systemctl daemon-reload && sudo systemctl enable --now bare
```

## Обновление — `scripts/deploy.sh`

```sh
#!/bin/sh
set -eu
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/bare ./cmd/bare
scp /tmp/bare xmatic:/tmp/bare
ssh xmatic 'sudo install -m 0755 -o root -g root /tmp/bare /opt/bare/bare && sudo systemctl restart bare && sleep 1 && curl -fsS http://127.0.0.1:8411/healthz'
```

Сверка подлинности: `sha256sum /opt/bare/bare` на сервере равен хешу сборки из тега на той же версии Go с теми же флагами.

## Проверка после деплоя

- `curl -I https://bare.xmatic.team/` — 200, заголовки CSP и nosniff.
- `curl -N https://bare.xmatic.team/api/events` — 401 (без cookie), без буферизации.
- `journalctl -u bare -f` — старт, применённые миграции, нет ошибок.

## Бэкап

`sqlite3 /var/lib/bare/bare.db "VACUUM INTO '/var/lib/bare/backup.db'"` или копия файла при остановленном сервисе. В базе только шифротексты и метаданные — бэкап не содержит переписки. Копия наследует режим 0600 (ADR-032); при восстановлении в другое место права надо выставить руками.

## Логи

Сервер пишет в stdout: время, метод, путь, статус, длительность; для маршрутов `/api/` вместо пути пишется шаблон (`/api/users/{nick}`), чтобы ник не попадал в журнал, а если отказ случился до маршрутизации (`Origin`, предел тела) и шаблона ещё нет — просто `/api/`; ник — только для ошибок аутентификации по лимитам; IP не пишется. Причины ответов `500 internal` (ADR-027) пишутся отдельной строкой, без данных запроса. journald хранит по своим правилам.
