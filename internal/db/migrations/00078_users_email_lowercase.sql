-- +goose Up
-- Email addresses are matched case-insensitively from here on (setup lowercases, the
-- OAuth callback looks up with COLLATE NOCASE). Existing rows are lowercased where that
-- does not collide with another row's address in any case; a pair that differs only by
-- case is left as it is (both spellings, both accounts) and reported at boot, where
-- db.Migrate also adds the case-insensitive unique index once no such pair remains.
UPDATE users SET email = lower(email)
WHERE email != lower(email)
  AND NOT EXISTS (SELECT 1 FROM users u2 WHERE u2.id != users.id AND lower(u2.email) = lower(users.email));

-- +goose Down
DROP INDEX IF EXISTS idx_users_email_nocase;
