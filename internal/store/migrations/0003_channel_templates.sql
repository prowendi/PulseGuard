-- ===== migrations/0003_channel_templates.sql =====
-- Phase B1: channel <-> template many-to-many.
--
-- Each channel now binds zero-or-more templates via the channel_templates
-- join table. Exactly one bound template per channel may carry
-- is_default = 1 — the default is used by push when the caller does not
-- pass ?template=<name>.
--
-- Migration is destructive: the legacy channels.template_id column is
-- copied into channel_templates as is_default=1 then dropped. Existing
-- data in dev databases is preserved; the user explicitly authorised
-- breaking schema changes (no data-migration tax).
--
-- modernc.org/sqlite 1.50 ships SQLite 3.46 so ALTER TABLE DROP COLUMN
-- is available natively.

CREATE TABLE channel_templates (
  channel_id  INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  template_id INTEGER NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
  is_default  INTEGER NOT NULL DEFAULT 0,
  sort_order  INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (channel_id, template_id)
);
CREATE UNIQUE INDEX uq_channel_templates_default
  ON channel_templates(channel_id)
  WHERE is_default = 1;
CREATE INDEX idx_channel_templates_template ON channel_templates(template_id);

-- Backfill: every existing channel.template_id becomes its default.
INSERT INTO channel_templates (channel_id, template_id, is_default, sort_order, created_at)
  SELECT id, template_id, 1, 0, COALESCE(updated_at, created_at)
    FROM channels
   WHERE template_id IS NOT NULL;

-- Drop the legacy single-template column.
ALTER TABLE channels DROP COLUMN template_id;
