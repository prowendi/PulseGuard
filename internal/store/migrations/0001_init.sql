-- ===== migrations/0001_init.sql =====
-- PulseGuard initial schema.
-- PRAGMA statements are applied via Open(); this file is data DDL only.

-- ─── tenants / invite codes / sessions ─────────────────
CREATE TABLE tenants (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  email           TEXT    NOT NULL UNIQUE,
  password_hash   TEXT    NOT NULL,
  display_name    TEXT    NOT NULL DEFAULT '',
  role            TEXT    NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin')),
  status          TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE invite_codes (
  code            TEXT    PRIMARY KEY,
  created_by      INTEGER NOT NULL REFERENCES tenants(id),
  used_by         INTEGER REFERENCES tenants(id),
  expires_at      INTEGER,
  used_at         INTEGER,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_invite_unused ON invite_codes(used_at) WHERE used_at IS NULL;

CREATE TABLE sessions (
  id              TEXT    PRIMARY KEY,
  tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  expires_at      INTEGER NOT NULL,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_sessions_tenant ON sessions(tenant_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- ─── resources: bots / templates / channels ────────────
CREATE TABLE bots (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  bot_token_enc   BLOB    NOT NULL,
  description     TEXT    NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (tenant_id, name)
);

CREATE TABLE templates (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  parse_mode      TEXT    NOT NULL DEFAULT 'MarkdownV2'
                  CHECK (parse_mode IN ('MarkdownV2','HTML','None')),
  body            TEXT    NOT NULL,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (tenant_id, name)
);

CREATE TABLE channels (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  push_token      TEXT    NOT NULL UNIQUE,
  bot_id          INTEGER NOT NULL REFERENCES bots(id) ON DELETE RESTRICT,
  template_id     INTEGER NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
  chat_id         TEXT    NOT NULL,
  rate_per_min    INTEGER NOT NULL DEFAULT 60,
  dedup_window_s  INTEGER NOT NULL DEFAULT 0,
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (tenant_id, name)
);
CREATE INDEX idx_channels_tenant ON channels(tenant_id);

-- ─── push pipeline: outbox / logs / DLQ ────────────────
CREATE TABLE push_outbox (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id      INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  tenant_id       INTEGER NOT NULL,
  payload_json    TEXT    NOT NULL,
  dedup_key       TEXT,
  status          TEXT    NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','in_flight','sent','retry','dead')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  last_error      TEXT,
  worker_id       TEXT,
  claimed_at      INTEGER,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_outbox_claim ON push_outbox(status, next_attempt_at)
  WHERE status IN ('pending','retry');

CREATE TABLE push_logs (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  outbox_id       INTEGER REFERENCES push_outbox(id) ON DELETE SET NULL,
  channel_id      INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  tenant_id       INTEGER NOT NULL,
  payload_json    TEXT    NOT NULL,
  rendered_text   TEXT    NOT NULL,
  tg_message_id   INTEGER,
  status          TEXT    NOT NULL CHECK (status IN ('sent','failed','dead')),
  error           TEXT,
  attempts        INTEGER NOT NULL,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_logs_channel_time ON push_logs(channel_id, created_at DESC);
CREATE INDEX idx_logs_tenant_time  ON push_logs(tenant_id, created_at DESC);

CREATE TABLE dead_letters (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  outbox_id       INTEGER NOT NULL,
  channel_id      INTEGER NOT NULL,
  tenant_id       INTEGER NOT NULL,
  payload_json    TEXT    NOT NULL,
  rendered_text   TEXT,
  last_error      TEXT    NOT NULL,
  attempts        INTEGER NOT NULL,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_dlq_tenant ON dead_letters(tenant_id, created_at DESC);

-- ─── governance: rate limit / dedup ────────────────────
CREATE TABLE rate_buckets (
  channel_id      INTEGER PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
  tokens          REAL    NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE dedup_keys (
  channel_id      INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  fingerprint     TEXT    NOT NULL,
  first_seen_at   INTEGER NOT NULL,
  last_seen_at    INTEGER NOT NULL,
  hit_count       INTEGER NOT NULL DEFAULT 1,
  expires_at      INTEGER NOT NULL,
  PRIMARY KEY (channel_id, fingerprint)
);
CREATE INDEX idx_dedup_expires ON dedup_keys(expires_at);
