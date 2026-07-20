-- +goose Up

-- The previous release stored two-answer cycles. They cannot be resumed
-- safely after changing the meaning of the attempt states, so close only
-- unfinished interview sessions while preserving all their history.
UPDATE sessions
SET status = 'ABANDONED', ended_at = now(), current_question_id = NULL
WHERE ended_at IS NULL AND status IN ('QUESTION_CYCLE', 'SUMMARY');

UPDATE question_attempts
SET status = 'COMPLETED', updated_at = now()
WHERE status <> 'COMPLETED'
  AND session_id IN (SELECT id FROM sessions WHERE status = 'ABANDONED');

ALTER TABLE question_attempts
    ADD COLUMN followup_feedback TEXT NOT NULL DEFAULT '',
    ADD COLUMN followup_2_question TEXT NOT NULL DEFAULT '',
    ADD COLUMN followup_2_answer TEXT NOT NULL DEFAULT '',
    ADD COLUMN followup_2_feedback TEXT NOT NULL DEFAULT '';

ALTER TABLE question_attempts DROP CONSTRAINT question_attempts_status_check;
ALTER TABLE question_attempts ADD CONSTRAINT question_attempts_status_check
    CHECK (status IN (
        'WAITING_PRIMARY', 'PRIMARY_FEEDBACK_READY', 'WAITING_FOLLOWUP',
        'FOLLOWUP_FEEDBACK_READY', 'WAITING_FOLLOWUP_2',
        'FINAL_FEEDBACK_READY', 'WAITING_VECTOR', 'COMPLETED'
    ));

-- +goose Down
UPDATE sessions
SET status = 'ABANDONED', ended_at = now(), current_question_id = NULL
WHERE ended_at IS NULL AND status IN ('QUESTION_CYCLE', 'SUMMARY');

UPDATE question_attempts
SET status = 'COMPLETED', updated_at = now()
WHERE status IN ('FOLLOWUP_FEEDBACK_READY', 'WAITING_FOLLOWUP_2');

ALTER TABLE question_attempts DROP CONSTRAINT question_attempts_status_check;
ALTER TABLE question_attempts ADD CONSTRAINT question_attempts_status_check
    CHECK (status IN (
        'WAITING_PRIMARY', 'PRIMARY_FEEDBACK_READY', 'WAITING_FOLLOWUP',
        'FINAL_FEEDBACK_READY', 'WAITING_VECTOR', 'COMPLETED'
    ));

ALTER TABLE question_attempts
    DROP COLUMN followup_2_feedback,
    DROP COLUMN followup_2_answer,
    DROP COLUMN followup_2_question,
    DROP COLUMN followup_feedback;
