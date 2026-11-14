-- New field on a populated table; existing events remain 'unknown'.
ALTER TABLE events
    ADD COLUMN team_eligibility text NOT NULL DEFAULT 'unknown'
    CHECK (team_eligibility IN ('unknown', 'allowed', 'not_allowed'));

CREATE INDEX events_start_at_idx ON events (start_at, id);
