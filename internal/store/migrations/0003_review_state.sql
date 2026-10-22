-- 0003_review_state.sql
-- Durable per-MR review intent and pre-created workflow dossiers.
--
-- review_controls survives worker restarts and lets a newer push supersede a
-- workflow that is still running (Claim forbids two running jobs per key).
-- workflow_dossiers is written before the DAG workflow is created, so it has
-- no foreign key to workflows; job_id is the persistent job -> workflow map.

CREATE TABLE review_controls (
  project_id INTEGER NOT NULL,
  mr_iid     INTEGER NOT NULL,
  generation INTEGER NOT NULL DEFAULT 0,  -- incremented on each accepted run/cancel
  head_sha   TEXT    NOT NULL DEFAULT '', -- last requested head; cancel keeps it
  cancelled  INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT    NOT NULL,
  PRIMARY KEY (project_id, mr_iid)
);

CREATE TABLE workflow_dossiers (
  workflow_id       TEXT    PRIMARY KEY,
  job_id            TEXT,                       -- NULL for non-job workflows
  generation        INTEGER NOT NULL DEFAULT 0, -- review generation at creation
  version           INTEGER NOT NULL,
  hash              TEXT    NOT NULL,           -- SHA-256 of uncompressed JSON
  data              BLOB    NOT NULL,           -- gzip(JSON), caller-sanitized
  publication_state TEXT    NOT NULL DEFAULT 'pending', -- pending | published | skipped
  created_at        TEXT    NOT NULL,
  updated_at        TEXT    NOT NULL
);

-- One workflow per job; a retried job must reuse the original mapping.
CREATE UNIQUE INDEX workflow_dossiers_job ON workflow_dossiers (job_id)
  WHERE job_id IS NOT NULL;

-- Completed-but-unpublished recovery scan; separate from ListUnfinished.
CREATE INDEX workflow_dossiers_publication
  ON workflow_dossiers (publication_state, updated_at);
