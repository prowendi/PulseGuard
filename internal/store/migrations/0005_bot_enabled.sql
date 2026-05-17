-- ===== migrations/0005_bot_enabled.sql =====
-- Phase B1: per-bot enable/disable flag.
--
-- Operators need a way to pause a bot's long-poll loop (and outbound
-- delivery via that bot) without destroying its row — the canonical
-- example is "token revoked upstream → 401 storm → auto-pause until I
-- rotate". The runtime, listener manager, and bot CRUD API all branch
-- on this column.
--
-- SQLite caveat: ALTER TABLE ADD COLUMN cannot append a CHECK so the
-- value is treated as a plain 0/1 INTEGER at the application layer
-- (domain.Bot.Enabled bool ↔ store layer mapping in bot_repo.go).
-- Existing rows back-fill to 1 (enabled) so an in-place upgrade does
-- not silently mute any working bot.

ALTER TABLE bots ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;
