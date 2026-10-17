-- TinyShip initial schema (SPEC §3). Authority: SPEC.md §3, which wins over
-- BACKLOG US-3.1 AC1 naming (app_env replaces a separate secrets table,
-- D-04: secrets are sealed, never stored in clear).
--
-- schema_migrations uses IF NOT EXISTS because the migration runner
-- bootstraps it before applying this file.

CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY, name TEXT NOT NULL,
  checksum TEXT NOT NULL, applied_at INTEGER NOT NULL);

CREATE TABLE settings (
  key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE registry_credentials (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  registry TEXT NOT NULL, username TEXT NOT NULL,
  secret_sealed BLOB NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE apps (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE CHECK (length(name) BETWEEN 3 AND 40),
  config_json TEXT NOT NULL,
  config_version INTEGER NOT NULL DEFAULT 1,
  desired_state TEXT NOT NULL DEFAULT 'running'
    CHECK (desired_state IN ('running','stopped')),
  current_deployment_id TEXT,
  registry_credential_id TEXT REFERENCES registry_credentials(id),
  deleting INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE app_env (
  app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('plain','secret')),
  value_plain TEXT, value_sealed BLOB,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (app_id, key),
  CHECK ((kind='plain'  AND value_plain IS NOT NULL AND value_sealed IS NULL)
      OR (kind='secret' AND value_sealed IS NOT NULL AND value_plain IS NULL)));

CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  app_id TEXT REFERENCES apps(id),
  type TEXT NOT NULL CHECK (type IN
    ('deploy','rollback','app_start','app_stop','app_delete',
     'domain_sync','gc','reconcile')),
  state TEXT NOT NULL DEFAULT 'queued' CHECK (state IN
    ('queued','running','succeeded','failed','cancelled')),
  payload_json TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 5,
  lease_owner TEXT, lease_until INTEGER,
  next_run_at INTEGER NOT NULL,
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  last_error TEXT, idempotency_key TEXT,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  finished_at INTEGER);
CREATE INDEX jobs_claim ON jobs(state, next_run_at);
CREATE UNIQUE INDEX jobs_idem ON jobs(app_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
-- Per-app lock: at most one running job per app (US-3.7), enforced at the DB
-- level so a claim-logic bug still cannot double-run an app.
CREATE UNIQUE INDEX jobs_one_running_per_app ON jobs(app_id)
  WHERE state = 'running' AND app_id IS NOT NULL;

CREATE TABLE deployments (
  id TEXT PRIMARY KEY,
  app_id TEXT NOT NULL REFERENCES apps(id),
  seq INTEGER NOT NULL,
  job_id TEXT NOT NULL REFERENCES jobs(id),
  kind TEXT NOT NULL CHECK (kind IN ('deploy','rollback')),
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN
    ('pending','running','succeeded','failed','superseded')),
  image_ref TEXT NOT NULL, image_digest TEXT,
  config_snapshot_json TEXT NOT NULL, config_version INTEGER NOT NULL,
  strategy_effective TEXT CHECK (strategy_effective IN
    ('start-first','stop-first')),
  host_port INTEGER, container_id TEXT, container_name TEXT,
  previous_deployment_id TEXT, rollback_of_id TEXT,
  failure_step TEXT, failure_code TEXT, failure_message TEXT,
  created_at INTEGER NOT NULL, started_at INTEGER, finished_at INTEGER,
  UNIQUE (app_id, seq));
CREATE UNIQUE INDEX deployments_port ON deployments(host_port)
  WHERE host_port IS NOT NULL AND state IN ('pending','running','succeeded');

CREATE TABLE deployment_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  ts INTEGER NOT NULL, step TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('start','ok','fail','skip')),
  detail_json TEXT);

CREATE TABLE domains (
  id TEXT PRIMARY KEY,
  app_id TEXT NOT NULL REFERENCES apps(id),
  hostname TEXT NOT NULL UNIQUE,
  is_primary INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending_dns'
    CHECK (status IN ('pending_dns','active','error')),
  created_at INTEGER NOT NULL);

CREATE TABLE container_state (
  app_id TEXT PRIMARY KEY REFERENCES apps(id),
  container_id TEXT, state TEXT, health TEXT,
  mem_usage_mb INTEGER, oom_kill_count INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL);

CREATE TABLE api_tokens (
  id TEXT PRIMARY KEY, name TEXT NOT NULL,
  token_hash TEXT NOT NULL, scope TEXT NOT NULL DEFAULT 'admin',
  created_at INTEGER NOT NULL, last_used_at INTEGER, revoked_at INTEGER);

CREATE TABLE audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL,
  target_type TEXT, target_id TEXT,
  result TEXT NOT NULL, detail_json TEXT);
CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit_events
  BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit_events
  BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
