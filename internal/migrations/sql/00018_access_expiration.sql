-- +goose Up

ALTER TABLE access_codes
    ADD COLUMN activated_at TIMESTAMPTZ,
    ADD COLUMN expires_at TIMESTAMPTZ;

-- Before this migration the student's creation time was also the moment when
-- their one-time code was consumed. Preserve that meaning for existing users.
UPDATE access_codes AS ac
SET activated_at = s.created_at,
    expires_at = s.created_at + INTERVAL '2 months'
FROM students AS s
WHERE ac.student_id = s.telegram_id
  AND ac.is_used = true;

CREATE INDEX idx_access_codes_expires_at
    ON access_codes (expires_at)
    WHERE is_used = true;

-- +goose Down

DROP INDEX idx_access_codes_expires_at;

ALTER TABLE access_codes
    DROP COLUMN expires_at,
    DROP COLUMN activated_at;
