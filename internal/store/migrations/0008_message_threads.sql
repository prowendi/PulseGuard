-- ===== migrations/0008_message_threads.sql =====
-- V7-2: persistent map from (channel, fingerprint) → live Telegram
-- message so the push pipeline can collapse repeat alerts into the
-- existing chat message via editMessageText instead of spamming the
-- subscriber with N copies.
--
-- The (channel_id, fingerprint) UNIQUE makes Upsert race-safe and
-- guarantees there is exactly one tracked message per logical alert
-- per channel. tg_message_id is the Telegram-side primary key used by
-- editMessageText.
--
-- ON DELETE CASCADE chains the row away when the operator removes
-- the channel so an orphan thread cannot resurrect an editMessageText
-- against a chat the channel no longer owns.
CREATE TABLE message_threads (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id    INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  tenant_id     INTEGER NOT NULL,
  fingerprint   TEXT    NOT NULL,
  chat_id       TEXT    NOT NULL,
  tg_message_id INTEGER NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE(channel_id, fingerprint)
);
CREATE INDEX idx_message_threads_tenant ON message_threads(tenant_id);
CREATE INDEX idx_message_threads_fp ON message_threads(channel_id, fingerprint);
