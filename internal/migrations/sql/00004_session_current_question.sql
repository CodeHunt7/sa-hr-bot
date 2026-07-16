-- +goose Up

-- Tracks which question_bank row the student is currently expected to
-- answer during QUESTION_CYCLE. Without it, the handler had no way to
-- know which question an incoming answer belonged to (no message
-- history is persisted), so it re-picked a random question every turn
-- and graded the answer against the wrong one's reference answers.
-- ON DELETE SET NULL: if a bank question is ever removed while a
-- session points to it, the session shouldn't be blocked from
-- advancing, just fall back to picking a fresh question.
ALTER TABLE sessions
    ADD COLUMN current_question_id BIGINT REFERENCES question_bank (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE sessions DROP COLUMN current_question_id;
