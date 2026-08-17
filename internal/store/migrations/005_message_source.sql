-- Messages sent by the operator CLI never reached outbound_messages, so their
-- delivery webhooks were discarded and their revenue could not be attributed.
-- The CLI now records every attempt through the daemon; `source` distinguishes
-- those rows from daemon-scheduled work.
ALTER TABLE outbound_messages ADD COLUMN source TEXT NOT NULL DEFAULT 'daemon';

CREATE INDEX IF NOT EXISTS idx_messages_source ON outbound_messages(source, created_at);
