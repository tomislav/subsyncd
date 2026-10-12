ALTER TABLE instances ADD COLUMN webhook_token TEXT NOT NULL DEFAULT '';
ALTER TABLE instances ADD COLUMN webhook_connection_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE instances ADD COLUMN webhook_connection_arr_url TEXT NOT NULL DEFAULT '';
ALTER TABLE instances ADD COLUMN webhook_checked_at_ns INTEGER NOT NULL DEFAULT 0;
