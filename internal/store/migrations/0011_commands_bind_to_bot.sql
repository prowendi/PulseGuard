-- ===== migrations/0011_commands_bind_to_bot.sql =====
-- Bind Starlark commands to a SPECIFIC bot instead of a whole tenant.
--
-- Rationale (operator feedback, 2026-05): tenant-global commands made
-- it impossible to give Bot A a /查询 that hits one database and Bot B
-- a /查询 that hits another. Per-bot ownership is the natural mental
-- model — every bot has its own command catalog, and listeners only
-- see commands that target *their* bot row.
--
-- Schema delta:
--   - new column `bot_id` (NOT NULL, FK bots.id ON DELETE CASCADE)
--   - drop old UNIQUE(tenant_id, name) ─ replaced by UNIQUE(bot_id, name)
--     so /查询 on Bot A and /查询 on Bot B can coexist
--   - tenant_id stays as a denormalised convenience for the UI list view
--     (also lets cascade-on-tenant-delete keep working)
--
-- SQLite cannot ALTER an existing UNIQUE constraint, so we rebuild the
-- table. Dev policy from the operator: "开发阶段不用考虑脏数据问题每次
-- 都清空重来" — we wipe existing rows rather than backfill. Subscribers
-- are wiped too because their command_id FKs would otherwise point at
-- a recreated id space that has no semantic continuity.
--
-- Foreign keys are enforced session-wide (PRAGMA foreign_keys=ON in
-- Open), but DROP TABLE itself bypasses the FK actions — that's why we
-- explicitly DELETE subscribers first.

DELETE FROM subscribers;
DROP TABLE commands;

CREATE TABLE commands (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id   INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  bot_id      INTEGER NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,
  description TEXT    NOT NULL DEFAULT '',
  code        TEXT    NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE(bot_id, name)
);
CREATE INDEX idx_commands_bot    ON commands(bot_id);
CREATE INDEX idx_commands_tenant ON commands(tenant_id);
