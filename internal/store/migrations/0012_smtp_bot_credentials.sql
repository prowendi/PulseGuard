-- ===== migrations/0012_smtp_bot_credentials.sql =====
-- Phase SMTP: email-via-SMTP push platform.
--
-- A SMTP bot stores the credentials needed to authenticate against a
-- mail relay (Gmail SMTP, SendGrid, self-hosted Postfix, ...) and
-- the From: address used in outbound messages. Like Lark Webhook,
-- this is a one-way push platform — no listener, no commands.
--
-- Channel.chat_id is repurposed as a comma-separated recipient list
-- (To: header) when the bound bot has platform="smtp"; the existing
-- string type fits without a schema change.
--
-- Columns:
--   smtp_host         — relay hostname (e.g. "smtp.gmail.com")
--   smtp_port         — TCP port (587 STARTTLS / 465 implicit TLS / 25 plain)
--   smtp_username     — login username (usually the from-address)
--   smtp_password_enc — AES-GCM ciphertext of the password, encrypted
--                       with the same master_key_b64 cipher used for
--                       bot_token / app_secret_enc / encrypt_key
--   smtp_from         — From: header; if empty, falls back to smtp_username
--   smtp_use_tls      — 1 = use TLS (STARTTLS on 587 or implicit on 465),
--                       0 = plaintext (only for testing localhost relays)
--
-- All columns are added with safe defaults so existing telegram/lark
-- rows keep working — the empty string / NULL on those columns is
-- ignored unless platform="smtp".
--
-- SQLite caveat: ALTER TABLE ADD COLUMN cannot attach a CHECK to the
-- new columns, so the smtp_port range (1-65535) and smtp_use_tls
-- (0/1) are enforced at the application layer in validateBot.

ALTER TABLE bots ADD COLUMN smtp_host         TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN smtp_port         INTEGER NOT NULL DEFAULT 587;
ALTER TABLE bots ADD COLUMN smtp_username     TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN smtp_password_enc BLOB;
ALTER TABLE bots ADD COLUMN smtp_from         TEXT NOT NULL DEFAULT '';
ALTER TABLE bots ADD COLUMN smtp_use_tls      INTEGER NOT NULL DEFAULT 1;
