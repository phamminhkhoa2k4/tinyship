-- Durable notification queue: producers insert rows best-effort (a
-- deploy must never wait on a webhook), the worker dispatcher claims
-- due rows, signs the payload and records the outcome. Dedup collapses
-- repeats of the same dedup_key inside the window into one row with a
-- growing dup_count; global send caps defer rather than drop.
CREATE TABLE notify_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  app_name TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '{}',
  dedup_key TEXT NOT NULL DEFAULT '',
  dup_count INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
    ('pending','delivered','failed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  sent_to TEXT NOT NULL DEFAULT '[]',
  next_attempt_at INTEGER NOT NULL,
  last_attempt_at INTEGER NOT NULL DEFAULT 0,
  delivered_at INTEGER,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL);

CREATE INDEX notify_events_due
  ON notify_events(status, next_attempt_at);
CREATE INDEX notify_events_dedup
  ON notify_events(dedup_key, created_at);
