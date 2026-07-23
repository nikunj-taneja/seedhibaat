ALTER TABLE orders ADD COLUMN return_recorded_at TEXT;
CREATE INDEX IF NOT EXISTS idx_orders_suppression ON orders(cancelled_at, refunded_at, return_recorded_at);
