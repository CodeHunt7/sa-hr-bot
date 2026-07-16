-- +goose Up

-- Recreate the grade constraint so databases that already applied an early
-- version of 00009 also accept bridge grades left by the old LLM qualification.
ALTER TABLE sessions DROP CONSTRAINT sessions_grade_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_grade_check
    CHECK (grade IN ('', 'джун', 'джун-мидл', 'мидл', 'мидл-сеньор', 'сеньор'));

ALTER TABLE sessions ADD CONSTRAINT sessions_cycle_count_check
    CHECK (cycle_count >= 0);
ALTER TABLE sessions ADD CONSTRAINT sessions_terminal_lifecycle_check
    CHECK (
        (status IN ('COMPLETED', 'ABANDONED') AND ended_at IS NOT NULL)
        OR
        (status NOT IN ('COMPLETED', 'ABANDONED') AND ended_at IS NULL)
    );

-- +goose Down
ALTER TABLE sessions DROP CONSTRAINT sessions_terminal_lifecycle_check;
ALTER TABLE sessions DROP CONSTRAINT sessions_cycle_count_check;
ALTER TABLE sessions DROP CONSTRAINT sessions_grade_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_grade_check
    CHECK (grade IN ('', 'джун', 'джун-мидл', 'мидл', 'мидл-сеньор', 'сеньор'));
