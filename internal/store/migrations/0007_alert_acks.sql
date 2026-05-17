-- ===== migrations/0007_alert_acks.sql =====
-- V6-3: per-tenant alert acknowledgement records.
--
-- alert_acks stores "this fingerprint was acked by this user via the
-- /ack built-in" — written by the Telegram listener when an operator
-- types /ack <fp> in chat. The downstream push pipeline will (in a
-- future sprint) consult this table to skip already-acked alerts so
-- noisy storms collapse to a single notification + a manual close.
--
-- The (tenant_id, fingerprint) UNIQUE collapses duplicate acks: a
-- second /ack on the same fingerprint is a silent no-op at the repo
-- layer (the listener turns the duplicate insert error into a friendly
-- '已记录' reply rather than surfacing a constraint violation).

CREATE TABLE alert_acks (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id    INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  fingerprint  TEXT    NOT NULL,
  acked_by     TEXT    NOT NULL,
  acked_at     INTEGER NOT NULL,
  bot_id       INTEGER NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
  chat_id      TEXT    NOT NULL,
  UNIQUE(tenant_id, fingerprint)
);
CREATE INDEX idx_alert_acks_tenant ON alert_acks(tenant_id);
CREATE INDEX idx_alert_acks_fp ON alert_acks(tenant_id, fingerprint);
