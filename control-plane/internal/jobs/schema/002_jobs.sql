-- Phase 2 job store (consumer job submission flow).
-- Shares the identity database; applied by internal/jobs at startup
-- (idempotent).

-- Pre-release: recreated because the initial TEXT[] definition shipped with
-- a scan incompatibility; only smoke-test rows existed. Bump to a proper
-- ALTER-based migration if jobs already exist on upgrade.
DROP TABLE IF EXISTS jobs CASCADE;

CREATE TABLE IF NOT EXISTS jobs (
    id         UUID PRIMARY KEY,
    user_id    UUID REFERENCES users(id),     -- NULL = owner/bootstrap submission
    name       TEXT NOT NULL DEFAULT '',
    image      TEXT NOT NULL,                 -- OCI image reference
    -- command/env stored as JSONB (not TEXT[]) because pgx's database/sql
    -- stdlib scans non-json types as strings, which cannot scan into
    -- []string/map[string]string; JSONB is round-tripped via encoding/json.
    command    JSONB NOT NULL DEFAULT '[]'::jsonb,
    env        JSONB NOT NULL DEFAULT '{}'::jsonb,
    cpu_cores  REAL NOT NULL DEFAULT 1,
    mem_bytes  BIGINT NOT NULL DEFAULT 1073741824,
    state      TEXT NOT NULL DEFAULT 'queued'
               CHECK (state IN ('queued','scheduling','running','completed','failed','cancelled')),
    node_id    TEXT NOT NULL DEFAULT '',      -- assigned node ("" while queued)
    task_id    TEXT NOT NULL DEFAULT '',      -- dispatched task id ("" until dispatch)
    error      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS jobs_user_idx ON jobs (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS jobs_state_idx ON jobs (state, created_at);
CREATE INDEX IF NOT EXISTS jobs_task_idx ON jobs (task_id) WHERE task_id <> '';
