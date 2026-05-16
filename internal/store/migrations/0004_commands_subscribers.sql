-- ===== migrations/0004_commands_subscribers.sql =====
-- Phase D2: per-tenant custom commands + chat-level subscribers.
--
-- commands holds a Starlark script bound to a tenant, callable from
-- any of that tenant's bots via /<name>. The (tenant_id, name) pair
-- is unique so /查询 cannot collide within one tenant; cross-tenant
-- duplicates are fine.
--
-- subscribers records every (command, bot, chat_id) tuple we have
-- ever dispatched. Used by the UI to surface "who is subscribed to
-- which command" and to let operators ack/forget rogue chats. The
-- (command_id, chat_id, platform) UNIQUE forces Upsert to update
-- last_seen_at on the same row instead of inserting duplicates.

CREATE TABLE commands (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id   INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,
  description TEXT    NOT NULL DEFAULT '',
  code        TEXT    NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE(tenant_id, name)
);
CREATE INDEX idx_commands_tenant ON commands(tenant_id);

CREATE TABLE subscribers (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id    INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  command_id   INTEGER NOT NULL REFERENCES commands(id) ON DELETE CASCADE,
  bot_id       INTEGER NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
  chat_id      TEXT    NOT NULL,
  platform     TEXT    NOT NULL DEFAULT 'telegram',
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  UNIQUE(command_id, chat_id, platform)
);
CREATE INDEX idx_subscribers_command ON subscribers(command_id);
CREATE INDEX idx_subscribers_chat ON subscribers(bot_id, chat_id);
