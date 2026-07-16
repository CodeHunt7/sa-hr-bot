-- +goose Up

-- The FSM in internal/handlers discovers the candidate's grade and
-- direction during the QUALIFICATION phase and needs somewhere durable
-- to keep them: PickQuestion (QUESTION_CYCLE) filters the question bank
-- by grade, so it has to survive across independent Telegram updates
-- rather than live only in a single request's memory.
ALTER TABLE sessions ADD COLUMN grade TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN direction TEXT NOT NULL DEFAULT '';

-- 00001_init.sql defaulted status to 'in_progress', a value from the
-- placeholder schema that predates the INSTRUCTION/QUALIFICATION/AUDIT/
-- QUESTION_CYCLE/SUMMARY FSM. StartSession always sets status
-- explicitly, so this default is never actually relied on, but it
-- should still name a real FSM state rather than a stale one.
ALTER TABLE sessions ALTER COLUMN status SET DEFAULT 'INSTRUCTION';

-- +goose Down
ALTER TABLE sessions ALTER COLUMN status SET DEFAULT 'in_progress';
ALTER TABLE sessions DROP COLUMN direction;
ALTER TABLE sessions DROP COLUMN grade;
