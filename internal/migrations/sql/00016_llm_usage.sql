-- +goose Up
CREATE TABLE llm_usage (
    id                BIGSERIAL PRIMARY KEY,
    student_id        BIGINT NOT NULL REFERENCES students (telegram_id) ON DELETE CASCADE,
    session_id        BIGINT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    operation         TEXT NOT NULL,
    prompt_tokens     BIGINT NOT NULL CHECK (prompt_tokens >= 0),
    completion_tokens BIGINT NOT NULL CHECK (completion_tokens >= 0),
    total_tokens      BIGINT NOT NULL CHECK (total_tokens >= 0),
    cached_tokens     BIGINT NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_llm_usage_student_id ON llm_usage (student_id);
CREATE INDEX idx_llm_usage_session_id ON llm_usage (session_id);

-- +goose Down
DROP TABLE llm_usage;
