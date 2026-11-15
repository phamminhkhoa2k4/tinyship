-- Time-series resource samples: one row per observation while an app
-- runs. Raw rows are folded into hourly rollups (aggregated=1) once
-- older than the raw window, and rows past the per-app retention cap
-- are pruned by the worker's periodic sweep. The (app_id, ts) primary
-- key makes re-sampling idempotent, and ON DELETE CASCADE removes an
-- app's history with the app.
CREATE TABLE app_metric_samples (
  app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  ts INTEGER NOT NULL,
  aggregated INTEGER NOT NULL DEFAULT 0,
  cpu_percent REAL NOT NULL DEFAULT 0,
  mem_bytes INTEGER NOT NULL DEFAULT 0,
  mem_limit_bytes INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (app_id, ts));

CREATE INDEX app_metric_samples_ts ON app_metric_samples(ts);
