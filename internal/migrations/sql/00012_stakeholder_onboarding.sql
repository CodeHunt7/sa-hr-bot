-- +goose Up

ALTER TABLE sessions ADD COLUMN strong_zones TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN weak_zones_input TEXT NOT NULL DEFAULT '';

-- Qualification steps 0..4 keep the same numeric range, but their meaning
-- changes to current grade, target grade, strong zones and weak zones. An
-- unfinished onboarding session from the previous release cannot be resumed
-- safely because its saved fields use the old meaning, so close only those
-- sessions and let the student start a clean one.
UPDATE sessions
SET status = 'ABANDONED', ended_at = now()
WHERE ended_at IS NULL
  AND status IN ('INSTRUCTION', 'QUALIFICATION', 'AUDIT');

ALTER TABLE sessions DROP CONSTRAINT sessions_status_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_status_check
    CHECK (status IN (
        'INSTRUCTION', 'QUALIFICATION', 'PROFILE_CONFIRMATION', 'KDIR_LESSON',
        'AUDIT', 'QUESTION_CYCLE', 'SUMMARY', 'COMPLETED', 'ABANDONED'
    ));

-- +goose Down
UPDATE sessions
SET status = 'ABANDONED', ended_at = now()
WHERE ended_at IS NULL
  AND status IN ('PROFILE_CONFIRMATION', 'KDIR_LESSON');

ALTER TABLE sessions DROP CONSTRAINT sessions_status_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_status_check
    CHECK (status IN (
        'INSTRUCTION', 'QUALIFICATION', 'AUDIT', 'QUESTION_CYCLE',
        'SUMMARY', 'COMPLETED', 'ABANDONED'
    ));

ALTER TABLE sessions DROP COLUMN weak_zones_input;
ALTER TABLE sessions DROP COLUMN strong_zones;
