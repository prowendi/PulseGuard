-- ===== migrations/0006_channel_template_conditions.sql =====
-- V3.B1+: per-binding condition expression for auto-routing.
--
-- Each row in channel_templates can now carry a tiny `field op value`
-- expression evaluated by internal/condeval against the incoming push
-- payload. The pipeline worker walks each channel's bindings ordered
-- by sort_order and picks the first whose condition matches; if none
-- match it falls back to the is_default=1 binding.
--
-- The column is NOT NULL with empty-string default so every existing
-- row reads as the "no condition / default-eligible" sentinel. This
-- keeps the behaviour of pre-0006 databases identical: payloads with
-- no _template_id resolve to the is_default binding exactly as before.

ALTER TABLE channel_templates ADD COLUMN condition TEXT NOT NULL DEFAULT '';
