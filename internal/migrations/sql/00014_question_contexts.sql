-- +goose Up
ALTER TABLE question_bank
    ADD COLUMN question_context TEXT NOT NULL DEFAULT '',
    ADD COLUMN followup_1_context TEXT NOT NULL DEFAULT '',
    ADD COLUMN followup_2_context TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE question_bank
    DROP COLUMN followup_2_context,
    DROP COLUMN followup_1_context,
    DROP COLUMN question_context;
