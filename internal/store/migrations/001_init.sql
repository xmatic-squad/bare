-- Полная схема v1 (docs/storage.md, ADR-020). Время — миллисекунды Unix.
-- Таблицы этапов 2–3 создаются сразу: схема одна, миграция одна.

CREATE TABLE users (
  nick        TEXT PRIMARY KEY,
  auth_hash   BLOB NOT NULL,            -- argon2id(authKey), 32 байта
  auth_salt   BLOB NOT NULL,            -- 16 байт
  auth_params TEXT NOT NULL,            -- "argon2id,m=19456,t=2,p=1"
  public_key  TEXT NOT NULL,            -- JWK, JSON
  key_blob    TEXT NOT NULL,            -- непрозрачный JSON клиента
  created_at  INTEGER NOT NULL
);

CREATE TABLE devices (
  id                TEXT PRIMARY KEY,   -- base64url 16 байт, выдаёт клиент
  nick              TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  created_at        INTEGER NOT NULL,
  last_seen         INTEGER NOT NULL,
  push_subscription TEXT,               -- JSON PushSubscription или NULL
  push_pending      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX devices_nick ON devices(nick);

CREATE TABLE sessions (
  token_hash  BLOB PRIMARY KEY,         -- SHA-256(токен)
  nick        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  device_id   TEXT REFERENCES devices(id) ON DELETE CASCADE,   -- NULL до POST /api/devices
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);
CREATE INDEX sessions_nick ON sessions(nick);

CREATE TABLE contacts (
  nick        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  peer        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (nick, peer)
);

CREATE TABLE rooms (
  id          TEXT PRIMARY KEY,         -- base64url 16 байт, выдаёт клиент (ADR-037)
  name        TEXT NOT NULL,
  owner       TEXT NOT NULL REFERENCES users(nick),
  created_at  INTEGER NOT NULL
);

CREATE TABLE room_members (
  room_id     TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  nick        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  joined_at   INTEGER NOT NULL,
  PRIMARY KEY (room_id, nick)
);
CREATE INDEX room_members_nick ON room_members(nick);

CREATE TABLE room_keys (
  room_id     TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  nick        TEXT NOT NULL REFERENCES users(nick) ON DELETE CASCADE,
  key_id      TEXT NOT NULL,
  sender      TEXT NOT NULL,            -- кто завернул
  iv          TEXT NOT NULL,
  ct          TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (room_id, nick, key_id)
);

CREATE TABLE queue (
  device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  msg_id      TEXT NOT NULL,
  envelope    TEXT NOT NULL,            -- готовый JSON Envelope
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (device_id, msg_id)
);
CREATE INDEX queue_created ON queue(created_at);
