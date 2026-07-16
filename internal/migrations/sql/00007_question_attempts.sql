-- +goose Up

ALTER TABLE sessions ADD COLUMN next_topic TEXT NOT NULL DEFAULT '';

CREATE TABLE question_attempts (
    id                BIGSERIAL PRIMARY KEY,
    session_id        BIGINT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    question_id       BIGINT NOT NULL REFERENCES question_bank (id) ON DELETE RESTRICT,
    primary_answer    TEXT NOT NULL DEFAULT '',
    primary_feedback  TEXT NOT NULL DEFAULT '',
    followup_question TEXT NOT NULL DEFAULT '',
    followup_answer   TEXT NOT NULL DEFAULT '',
    final_feedback    TEXT NOT NULL DEFAULT '',
    selected_vector   TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'WAITING_PRIMARY'
        CHECK (status IN ('WAITING_PRIMARY', 'WAITING_FOLLOWUP', 'WAITING_VECTOR', 'COMPLETED')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_question_attempts_session_id ON question_attempts (session_id);
CREATE UNIQUE INDEX idx_question_attempts_one_active_per_session
    ON question_attempts (session_id)
    WHERE status <> 'COMPLETED';

CREATE UNIQUE INDEX idx_session_summaries_one_per_session
    ON session_summaries (session_id);

ALTER TABLE weak_zones DROP CONSTRAINT weak_zones_status_check;
ALTER TABLE weak_zones ADD CONSTRAINT weak_zones_status_check
    CHECK (status IN ('hypothesis', 'confirmed', 'closed'));

-- +goose Down
ALTER TABLE weak_zones DROP CONSTRAINT weak_zones_status_check;
ALTER TABLE weak_zones ADD CONSTRAINT weak_zones_status_check
    CHECK (status IN ('hypothesis', 'confirmed'));
DROP INDEX idx_session_summaries_one_per_session;
DROP TABLE question_attempts;
ALTER TABLE sessions DROP COLUMN next_topic;
