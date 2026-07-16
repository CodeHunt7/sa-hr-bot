-- +goose Up

ALTER TABLE question_attempts DROP CONSTRAINT question_attempts_status_check;
ALTER TABLE question_attempts ADD CONSTRAINT question_attempts_status_check
    CHECK (status IN (
        'WAITING_PRIMARY', 'PRIMARY_FEEDBACK_READY', 'WAITING_FOLLOWUP',
        'FINAL_FEEDBACK_READY', 'WAITING_VECTOR', 'COMPLETED'
    ));

-- +goose Down
UPDATE question_attempts
SET status = 'WAITING_PRIMARY'
WHERE status = 'PRIMARY_FEEDBACK_READY';
UPDATE question_attempts
SET status = 'WAITING_FOLLOWUP'
WHERE status = 'FINAL_FEEDBACK_READY';
ALTER TABLE question_attempts DROP CONSTRAINT question_attempts_status_check;
ALTER TABLE question_attempts ADD CONSTRAINT question_attempts_status_check
    CHECK (status IN ('WAITING_PRIMARY', 'WAITING_FOLLOWUP', 'WAITING_VECTOR', 'COMPLETED'));
