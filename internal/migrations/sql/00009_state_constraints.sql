-- +goose Up

-- Older versions only enforced the state machine in Go. Clean up a possible
-- legacy race before making the invariant permanent at the database level:
-- keep the newest active session and mark older active rows abandoned.
WITH ranked_active AS (
    SELECT id,
           row_number() OVER (PARTITION BY student_id ORDER BY started_at DESC, id DESC) AS position
    FROM sessions
    WHERE ended_at IS NULL
)
UPDATE sessions
SET status = 'ABANDONED', ended_at = now()
WHERE id IN (SELECT id FROM ranked_active WHERE position > 1);

ALTER TABLE sessions ADD CONSTRAINT sessions_status_check
    CHECK (status IN (
        'INSTRUCTION', 'QUALIFICATION', 'AUDIT', 'QUESTION_CYCLE',
        'SUMMARY', 'COMPLETED', 'ABANDONED'
    ));
ALTER TABLE sessions ADD CONSTRAINT sessions_grade_check
    CHECK (grade IN ('', 'джун', 'джун-мидл', 'мидл', 'мидл-сеньор', 'сеньор'));
ALTER TABLE sessions ADD CONSTRAINT sessions_next_topic_check
    CHECK (next_topic IN (
        '', '*', 'интеграции', 'архитектура', 'бд',
        'требования', 'безопасность', 'подача'
    ));

ALTER TABLE question_bank ADD CONSTRAINT question_bank_grade_check
    CHECK (grade IN ('джун', 'джун-мидл', 'мидл', 'мидл-сеньор', 'сеньор'));
ALTER TABLE question_bank ADD CONSTRAINT question_bank_topic_check
    CHECK (topic IN (
        'интеграции', 'архитектура', 'бд',
        'требования', 'безопасность', 'подача'
    ));

CREATE UNIQUE INDEX idx_sessions_one_active_per_student
    ON sessions (student_id)
    WHERE ended_at IS NULL;
CREATE INDEX idx_sessions_active_student
    ON sessions (student_id, started_at DESC)
    WHERE ended_at IS NULL;
CREATE INDEX idx_question_attempts_session_status
    ON question_attempts (session_id, status);

-- +goose Down
DROP INDEX idx_question_attempts_session_status;
DROP INDEX idx_sessions_active_student;
DROP INDEX idx_sessions_one_active_per_student;
ALTER TABLE question_bank DROP CONSTRAINT question_bank_topic_check;
ALTER TABLE question_bank DROP CONSTRAINT question_bank_grade_check;
ALTER TABLE sessions DROP CONSTRAINT sessions_next_topic_check;
ALTER TABLE sessions DROP CONSTRAINT sessions_grade_check;
ALTER TABLE sessions DROP CONSTRAINT sessions_status_check;
