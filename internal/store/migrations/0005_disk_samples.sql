-- Host disk trend: one row per hourly observation of the filesystem
-- holding Docker's data. The right-size forecast fits a line through
-- these rows to estimate days until the disk fills; the worker prunes
-- rows past the retention window on every sample.
CREATE TABLE host_disk_samples (
  ts INTEGER PRIMARY KEY,
  free_mb INTEGER NOT NULL,
  total_mb INTEGER NOT NULL);
