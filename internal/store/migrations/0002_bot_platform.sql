-- ===== migrations/0002_bot_platform.sql =====
-- Add `platform` to bots so PulseGuard can later host non-Telegram bot
-- listeners (Discord, Slack, WeChat, ...) without another schema change.
--
-- SQLite limitations:
--   * ALTER TABLE ADD COLUMN cannot append a CHECK constraint, so the
--     allow-list ("telegram" only for now) is enforced at the application
--     layer (see domain.IsValidPlatform / store/bot_repo validateBot).
--   * Existing rows receive 'telegram' via the NOT NULL DEFAULT below
--     (back-fills in place, no rewrite required).

ALTER TABLE bots ADD COLUMN platform TEXT NOT NULL DEFAULT 'telegram';
