ALTER TABLE campaigns ADD COLUMN tracked_url TEXT;
ALTER TABLE campaigns ADD COLUMN frequency_messages INTEGER NOT NULL DEFAULT 1;
ALTER TABLE campaigns ADD COLUMN frequency_window TEXT NOT NULL DEFAULT '24h';
