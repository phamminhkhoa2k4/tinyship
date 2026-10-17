-- Rate limit for domain attaches: each attempt (successful or not) records
-- one row so a hostname cannot be re-attached more than the allowed
-- number of times per window; certificate authorities throttle hard and
-- a rate-limited name stalls for hours.

CREATE TABLE domain_add_attempts (
  hostname TEXT NOT NULL,
  at INTEGER NOT NULL);

CREATE INDEX idx_domain_add_attempts ON domain_add_attempts (hostname, at);
