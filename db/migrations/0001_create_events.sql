-- First PostgreSQL entity from the prototype plan. The PoC FileStore remains in use.
CREATE TABLE events (
    id text PRIMARY KEY,
    source_id text NOT NULL,
    source_event_id text NOT NULL,
    title text NOT NULL CHECK (length(trim(title)) > 0),
    start_at timestamptz NOT NULL,
    official_url text NOT NULL,
    source_url text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_id, source_event_id)
);
