-- Phase 1 identity layer for public access (per-contributor credentials).
-- Applied by internal/identity at startup (idempotent).

CREATE TABLE IF NOT EXISTS users (
    id           UUID PRIMARY KEY,
    email        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    role         TEXT NOT NULL CHECK (role IN ('owner', 'provider', 'consumer')),
    status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active', 'pending', 'disabled')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS invite_codes (
    code       TEXT PRIMARY KEY,              -- stored hashed (sha256 hex)
    role       TEXT NOT NULL CHECK (role IN ('provider', 'consumer')),
    created_by UUID REFERENCES users(id),
    max_uses   INT  NOT NULL DEFAULT 1,
    uses       INT  NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS node_tokens (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id),
    token_hash   TEXT NOT NULL UNIQUE,        -- sha256 hex of "nmt_<secret>"
    label        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active', 'revoked')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS node_tokens_user_idx ON node_tokens (user_id);

CREATE TABLE IF NOT EXISTS node_bindings (
    node_id  TEXT PRIMARY KEY,                -- registry node id (hostname-derived)
    user_id  UUID NOT NULL REFERENCES users(id),
    token_id UUID REFERENCES node_tokens(id),
    bound_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS access_requests (
    id           UUID PRIMARY KEY,
    email        TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    role         TEXT NOT NULL CHECK (role IN ('provider', 'consumer')),
    invite_code  TEXT,                        -- hashed; NULL = open request
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'approved', 'denied')),
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by   UUID REFERENCES users(id),
    decided_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS audit_log (
    id     BIGSERIAL PRIMARY KEY,
    ts     TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor  TEXT NOT NULL,                     -- user email, node id, or "system"
    action TEXT NOT NULL,                     -- e.g. "user.create", "auth.fail"
    object TEXT NOT NULL DEFAULT '',
    detail JSONB NOT NULL DEFAULT '{}'::jsonb,
    outcome TEXT NOT NULL DEFAULT 'ok'        -- ok | denied | error
);
CREATE INDEX IF NOT EXISTS audit_log_ts_idx ON audit_log (ts DESC);
CREATE INDEX IF NOT EXISTS audit_log_action_idx ON audit_log (action);
