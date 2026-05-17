-- ===== migrations/0010_bot_app_credentials.sql =====
-- Phase Lark-B: Lark application bots.
--
-- Phase A only supports Lark Custom Bot Webhook (push-only, no two-way
-- chat). Phase B adds the Lark application bot — OAuth2 tenant_access
-- _token + IM API send + event subscription with HMAC-SHA256 signature
-- verification — so operators can chat with the bot and trigger Starlark
-- commands the same way a Telegram bot already does.
--
-- Application bots need credentials that webhook bots do not:
--
--   * app_id       (public, identifies the application in Lark)
--   * app_secret   (secret, AES-GCM encrypted at rest using the
--                   master_key_b64 cipher already in use for bot_token)
--   * verify_token (Lark "Verification Token" from the developer
--                   console, surfaced in URL-verification events)
--   * encrypt_key  (Lark "Encrypt Key" used to verify the
--                   X-Lark-Signature header on inbound events)
--
-- We also add `bot_kind` to distinguish the two flavours on the same
-- bots row so the runtime sender_router + listener manager can branch
-- without a schema split. The existing column `platform` keeps its
-- meaning ("telegram" | "lark") — `bot_kind` further partitions the
-- "lark" rows into ("webhook" | "app"). All existing rows back-fill to
-- "webhook" so Phase A behaviour is preserved.
--
-- SQLite caveats:
--   * ALTER TABLE ADD COLUMN cannot append a CHECK to the new column,
--     so the kind allow-list is enforced at the application layer via
--     domain.IsValidBotKind / store/bot_repo validateBot. Same pattern
--     as migration 0002.
--   * app_secret_enc is BLOB and nullable: webhook rows leave it NULL.

ALTER TABLE bots ADD COLUMN bot_kind TEXT NOT NULL DEFAULT 'webhook'
  CHECK (bot_kind IN ('webhook','app'));
ALTER TABLE bots ADD COLUMN app_id TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN app_secret_enc BLOB;
ALTER TABLE bots ADD COLUMN verify_token TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN encrypt_key TEXT NOT NULL DEFAULT '';
