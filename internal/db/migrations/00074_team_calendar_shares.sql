-- +goose Up
-- Share links for the embeddable team calendar (GET /embed/team-calendar?token=…).
-- Only the SHA-256 of the token is stored, as api_keys does; the plaintext is shown
-- once at creation. Revocation is a timestamp rather than a delete so the list keeps
-- a record of what was handed out. Admin-only to create and revoke.
CREATE TABLE team_calendar_shares (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);

-- +goose Down
DROP TABLE team_calendar_shares;
