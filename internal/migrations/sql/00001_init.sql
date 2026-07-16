-- +goose Up

-- access_codes and students reference each other (a student is created
-- from a code, and a code records which student consumed it), so
-- access_codes is created first without the FK to students, and the
-- FK is added afterwards once students exists.
CREATE TABLE access_codes (
    code       TEXT PRIMARY KEY,
    is_used    BOOLEAN NOT NULL DEFAULT false,
    student_id BIGINT
);

CREATE TABLE students (
    telegram_id BIGINT PRIMARY KEY,
    name        TEXT NOT NULL,
    access_code TEXT NOT NULL REFERENCES access_codes (code),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE access_codes
    ADD CONSTRAINT fk_access_codes_student
    FOREIGN KEY (student_id) REFERENCES students (telegram_id);

CREATE TABLE sessions (
    id          BIGSERIAL PRIMARY KEY,
    student_id  BIGINT NOT NULL REFERENCES students (telegram_id) ON DELETE CASCADE,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at    TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'in_progress',
    cycle_count INT NOT NULL DEFAULT 0
);

CREATE TABLE weak_zones (
    id         BIGSERIAL PRIMARY KEY,
    student_id BIGINT NOT NULL REFERENCES students (telegram_id) ON DELETE CASCADE,
    zone_text  TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'hypothesis' CHECK (status IN ('hypothesis', 'confirmed')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (student_id, zone_text)
);

CREATE TABLE question_bank (
    id               BIGSERIAL PRIMARY KEY,
    question_text    TEXT NOT NULL,
    grade            TEXT NOT NULL DEFAULT '',
    topic            TEXT NOT NULL DEFAULT '',
    reference_answer TEXT NOT NULL DEFAULT '',
    source           TEXT NOT NULL DEFAULT ''
);

CREATE TABLE session_summaries (
    id           BIGSERIAL PRIMARY KEY,
    session_id   BIGINT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    summary_text TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_access_codes_student_id ON access_codes (student_id);
CREATE INDEX idx_sessions_student_id ON sessions (student_id);
CREATE INDEX idx_weak_zones_student_id ON weak_zones (student_id);
CREATE INDEX idx_session_summaries_session_id ON session_summaries (session_id);

-- +goose Down
DROP TABLE session_summaries;
DROP TABLE question_bank;
DROP TABLE weak_zones;
DROP TABLE sessions;
ALTER TABLE access_codes DROP CONSTRAINT fk_access_codes_student;
DROP TABLE students;
DROP TABLE access_codes;
