-- ===== migrations/0009_silences.sql =====
-- V7-3: time-windowed mute rules created via Telegram /silence so an
-- operator can suppress a known-flapping alert from a chat without
-- touching the channel or template config. Matching is prefix-based:
-- a silence row's `pattern` is treated as a prefix of the inbound
-- alert fingerprint (matches the spec convention "any active silence
-- pattern is a prefix of fingerprint = silenced").
--
-- expires_at is an absolute millisecond unix timestamp; the worker
-- compares against the injected clock so a silence created with a
-- finite duration auto-stops shielding once that wall-clock instant
-- passes — there is no explicit TTL sweep, the (tenant_id, expires_at)
-- index just keeps "ListActive" cheap.
CREATE TABLE silences (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id    INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  pattern      TEXT    NOT NULL,
  created_by   TEXT    NOT NULL,
  expires_at   INTEGER NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX idx_silences_tenant_expires ON silences(tenant_id, expires_at);
