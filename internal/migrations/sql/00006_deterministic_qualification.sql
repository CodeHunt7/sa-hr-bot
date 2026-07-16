-- +goose Up

-- Qualification is now a deterministic four-step flow controlled by code.
-- `grade` is reused for the single grade requested by the customer. The other
-- three answers get explicit columns instead of being inferred by the LLM and
-- squeezed into the previous current_grade/request/self_assessment fields.
ALTER TABLE sessions ADD COLUMN direction TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN experience TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN interview_target TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN qualification_step INT NOT NULL DEFAULT 0
    CHECK (qualification_step BETWEEN 0 AND 4);

-- +goose Down
ALTER TABLE sessions DROP COLUMN qualification_step;
ALTER TABLE sessions DROP COLUMN interview_target;
ALTER TABLE sessions DROP COLUMN experience;
ALTER TABLE sessions DROP COLUMN direction;
