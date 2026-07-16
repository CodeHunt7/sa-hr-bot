-- +goose Up

-- Reworked phase 1 (QUALIFICATION) tracks four different fields now:
-- current grade, target grade (still the existing `grade` column, used
-- by PickQuestion for question difficulty), the student's request for
-- the session, and a free-text self-assessment of strong/weak areas.
-- `direction` (industry/direction) is dropped: every student is already
-- a systems analyst, so that question no longer makes sense to ask.
-- `weak_topics` holds the comma-separated question_bank.topic values
-- the mini-audit (phase 2) flags as likely weak, used as a priority
-- filter in QUESTION_CYCLE's PickQuestion calls.
ALTER TABLE sessions ADD COLUMN current_grade TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN student_request TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN self_assessment TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN weak_topics TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions DROP COLUMN direction;

-- +goose Down
ALTER TABLE sessions ADD COLUMN direction TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions DROP COLUMN weak_topics;
ALTER TABLE sessions DROP COLUMN self_assessment;
ALTER TABLE sessions DROP COLUMN student_request;
ALTER TABLE sessions DROP COLUMN current_grade;
